package acpagent

import (
	"context"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func (c *Client) beginSessionOperation(active *activePrompt) error {
	if active == nil || active.sessionID == "" {
		return errSessionIDRequired
	}
	if c.closing.Load() {
		return ErrClientClosed
	}
	select {
	case <-c.closed:
		return ErrClientClosed
	default:
	}

	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closing.Load() {
		return ErrClientClosed
	}
	if c.activeBySession[active.sessionID] != nil {
		return ErrSessionOperationActive
	}
	c.activeBySession[active.sessionID] = active
	active.admitted = true
	return nil
}

func (c *Client) endSessionOperation(active *activePrompt) {
	if active == nil || !active.admitted {
		return
	}
	active.releaseOnce.Do(func() {
		c.stateMu.Lock()
		if c.activeBySession[active.sessionID] == active {
			delete(c.activeBySession, active.sessionID)
		}
		c.stateMu.Unlock()
		c.finishSessionUpdates(active)
	})
}

func (c *Client) finishSessionUpdates(active *activePrompt) {
	if active == nil || active.updates == nil {
		return
	}
	active.finishOnce.Do(func() {
		select {
		case c.deactivate <- active:
		case <-c.closed:
			<-c.dispatchDone
			closeActivePrompt(active)
		}
	})
}

func closeActivePrompt(active *activePrompt) {
	if active == nil {
		return
	}
	active.closeOnce.Do(func() {
		if active.updates != nil {
			close(active.updates)
		}
	})
}

func (c *Client) clearActive(sessionID acp.SessionId) {
	c.stateMu.Lock()
	active := c.activeBySession[sessionID]
	delete(c.activeBySession, sessionID)
	c.stateMu.Unlock()
	if active != nil {
		closeActivePrompt(active)
	}
}

func (c *Client) logLastChunkInSeries(sessionID acp.SessionId) {
	c.stateMu.Lock()
	active := c.activeBySession[sessionID]
	var last *loggedACPChunk
	sessionLogger := c.logger
	if active != nil && active.lastChunk != nil {
		chunkCopy := *active.lastChunk
		last = &chunkCopy
	}
	if active != nil {
		sessionLogger = active.logger
	}
	c.stateMu.Unlock()
	if last == nil {
		return
	}

	logEvent := sessionLogger.Debug().
		Str("acp_session_id", string(sessionID)).
		Str("update_kind", last.kind).
		Bool("partial", last.partial).
		Bool("thought", last.thought).
		Bool("last_in_series", true)
	logEvent.Msg("received last acp chunk in series")
	if sessionLogger.enabled(levelTrace) && last.contentBlock != nil {
		sessionLogger.Trace().
			Str("acp_session_id", string(sessionID)).
			Interface("acp_content_block", last.contentBlock).
			Msg("received last acp chunk metadata")
	}
}

func (c *Client) failAll(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		close(c.closed)
		<-c.dispatchDone
		c.closeAllActiveSessions()
	})
}

func (c *Client) enqueueUpdateFromWire(ext ExtendedSessionNotification) {
	select {
	case c.updates <- ext:
	default:
		c.logger.Warn().Str("acp_session_id", string(ext.SessionId)).Msg("dropping ordered wire update due to full buffer")
	}
}

func (c *Client) dispatchUpdates() {
	defer close(c.dispatchDone)
	for {
		select {
		case <-c.closed:
			return
		case active := <-c.deactivate:
			c.closePromptUpdates(active)
		case ext := <-c.updates:
			c.dispatchSessionUpdate(ext)
		}
	}
}

func (c *Client) closePromptUpdates(active *activePrompt) {
	if active == nil {
		return
	}
	c.stateMu.Lock()
	active.updatesDone = true
	c.stateMu.Unlock()
	closeActivePrompt(active)
}

func (c *Client) closeActiveSession(sessionID acp.SessionId) {
	c.stateMu.Lock()
	active := c.activeBySession[sessionID]
	delete(c.activeBySession, sessionID)
	c.stateMu.Unlock()
	if active != nil {
		active.closeOnce.Do(func() {
			if active.updates != nil {
				close(active.updates)
			}
		})
	}
}

func (c *Client) closeAllActiveSessions() {
	c.stateMu.Lock()
	active := make([]*activePrompt, 0, len(c.activeBySession))
	for sessionID, prompt := range c.activeBySession {
		active = append(active, prompt)
		delete(c.activeBySession, sessionID)
	}
	c.stateMu.Unlock()
	for _, prompt := range active {
		if prompt == nil {
			continue
		}
		closeActivePrompt(prompt)
	}
}

func (c *Client) dispatchSessionUpdate(ext ExtendedSessionNotification) {
	updateType := sessionUpdateKind(ext.Update)
	c.stateMu.Lock()
	active := c.activeBySession[ext.SessionId]
	if active != nil && active.updates != nil && !active.updatesDone {
		active.lastChunk = loggedACPChunkFromUpdate(ext.Update)
	} else {
		active = nil
	}
	c.stateMu.Unlock()

	sessionLogger := c.logger
	if active != nil {
		sessionLogger = active.logger
	}
	if sessionLogger.enabled(levelTrace) {
		logEvent := sessionLogger.Trace().
			Str("acp_session_id", string(ext.SessionId)).
			Str("update_kind", updateType)

		if updateType == unknownValue {
			logEvent = logEvent.Int("raw_update_bytes", len(ext.Raw))
		}

		logACPUpdateContentFields(logEvent, ext.Update)
		logACPUpdateChunkFields(logEvent, ext.Update)
		logEvent.Msg("received acp session update")
	}

	if active == nil || active.updates == nil {
		return
	}
	select {
	case active.updates <- ext:
		select {
		case active.signal <- struct{}{}:
		default:
		}
	case <-c.closed:
	}
}

func waitForUpdateIdle(ctx context.Context, signal <-chan struct{}) {
	timer := time.NewTimer(idleUpdateWindow)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-signal:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleUpdateWindow)
		case <-timer.C:
			return
		}
	}
}
