package acpagent

import (
	"context"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func (c *Client) clearActive(sessionID acp.SessionId) {
	select {
	case c.deactivate <- sessionID:
	case <-c.closed:
		<-c.dispatchDone
		c.closeActiveSession(sessionID)
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
	if last.contentBlock != nil {
		logEvent = logEvent.Interface("acp_content_block", last.contentBlock)
	}
	logEvent.Msg("received last acp chunk in series")
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
		case sessionID := <-c.deactivate:
			c.closeActiveSession(sessionID)
		case ext := <-c.updates:
			c.dispatchSessionUpdate(ext)
		}
	}
}

func (c *Client) closeActiveSession(sessionID acp.SessionId) {
	c.stateMu.Lock()
	active := c.activeBySession[sessionID]
	delete(c.activeBySession, sessionID)
	c.stateMu.Unlock()
	if active != nil {
		active.closeOnce.Do(func() {
			close(active.updates)
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
		prompt.closeOnce.Do(func() {
			close(prompt.updates)
		})
	}
}

func (c *Client) dispatchSessionUpdate(ext ExtendedSessionNotification) {
	updateType := sessionUpdateKind(ext.Update)
	c.stateMu.Lock()
	active := c.activeBySession[ext.SessionId]
	if active != nil {
		active.lastChunk = loggedACPChunkFromUpdate(ext.Update)
	}
	c.stateMu.Unlock()

	sessionLogger := c.logger
	if active != nil {
		sessionLogger = active.logger
	}
	logEvent := sessionLogger.Trace().
		Str("acp_session_id", string(ext.SessionId)).
		Str("update_kind", updateType)

	if updateType == unknownValue {
		logEvent = logEvent.RawJSON("raw_update", ext.Raw)
	}

	logACPUpdateContentFields(logEvent, ext.Update)
	logACPUpdateChunkFields(logEvent, ext.Update)
	logEvent.Msg("received acp session update")

	if active == nil {
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
