package acpagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/normahq/go-adk-acpagent/v2/acperror"
)

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
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(t.Context(), t.TempDir(), nil)

	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	updates, resultCh, err := client.Prompt(t.Context(), string(sess.SessionId), "hello")
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

func TestClientPromptLogsPayloadAtTrace(t *testing.T) {
	var logBuf testLogBuffer
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
		Logger:  testSlogLogger(&logBuf, levelTrace),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	updates, results, err := client.Prompt(t.Context(), string(sess.SessionId), "sensitive-prompt")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	readPromptOutput(t, updates, results)

	if got := logBuf.String(); !strings.Contains(got, "sensitive-prompt") {
		t.Fatalf("trace log does not contain prompt payload: %q", got)
	}
}

func TestLoggerPassesCallerContextToHandler(t *testing.T) {
	type contextKey struct{}
	key := contextKey{}
	seen := make(chan any, 1)
	handler := &contextCaptureHandler{
		Handler: slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
		key:     key,
		seen:    seen,
	}
	ctx := context.WithValue(t.Context(), key, "context-value")

	newLogger(slog.New(handler), "test").withContext(ctx).Debug().Msg("context test")

	select {
	case got := <-seen:
		if got != "context-value" {
			t.Errorf("slog handler context value = %v, want %q", got, "context-value")
		}
	case <-t.Context().Done():
		t.Fatal("slog handler did not receive a record")
	}
}

func TestClientCreateSessionSetsConfigValue(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_MODEL": "openai/gpt-5.4",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{{ID: "model", Value: "openai/gpt-5.4"}}, nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
}

func TestClientCreateSessionWarnsOnSetConfigOptionUnsupported(t *testing.T) {
	var logBuf testLogBuffer
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_DISABLE_SET_CONFIG_OPTION": "1",
			"GO_EXPECT_SESSION_MODEL":      "openai/gpt-5.4",
		}),
		Logger: testSlogLogger(&logBuf, slog.LevelWarn),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if _, err := client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{{ID: "model", Value: "openai/gpt-5.4"}}, nil); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := logBuf.String(); !strings.Contains(got, "session/set_config_option unsupported") {
		t.Fatalf("warn log = %q", got)
	}
}

func TestClientCreateSessionWarnsOnUnavailableConfigOption(t *testing.T) {
	var logBuf testLogBuffer
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
		Logger:  testSlogLogger(&logBuf, slog.LevelWarn),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if _, err := client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{{ID: "reasoning_effort", Value: "high"}}, nil); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := logBuf.String(); !strings.Contains(got, "config option unavailable") || !strings.Contains(got, "reasoning_effort") {
		t.Fatalf("warn log = %q", got)
	}
}

func TestClientCreateSessionFailsOnSetConfigOptionRequestError(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_MODEL": "different/model",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	_, err = client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{{ID: "model", Value: "openai/gpt-5.4"}}, nil)
	if err == nil {
		t.Fatal("CreateSession() error = nil, want set model error")
	}
}

func TestClientCreateSessionUsesCustomConfigID(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_CONFIG_ID":     "provider.model",
			"GO_EXPECT_SESSION_MODEL": "openai/gpt-5.4",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	resp, err := client.NewSessionWithMeta(t.Context(), t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("NewSessionWithMeta() error = %v", err)
	}
	if _, err := client.applySessionConfig(t.Context(), string(resp.SessionId), []SessionConfigValue{{ID: "provider.model", Value: "openai/gpt-5.4"}}, resp.ConfigOptions, resp.Modes); err != nil {
		t.Fatalf("applySessionConfig() error = %v", err)
	}
}

func TestClientCreateSessionSetsMode(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_MODE": "code",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{{ID: "mode", Value: "code"}}, nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
}

func TestClientCreateSessionSetsModeConfigOption(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_ADVERTISE_MODE_CONFIG_OPTION": "1",
			"GO_EXPECT_SESSION_MODE":          "code",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{{ID: "mode", Value: "code"}}, nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
}

func TestClientCreateSessionSetsBooleanConfigOption(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_BOOLEAN_CONFIG_ID":    "fast_mode",
			"GO_EXPECT_BOOLEAN_CONFIG_VALUE": "false",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{BooleanSessionConfigValue("fast_mode", false)}, nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
}

func TestClientCreateSessionIgnoresSetModeUnsupported(t *testing.T) {
	var logBuf testLogBuffer
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_DISABLE_SET_MODE":    "1",
			"GO_EXPECT_SESSION_MODE": "code",
		}),
		Logger: testSlogLogger(&logBuf, slog.LevelWarn),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{{ID: "mode", Value: "code"}}, nil)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("CreateSession() returned empty session id")
	}
	if got := logBuf.String(); !strings.Contains(got, "session/set_mode unsupported") {
		t.Fatalf("warn log = %q", got)
	}
}

func TestClientCreateSessionFailsOnSetModeRequestError(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_MODE": "different-mode",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	_, err = client.CreateSession(t.Context(), t.TempDir(), []SessionConfigValue{{ID: "mode", Value: "code"}}, nil)
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
			name: "plain error",
			err:  errors.New("not found"),
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
	if isACPSessionAlreadyExistsError(errors.New("already exists")) {
		t.Fatal("isACPSessionAlreadyExistsError(plain error) = true, want false")
	}

	err := &acp.RequestError{
		Code:    -32602,
		Message: "Invalid params",
		Data:    `session "019e8707-094e-7723-8aa4-ab45abe89d51" already exists`,
	}
	if !isACPSessionAlreadyExistsError(err) {
		t.Fatalf("isACPSessionAlreadyExistsError(%v) = false, want true", err)
	}
}

func TestAgentNewValidationAndDefaults(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{}); err == nil || !strings.Contains(err.Error(), "acp command is required") {
		t.Fatalf("New(empty command) error = %v, want command required", err)
	}

	a, err := New(Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New(defaults) error = %v", err)
	}
	defer closeTestCloser(t, a)
	if a.Name() != defaultAgentName {
		t.Fatalf("Name() = %q, want %q", a.Name(), defaultAgentName)
	}
	if a.Description() != defaultAgentDescription {
		t.Fatalf("Description() = %q, want %q", a.Description(), defaultAgentDescription)
	}

	if _, err := New(Config{
		Command:    []string{"sh", "-c", "exit 1"},
		WorkingDir: t.TempDir(),
	}); err == nil || !strings.Contains(err.Error(), "initialize acp client") {
		t.Fatalf("New(init failure) error = %v, want initialize error", err)
	}
}

func TestNewWithContext(t *testing.T) {
	t.Parallel()

	var nilContext context.Context
	if _, err := NewWithContext(nilContext, Config{}); err == nil || !strings.Contains(err.Error(), "context is required") {
		t.Fatalf("NewWithContext(nil, Config{}) error = %v, want context required", err)
	}

	legacyCtx, cancel := context.WithCancel(t.Context())
	cancel()
	a, err := NewWithContext(t.Context(), Config{
		Context:    legacyCtx,
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewWithContext() error = %v", err)
	}
	defer closeTestCloser(t, a)
}

func TestClientSuppressesPeerDisconnectInfoByDefault(t *testing.T) {
	var stderr bytes.Buffer
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if _, err := client.NewSession(t.Context(), t.TempDir(), nil); err != nil {
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

	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
		Stderr:  io.Discard,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if _, err := client.NewSession(t.Context(), t.TempDir(), nil); err != nil {
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
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
		PermissionHandler: func(_ context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(req.Options[0].OptionId)}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(t.Context(), t.TempDir(), nil)

	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	updates, resultCh, err := client.Prompt(t.Context(), string(sess.SessionId), "permission")
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
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_CLIENT_NAME":    "runtime-acpagent",
			"GO_EXPECT_CLIENT_VERSION": "dev",
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func TestClientInitializeUsesConfiguredIdentity(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
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
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func TestClientAuthenticateRoundTrip(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{"GO_EXPECT_AUTH_METHOD": "oauth"}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if err := client.Authenticate(t.Context(), "oauth"); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
}

func TestClientAuthenticateReturnsRPCError(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{"GO_FAIL_AUTHENTICATE": "1"}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if err := client.Authenticate(t.Context(), "oauth"); err == nil {
		t.Fatal("Authenticate() error = nil, want RPC error")
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
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func TestClientPromptAllowsConcurrentDifferentSessions(t *testing.T) {
	const (
		wantSession1 = testSessionOneOne
		wantSession2 = testSessionTwoTwo
	)

	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess1, err := client.NewSession(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	sess2, err := client.NewSession(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	updates1, resultCh1, err := client.Prompt(t.Context(), string(sess1.SessionId), "slow:one")
	if err != nil {
		t.Fatalf("Prompt(session1) error = %v", err)
	}
	updates2, resultCh2, err := client.Prompt(t.Context(), string(sess2.SessionId), "two")
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

func TestClientLoadSessionWithMetaRoundTrip(t *testing.T) {
	cwd := t.TempDir()
	expectedMeta := `{"provider":"test"}`
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_SUPPORT_LOAD_SESSION":    "1",
			"GO_EXPECT_LOAD_SESSION_ID":  "session-1",
			"GO_EXPECT_LOAD_SESSION_CWD": cwd,
			"GO_EXPECT_LOAD_META_RAW":    expectedMeta,
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if !client.SupportsSessionLoad() {
		t.Fatal("SupportsSessionLoad() = false, want true")
	}
	if _, err := client.LoadSessionWithMeta(t.Context(), "session-1", cwd, nil, map[string]any{"provider": "test"}); err != nil {
		t.Fatalf("LoadSessionWithMeta() error = %v", err)
	}
}

func TestClientLoadSessionWithMetaReturnsRPCError(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{"GO_FAIL_LOAD_ENTITY_NOT_FOUND": "1"}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.LoadSessionWithMeta(t.Context(), "missing", t.TempDir(), nil, nil); err == nil {
		t.Fatal("LoadSessionWithMeta() error = nil, want RPC error")
	}
}

func TestClientPromptRejectsConcurrentSameSession(t *testing.T) {
	const wantSession1 = testSessionOneOne

	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(t.Context(), t.TempDir(), nil)

	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	updates1, resultCh1, err := client.Prompt(t.Context(), string(sess.SessionId), "slow:one")
	if err != nil {
		t.Fatalf("Prompt(first) error = %v", err)
	}
	if _, _, err := client.Prompt(t.Context(), string(sess.SessionId), "two"); !errors.Is(err, ErrPromptAlreadyActive) {
		t.Fatalf("Prompt(second) error = %v, want %v", err, ErrPromptAlreadyActive)
	}

	got1 := readPromptOutput(t, updates1, resultCh1)
	if got1 != wantSession1 {
		t.Fatalf("session output = %q, want %q", got1, wantSession1)
	}
}

func TestClientPromptValidatesInputs(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommand(t),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
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
			_, _, gotErr := client.Prompt(t.Context(), tc.sessionID, tc.prompt)
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
	err := client.SessionUpdate(t.Context(), acp.SessionNotification{
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

	_, err := c.RequestPermission(context.WithValue(t.Context(), ctxKey, ctxVal), acp.RequestPermissionRequest{
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

func TestRequestPermissionUsesActivePromptContext(t *testing.T) {
	type key string
	const ctxKey key = "ctx-key"

	var seen string
	c := &Client{
		logger: newLogger(nil, ""),
		activeBySession: map[acp.SessionId]*activePrompt{
			"session-1": {
				sessionID: "session-1",
				logger:    newLogger(nil, "").withContext(context.WithValue(t.Context(), ctxKey, "prompt-context")),
			},
		},
		permissionHandler: func(ctx context.Context, _ acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			seen, _ = ctx.Value(ctxKey).(string)
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
		},
	}

	_, err := c.RequestPermission(context.WithValue(t.Context(), ctxKey, "callback-context"), acp.RequestPermissionRequest{
		SessionId: "session-1",
	})
	if err != nil {
		t.Fatalf("RequestPermission() error = %v", err)
	}
	if seen != "prompt-context" {
		t.Fatalf("handler context value = %q, want prompt-context", seen)
	}
}
