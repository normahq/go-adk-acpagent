package acpagent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/adk/session"
)

func TestReadonlyInvocationContextNil(t *testing.T) {
	t.Parallel()

	ctx := readonlyInvocationContext{}
	if deadline, ok := ctx.Deadline(); ok || !deadline.IsZero() {
		t.Fatalf("Deadline() = (%v, %v), want zero false", deadline, ok)
	}
	if ctx.Done() != nil {
		t.Fatal("Done() != nil, want nil")
	}
	if ctx.Err() != nil {
		t.Fatal("Err() != nil, want nil")
	}
	if got := ctx.Value("key"); got != nil {
		t.Fatalf("Value() = %#v, want nil", got)
	}
	if ctx.UserContent() != nil {
		t.Fatal("UserContent() != nil, want nil")
	}
	if ctx.InvocationID() != "" || ctx.AgentName() != "" || ctx.UserID() != "" || ctx.AppName() != "" || ctx.SessionID() != "" || ctx.Branch() != "" {
		t.Fatalf("readonlyInvocationContext returned non-empty identity fields: %#v", ctx)
	}
	if _, err := ctx.ReadonlyState().Get("missing"); !errors.Is(err, session.ErrStateKeyNotExist) {
		t.Fatalf("ReadonlyState().Get() error = %v, want ErrStateKeyNotExist", err)
	}
	for key, value := range ctx.ReadonlyState().All() {
		t.Fatalf("ReadonlyState().All() yielded (%q, %#v), want empty", key, value)
	}
}

func TestClientCapabilityAccessors(t *testing.T) {
	t.Parallel()

	client := &Client{}
	if client.SupportsSessionLoad() {
		t.Fatal("SupportsSessionLoad() = true, want false")
	}
	if client.SupportsSessionResume() {
		t.Fatal("SupportsSessionResume() = true, want false")
	}

	client.agentCaps.LoadSession = true
	client.agentCaps.SessionCapabilities.Resume = &acp.SessionResumeCapabilities{}
	if !client.SupportsSessionLoad() {
		t.Fatal("SupportsSessionLoad() = false, want true")
	}
	if !client.SupportsSessionResume() {
		t.Fatal("SupportsSessionResume() = false, want true")
	}
}

func TestClientAuthenticateIgnoresEmptyMethod(t *testing.T) {
	t.Parallel()

	if err := (&Client{}).Authenticate(context.Background(), " \t "); err != nil {
		t.Fatalf("Authenticate(empty) error = %v, want nil", err)
	}
}

func TestClientSessionRestoreRejectsEmptySessionID(t *testing.T) {
	t.Parallel()

	client := &Client{logger: newLogger(nil, "")}
	if _, err := client.ResumeSession(context.Background(), " \t ", "/tmp", nil); !errors.Is(err, errSessionIDRequired) {
		t.Fatalf("ResumeSession(empty) error = %v, want errSessionIDRequired", err)
	}
	if _, err := client.LoadSession(context.Background(), "", "/tmp", nil); !errors.Is(err, errSessionIDRequired) {
		t.Fatalf("LoadSession(empty) error = %v, want errSessionIDRequired", err)
	}
	if _, err := client.LoadSessionWithMeta(context.Background(), "\n", "/tmp", nil, map[string]any{"x": 1}); !errors.Is(err, errSessionIDRequired) {
		t.Fatalf("LoadSessionWithMeta(empty) error = %v, want errSessionIDRequired", err)
	}
}

func TestClientPromptWithContentValidation(t *testing.T) {
	t.Parallel()

	client := &Client{logger: newLogger(nil, "")}
	if _, _, err := client.PromptWithContent(context.Background(), "session-1", nil); !errors.Is(err, errPromptContentReq) {
		t.Fatalf("PromptWithContent(empty prompt) error = %v, want errPromptContentReq", err)
	}
	if _, _, err := client.PromptWithContent(context.Background(), " ", []acp.ContentBlock{acp.TextBlock("hi")}); !errors.Is(err, errSessionIDRequired) {
		t.Fatalf("PromptWithContent(empty session) error = %v, want errSessionIDRequired", err)
	}
}

func TestClientCloseErrorHelpers(t *testing.T) {
	t.Parallel()

	if !isBenignStdinCloseErr(os.ErrClosed) {
		t.Fatal("isBenignStdinCloseErr(os.ErrClosed) = false, want true")
	}
	if !isBenignStdinCloseErr(errors.New("file already closed")) {
		t.Fatal("isBenignStdinCloseErr(file already closed) = false, want true")
	}
	if isBenignStdinCloseErr(errors.New("broken pipe")) {
		t.Fatal("isBenignStdinCloseErr(broken pipe) = true, want false")
	}
}

func TestLoggerErrorWritesRecord(t *testing.T) {
	t.Parallel()

	var buf testLogBuffer
	testLogger(&buf, 0).Error().Err(errors.New("boom")).Str("key", "value").Msg("failed")
	got := buf.String()
	if !strings.Contains(got, `"msg":"failed"`) || !strings.Contains(got, `"key":"value"`) || !strings.Contains(got, `"error":"boom"`) {
		t.Fatalf("logged record = %q, want message and attributes", got)
	}
}

func TestClientUnsupportedCallbacksReturnMethodNotFound(t *testing.T) {
	t.Parallel()

	client := &Client{}
	tests := []struct {
		name string
		call func() error
	}{
		{name: "read text file", call: func() error {
			_, err := client.ReadTextFile(context.Background(), acp.ReadTextFileRequest{})
			return err
		}},
		{name: "write text file", call: func() error {
			_, err := client.WriteTextFile(context.Background(), acp.WriteTextFileRequest{})
			return err
		}},
		{name: "create terminal", call: func() error {
			_, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{})
			return err
		}},
		{name: "kill terminal", call: func() error {
			_, err := client.KillTerminal(context.Background(), acp.KillTerminalRequest{})
			return err
		}},
		{name: "terminal output", call: func() error {
			_, err := client.TerminalOutput(context.Background(), acp.TerminalOutputRequest{})
			return err
		}},
		{name: "release terminal", call: func() error {
			_, err := client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{})
			return err
		}},
		{name: "wait for terminal exit", call: func() error {
			_, err := client.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{})
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(); err == nil {
				t.Fatal("callback error = nil, want method not found")
			}
		})
	}
}

func TestRequestPermissionFallbacks(t *testing.T) {
	t.Parallel()

	rejectResp, err := (&Client{logger: newLogger(nil, "")}).RequestPermission(context.Background(), acp.RequestPermissionRequest{
		SessionId: "session-1",
		Options: []acp.PermissionOption{
			{OptionId: "allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		t.Fatalf("RequestPermission(reject option) error = %v", err)
	}
	if rejectResp.Outcome.Selected == nil || rejectResp.Outcome.Selected.OptionId != "reject" {
		t.Fatalf("RequestPermission(reject option) outcome = %#v", rejectResp.Outcome)
	}

	cancelResp, err := (&Client{logger: newLogger(nil, "")}).RequestPermission(context.Background(), acp.RequestPermissionRequest{
		SessionId: "session-1",
		Options:   []acp.PermissionOption{{OptionId: "allow", Kind: acp.PermissionOptionKindAllowOnce}},
	})
	if err != nil {
		t.Fatalf("RequestPermission(cancel) error = %v", err)
	}
	if cancelResp.Outcome.Cancelled == nil {
		t.Fatalf("RequestPermission(cancel) outcome = %#v, want cancelled", cancelResp.Outcome)
	}

	if got := permissionOutcomeLabel(acp.RequestPermissionOutcome{}); got != unknownValue {
		t.Fatalf("permissionOutcomeLabel(empty) = %q, want %q", got, unknownValue)
	}
}

func TestClientActiveSessionHelpers(t *testing.T) {
	t.Parallel()

	sessionID := acp.SessionId("session-1")
	active := &activePrompt{
		sessionID: sessionID,
		updates:   make(chan ExtendedSessionNotification, 1),
		signal:    make(chan struct{}, 1),
		logger:    newLogger(nil, ""),
	}
	client := &Client{
		logger:          newLogger(nil, ""),
		activeBySession: map[acp.SessionId]*activePrompt{sessionID: active},
	}

	client.dispatchSessionUpdate(ExtendedSessionNotification{
		SessionNotification: acp.SessionNotification{
			SessionId: sessionID,
			Update:    acp.UpdateAgentMessageText("hi"),
		},
	})
	if active.lastChunk == nil || active.lastChunk.kind != "agent_message_chunk" {
		t.Fatalf("lastChunk = %#v, want agent message chunk", active.lastChunk)
	}
	select {
	case got := <-active.updates:
		if got.SessionId != sessionID {
			t.Fatalf("dispatched SessionId = %q, want %q", got.SessionId, sessionID)
		}
	default:
		t.Fatal("active update was not dispatched")
	}
	select {
	case <-active.signal:
	default:
		t.Fatal("active signal was not sent")
	}

	client.closeActiveSession(sessionID)
	if _, ok := client.activeBySession[sessionID]; ok {
		t.Fatal("active session still present after closeActiveSession")
	}
	if _, ok := <-active.updates; ok {
		t.Fatal("active updates channel open after closeActiveSession")
	}
}

func TestClientCloseAllActiveSessions(t *testing.T) {
	t.Parallel()

	sessionID := acp.SessionId("session-1")
	active := &activePrompt{updates: make(chan ExtendedSessionNotification)}
	client := &Client{activeBySession: map[acp.SessionId]*activePrompt{
		sessionID: active,
		"nil":     nil,
	}}
	client.closeAllActiveSessions()
	if len(client.activeBySession) != 0 {
		t.Fatalf("activeBySession length = %d, want 0", len(client.activeBySession))
	}
	if _, ok := <-active.updates; ok {
		t.Fatal("active updates channel open after closeAllActiveSessions")
	}
}

func TestClientUpdateClassificationHelpers(t *testing.T) {
	t.Parallel()

	status := acp.ToolCallStatusPending
	updateTests := []struct {
		name   string
		update acp.SessionUpdate
		want   string
	}{
		{name: "agent", update: acp.UpdateAgentMessageText("hi"), want: "agent_message_chunk"},
		{name: "user", update: acp.UpdateUserMessageText("hi"), want: "user_message_chunk"},
		{name: "thought", update: acp.UpdateAgentThoughtText("think"), want: "agent_thought_chunk"},
		{name: "tool call", update: acp.StartToolCall("tool-1", "run"), want: "tool_call"},
		{name: "tool update", update: acp.UpdateToolCall("tool-1", acp.WithUpdateStatus(status)), want: "tool_call_update"},
		{name: "plan", update: acp.UpdatePlan(acp.PlanEntry{Content: "step"}), want: "plan"},
		{name: "commands", update: acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{}}, want: "available_commands_update"},
		{name: "mode", update: acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{}}, want: "current_mode_update"},
		{name: "config", update: acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{}}, want: "config_option_update"},
		{name: "session info", update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}}, want: "session_info_update"},
		{name: "usage", update: acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{}}, want: "usage_update"},
		{name: "unknown", update: acp.SessionUpdate{}, want: unknownValue},
	}
	for _, tc := range updateTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sessionUpdateKind(tc.update); got != tc.want {
				t.Fatalf("sessionUpdateKind() = %q, want %q", got, tc.want)
			}
		})
	}

	chunkTests := []struct {
		name    string
		update  acp.SessionUpdate
		want    string
		thought bool
	}{
		{name: "agent", update: acp.UpdateAgentMessageText("hi"), want: "agent_message_chunk"},
		{name: "thought", update: acp.UpdateAgentThoughtText("think"), want: "agent_thought_chunk", thought: true},
		{name: "user", update: acp.UpdateUserMessageText("hi"), want: "user_message_chunk"},
	}
	for _, tc := range chunkTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chunk := loggedACPChunkFromUpdate(tc.update)
			if chunk == nil || chunk.kind != tc.want || !chunk.partial || chunk.thought != tc.thought {
				t.Fatalf("loggedACPChunkFromUpdate() = %#v, want kind %q thought %v", chunk, tc.want, tc.thought)
			}
		})
	}
	if got := loggedACPChunkFromUpdate(acp.SessionUpdate{}); got != nil {
		t.Fatalf("loggedACPChunkFromUpdate(unknown) = %#v, want nil", got)
	}
}

func TestClientQueueAndMarshalHelpers(t *testing.T) {
	t.Parallel()

	client := &Client{
		logger:  newLogger(nil, ""),
		updates: make(chan ExtendedSessionNotification, 1),
	}
	first := ExtendedSessionNotification{SessionNotification: acp.SessionNotification{SessionId: "one"}}
	second := ExtendedSessionNotification{SessionNotification: acp.SessionNotification{SessionId: "two"}}
	client.enqueueUpdateFromWire(first)
	client.enqueueUpdateFromWire(second)
	if got := <-client.updates; got.SessionId != "one" {
		t.Fatalf("queued SessionId = %q, want one", got.SessionId)
	}

	if sessionID, ok := extractPromptScopedSessionID([]byte(`{"sessionId":" session-1 "}`)); !ok || sessionID != "session-1" {
		t.Fatalf("extractPromptScopedSessionID(sessionId) = (%q, %v), want session-1 true", sessionID, ok)
	}
	if sessionID, ok := extractPromptScopedSessionID([]byte(`{"threadId":" thread-1 "}`)); !ok || sessionID != "thread-1" {
		t.Fatalf("extractPromptScopedSessionID(threadId) = (%q, %v), want thread-1 true", sessionID, ok)
	}
	if sessionID, ok := extractPromptScopedSessionID([]byte(`{`)); ok || sessionID != "" {
		t.Fatalf("extractPromptScopedSessionID(invalid) = (%q, %v), want empty false", sessionID, ok)
	}
	if got := mustMarshalJSON(func() {}); got != nil {
		t.Fatalf("mustMarshalJSON(unmarshalable) = %q, want nil", got)
	}
}
