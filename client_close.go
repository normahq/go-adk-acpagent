package acpagent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func closeClientAfterError(client *Client, err error, closeMsg string) error {
	if closeErr := client.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("%s: %w", closeMsg, closeErr))
	}
	return err
}

// Close shuts down the owned ACP subprocess. It allows 200ms for graceful exit,
// then kills the immediate child and waits up to 5s for cleanup. If cleanup
// times out, the original reaper retains ownership until Wait completes.
// Repeated and concurrent calls return the same result.
func (c *Client) Close() error {
	c.shutdownOnce.Do(func() { c.shutdownErr = c.closeProcess() })
	return c.shutdownErr
}

func (c *Client) closeProcess() error {
	c.stateMu.Lock()
	c.closing.Store(true)
	c.stateMu.Unlock()
	if c.cancelProcess != nil {
		defer c.cancelProcess()
	}
	if err := c.stdin.Close(); err != nil && !isBenignStdinCloseErr(err) {
		c.logger.Warn().Err(err).Msg("failed to close stdin")
	}
	select {
	case <-c.closed:
		return c.finalizeCloseErr()
	case <-time.After(200 * time.Millisecond):
	}
	if c.cancelProcess != nil {
		c.cancelProcess()
	}
	if c.cmd.Process != nil {
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			c.logger.Warn().Err(err).Msg("failed to kill acp process")
		}
	}
	select {
	case <-c.closed:
		return c.finalizeCloseErr()
	case <-time.After(5 * time.Second):
		return errors.New("acp client cleanup timeout: process wait did not finish within 5s after kill")
	}
}

func (c *Client) waitLoop() {
	err := c.cmd.Wait()
	if err != nil {
		if c.closing.Load() {
			c.logger.Debug().Err(err).Msg("acp process exited during close")
			c.failAll(io.EOF)
			return
		}
		// If the context was cancelled, this is an expected termination.
		if c.ctx.Err() != nil {
			c.logger.Debug().Err(err).Msg("acp process exited due to context cancellation")
			c.failAll(c.ctx.Err())
			return
		}
		c.logger.Warn().Err(err).Msg("acp process exited with error")
		c.failAll(fmt.Errorf("acp process exit: %w", err))
		return
	}
	c.logger.Debug().Msg("acp process exited")
	c.failAll(io.EOF)
}

func (c *Client) finalizeCloseErr() error {
	if c.closeErr != nil && !errors.Is(c.closeErr, io.EOF) {
		return fmt.Errorf("acp client close: %w", c.closeErr)
	}
	return nil
}

func isBenignStdinCloseErr(err error) bool {
	return errors.Is(err, os.ErrClosed) || strings.Contains(strings.ToLower(err.Error()), "file already closed")
}
