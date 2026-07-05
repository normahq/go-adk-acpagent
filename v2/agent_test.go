package acpagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/normahq/go-adk-acpagent/v2/acperror"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const (
	testACPCallID     = "call-1"
	testACPToolID     = "tool-1"
	testACPPlanPrompt = "planning"

	testSessionOneID    = "session-1"
	testSessionOneHello = "session-1:hello"
	testSessionOneOne   = "session-1:one"
	testSessionOneTwo   = "session-1:two"
	testSessionTwoTwo   = "session-2:two"
)

func instructionPrompt(instruction string, prompt string) string {
	return strings.TrimSpace(instruction) + "\n\nUser message:\n" + prompt
}

func expectedPromptsJSON(t *testing.T, prompts ...string) string {
	t.Helper()
	raw, err := json.Marshal(prompts)
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}
	return string(raw)
}

func testSlogLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

func testLogger(w io.Writer, level slog.Level) logger {
	return newLogger(testSlogLogger(w, level), "")
}

type testLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *testLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *testLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *testLogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func TestMapACPAgentMessageChunkCopiesProviderErrorMetadata(t *testing.T) {
	t.Parallel()

	ev, ok := mapACPAgentMessageChunk(t.Context(), newLogger(nil, ""), "inv-1", &acp.SessionUpdateAgentMessageChunk{
		Content: acp.TextBlock("Error: quota exceeded"),
		Meta: map[string]any{
			"provider_error": map[string]any{
				"kind":       "quota_exceeded",
				"message":    "quota exceeded",
				"request_id": "req-1",
			},
		},
	})
	if !ok {
		t.Fatal("mapACPAgentMessageChunk() ok = false, want true")
	}
	got, ok := acperror.FromADKMetadata(ev.CustomMetadata)
	if !ok {
		t.Fatalf("provider error metadata missing: %#v", ev.CustomMetadata)
	}
	if got.Kind != acperror.KindQuotaExceeded {
		t.Fatalf("Kind = %q, want %q", got.Kind, acperror.KindQuotaExceeded)
	}
	if got.RequestID != "req-1" {
		t.Fatalf("RequestID = %q, want req-1", got.RequestID)
	}
	if got := ev.ErrorCode; got != "acp_provider_error:quota_exceeded" {
		t.Fatalf("ErrorCode = %q, want acp_provider_error:quota_exceeded", got)
	}
	if got := ev.ErrorMessage; got != "provider error quota_exceeded: quota exceeded" {
		t.Fatalf("ErrorMessage = %q, want provider error quota_exceeded: quota exceeded", got)
	}
}

func TestClientPromptReceivesUpdates(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(context.Background(), t.TempDir(), nil)

	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	updates, resultCh, err := client.Prompt(context.Background(), string(sess.SessionId), "hello")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}

	var chunks []string
	for note := range updates {
		ev, ok := mapACPUpdateToEvent(t.Context(), newLogger(nil, ""), "inv-1", ExtendedSessionNotification{SessionNotification: note.SessionNotification, Raw: note.Raw})
		if ok {
			if text := extractPromptText(ev.Content); text != "" {
				chunks = append(chunks, text)
			}
		}
	}
	result := <-resultCh
	if result.Err != nil {
		t.Fatalf("PromptResult.Err = %v", result.Err)
	}
	if result.Response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("StopReason = %q, want %q", result.Response.StopReason, acp.StopReasonEndTurn)
	}
	got := strings.Join(chunks, "")
	want := string(sess.SessionId) + ":hello"
	if got != want {
		t.Fatalf("joined chunks = %q, want %q", got, want)
	}
}

func TestClientCreateSessionSetsModel(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_MODEL": "openai/gpt-5.4",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(context.Background(), t.TempDir(), "openai/gpt-5.4", "", nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
}

func TestClientCreateSessionIgnoresSetModelUnsupported(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_DISABLE_SET_MODEL": "1",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(context.Background(), t.TempDir(), "openai/gpt-5.4", "", nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
}

func TestClientCreateSessionFailsOnSetModelRequestError(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_MODEL": "different/model",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	_, err = client.CreateSession(context.Background(), t.TempDir(), "openai/gpt-5.4", "", nil)
	if err == nil {
		t.Fatal("CreateSession() error = nil, want set model error")
	}
}

func TestClientCreateSessionSetsMode(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_MODE": "code",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(context.Background(), t.TempDir(), "", "code", nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
}

func TestClientCreateSessionIgnoresSetModeUnsupported(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_DISABLE_SET_MODE": "1",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(context.Background(), t.TempDir(), "", "code", nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
}

func TestClientCreateSessionFailsOnSetModeRequestError(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_MODE": "different-mode",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	_, err = client.CreateSession(context.Background(), t.TempDir(), "", "code", nil)
	if err == nil {
		t.Fatal("CreateSession() error = nil, want set mode error")
	}
}

func TestIsACPSessionNotFoundError(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "message contains not found",
			err: &acp.RequestError{
				Code:    -32000,
				Message: "Requested entity was not found.",
			},
			want: true,
		},
		{
			name: "data string contains session not found",
			err: &acp.RequestError{
				Code:    -32602,
				Message: "Invalid params",
				Data:    "session not found",
			},
			want: true,
		},
		{
			name: "data object contains session not found",
			err: &acp.RequestError{
				Code:    -32602,
				Message: "Invalid params",
				Data:    map[string]any{"detail": "session not found"},
			},
			want: true,
		},
		{
			name: "invalid params without stale session text",
			err: &acp.RequestError{
				Code:    -32602,
				Message: "Invalid params",
				Data:    "workspace not found",
			},
			want: false,
		},
		{
			name: "wrapped invalid thread id data",
			err: &acp.RequestError{
				Code:    -32603,
				Message: "Internal error",
				Data: map[string]any{
					"error": "thread/resume: bridge backend rpc error (-32600): invalid thread id: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `s` at 1",
				},
			},
			want: true,
		},
		{
			name: "wrapped invalid session id data",
			err: &acp.RequestError{
				Code:    -32603,
				Message: "Internal error",
				Data: map[string]any{
					"error": "thread/resume: bridge backend rpc error (-32600): invalid session id: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `s` at 1",
				},
			},
			want: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isACPSessionNotFoundError(tc.err); got != tc.want {
				t.Fatalf("isACPSessionNotFoundError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsACPSessionAlreadyExistsError(t *testing.T) {
	err := &acp.RequestError{
		Code:    -32602,
		Message: "Invalid params",
		Data:    `session "019e8707-094e-7723-8aa4-ab45abe89d51" already exists`,
	}
	if !isACPSessionAlreadyExistsError(err) {
		t.Fatalf("isACPSessionAlreadyExistsError(%v) = false, want true", err)
	}
}

func TestClientSuppressesPeerDisconnectInfoByDefault(t *testing.T) {
	var stderr bytes.Buffer
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommand(t),
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if _, err := client.NewSession(context.Background(), t.TempDir(), nil); err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	_ = client.Close()
	if got := stderr.String(); strings.Contains(got, "peer connection closed") {
		t.Fatalf("stderr contains peer disconnect noise: %q", got)
	}
}

func TestWireLogBufferSuppressesWirePayloadInDebug(t *testing.T) {
	var logBuf testLogBuffer
	logger := testLogger(&logBuf, slog.LevelDebug)

	buf := newWireLogBuffer("send", logger, nil)
	buf.logLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`))

	if got := logBuf.String(); strings.Contains(got, "acp wire") {
		t.Fatalf("debug log unexpectedly contains trace-only wire payload: %q", got)
	}
}

func TestWireLogBufferEmitsWirePayloadInTrace(t *testing.T) {
	var logBuf testLogBuffer
	logger := testLogger(&logBuf, levelTrace)

	buf := newWireLogBuffer("recv", logger, nil)
	buf.logLine([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))

	got := logBuf.String()
	if !strings.Contains(got, "acp wire") {
		t.Fatalf("trace log missing wire payload marker: %q", got)
	}
	if !strings.Contains(got, `"direction":"recv"`) {
		t.Fatalf("trace log missing direction field: %q", got)
	}
}

func TestClientCloseSuppressesExpectedProcessExitWarnings(t *testing.T) {
	var logBuf testLogBuffer
	logger := testSlogLogger(&logBuf, slog.LevelDebug)

	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommand(t),
		Stderr:  io.Discard,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if _, err := client.NewSession(context.Background(), t.TempDir(), nil); err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	got := logBuf.String()
	if strings.Contains(got, "acp process exited with error") {
		t.Fatalf("log unexpectedly contains process exit warning: %q", got)
	}
	if strings.Contains(got, "failed to kill acp process") {
		t.Fatalf("log unexpectedly contains kill warning: %q", got)
	}
	if strings.Contains(got, "failed to close stdin") {
		t.Fatalf("log unexpectedly contains stdin close warning: %q", got)
	}
}

func TestClientHandlesPermissionRequest(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommand(t),
		PermissionHandler: func(_ context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(req.Options[0].OptionId)}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(context.Background(), t.TempDir(), nil)

	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	updates, resultCh, err := client.Prompt(context.Background(), string(sess.SessionId), "permission")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}

	var chunks []string
	for note := range updates {
		ev, ok := mapACPUpdateToEvent(t.Context(), newLogger(nil, ""), "inv-1", ExtendedSessionNotification{SessionNotification: note.SessionNotification, Raw: note.Raw})
		if ok {
			if text := extractPromptText(ev.Content); text != "" {
				chunks = append(chunks, text)
			}
		}
	}
	result := <-resultCh
	if result.Err != nil {
		t.Fatalf("PromptResult.Err = %v", result.Err)
	}
	if got := strings.Join(chunks, ""); got != "approved" {
		t.Fatalf("joined chunks = %q, want approved", got)
	}
}

func TestClientInitializeUsesDefaultIdentity(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_CLIENT_NAME":    "runtime-acpagent",
			"GO_EXPECT_CLIENT_VERSION": "dev",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func TestClientInitializeUsesConfiguredIdentity(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_CLIENT_NAME":    "custom-acp-client",
			"GO_EXPECT_CLIENT_VERSION": "1.2.3",
		}),
		ClientName:    "custom-acp-client",
		ClientVersion: "1.2.3",
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func TestNewClientDefaultsNilContext(t *testing.T) {
	var ctx context.Context
	client, err := NewClient(ctx, ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient(nil, cfg) error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func TestClientPromptAllowsConcurrentDifferentSessions(t *testing.T) {
	const (
		wantSession1 = testSessionOneOne
		wantSession2 = testSessionTwoTwo
	)

	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess1, err := client.NewSession(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	sess2, err := client.NewSession(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	updates1, resultCh1, err := client.Prompt(context.Background(), string(sess1.SessionId), "slow:one")
	if err != nil {
		t.Fatalf("Prompt(session1) error = %v", err)
	}
	updates2, resultCh2, err := client.Prompt(context.Background(), string(sess2.SessionId), "two")
	if err != nil {
		t.Fatalf("Prompt(session2) error = %v", err)
	}

	got1 := readPromptOutput(t, updates1, resultCh1)
	got2 := readPromptOutput(t, updates2, resultCh2)
	if got1 != wantSession1 {
		t.Fatalf("session1 output = %q, want %q", got1, wantSession1)
	}
	if got2 != wantSession2 {
		t.Fatalf("session2 output = %q, want %q", got2, wantSession2)
	}
}

func TestClientPromptRejectsConcurrentSameSession(t *testing.T) {
	const wantSession1 = testSessionOneOne

	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(context.Background(), t.TempDir(), nil)

	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	updates1, resultCh1, err := client.Prompt(context.Background(), string(sess.SessionId), "slow:one")
	if err != nil {
		t.Fatalf("Prompt(first) error = %v", err)
	}
	if _, _, err := client.Prompt(context.Background(), string(sess.SessionId), "two"); !errors.Is(err, ErrPromptAlreadyActive) {
		t.Fatalf("Prompt(second) error = %v, want %v", err, ErrPromptAlreadyActive)
	}

	got1 := readPromptOutput(t, updates1, resultCh1)
	if got1 != wantSession1 {
		t.Fatalf("session output = %q, want %q", got1, wantSession1)
	}
}

func TestClientPromptValidatesInputs(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	testCases := []struct {
		name      string
		sessionID string
		prompt    string
		wantErr   error
	}{
		{
			name:      "missing session id",
			sessionID: " ",
			prompt:    "prompt",
			wantErr:   errSessionIDRequired,
		},
		{
			name:      "missing prompt",
			sessionID: "session-1",
			prompt:    " ",
			wantErr:   errPromptRequired,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, gotErr := client.Prompt(context.Background(), tc.sessionID, tc.prompt)
			if !errors.Is(gotErr, tc.wantErr) {
				t.Fatalf("Prompt() error = %v, want %v", gotErr, tc.wantErr)
			}
		})
	}
}

func TestClientSessionUpdateCallbackLogsContentBlock(t *testing.T) {
	var logBuf testLogBuffer
	logger := testLogger(&logBuf, levelTrace)

	client := &Client{logger: logger}
	err := client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1",
		Update: acp.SessionUpdate{
			UserMessageChunk: &acp.SessionUpdateUserMessageChunk{
				Content: acp.ContentBlock{},
			},
		},
	})
	if err != nil {
		t.Fatalf("SessionUpdate() error = %v", err)
	}

	got := logBuf.String()
	if !strings.Contains(got, "received acp session update callback") {
		t.Fatalf("debug log = %q, want callback message", got)
	}
	if !strings.Contains(got, "\"acp_content_block\":{\"type\":\"unknown\"}") {
		t.Fatalf("debug log = %q, want structured content block payload", got)
	}
	if !strings.Contains(got, "\"update_kind\":\"user_message_chunk\"") {
		t.Fatalf("debug log = %q, want update kind", got)
	}
	if !strings.Contains(got, "\"partial\":true") {
		t.Fatalf("debug log = %q, want partial flag", got)
	}
	if !strings.Contains(got, "\"thought\":false") {
		t.Fatalf("debug log = %q, want thought flag", got)
	}
}

func TestClientLogsSessionUpdateAtTraceOnly(t *testing.T) {
	var logBuf testLogBuffer
	logger := testLogger(&logBuf, slog.LevelDebug)

	client := &Client{
		logger: logger,
	}

	ext := ExtendedSessionNotification{
		SessionNotification: acp.SessionNotification{
			SessionId: "session-1",
			Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
					Content: acp.ContentBlock{},
				},
			},
		},
	}
	client.dispatchSessionUpdate(ext)

	got := logBuf.String()
	if strings.Contains(got, "received acp session update") {
		t.Fatalf("debug log unexpectedly contains trace-only session update: %q", got)
	}
}

func TestClientLogsSessionUpdateAtTrace(t *testing.T) {
	var logBuf testLogBuffer
	logger := testLogger(&logBuf, levelTrace)

	client := &Client{
		logger: logger,
	}

	ext := ExtendedSessionNotification{
		SessionNotification: acp.SessionNotification{
			SessionId: "session-1",
			Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
					Content: acp.ContentBlock{},
				},
			},
		},
	}
	client.dispatchSessionUpdate(ext)

	got := logBuf.String()
	if !strings.Contains(got, "received acp session update") {
		t.Fatalf("trace log missing session update message: %q", got)
	}
}

func TestClientLogsLastChunkInSeries(t *testing.T) {
	var logBuf testLogBuffer
	logger := testLogger(&logBuf, slog.LevelDebug)

	client := &Client{
		logger: logger,
		activeBySession: map[acp.SessionId]*activePrompt{
			"session-1": {
				sessionID: "session-1",
				logger:    logger,
				lastChunk: &loggedACPChunk{
					kind:         "agent_thought_chunk",
					contentBlock: map[string]any{"type": "unknown"},
					partial:      true,
					thought:      true,
				},
			},
		},
	}

	client.logLastChunkInSeries("session-1")

	got := logBuf.String()
	if !strings.Contains(got, "received last acp chunk in series") {
		t.Fatalf("debug log = %q, want last chunk message", got)
	}
	if !strings.Contains(got, "\"last_in_series\":true") {
		t.Fatalf("debug log = %q, want last_in_series flag", got)
	}
	if !strings.Contains(got, "\"thought\":true") {
		t.Fatalf("debug log = %q, want thought flag", got)
	}
	if !strings.Contains(got, "\"update_kind\":\"agent_thought_chunk\"") {
		t.Fatalf("debug log = %q, want update kind", got)
	}
}

func TestRequestPermissionPassesContextToHandler(t *testing.T) {
	type key string
	const ctxKey key = "ctx-key"
	const ctxVal = "ctx-value"

	var seen string
	c := &Client{
		logger: newLogger(nil, ""),
		permissionHandler: func(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			seen, _ = ctx.Value(ctxKey).(string)
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
		},
	}

	_, err := c.RequestPermission(context.WithValue(context.Background(), ctxKey, ctxVal), acp.RequestPermissionRequest{
		SessionId: "session-1",
		Options:   []acp.PermissionOption{},
	})
	if err != nil {
		t.Fatalf("RequestPermission() error = %v", err)
	}
	if seen != ctxVal {
		t.Fatalf("permission handler context value = %q, want %q", seen, ctxVal)
	}
}

func TestAgentReusesRemoteSession(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("one", genai.RoleUser), agent.RunConfig{}))
	second := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("two", genai.RoleUser), agent.RunConfig{}))

	if first != testSessionOneOne {
		t.Fatalf("first final text = %q, want session-1:one", first)
	}
	if second != testSessionOneTwo {
		t.Fatalf("second final text = %q, want session-1:two", second)
	}
}

func TestAgentBeforeAgentCallbacksShortCircuitACPPrompt(t *testing.T) {
	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1",
		}),
		WorkingDir: t.TempDir(),
		BeforeAgentCallbacks: []agent.BeforeAgentCallback{
			func(agent.Context) (*genai.Content, error) {
				return genai.NewContentFromText("before-callback", genai.RoleModel), nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectEventTexts(t, r.Run(
		context.Background(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("hello", genai.RoleUser),
		agent.RunConfig{},
	))
	if !reflect.DeepEqual(got, []string{"before-callback"}) {
		t.Fatalf("event texts = %#v, want %#v", got, []string{"before-callback"})
	}
}

func TestAgentAfterAgentCallbacksEmitPostRunEvent(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
		AfterAgentCallbacks: []agent.AfterAgentCallback{
			func(agent.Context) (*genai.Content, error) {
				return genai.NewContentFromText("after-callback", genai.RoleModel), nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectEventTexts(t, r.Run(
		context.Background(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("hello", genai.RoleUser),
		agent.RunConfig{},
	))
	want := []string{"session-1:", "hello", testSessionOneHello, "after-callback"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event texts = %#v, want %#v", got, want)
	}
}

func TestAgentUsesWorkingDirAsDefaultSessionCWD(t *testing.T) {
	workingDir := t.TempDir()
	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": workingDir,
		}),
		WorkingDir: workingDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentUsesSessionStateCWDOverride(t *testing.T) {
	defaultWorkingDir := t.TempDir()
	overrideWorkingDir := t.TempDir()
	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": overrideWorkingDir,
		}),
		WorkingDir: defaultWorkingDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": overrideWorkingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentInjectsSessionStateIntoInstruction(t *testing.T) {
	workingDir := t.TempDir()
	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": workingDir,
			"GO_EXPECT_PROMPTS":     expectedPromptsJSON(t, instructionPrompt("project=relay cwd="+workingDir, "hello")),
		}),
		WorkingDir:  workingDir,
		Instruction: "project={project} cwd={cwd}",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd":     workingDir,
			"project": "relay",
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	want := testSessionOneHello
	if got != want {
		t.Fatalf("final text = %q, want %q", got, want)
	}
}

func TestAgentInstructionProviderSkipsTemplateInjection(t *testing.T) {
	workingDir := t.TempDir()
	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": workingDir,
			"GO_EXPECT_PROMPTS":     expectedPromptsJSON(t, instructionPrompt("provider {project}", "hello")),
		}),
		WorkingDir: workingDir,
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return "provider {project}", nil
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd":     workingDir,
			"project": "relay",
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	want := "session-1:hello"
	if got != want {
		t.Fatalf("final text = %q, want %q", got, want)
	}
}

func TestAgentPrependsInstructionsOnlyOncePerADKSession(t *testing.T) {
	workingDir := t.TempDir()
	expectedPrompts := expectedPromptsJSON(
		t,
		instructionPrompt("project=relay cwd="+workingDir, "one"),
		"two",
	)

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": workingDir,
			"GO_EXPECT_PROMPTS":     expectedPrompts,
		}),
		WorkingDir:  workingDir,
		Instruction: "project={project} cwd={cwd}",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd":     workingDir,
			"project": "relay",
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("one", genai.RoleUser), agent.RunConfig{}))
	if first != testSessionOneOne {
		t.Fatalf("first final text = %q, want session-1:one", first)
	}
	second := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("two", genai.RoleUser), agent.RunConfig{}))
	if second != testSessionOneTwo {
		t.Fatalf("second final text = %q, want session-1:two", second)
	}
}

func TestAgentPrependsInstructionsPerADKSession(t *testing.T) {
	workingDir := t.TempDir()
	expectedPrompts := expectedPromptsJSON(
		t,
		instructionPrompt("project=relay cwd="+workingDir, "one"),
		instructionPrompt("project=relay cwd="+workingDir, "two"),
	)

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": workingDir,
			"GO_EXPECT_PROMPTS":     expectedPrompts,
		}),
		WorkingDir:  workingDir,
		Instruction: "project={project} cwd={cwd}",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	firstSession, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd":     workingDir,
			"project": "relay",
		},
	})
	if err != nil {
		t.Fatalf("Create(firstSession) error = %v", err)
	}
	secondSession, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd":     workingDir,
			"project": "relay",
		},
	})
	if err != nil {
		t.Fatalf("Create(secondSession) error = %v", err)
	}

	first := collectFinalText(t, r.Run(context.Background(), "test-user", firstSession.Session.ID(), genai.NewContentFromText("one", genai.RoleUser), agent.RunConfig{}))
	if first != testSessionOneOne {
		t.Fatalf("first final text = %q, want session-1:one", first)
	}
	second := collectFinalText(t, r.Run(context.Background(), "test-user", secondSession.Session.ID(), genai.NewContentFromText("two", genai.RoleUser), agent.RunConfig{}))
	if second != testSessionTwoTwo {
		t.Fatalf("second final text = %q, want session-2:two", second)
	}
}

func TestAgentFailsWhenInstructionTemplateRequiresMissingState(t *testing.T) {
	assertInstructionTemplateRunError(t, "missing={not_set}", "inject session state into instruction")
}

func TestAgentInjectsOptionalMissingStateIntoInstruction(t *testing.T) {
	assertInstructionTemplatePrependAndRun(
		t,
		"optional={missing?}",
		expectedPromptsJSON(t, instructionPrompt("optional=", "hello")),
		map[string]any{},
	)
}

func TestAgentLeavesInvalidStateNamesLiteralInInstruction(t *testing.T) {
	assertInstructionTemplatePrependAndRun(
		t,
		"invalid={invalid-key} prefix={invalid:key}",
		expectedPromptsJSON(t, instructionPrompt("invalid={invalid-key} prefix={invalid:key}", "hello")),
		map[string]any{},
	)
}

func TestAgentInjectsPrefixedStateIntoInstruction(t *testing.T) {
	assertInstructionTemplatePrependAndRun(
		t,
		"prefixed={app:user_name}",
		expectedPromptsJSON(t, instructionPrompt("prefixed=Foo", "hello")),
		map[string]any{"app:user_name": "Foo"},
	)
}

func TestAgentFailsWhenInstructionTemplateRequiresArtifactWithoutService(t *testing.T) {
	assertInstructionTemplateRunError(t, "artifact={artifact.my_file}", "artifact service is not initialized")
}

func TestAgentInjectsArtifactsIntoInstruction(t *testing.T) {
	workingDir := t.TempDir()
	expectedPrompts := expectedPromptsJSON(t, instructionPrompt("artifact=artifact-content optional=", "hello"))

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": workingDir,
			"GO_EXPECT_PROMPTS":     expectedPrompts,
		}),
		WorkingDir:  workingDir,
		Instruction: "artifact={artifact.my_file} optional={artifact.other_file?}",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	artifactService := artifact.InMemoryService()
	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:           "test-app",
		Agent:             a,
		SessionService:    sessionService,
		ArtifactService:   artifactService,
		AutoCreateSession: false,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err = artifactService.Save(context.Background(), &artifact.SaveRequest{
		AppName:   "test-app",
		UserID:    "test-user",
		SessionID: sess.Session.ID(),
		FileName:  "my_file",
		Part:      &genai.Part{Text: "artifact-content"},
	})
	if err != nil {
		t.Fatalf("artifact.Save() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func assertInstructionTemplatePrependAndRun(
	t *testing.T,
	instruction string,
	expectedPrompts string,
	extraState map[string]any,
) {
	t.Helper()

	workingDir := t.TempDir()
	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": workingDir,
			"GO_EXPECT_PROMPTS":     expectedPrompts,
		}),
		WorkingDir:  workingDir,
		Instruction: instruction,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}

	state := map[string]any{"cwd": workingDir}
	for k, v := range extraState {
		state[k] = v
	}

	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State:   state,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func assertInstructionTemplateRunError(t *testing.T, instruction string, wantErrSubstring string) {
	t.Helper()

	workingDir := t.TempDir()
	a, err := New(Config{
		Context:     context.Background(),
		Command:     helperCommandWithEnv(t, map[string]string{"GO_EXPECT_SESSION_CWD": workingDir}),
		WorkingDir:  workingDir,
		Instruction: instruction,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var runErr error
	for _, err := range r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		t.Fatalf("run error = nil, want %q", wantErrSubstring)
	}
	if !strings.Contains(runErr.Error(), wantErrSubstring) {
		t.Fatalf("run error = %q, want %q", runErr, wantErrSubstring)
	}
}

func TestAgentNormalizesRelativeSessionStateCWDOverride(t *testing.T) {
	defaultWorkingDir := t.TempDir()
	overrideWorkingDir := t.TempDir()
	currentWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	relativeOverride, err := filepath.Rel(currentWorkingDir, overrideWorkingDir)
	if err != nil {
		t.Fatalf("filepath.Rel() error = %v", err)
	}
	if strings.TrimSpace(relativeOverride) == "" {
		t.Fatal("relative override cwd is empty")
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": overrideWorkingDir,
		}),
		WorkingDir: defaultWorkingDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": relativeOverride,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentFailsOnInvalidSessionStateCWDOverride(t *testing.T) {
	defaultWorkingDir := t.TempDir()
	missingWorkingDir := filepath.Join(t.TempDir(), "missing")
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: defaultWorkingDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": missingWorkingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var runErr error
	for _, err := range r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		t.Fatal("run error = nil, want invalid cwd error")
	}
	if got := runErr.Error(); !strings.Contains(got, "stat acp session cwd") {
		t.Fatalf("run error = %q, want invalid cwd message", got)
	}
}

func TestAgentForwardsSessionStateMetaToSessionNew(t *testing.T) {
	workingDir := t.TempDir()
	meta := map[string]any{
		"codex": map[string]any{
			"approvalMode": "manual",
		},
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("json.Marshal(meta) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD":          workingDir,
			"GO_EXPECT_NEW_SESSION_META_RAW": string(metaJSON),
		}),
		WorkingDir: workingDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"meta": meta,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentAddsInstructionsToCodexSessionMeta(t *testing.T) {
	workingDir := t.TempDir()
	expectedMetaRaw, err := json.Marshal(map[string]any{
		"codex": map[string]any{
			"baseInstructions":      "global base",
			"developerInstructions": "developer guide",
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(expected meta) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD":          workingDir,
			"GO_EXPECT_NEW_SESSION_META_RAW": string(expectedMetaRaw),
			"GO_EXPECT_PROMPTS":              expectedPromptsJSON(t, instructionPrompt("global base\n\ndeveloper guide", "hello")),
		}),
		WorkingDir:         workingDir,
		GlobalInstruction:  "global base",
		Instruction:        "developer guide",
		SystemInstructions: "deprecated guide",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentAddsReasoningEffortToCodexSessionMeta(t *testing.T) {
	workingDir := t.TempDir()
	expectedMetaRaw, err := json.Marshal(map[string]any{
		"codex": map[string]any{
			"config": map[string]any{
				"model_reasoning_effort": "high",
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(expected meta) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD":          workingDir,
			"GO_EXPECT_NEW_SESSION_META_RAW": string(expectedMetaRaw),
		}),
		WorkingDir:      workingDir,
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentPreservesExistingCodexConfigWhenAddingReasoningEffort(t *testing.T) {
	workingDir := t.TempDir()
	meta := map[string]any{
		"codex": map[string]any{
			"approvalMode": "manual",
			"config": map[string]any{
				"profile": "team",
			},
		},
	}
	expectedMetaRaw, err := json.Marshal(map[string]any{
		"codex": map[string]any{
			"approvalMode": "manual",
			"config": map[string]any{
				"profile":                "team",
				"model_reasoning_effort": "medium",
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(expected meta) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD":          workingDir,
			"GO_EXPECT_NEW_SESSION_META_RAW": string(expectedMetaRaw),
		}),
		WorkingDir:      workingDir,
		ReasoningEffort: "medium",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"meta": meta,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentPreservesExplicitCodexInstructionMeta(t *testing.T) {
	workingDir := t.TempDir()
	meta := map[string]any{
		"codex": map[string]any{
			"approvalMode":          "manual",
			"baseInstructions":      "state base",
			"developerInstructions": "state developer",
		},
	}
	expectedMetaRaw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("json.Marshal(expected meta) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD":          workingDir,
			"GO_EXPECT_NEW_SESSION_META_RAW": string(expectedMetaRaw),
			"GO_EXPECT_PROMPTS":              expectedPromptsJSON(t, instructionPrompt("config base\n\nconfig developer", "hello")),
		}),
		WorkingDir:        workingDir,
		GlobalInstruction: "config base",
		Instruction:       "config developer",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"meta": meta,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentFailsWhenCodexInstructionMetaIsNotObject(t *testing.T) {
	workingDir := t.TempDir()
	a, err := New(Config{
		Context:     context.Background(),
		Command:     helperCommandWithEnv(t, map[string]string{"GO_EXPECT_SESSION_CWD": workingDir}),
		WorkingDir:  workingDir,
		Instruction: "developer guide",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"meta": map[string]any{
					"codex": "manual",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var runErr error
	for _, err := range r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		t.Fatal("run error = nil, want codex meta object error")
	}
	if got := runErr.Error(); !strings.Contains(got, "acp session meta codex must be an object") {
		t.Fatalf("run error = %q, want codex meta object error", got)
	}
}

func TestAgentResumesSessionFromStateAndPersistsSessionState(t *testing.T) {
	workingDir := t.TempDir()
	sessionID := "session-resume-1"
	meta := map[string]any{
		"codex": map[string]any{
			"approvalMode": "manual",
		},
	}
	expectedPromptsRaw, err := json.Marshal([]string{"hello"})
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_SUPPORT_SESSION_RESUME": "1",
			"GO_FAIL_IF_RESUME_CALLED":  "1",
			"GO_EXPECT_PROMPTS":         string(expectedPromptsRaw),
		}),
		WorkingDir: workingDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"session_id": sessionID,
				"meta":       meta,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalText, finalSessionState := collectFinalTextAndSessionState(
		t,
		r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if finalText != sessionID+":hello" {
		t.Fatalf("final text = %q, want %q", finalText, sessionID+":hello")
	}
	if got := finalSessionState["session_id"]; got != sessionID {
		t.Fatalf("final %s.session_id = %v, want %q", SessionStateKey, got, sessionID)
	}
	if got := finalSessionState["meta"]; !reflect.DeepEqual(got, meta) {
		t.Fatalf("final %s.meta = %#v, want %#v", SessionStateKey, got, meta)
	}
}

func TestAgentUsesStateSessionFromSessionID(t *testing.T) {
	workingDir := t.TempDir()
	callerSessionID := "caller-provided-session"
	expectedPromptsRaw, err := json.Marshal([]string{"hello"})
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_SUPPORT_SESSION_RESUME": "1",
			"GO_FAIL_IF_RESUME_CALLED":  "1",
			"GO_EXPECT_PROMPTS":         string(expectedPromptsRaw),
		}),
		WorkingDir: workingDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"session_id": callerSessionID,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalText, finalSessionState := collectFinalTextAndSessionState(
		t,
		r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if finalText != callerSessionID+":hello" {
		t.Fatalf("final text = %q, want %q", finalText, callerSessionID+":hello")
	}
	if got := finalSessionState["session_id"]; got != callerSessionID {
		t.Fatalf("final %s.session_id = %v, want %s", SessionStateKey, got, callerSessionID)
	}
}

func TestAgentUsesStateSessionWhenResumeCapabilityMissing(t *testing.T) {
	workingDir := t.TempDir()
	sessionID := "session-new-when-no-resume"
	meta := map[string]any{
		"codex": map[string]any{
			"approvalMode": "manual",
		},
	}
	expectedPromptsRaw, err := json.Marshal([]string{"hello"})
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_SUPPORT_LOAD_SESSION":  "1",
			"GO_FAIL_IF_RESUME_CALLED": "1",
			"GO_FAIL_IF_LOAD_CALLED":   "1",
			"GO_EXPECT_PROMPTS":        string(expectedPromptsRaw),
		}),
		WorkingDir: workingDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"session_id": sessionID,
				"meta":       meta,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalText, finalSessionState := collectFinalTextAndSessionState(
		t,
		r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if finalText != sessionID+":hello" {
		t.Fatalf("final text = %q, want %s:hello", finalText, sessionID)
	}
	if got := finalSessionState["session_id"]; got != sessionID {
		t.Fatalf("final %s.session_id = %v, want %s", SessionStateKey, got, sessionID)
	}
}

func TestAgentAddsReasoningEffortToResumeMetaDuringRecovery(t *testing.T) {
	workingDir := t.TempDir()
	sessionID := "stale-session"
	expectedPromptsRaw, err := json.Marshal([]string{"hello", "hello"})
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}
	expectedResumeMetaRaw, err := json.Marshal(map[string]any{
		"codex": map[string]any{
			"approvalMode": "manual",
			"config": map[string]any{
				"model_reasoning_effort": "high",
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(expected resume meta) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_SUPPORT_SESSION_RESUME":             "1",
			"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1",
			"GO_EXPECT_RESUME_SESSION_ID":           sessionID,
			"GO_EXPECT_RESUME_META_RAW":             string(expectedResumeMetaRaw),
			"GO_EXPECT_PROMPTS":                     string(expectedPromptsRaw),
		}),
		WorkingDir:      workingDir,
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"session_id": sessionID,
				"meta": map[string]any{
					"codex": map[string]any{
						"approvalMode": "manual",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalText, finalSessionState := collectFinalTextAndSessionState(
		t,
		r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if finalText != sessionID+":hello" {
		t.Fatalf("final text = %q, want %s:hello", finalText, sessionID)
	}
	if got := finalSessionState["session_id"]; got != sessionID {
		t.Fatalf("final %s.session_id = %v, want %q", SessionStateKey, got, sessionID)
	}
	if got := finalSessionState["meta"]; !reflect.DeepEqual(got, map[string]any{
		"codex": map[string]any{
			"approvalMode": "manual",
			"config": map[string]any{
				"model_reasoning_effort": "high",
			},
		},
	}) {
		t.Fatalf("final %s.meta = %#v, want reasoning effort persisted", SessionStateKey, got)
	}
}

func TestAgentRecoversPromptFailureWithResumeOrNewSession(t *testing.T) {
	testCases := []struct {
		name      string
		env       map[string]string
		sessionID string
		wantID    string
	}{
		{
			name: "resume capability missing",
			env: map[string]string{
				"GO_FAIL_IF_RESUME_CALLED":              "1",
				"GO_FAIL_IF_LOAD_CALLED":                "1",
				"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1",
			},
			sessionID: "missing-session",
			wantID:    testSessionOneID,
		},
		{
			name: "missing",
			env: map[string]string{
				"GO_SUPPORT_SESSION_RESUME":             "1",
				"GO_FAIL_RESUME_ENTITY_NOT_FOUND":       "1",
				"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1",
			},
			sessionID: "stale-session",
			wantID:    testSessionOneID,
		},
		{
			name: "already active",
			env: map[string]string{
				"GO_SUPPORT_SESSION_RESUME":             "1",
				"GO_FAIL_RESUME_ALREADY_EXISTS":         "1",
				"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1",
			},
			sessionID: "active-session",
			wantID:    "active-session",
		},
		{
			name: "invalid params with session not found data",
			env: map[string]string{
				"GO_SUPPORT_SESSION_RESUME":                       "1",
				"GO_SUPPORT_LOAD_SESSION":                         "1",
				"GO_FAIL_IF_LOAD_CALLED":                          "1",
				"GO_FAIL_RESUME_INVALID_PARAMS_SESSION_NOT_FOUND": "1",
				"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND":           "1",
			},
			sessionID: "stale-session-data",
			wantID:    testSessionOneID,
		},
		{
			name: "invalid thread id from bridge backend",
			env: map[string]string{
				"GO_SUPPORT_SESSION_RESUME":             "1",
				"GO_SUPPORT_LOAD_SESSION":               "1",
				"GO_FAIL_IF_LOAD_CALLED":                "1",
				"GO_FAIL_RESUME_INVALID_THREAD":         "1",
				"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1",
			},
			sessionID: "session-1",
			wantID:    testSessionOneID,
		},
		{
			name: "invalid session id from bridge backend",
			env: map[string]string{
				"GO_SUPPORT_SESSION_RESUME":             "1",
				"GO_SUPPORT_LOAD_SESSION":               "1",
				"GO_FAIL_IF_LOAD_CALLED":                "1",
				"GO_FAIL_RESUME_INVALID_SESSION_ID":     "1",
				"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1",
			},
			sessionID: "session-1",
			wantID:    testSessionOneID,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			workingDir := t.TempDir()
			expectedPromptsRaw, err := json.Marshal([]string{"hello", "hello"})
			if err != nil {
				t.Fatalf("json.Marshal(expected prompts) error = %v", err)
			}

			env := map[string]string{
				"GO_EXPECT_PROMPTS": string(expectedPromptsRaw),
			}
			for key, value := range tc.env {
				env[key] = value
			}

			a, err := New(Config{
				Context:    context.Background(),
				Command:    helperCommandWithEnv(t, env),
				WorkingDir: workingDir,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer func() { _ = a.Close() }()

			sessionService := session.InMemoryService()
			r, err := runnerpkg.New(runnerpkg.Config{
				AppName:        "test-app",
				Agent:          a,
				SessionService: sessionService,
			})
			if err != nil {
				t.Fatalf("runner.New() error = %v", err)
			}
			sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
				AppName: "test-app",
				UserID:  "test-user",
				State: map[string]any{
					"cwd": workingDir,
					"acp_session": map[string]any{
						"session_id": tc.sessionID,
					},
				},
			})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}

			finalText, finalSessionState := collectFinalTextAndSessionState(
				t,
				r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
			)
			if finalText != tc.wantID+":hello" {
				t.Fatalf("final text = %q, want %s:hello", finalText, tc.wantID)
			}
			if got := finalSessionState["session_id"]; got != tc.wantID {
				t.Fatalf("final %s.session_id = %v, want %s", SessionStateKey, got, tc.wantID)
			}
		})
	}
}

func TestAgentPersistsReplacementSessionIDAfterResumeFallback(t *testing.T) {
	workingDir := t.TempDir()
	sessionService := session.InMemoryService()

	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"session_id": "stale-session",
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	firstPromptsRaw, err := json.Marshal([]string{"hello", "hello"})
	if err != nil {
		t.Fatalf("json.Marshal(first prompts) error = %v", err)
	}
	firstAgent, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_SUPPORT_SESSION_RESUME":                             "1",
			"GO_SUPPORT_LOAD_SESSION":                               "1",
			"GO_FAIL_IF_LOAD_CALLED":                                "1",
			"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND":                 "1",
			"GO_FAIL_FIRST_RESUME_INVALID_PARAMS_SESSION_NOT_FOUND": "1",
			"GO_EXPECT_PROMPTS":                                     string(firstPromptsRaw),
		}),
		WorkingDir: workingDir,
	})
	if err != nil {
		t.Fatalf("first New() error = %v", err)
	}
	defer func() { _ = firstAgent.Close() }()

	firstRunner, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          firstAgent,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("first runner.New() error = %v", err)
	}

	firstText, firstSessionState := collectFinalTextAndSessionState(
		t,
		firstRunner.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if firstText != testSessionOneHello {
		t.Fatalf("first final text = %q, want %q", firstText, testSessionOneHello)
	}
	if got := firstSessionState["session_id"]; got != testSessionOneID {
		t.Fatalf("first final %s.session_id = %v, want %s", SessionStateKey, got, testSessionOneID)
	}

	stored, err := sessionService.Get(context.Background(), &session.GetRequest{
		AppName:   "test-app",
		UserID:    "test-user",
		SessionID: sess.Session.ID(),
	})
	if err != nil {
		t.Fatalf("Get() after first run error = %v", err)
	}
	rawState, err := stored.Session.State().Get(SessionStateKey)
	if err != nil {
		t.Fatalf("stored session missing %s after first run: %v", SessionStateKey, err)
	}
	storedState, ok := rawState.(map[string]any)
	if !ok {
		t.Fatalf("stored %s type = %T, want map[string]any", SessionStateKey, rawState)
	}
	if got := storedState["session_id"]; got != testSessionOneID {
		t.Fatalf("stored %s.session_id = %v, want %s", SessionStateKey, got, testSessionOneID)
	}

	secondPromptsRaw, err := json.Marshal([]string{"again"})
	if err != nil {
		t.Fatalf("json.Marshal(second prompts) error = %v", err)
	}
	secondAgent, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_SUPPORT_SESSION_RESUME": "1",
			"GO_FAIL_IF_RESUME_CALLED":  "1",
			"GO_EXPECT_PROMPTS":         string(secondPromptsRaw),
		}),
		WorkingDir: workingDir,
	})
	if err != nil {
		t.Fatalf("second New() error = %v", err)
	}
	defer func() { _ = secondAgent.Close() }()

	secondRunner, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          secondAgent,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("second runner.New() error = %v", err)
	}

	secondText := collectFinalText(
		t,
		secondRunner.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("again", genai.RoleUser), agent.RunConfig{}),
	)
	if secondText != testSessionOneID+":again" {
		t.Fatalf("second final text = %q, want %s:again", secondText, testSessionOneID)
	}
}

func TestAgentSkipsInstructionPrependForStateSession(t *testing.T) {
	workingDir := t.TempDir()
	sessionID := "session-resume-bootstrap"
	expectedPromptsRaw, err := json.Marshal([]string{"hello", "again"})
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_SUPPORT_SESSION_RESUME": "1",
			"GO_FAIL_IF_RESUME_CALLED":  "1",
			"GO_EXPECT_PROMPTS":         string(expectedPromptsRaw),
		}),
		WorkingDir:  workingDir,
		Instruction: "missing={not_set}",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
			"acp_session": map[string]any{
				"session_id": sessionID,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if first != sessionID+":hello" {
		t.Fatalf("first final text = %q, want %q", first, sessionID+":hello")
	}

	second := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("again", genai.RoleUser), agent.RunConfig{}))
	if second != sessionID+":again" {
		t.Fatalf("second final text = %q, want %q", second, sessionID+":again")
	}
}

func TestAgentUsesStateSessionWhenSessionConfigChanges(t *testing.T) {
	defaultWorkingDir := t.TempDir()
	overrideWorkingDir := t.TempDir()
	var bootstrapBuf testLogBuffer
	bootstrapLogger := testSlogLogger(&bootstrapBuf, slog.LevelDebug)

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": defaultWorkingDir,
		}),
		WorkingDir: defaultWorkingDir,
		Logger:     bootstrapLogger,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var invocationBuf testLogBuffer
	invocationLogger := testSlogLogger(&invocationBuf, slog.LevelDebug).With("source", "invocation")
	invocationCtx := contextWithLogger(context.Background(), newLogger(invocationLogger, ""))

	first := collectFinalText(t, r.Run(invocationCtx, "test-user", sess.Session.ID(), genai.NewContentFromText("one", genai.RoleUser), agent.RunConfig{}))
	second := collectFinalText(t, r.Run(
		invocationCtx,
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("two", genai.RoleUser),
		agent.RunConfig{},
		runnerpkg.WithStateDelta(map[string]any{
			"cwd": overrideWorkingDir,
		}),
	))

	if first != testSessionOneOne {
		t.Fatalf("first final text = %q, want session-1:one", first)
	}
	if second != testSessionOneTwo {
		t.Fatalf("second final text = %q, want session-1:two", second)
	}

	logs := invocationBuf.String()
	if !strings.Contains(logs, "using acp session id from adk session state") {
		t.Fatalf("invocation log missing state-session reuse: %q", logs)
	}
}

func TestAgentRecoversMissingRemoteSessionDuringPrompt(t *testing.T) {
	expectedPromptsRaw, err := json.Marshal([]string{
		instructionPrompt("bootstrap instruction", "hello"),
		instructionPrompt("bootstrap instruction", "hello"),
		"again",
	})
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}

	a, err := New(Config{
		Context: context.Background(),
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1",
			"GO_EXPECT_PROMPTS":                     string(expectedPromptsRaw),
		}),
		WorkingDir:  t.TempDir(),
		Instruction: "bootstrap instruction",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first, firstSessionState := collectFinalTextAndSessionState(
		t,
		r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if first != "session-2:hello" {
		t.Fatalf("first final text = %q, want session-2:hello", first)
	}
	if got := firstSessionState["session_id"]; got != "session-2" {
		t.Fatalf("first final %s.session_id = %v, want session-2", SessionStateKey, got)
	}

	second := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("again", genai.RoleUser), agent.RunConfig{}))
	if second != "session-2:again" {
		t.Fatalf("second final text = %q, want session-2:again", second)
	}
}

func collectFinalText(t *testing.T, events iter.Seq2[*session.Event, error]) string {
	t.Helper()
	var fullText strings.Builder
	finalText := ""
	turnCompleteSeen := false
	for ev, err := range events {
		if err != nil {
			t.Fatalf("runner event error = %v", err)
		}
		if ev == nil {
			continue
		}
		if ev.TurnComplete {
			turnCompleteSeen = true
		}
		text := extractPromptText(ev.Content)
		if ev.TurnComplete && !ev.Partial && text != "" {
			finalText = text
		} else {
			fullText.WriteString(text)
		}
	}
	if !turnCompleteSeen {
		t.Fatalf("expected turn complete event")
	}
	if finalText != "" {
		return finalText
	}
	return fullText.String()
}

func collectFinalTextAndSessionState(t *testing.T, events iter.Seq2[*session.Event, error]) (string, map[string]any) {
	t.Helper()
	var fullText strings.Builder
	finalText := ""
	finalSessionState := map[string]any{}
	turnCompleteSeen := false
	for ev, err := range events {
		if err != nil {
			t.Fatalf("runner event error = %v", err)
		}
		if ev == nil {
			continue
		}
		text := extractPromptText(ev.Content)
		if ev.TurnComplete && !ev.Partial {
			turnCompleteSeen = true
			if text != "" {
				finalText = text
			}
			if ev.Actions.StateDelta != nil {
				if rawState, ok := ev.Actions.StateDelta[SessionStateKey]; ok {
					state, ok := rawState.(map[string]any)
					if !ok {
						t.Fatalf("final state delta %s type = %T, want map[string]any", SessionStateKey, rawState)
					}
					finalSessionState = state
				}
			}
			continue
		}
		fullText.WriteString(text)
	}
	if !turnCompleteSeen {
		t.Fatalf("expected turn complete event")
	}
	if finalText == "" {
		finalText = fullText.String()
	}
	return finalText, finalSessionState
}

func collectFinalEvent(t *testing.T, events iter.Seq2[*session.Event, error]) *session.Event {
	t.Helper()
	var finalEvent *session.Event
	for ev, err := range events {
		if err != nil {
			t.Fatalf("runner event error = %v", err)
		}
		if ev == nil || !ev.TurnComplete || ev.Partial {
			continue
		}
		finalEvent = ev
	}
	if finalEvent == nil {
		t.Fatal("expected turn complete event")
	}
	return finalEvent
}

func collectEventTexts(t *testing.T, events iter.Seq2[*session.Event, error]) []string {
	t.Helper()
	var texts []string
	for ev, err := range events {
		if err != nil {
			t.Fatalf("runner event error = %v", err)
		}
		if ev == nil {
			continue
		}
		if text := extractPromptText(ev.Content); text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

func TestAgentRunDoesNotDuplicatePartialInFinalEvent(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %q", got, testSessionOneHello)
	}
}

func TestAgentRunTurnCompleteIncludesFinalContent(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	partialText := ""
	finalText := ""
	turnCompleteCount := 0
	for ev, err := range r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner event error = %v", err)
		}
		if ev == nil {
			continue
		}
		text := extractPromptText(ev.Content)
		if ev.Partial {
			partialText += text
		}
		if ev.TurnComplete {
			turnCompleteCount++
			finalText = text
			if ev.Partial {
				t.Fatal("turn complete event was partial")
			}
			if ev.FinishReason != genai.FinishReasonStop {
				t.Fatalf("finish reason = %q, want %q", ev.FinishReason, genai.FinishReasonStop)
			}
		}
	}
	if partialText != testSessionOneHello {
		t.Fatalf("partial text = %q, want %q", partialText, testSessionOneHello)
	}
	if finalText != testSessionOneHello {
		t.Fatalf("final text = %q, want %q", finalText, testSessionOneHello)
	}
	if turnCompleteCount != 1 {
		t.Fatalf("turnCompleteCount = %d, want 1", turnCompleteCount)
	}
}

func TestAgentRunTurnCompleteIncludesTerminalProviderErrorWithoutVisibleReply(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalEvent := collectFinalEvent(t, r.Run(
		context.Background(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("terminal-error", genai.RoleUser),
		agent.RunConfig{},
	))

	if finalEvent.Content != nil {
		t.Fatalf("final content = %v, want nil", finalEvent.Content)
	}
	if got := finalEvent.ErrorMessage; got != "unexpected status 401 Unauthorized: Missing bearer or basic authentication in header" {
		t.Fatalf("error message = %q, want terminal provider error", got)
	}
	if got := finalEvent.ErrorCode; got != "other" {
		t.Fatalf("error code = %q, want other", got)
	}
}

func TestAgentRunIgnoresRetryOnlyErrorsWhenTurnLaterSucceeds(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalEvent := collectFinalEvent(t, r.Run(
		context.Background(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("retry-then-success", genai.RoleUser),
		agent.RunConfig{},
	))

	if got := extractPromptText(finalEvent.Content); got != "session-1:retry-then-success" {
		t.Fatalf("final text = %q, want successful visible reply", got)
	}
	if finalEvent.ErrorMessage != "" {
		t.Fatalf("error message = %q, want empty", finalEvent.ErrorMessage)
	}
	if finalEvent.ErrorCode != "" {
		t.Fatalf("error code = %q, want empty", finalEvent.ErrorCode)
	}
}

func TestMapACPUsageToUsageMetadata(t *testing.T) {
	cached := 7
	got := mapACPUsageToUsageMetadata(&acp.Usage{
		InputTokens:      11,
		OutputTokens:     13,
		TotalTokens:      31,
		CachedReadTokens: &cached,
	})
	if got == nil {
		t.Fatal("usage metadata is nil")
	}
	if got.PromptTokenCount != 11 {
		t.Fatalf("PromptTokenCount = %d, want 11", got.PromptTokenCount)
	}
	if got.CandidatesTokenCount != 13 {
		t.Fatalf("CandidatesTokenCount = %d, want 13", got.CandidatesTokenCount)
	}
	if got.TotalTokenCount != 31 {
		t.Fatalf("TotalTokenCount = %d, want 31", got.TotalTokenCount)
	}
	if got.CachedContentTokenCount != 7 {
		t.Fatalf("CachedContentTokenCount = %d, want 7", got.CachedContentTokenCount)
	}
}

func TestAgentRunUsesInvocationLogger(t *testing.T) {
	var bootstrapBuf testLogBuffer
	bootstrapLogger := testSlogLogger(&bootstrapBuf, slog.LevelDebug)

	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
		Logger:     bootstrapLogger,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = a.Close()
		}
	}()
	bootstrapBuf.Reset()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var invocationBuf testLogBuffer
	invocationLogger := testSlogLogger(&invocationBuf, slog.LevelDebug).With("source", "invocation")
	invocationCtx := contextWithLogger(context.Background(), newLogger(invocationLogger, ""))

	for _, runErr := range r.Run(invocationCtx, "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("runner event error = %v", runErr)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	closed = true

	invocationLogs := invocationBuf.String()
	if !strings.Contains(invocationLogs, `"source":"invocation"`) {
		t.Fatalf("invocation log missing source marker: %q", invocationLogs)
	}
	for _, mustContain := range []string{
		`"session_id":"` + sess.Session.ID() + `"`,
		`"adk_session_id":"` + sess.Session.ID() + `"`,
		`"acp_session_id":"` + testSessionOneID + `"`,
		`"prompt":"hello"`,
	} {
		if !strings.Contains(invocationLogs, mustContain) {
			t.Fatalf("invocation log missing %q: %q", mustContain, invocationLogs)
		}
	}
	if strings.Contains(invocationLogs, `"session_id":"`+testSessionOneID+`"`) {
		t.Fatalf("invocation log reused ACP session id as session_id: %q", invocationLogs)
	}
	for _, mustContain := range []string{"starting adk invocation", "sending acp session/prompt"} {
		if !strings.Contains(invocationLogs, mustContain) {
			t.Fatalf("invocation log missing %q: %q", mustContain, invocationLogs)
		}
	}

	if got := bootstrapBuf.String(); strings.Contains(got, "starting adk invocation") || strings.Contains(got, "sending acp session/prompt") {
		t.Fatalf("bootstrap logger unexpectedly received invocation logs: %q", got)
	}
}

func TestAgentRunMapsACPEventsToADKEvents(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	seenCall := false
	seenUpdate := false
	seenMessage := false
	seenTurnComplete := false

	for ev, err := range r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("tooling", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner event error = %v", err)
		}
		if ev == nil {
			continue
		}
		if ev.TurnComplete {
			seenTurnComplete = true
		}
		if ev.Content == nil {
			continue
		}
		if ev.Partial && extractPromptText(ev.Content) == "tooling-done" {
			seenMessage = true
		}
		for _, part := range ev.Content.Parts {
			if part == nil {
				continue
			}
			if part.FunctionCall != nil && part.FunctionCall.ID == testACPToolID && part.FunctionCall.Name == "acp_tool_call" {
				seenCall = true
			}
			if part.FunctionResponse != nil && part.FunctionResponse.ID == testACPToolID && part.FunctionResponse.Name == "acp_tool_call_update" {
				seenUpdate = true
			}
		}
	}

	if !seenCall {
		t.Fatalf("expected mapped tool call event")
	}
	if !seenUpdate {
		t.Fatalf("expected mapped tool call update event")
	}
	if !seenMessage {
		t.Fatalf("expected mapped agent message chunk event")
	}
	if !seenTurnComplete {
		t.Fatalf("expected final turn complete event")
	}
}

func TestAgentRunMapsACPPlanUpdatesToStateDelta(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var finalPlanSnapshot map[string]any
	var streamedPlanSnapshots []map[string]any
	seenMessage := false
	seenTurnComplete := false

	for ev, err := range r.Run(
		context.Background(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText(testACPPlanPrompt, genai.RoleUser),
		agent.RunConfig{},
	) {
		if err != nil {
			t.Fatalf("runner event error = %v", err)
		}
		if ev == nil {
			continue
		}
		if planSnapshot, ok := planStateSnapshotFromEvent(t, ev); ok {
			if !ev.TurnComplete && ev.Content != nil {
				t.Fatalf("plan event content = %#v, want nil", ev.Content)
			}
			switch {
			case ev.TurnComplete:
				finalPlanSnapshot = planSnapshot
			case !ev.Partial:
				t.Fatal("plan event Partial = false, want true")
			default:
				streamedPlanSnapshots = append(streamedPlanSnapshots, planSnapshot)
			}
		}
		if ev.Partial && extractPromptText(ev.Content) == "planning-done" {
			seenMessage = true
		}
		if ev.TurnComplete {
			seenTurnComplete = true
		}
	}

	if len(streamedPlanSnapshots) != 2 {
		t.Fatalf("streamed plan snapshot count = %d, want 2", len(streamedPlanSnapshots))
	}
	if got := planSnapshotEntries(t, streamedPlanSnapshots[0]); len(got) != 1 {
		t.Fatalf("first plan entry count = %d, want 1", len(got))
	}
	secondEntries := planSnapshotEntries(t, streamedPlanSnapshots[1])
	if len(secondEntries) != 2 {
		t.Fatalf("second plan entry count = %d, want 2", len(secondEntries))
	}
	if got := secondEntries[0]["status"]; got != acp.PlanEntryStatusCompleted {
		t.Fatalf("second plan first status = %v, want %q", got, acp.PlanEntryStatusCompleted)
	}
	if got := secondEntries[1]["content"]; got != "Run linters" {
		t.Fatalf("second plan second content = %v, want %q", got, "Run linters")
	}

	stored, err := sessionService.Get(context.Background(), &session.GetRequest{
		AppName:   "test-app",
		UserID:    "test-user",
		SessionID: sess.Session.ID(),
	})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	storedSnapshotValue, err := stored.Session.State().Get(PlanStateKey)
	if err != nil {
		t.Fatalf("State().Get(%q) error = %v", PlanStateKey, err)
	}
	if !reflect.DeepEqual(finalPlanSnapshot, streamedPlanSnapshots[1]) {
		t.Fatalf("final plan snapshot = %#v, want %#v", finalPlanSnapshot, streamedPlanSnapshots[1])
	}
	storedSnapshot := planSnapshotFromValue(t, storedSnapshotValue)
	if !reflect.DeepEqual(storedSnapshot, streamedPlanSnapshots[1]) {
		t.Fatalf("stored plan snapshot = %#v, want %#v", storedSnapshot, streamedPlanSnapshots[1])
	}
	if !seenMessage {
		t.Fatalf("expected mapped agent message chunk event")
	}
	if !seenTurnComplete {
		t.Fatalf("expected final turn complete event")
	}
}

func TestClientCreateSessionSetsMCPServers(t *testing.T) {
	expectedServers := []acp.McpServer{
		{
			Stdio: &acp.McpServerStdio{
				Name:    "test-server",
				Command: "echo",
				Args:    []string{"hello"},
			},
		},
	}
	expectedJSON, _ := json.Marshal(expectedServers)

	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_MCP_SERVERS": string(expectedJSON),
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(context.Background(), t.TempDir(), expectedServers)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("NewSession() returned empty session id")
	}
}

func TestClientNewSessionSendsEmptyMCPServersArrayWhenNil(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_MCP_SERVERS_RAW": "[]",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("NewSession() returned empty session id")
	}
}

func TestAgentConfigMCPServersUseEmptyArraysNotNull(t *testing.T) {
	expectedRaw := `[{"headers":[],"name":"http_server","type":"http","url":"http://localhost:9999/mcp"},{"headers":[],"name":"sse_server","type":"sse","url":"http://localhost:9998/sse"},{"args":[],"command":"echo","env":[],"name":"stdio_server"}]`

	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommandWithEnv(t, map[string]string{"GO_EXPECT_MCP_SERVERS_RAW": expectedRaw}),
		WorkingDir: t.TempDir(),
		MCPServers: map[string]MCPServerConfig{
			"stdio_server": {
				Type: MCPServerTypeStdio,
				Cmd:  []string{"echo"},
			},
			"http_server": {
				Type: MCPServerTypeHTTP,
				URL:  "http://localhost:9999/mcp",
			},
			"sse_server": {
				Type: MCPServerTypeSSE,
				URL:  "http://localhost:9998/sse",
			},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	for _, runErr := range r.Run(context.Background(), "test-user", sess.Session.ID(), genai.NewContentFromText("ping", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("runner event error = %v", runErr)
		}
	}
}

func TestAgentRunStoresOutputKeyInFinalStateDelta(t *testing.T) {
	a, err := New(Config{
		Context:    context.Background(),
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
		OutputKey:  "result",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = a.Close() }()

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var finalOutput string
	for ev, runErr := range r.Run(
		context.Background(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("hello", genai.RoleUser),
		agent.RunConfig{},
	) {
		if runErr != nil {
			t.Fatalf("runner event error = %v", runErr)
		}
		if ev == nil {
			continue
		}
		if !ev.TurnComplete {
			if ev.Actions.StateDelta != nil {
				if _, ok := ev.Actions.StateDelta["result"]; ok {
					t.Fatalf("partial event unexpectedly contains output key state delta")
				}
			}
			continue
		}
		got, ok := ev.Actions.StateDelta["result"]
		if !ok {
			t.Fatalf("turn-complete event missing output key state delta")
		}
		output, ok := got.(string)
		if !ok {
			t.Fatalf("output key value type = %T, want string", got)
		}
		finalOutput = output
	}

	if finalOutput != testSessionOneHello {
		t.Fatalf("final output state = %q, want %q", finalOutput, testSessionOneHello)
	}

	stored, err := sessionService.Get(context.Background(), &session.GetRequest{
		AppName:   "test-app",
		UserID:    "test-user",
		SessionID: sess.Session.ID(),
	})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	storedOutput, err := stored.Session.State().Get("result")
	if err != nil {
		t.Fatalf("State().Get(\"result\") error = %v", err)
	}
	if storedOutput != testSessionOneHello {
		t.Fatalf("stored output state = %v, want %q", storedOutput, testSessionOneHello)
	}
}

func TestAgentMaybeSaveOutputToStateSkipsEmptyOutput(t *testing.T) {
	a := &Agent{outputKey: "result"}
	ev := session.NewEvent(t.Context(), "inv-1")
	a.maybeSaveOutputToState(ev, "")
	if _, ok := ev.Actions.StateDelta["result"]; ok {
		t.Fatalf("state delta unexpectedly contains result for empty output")
	}
}

func helperCommand(t *testing.T) []string {
	return helperCommandWithEnv(t, nil)
}

func helperCommandWithEnv(t *testing.T, env map[string]string) []string {
	t.Helper()
	cmd := []string{"env", "GO_WANT_ACP_HELPER=1"}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			cmd = append(cmd, key+"="+env[key])
		}
	}
	cmd = append(cmd, os.Args[0], "-test.run=TestACPHelperProcess", "--")
	return cmd
}

func TestACPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_HELPER") != "1" {
		return
	}
	runACPHelper(os.Stdin, os.Stdout)
	os.Exit(0)
}

func runACPHelper(stdin *os.File, stdout *os.File) {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	sessionCount := 0
	promptCount := 0
	expectedClientName := os.Getenv("GO_EXPECT_CLIENT_NAME")
	expectedClientVersion := os.Getenv("GO_EXPECT_CLIENT_VERSION")
	expectedSessionModel := os.Getenv("GO_EXPECT_SESSION_MODEL")
	expectedSessionMode := os.Getenv("GO_EXPECT_SESSION_MODE")
	expectedMCPServers := os.Getenv("GO_EXPECT_MCP_SERVERS")
	expectedMCPServersRaw := os.Getenv("GO_EXPECT_MCP_SERVERS_RAW")
	expectedSessionCWD := os.Getenv("GO_EXPECT_SESSION_CWD")
	expectedNewSessionMetaRaw := os.Getenv("GO_EXPECT_NEW_SESSION_META_RAW")
	expectedResumeSessionID := os.Getenv("GO_EXPECT_RESUME_SESSION_ID")
	expectedResumeSessionCWD := os.Getenv("GO_EXPECT_RESUME_SESSION_CWD")
	expectedResumeMetaRaw := os.Getenv("GO_EXPECT_RESUME_META_RAW")
	expectedLoadSessionID := os.Getenv("GO_EXPECT_LOAD_SESSION_ID")
	expectedLoadSessionCWD := os.Getenv("GO_EXPECT_LOAD_SESSION_CWD")
	expectedLoadMetaRaw := os.Getenv("GO_EXPECT_LOAD_META_RAW")
	supportSessionResume := os.Getenv("GO_SUPPORT_SESSION_RESUME") == "1"
	supportLoadSession := os.Getenv("GO_SUPPORT_LOAD_SESSION") == "1"
	expectedPromptsRaw := os.Getenv("GO_EXPECT_PROMPTS")
	forceNewSessionID := os.Getenv("GO_FORCE_NEW_SESSION_ID")
	disableSetModel := os.Getenv("GO_DISABLE_SET_MODEL") == "1"
	disableSetMode := os.Getenv("GO_DISABLE_SET_MODE") == "1"
	failResumeMethodNotFound := os.Getenv("GO_FAIL_RESUME_METHOD_NOT_FOUND") == "1"
	failResumeEntityNotFound := os.Getenv("GO_FAIL_RESUME_ENTITY_NOT_FOUND") == "1"
	failResumeInvalidParamsSessionNotFound := os.Getenv("GO_FAIL_RESUME_INVALID_PARAMS_SESSION_NOT_FOUND") == "1"
	failResumeInvalidThread := os.Getenv("GO_FAIL_RESUME_INVALID_THREAD") == "1"
	failResumeInvalidSessionID := os.Getenv("GO_FAIL_RESUME_INVALID_SESSION_ID") == "1"
	failResumeAlreadyExists := os.Getenv("GO_FAIL_RESUME_ALREADY_EXISTS") == "1"
	failFirstResumeInvalidParamsSessionNotFound := os.Getenv("GO_FAIL_FIRST_RESUME_INVALID_PARAMS_SESSION_NOT_FOUND") == "1"
	failLoadMethodNotFound := os.Getenv("GO_FAIL_LOAD_METHOD_NOT_FOUND") == "1"
	failLoadEntityNotFound := os.Getenv("GO_FAIL_LOAD_ENTITY_NOT_FOUND") == "1"
	failFirstPromptEntityNotFound := os.Getenv("GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND") == "1"
	failIfResumeCalled := os.Getenv("GO_FAIL_IF_RESUME_CALLED") == "1"
	failIfLoadCalled := os.Getenv("GO_FAIL_IF_LOAD_CALLED") == "1"
	resumeCount := 0
	var expectedPrompts []string
	if strings.TrimSpace(expectedPromptsRaw) != "" {
		must(json.Unmarshal([]byte(expectedPromptsRaw), &expectedPrompts))
	}
	handleSessionRestore := func(
		msg helperEnvelope,
		method string,
		expectedSessionID string,
		expectedCWD string,
		expectedMetaRaw string,
		failMethodNotFound bool,
		failEntityNotFound bool,
	) {
		if method == acp.AgentMethodSessionResume && failIfResumeCalled {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32000, Message: "session/resume should not have been called"},
			})
			return
		}
		if method == acp.AgentMethodSessionLoad && failIfLoadCalled {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32000, Message: "session/load should not have been called"},
			})
			return
		}
		if method == acp.AgentMethodSessionResume {
			resumeCount++
		}
		if failMethodNotFound {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32601, Message: "unsupported"},
			})
			return
		}
		var req helperSessionRestoreRequest
		must(json.Unmarshal(msg.Params, &req))
		if expectedSessionID != "" && req.SessionID != expectedSessionID {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected %s sessionId: %q, want %q", method, req.SessionID, expectedSessionID)},
			})
			return
		}
		if expectedCWD != "" && req.Cwd != expectedCWD {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected %s cwd: %q, want %q", method, req.Cwd, expectedCWD)},
			})
			return
		}
		if expectedMetaRaw != "" {
			var reqRaw struct {
				Meta json.RawMessage `json:"_meta"`
			}
			must(json.Unmarshal(msg.Params, &reqRaw))
			gotRaw := compactJSONForCompare(reqRaw.Meta)
			wantRaw := compactJSONForCompare([]byte(expectedMetaRaw))
			if gotRaw != wantRaw {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected raw %s _meta payload: %q, want %q", method, gotRaw, wantRaw)},
				})
				return
			}
		}
		if failEntityNotFound {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    500,
					Message: "Requested entity was not found.",
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failResumeInvalidParamsSessionNotFound {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32602,
					Message: "Invalid params",
					Data:    "session not found",
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failResumeInvalidThread {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32603,
					Message: "Internal error",
					Data: map[string]any{
						"error": "thread/resume: bridge backend rpc error (-32600): invalid thread id: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `s` at 1",
					},
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failResumeInvalidSessionID {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32603,
					Message: "Internal error",
					Data: map[string]any{
						"error": "thread/resume: bridge backend rpc error (-32600): invalid session id: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `s` at 1",
					},
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failResumeAlreadyExists {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32602,
					Message: "Invalid params",
					Data:    fmt.Sprintf("session %q already exists", req.SessionID),
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failFirstResumeInvalidParamsSessionNotFound && resumeCount == 1 {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32602,
					Message: "Invalid params",
					Data:    "session not found",
				},
			})
			return
		}
		writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperSessionRestoreResponse{})})
	}

	for scanner.Scan() {
		var msg helperEnvelope
		must(json.Unmarshal(scanner.Bytes(), &msg))
		switch msg.Method {
		case acp.AgentMethodInitialize:
			var req helperInitializeRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedClientName != "" && req.ClientInfo.Name != expectedClientName {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected client name: %s", req.ClientInfo.Name)},
				})
				continue
			}
			if expectedClientVersion != "" && req.ClientInfo.Version != expectedClientVersion {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected client version: %s", req.ClientInfo.Version)},
				})
				continue
			}
			initResp := helperInitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}
			if supportSessionResume || supportLoadSession {
				initResp.AgentCapabilities = &helperAgentCapabilities{}
				if supportLoadSession {
					initResp.AgentCapabilities.LoadSession = true
				}
				if supportSessionResume {
					initResp.AgentCapabilities.SessionCapabilities = &helperSessionCapabilities{
						Resume: &helperSessionResumeCapabilities{},
					}
				}
			}
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(initResp)})
		case acp.AgentMethodSessionNew:
			var req helperNewSessionRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedSessionCWD != "" && req.Cwd != expectedSessionCWD {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session cwd: %q, want %q", req.Cwd, expectedSessionCWD)},
				})
				continue
			}
			if expectedMCPServersRaw != "" {
				var reqRaw struct {
					McpServers json.RawMessage `json:"mcpServers"`
				}
				must(json.Unmarshal(msg.Params, &reqRaw))
				gotRaw := compactJSONForCompare(reqRaw.McpServers)
				wantRaw := compactJSONForCompare([]byte(expectedMCPServersRaw))
				if gotRaw != wantRaw {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected raw mcp servers payload: %q, want %q", gotRaw, wantRaw)},
					})
					continue
				}
			}
			if expectedMCPServers != "" {
				gotJSON, _ := json.Marshal(req.McpServers)
				// Basic string comparison of JSON might be flaky if key order differs,
				// but for simple struct it might work if mostly empty.
				// Better to unmarshal expected and compare.
				var expected []acp.McpServer
				must(json.Unmarshal([]byte(expectedMCPServers), &expected))

				// Re-marshal both to ensure consistent ordering/formatting if possible,
				// or just check count and first element name.
				if len(req.McpServers) != len(expected) {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected mcp servers count: %d, want %d", len(req.McpServers), len(expected))},
					})
					continue
				}
				if len(expected) > 0 {
					if req.McpServers[0].Stdio == nil || expected[0].Stdio == nil || req.McpServers[0].Stdio.Name != expected[0].Stdio.Name {
						writeEnvelope(stdout, helperEnvelope{
							JSONRPC: "2.0",
							ID:      msg.ID,
							Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected mcp server: %s", string(gotJSON))},
						})
						continue
					}
				}
			}
			if expectedNewSessionMetaRaw != "" {
				var reqRaw struct {
					Meta json.RawMessage `json:"_meta"`
				}
				must(json.Unmarshal(msg.Params, &reqRaw))
				gotRaw := compactJSONForCompare(reqRaw.Meta)
				wantRaw := compactJSONForCompare([]byte(expectedNewSessionMetaRaw))
				if gotRaw != wantRaw {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected raw session/new _meta payload: %q, want %q", gotRaw, wantRaw)},
					})
					continue
				}
			}
			sessionCount++
			sessionID := fmt.Sprintf("session-%d", sessionCount)
			if forceNewSessionID != "" {
				sessionID = forceNewSessionID
			}
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperNewSessionResponse{SessionID: sessionID})})
		case acp.AgentMethodSessionResume:
			handleSessionRestore(
				msg,
				"session/resume",
				expectedResumeSessionID,
				expectedResumeSessionCWD,
				expectedResumeMetaRaw,
				failResumeMethodNotFound,
				failResumeEntityNotFound,
			)
		case acp.AgentMethodSessionLoad:
			handleSessionRestore(
				msg,
				"session/load",
				expectedLoadSessionID,
				expectedLoadSessionCWD,
				expectedLoadMetaRaw,
				failLoadMethodNotFound,
				failLoadEntityNotFound,
			)
		case acp.AgentMethodSessionSetModel:
			if disableSetModel {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32601, Message: "unsupported"},
				})
				continue
			}
			var req helperSetSessionModelRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedSessionModel != "" && req.ModelID != expectedSessionModel {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session model: %s", req.ModelID)},
				})
				continue
			}
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperSetSessionModelResponse{})})
		case acp.AgentMethodSessionSetMode:
			if disableSetMode {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32601, Message: "unsupported"},
				})
				continue
			}
			var req helperSetSessionModeRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedSessionMode != "" && req.ModeID != expectedSessionMode {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session mode: %s", req.ModeID)},
				})
				continue
			}
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperSetSessionModeResponse{})})
		case acp.AgentMethodSessionPrompt:
			var req helperPromptRequest
			must(json.Unmarshal(msg.Params, &req))
			promptCount++
			if failFirstPromptEntityNotFound && promptCount == 1 {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error: &helperError{
						Code:    500,
						Message: "Requested entity was not found.",
					},
				})
				continue
			}
			prompt := req.Prompt[0].Text
			if len(expectedPrompts) > 0 {
				if promptCount > len(expectedPrompts) {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected extra prompt %d: %q", promptCount, prompt)},
					})
					continue
				}
				wantPrompt := expectedPrompts[promptCount-1]
				if prompt != wantPrompt {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected prompt[%d]: %q, want %q", promptCount, prompt, wantPrompt)},
					})
					continue
				}
			}
			responsePrompt := prompt
			if _, after, ok := strings.Cut(prompt, "\n\nUser message:\n"); ok {
				responsePrompt = after
			}
			if strings.HasPrefix(responsePrompt, "slow:") {
				time.Sleep(150 * time.Millisecond)
				responsePrompt = strings.TrimPrefix(responsePrompt, "slow:")
			}
			if responsePrompt == "permission" {
				title := "Edit file"
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: json.RawMessage(`"perm-1"`), Method: acp.ClientMethodSessionRequestPermission, Params: mustJSON(helperPermissionRequest{
					SessionID: req.SessionID,
					ToolCall:  helperPermissionToolCall{Title: &title},
					Options: []helperPermissionOption{
						{Kind: string(acp.PermissionOptionKindAllowOnce), Name: "Allow", OptionID: "allow"},
						{Kind: string(acp.PermissionOptionKindRejectOnce), Name: "Reject", OptionID: "reject"},
					},
				})})
				if !scanner.Scan() {
					return
				}
				var permitResp helperEnvelope
				must(json.Unmarshal(scanner.Bytes(), &permitResp))
				var outcome helperPermissionResponse
				must(json.Unmarshal(permitResp.Result, &outcome))
				text := "rejected"
				if outcome.Outcome.Outcome == "selected" && outcome.Outcome.OptionID == "allow" {
					text = "approved"
				}
				writeUpdate(stdout, req.SessionID, text)
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
				continue
			}
			if responsePrompt == "tooling" {
				writeToolCall(stdout, req.SessionID, testACPToolID, "run shell", acp.ToolCallStatusInProgress)
				writeToolCallUpdate(stdout, req.SessionID, testACPToolID, acp.ToolCallStatusCompleted, map[string]any{"ok": true})
				writeUpdate(stdout, req.SessionID, "tooling-done")
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
				continue
			}
			if responsePrompt == testACPPlanPrompt {
				writePlanUpdate(stdout, req.SessionID, []acp.PlanEntry{
					{
						Content:  "Run tests",
						Status:   acp.PlanEntryStatusInProgress,
						Priority: acp.PlanEntryPriorityMedium,
					},
				})
				writePlanUpdate(stdout, req.SessionID, []acp.PlanEntry{
					{
						Content:  "Run tests",
						Status:   acp.PlanEntryStatusCompleted,
						Priority: acp.PlanEntryPriorityMedium,
					},
					{
						Content:  "Run linters",
						Status:   acp.PlanEntryStatusPending,
						Priority: acp.PlanEntryPriorityHigh,
					},
				})
				writeUpdate(stdout, req.SessionID, "planning-done")
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
				continue
			}
			if responsePrompt == "terminal-error" {
				writePromptErrorNotification(stdout, req.SessionID, "Reconnecting... 1/5", true)
				writePromptErrorNotification(stdout, req.SessionID, "Reconnecting... 2/5", true)
				writeTurnCompletedFailure(stdout, req.SessionID, "unexpected status 401 Unauthorized: Missing bearer or basic authentication in header", "other")
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
				continue
			}
			if responsePrompt == "retry-then-success" {
				writePromptErrorNotification(stdout, req.SessionID, "Reconnecting... 1/5", true)
				writePromptErrorNotification(stdout, req.SessionID, "Reconnecting... 2/5", true)
			}
			prefix := req.SessionID + ":"
			writeUpdate(stdout, req.SessionID, prefix)
			writeUpdate(stdout, req.SessionID, responsePrompt)
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
		case acp.AgentMethodSessionCancel:
			// Ignore in helper.
		default:
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Error: &helperError{Code: -32601, Message: "unsupported"}})
		}
	}
}

func writeUpdate(stdout *os.File, sessionID, text string) {
	writeSessionUpdate(stdout, sessionID, map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content": map[string]any{
			"type": "text",
			"text": text,
		},
	})
}

func writeToolCall(stdout *os.File, sessionID, toolCallID, title string, status acp.ToolCallStatus) {
	writeSessionUpdate(stdout, sessionID, map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    toolCallID,
		"title":         title,
		"kind":          acp.ToolKindExecute,
		"status":        status,
		"rawInput": map[string]any{
			"cmd": "ls",
		},
	})
}

func writeToolCallUpdate(stdout *os.File, sessionID, toolCallID string, status acp.ToolCallStatus, rawOutput map[string]any) {
	writeSessionUpdate(stdout, sessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    toolCallID,
		"status":        status,
		"rawOutput":     rawOutput,
	})
}

func writePlanUpdate(stdout *os.File, sessionID string, entries []acp.PlanEntry) {
	writeSessionUpdate(stdout, sessionID, map[string]any{
		"sessionUpdate":   "plan",
		acpPlanEntriesKey: entries,
	})
}

func writePromptErrorNotification(stdout *os.File, sessionID, message string, willRetry bool) {
	writeEnvelope(stdout, helperEnvelope{
		JSONRPC: "2.0",
		Method:  "error",
		Params: mustJSON(map[string]any{
			"threadId":  sessionID,
			"willRetry": willRetry,
			"error": map[string]any{
				"message":        message,
				"codexErrorInfo": map[string]any{"responseStreamDisconnected": map[string]any{"httpStatusCode": 401}},
			},
		}),
	})
}

func writeTurnCompletedFailure(stdout *os.File, sessionID, message, code string) {
	writeEnvelope(stdout, helperEnvelope{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params: mustJSON(map[string]any{
			"threadId": sessionID,
			"turn": map[string]any{
				"id":     "turn-1",
				"status": "failed",
				"items":  []any{},
				"error": map[string]any{
					"message":        message,
					"codexErrorInfo": code,
				},
			},
		}),
	})
}

func writeSessionUpdate(stdout *os.File, sessionID string, update map[string]any) {
	writeEnvelope(stdout, helperEnvelope{
		JSONRPC: "2.0",
		Method:  acp.ClientMethodSessionUpdate,
		Params: mustJSON(map[string]any{
			"sessionId": sessionID,
			"update":    update,
		}),
	})
}

func writeEnvelope(stdout *os.File, msg helperEnvelope) {
	must(json.NewEncoder(stdout).Encode(msg))
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	must(err)
	return data
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func compactJSONForCompare(raw []byte) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return out.String()
}

type helperEnvelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *helperError    `json:"error,omitempty"`
}

type helperError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type helperInitializeResponse struct {
	AgentCapabilities *helperAgentCapabilities `json:"agentCapabilities,omitempty"`
	ProtocolVersion   int                      `json:"protocolVersion"`
}

type helperInitializeRequest struct {
	ClientInfo helperImplementation `json:"clientInfo"`
}

type helperAgentCapabilities struct {
	LoadSession         bool                       `json:"loadSession,omitempty"`
	SessionCapabilities *helperSessionCapabilities `json:"sessionCapabilities,omitempty"`
}

type helperSessionCapabilities struct {
	Resume *helperSessionResumeCapabilities `json:"resume,omitempty"`
}

type helperSessionResumeCapabilities struct{}

type helperImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type helperNewSessionResponse struct {
	SessionID string `json:"sessionId"`
}

type helperNewSessionRequest struct {
	Meta       map[string]any  `json:"_meta,omitempty"`
	Cwd        string          `json:"cwd"`
	McpServers []acp.McpServer `json:"mcpServers,omitempty"`
}

type helperSessionRestoreRequest struct {
	Meta       map[string]any  `json:"_meta,omitempty"`
	Cwd        string          `json:"cwd"`
	McpServers []acp.McpServer `json:"mcpServers,omitempty"`
	SessionID  string          `json:"sessionId"`
}

type helperSessionRestoreResponse struct{}

type helperPromptResponse struct {
	StopReason string `json:"stopReason"`
}

type helperSetSessionModelRequest struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

type helperSetSessionModelResponse struct{}

type helperSetSessionModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type helperSetSessionModeResponse struct{}

type helperPromptRequest struct {
	SessionID string              `json:"sessionId"`
	Prompt    []helperContentPart `json:"prompt"`
}

type helperContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type helperPermissionRequest struct {
	SessionID string                   `json:"sessionId"`
	Options   []helperPermissionOption `json:"options"`
	ToolCall  helperPermissionToolCall `json:"toolCall"`
}

type helperPermissionOption struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	OptionID string `json:"optionId"`
}

type helperPermissionToolCall struct {
	Title *string `json:"title,omitempty"`
}

type helperPermissionResponse struct {
	Outcome helperPermissionOutcome `json:"outcome"`
}

type helperPermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

func readPromptOutput(t *testing.T, updates <-chan ExtendedSessionNotification, resultCh <-chan PromptResult) string {
	t.Helper()
	var chunks []string
	for note := range updates {
		ev, ok := mapACPUpdateToEvent(t.Context(), newLogger(nil, ""), "inv-1", ExtendedSessionNotification{SessionNotification: note.SessionNotification, Raw: note.Raw})
		if ok {
			if text := extractPromptText(ev.Content); text != "" {
				chunks = append(chunks, text)
			}
		}
	}
	result := <-resultCh
	if result.Err != nil {
		t.Fatalf("PromptResult.Err = %v", result.Err)
	}
	return strings.Join(chunks, "")
}

func TestMapACPPlanUpdate(t *testing.T) {
	logger := newLogger(nil, "")

	tests := []struct {
		name        string
		plan        *acp.SessionUpdatePlan
		wantOK      bool
		wantEntries []map[string]any
	}{
		{
			name:   "nil plan",
			plan:   nil,
			wantOK: false,
		},
		{
			name:   "empty entries",
			plan:   &acp.SessionUpdatePlan{Entries: []acp.PlanEntry{}},
			wantOK: false,
		},
		{
			name: "single entry",
			plan: &acp.SessionUpdatePlan{
				Entries: []acp.PlanEntry{
					{
						Content:  "Run tests",
						Status:   acp.PlanEntryStatusInProgress,
						Priority: acp.PlanEntryPriorityMedium,
					},
				},
			},
			wantOK: true,
			wantEntries: []map[string]any{
				{
					"content":  "Run tests",
					"status":   acp.PlanEntryStatusInProgress,
					"priority": acp.PlanEntryPriorityMedium,
				},
			},
		},
		{
			name: "multiple entries",
			plan: &acp.SessionUpdatePlan{
				Entries: []acp.PlanEntry{
					{Content: "Step 1", Status: acp.PlanEntryStatusCompleted},
					{Content: "Step 2", Status: acp.PlanEntryStatusPending},
				},
			},
			wantOK: true,
			wantEntries: []map[string]any{
				{
					"content":  "Step 1",
					"status":   acp.PlanEntryStatusCompleted,
					"priority": acp.PlanEntryPriority(""),
				},
				{
					"content":  "Step 2",
					"status":   acp.PlanEntryStatusPending,
					"priority": acp.PlanEntryPriority(""),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok := mapACPPlanUpdate(t.Context(), logger, "inv-1", tt.plan)
			if ok != tt.wantOK {
				t.Errorf("mapACPPlanUpdate() ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && ev == nil {
				t.Errorf("mapACPPlanUpdate() ev = nil, want event")
			}
			if !tt.wantOK {
				return
			}
			if ev.Content != nil {
				t.Fatalf("mapACPPlanUpdate() content = %#v, want nil", ev.Content)
			}
			if !ev.Partial {
				t.Fatal("mapACPPlanUpdate() Partial = false, want true")
			}
			gotSnapshot, ok := planStateSnapshotFromEvent(t, ev)
			if !ok {
				t.Fatalf("mapACPPlanUpdate() missing plan state delta")
			}
			if gotEntries := planSnapshotEntries(t, gotSnapshot); !reflect.DeepEqual(gotEntries, tt.wantEntries) {
				t.Fatalf("mapACPPlanUpdate() entries = %#v, want %#v", gotEntries, tt.wantEntries)
			}
		})
	}
}

func planStateSnapshotFromEvent(t *testing.T, ev *session.Event) (map[string]any, bool) {
	t.Helper()
	if ev == nil || ev.Actions.StateDelta == nil {
		return nil, false
	}
	rawSnapshot, ok := ev.Actions.StateDelta[PlanStateKey]
	if !ok {
		return nil, false
	}
	return planSnapshotFromValue(t, rawSnapshot), true
}

func planSnapshotFromValue(t *testing.T, rawSnapshot any) map[string]any {
	t.Helper()
	snapshot, ok := rawSnapshot.(map[string]any)
	if !ok {
		t.Fatalf("plan snapshot type = %T, want map[string]any", rawSnapshot)
	}
	return snapshot
}

func planSnapshotEntries(t *testing.T, snapshot map[string]any) []map[string]any {
	t.Helper()
	rawEntries, ok := snapshot[acpPlanEntriesKey]
	if !ok {
		t.Fatalf("plan snapshot missing %q", acpPlanEntriesKey)
	}
	switch entries := rawEntries.(type) {
	case []map[string]any:
		return entries
	case []any:
		normalized := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			entryMap, ok := entry.(map[string]any)
			if !ok {
				t.Fatalf("plan entry type = %T, want map[string]any", entry)
			}
			normalized = append(normalized, entryMap)
		}
		return normalized
	default:
		t.Fatalf("plan entries type = %T, want []map[string]any", rawEntries)
		return nil
	}
}
