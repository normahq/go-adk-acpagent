package acpagent

import (
	"context"

	acp "github.com/coder/acp-go-sdk"
)

// RequestPermission handles ACP permission requests from the server.
func (c *Client) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	l := c.loggerForContext(ctx)
	title := ""
	if params.ToolCall.Title != nil {
		title = *params.ToolCall.Title
	}
	l.Debug().
		Str("acp_session_id", string(params.SessionId)).
		Str("title", title).
		Int("option_count", len(params.Options)).
		Msg("received acp permission request")

	if c.permissionHandler != nil {
		resp, err := c.permissionHandler(ctx, params)
		if err != nil {
			l.Error().Err(err).Str("acp_session_id", string(params.SessionId)).Msg("permission handler failed")
			return acp.RequestPermissionResponse{}, err
		}
		l.Debug().
			Str("acp_session_id", string(params.SessionId)).
			Str("outcome", permissionOutcomeLabel(resp.Outcome)).
			Msg("permission handler returned")
		return resp, nil
	}

	for _, option := range params.Options {
		if option.Kind == acp.PermissionOptionKindRejectOnce || option.Kind == acp.PermissionOptionKindRejectAlways {
			resp := acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}
			l.Debug().
				Str("acp_session_id", string(params.SessionId)).
				Str("option_id", string(option.OptionId)).
				Str("option_kind", string(option.Kind)).
				Msg("permission auto-selected reject option")
			return resp, nil
		}
	}

	resp := acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}
	l.Debug().Str("acp_session_id", string(params.SessionId)).Msg("permission auto-cancelled")
	return resp, nil
}

// SessionUpdate is part of the ACP client callback contract.
func (c *Client) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	l := c.loggerForContext(ctx)
	logEvent := l.Trace().
		Str("acp_session_id", string(params.SessionId)).
		Str("update_kind", sessionUpdateKind(params.Update))
	logACPUpdateContentFields(logEvent, params.Update)
	logACPUpdateChunkFields(logEvent, params.Update)
	logEvent.Msg("received acp session update callback")
	return nil
}

// ReadTextFile reports unsupported file read for this ACP client.
func (c *Client) ReadTextFile(_ context.Context, _ acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}

// WriteTextFile reports unsupported file write for this ACP client.
func (c *Client) WriteTextFile(_ context.Context, _ acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}

// CreateTerminal reports unsupported terminal creation for this ACP client.
func (c *Client) CreateTerminal(_ context.Context, _ acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

// KillTerminal reports unsupported terminal command control for this ACP client.
func (c *Client) KillTerminal(_ context.Context, _ acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

// TerminalOutput reports unsupported terminal output streaming for this ACP client.
func (c *Client) TerminalOutput(_ context.Context, _ acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

// ReleaseTerminal reports unsupported terminal release for this ACP client.
func (c *Client) ReleaseTerminal(_ context.Context, _ acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

// WaitForTerminalExit reports unsupported terminal wait operations for this ACP client.
func (c *Client) WaitForTerminalExit(_ context.Context, _ acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

func permissionOutcomeLabel(outcome acp.RequestPermissionOutcome) string {
	switch {
	case outcome.Selected != nil:
		return "selected"
	case outcome.Cancelled != nil:
		return "cancelled"
	default:
		return unknownValue
	}
}
