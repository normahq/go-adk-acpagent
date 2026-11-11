package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/normahq/go-adk-acpagent/v2/acperror"
	"google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestLazyAgentPreservesConfiguredEnvironment(t *testing.T) {
	environment := []string{"ACPAGENT_EXACT=value"}
	a, err := NewWithContext(t.Context(), Config{
		Command:            []string{"/does/not/exist"},
		Environment:        environment,
		LazyInitialization: true,
	})
	if err != nil {
		t.Fatalf("NewWithContext() error = %v", err)
	}
	defer closeTestCloser(t, a)
	environment[0] = "ACPAGENT_EXACT=mutated"
	if got := a.clientConfig.Environment; len(got) != 1 || got[0] != "ACPAGENT_EXACT=value" {
		t.Fatalf("client environment = %q, want copied original", got)
	}

	emptyAgent, err := NewWithContext(t.Context(), Config{
		Command:            []string{"/does/not/exist"},
		Environment:        []string{},
		LazyInitialization: true,
	})
	if err != nil {
		t.Fatalf("NewWithContext(empty environment) error = %v", err)
	}
	defer closeTestCloser(t, emptyAgent)
	if emptyAgent.clientConfig.Environment == nil {
		t.Fatal("client environment = nil, want explicit empty slice")
	}
}

func TestLazyConstructorDefersProcessLaunch(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{Command: []string{"/does/not/exist"}, LazyInitialization: true, SetupTimeout: time.Second})
	if err != nil {
		t.Fatalf("local constructor: %v", err)
	}
	defer closeTestCloser(t, a)
	if err := a.ensureInitialized(t.Context()); err == nil {
		t.Fatal("first attachment must report missing executable")
	}
}

func TestLazyInitializationRequestDoesNotOwnProcess(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{Command: helperCommand(t), LazyInitialization: true, SetupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	if err := a.ensureInitialized(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := a.client.NewSession(t.Context(), t.TempDir(), nil); err != nil {
		t.Fatalf("process died after setup cancellation: %v", err)
	}
}

func TestLazyInitializeTimeoutReapsAndAllowsLaterAttempt(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_HANG_METHOD": "initialize"}), LazyInitialization: true, SetupTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	for range 2 {
		start := time.Now()
		if err := a.ensureInitialized(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("initialize error = %v", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("initialize exceeded setup and cleanup budget")
		}
	}
}

func TestLazyNewTimeoutPreventsDuplicateAttachment(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_HANG_METHOD": "session/new"}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 100 * time.Millisecond, InitializeTimeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	if err := a.ensureInitialized(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "lazy", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testInvocationContext{Context: t.Context(), session: created.Session}
	start := time.Now()
	_, err = a.ensureRemoteSession(ctx, ctx, newLogger(nil, ""), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new error=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("new not bounded")
	}
	start = time.Now()
	_, err = a.ensureRemoteSession(ctx, ctx, newLogger(nil, ""), nil)
	if !errors.Is(err, ErrSessionAttachmentIndeterminate) {
		t.Fatalf("repeat new error=%v", err)
	}
	if time.Since(start) > 40*time.Millisecond {
		t.Fatal("repeat issued a remote request")
	}
}

func TestLazySlowFirstInvocationAttachesWithoutIdleRetry(t *testing.T) {
	path := t.TempDir() + "/methods"
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_METHOD_LOG": path, "GO_PID_LOG": path + ".pid", "GO_INITIALIZE_DELAY": "11s"}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 10 * time.Second, InitializeTimeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("constructor performed protocol work: %v", err)
	}
	svc := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{AppName: "lazy", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "lazy", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got := collectFinalText(t, r.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
		if got != "session-1:hello" {
			t.Fatalf("output=%q", got)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "initialize\n") != 1 || strings.Count(string(data), "session/new\n") != 1 || strings.Count(string(data), "session/prompt\n") != 2 {
		t.Fatalf("methods=%s", data)
	}
	pidData, err := os.ReadFile(path + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(string(pidData))) != 1 {
		t.Fatalf("cold/warm turns launched multiple children: %s", pidData)
	}
}

func TestLazyResumeTimeoutPreservesKnownID(t *testing.T) {
	for _, method := range []string{"session/resume", "session/prompt"} {
		t.Run(method, func(t *testing.T) {
			env := map[string]string{"GO_SUPPORT_SESSION_RESUME": "1", "GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1"}
			if method == "session/resume" {
				env["GO_HANG_METHOD"] = method
			} else {
				env["GO_FAIL_METHOD"] = method
			}
			a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, env), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 100 * time.Millisecond, InitializeTimeout: 60 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, a)
			if err := a.ensureInitialized(t.Context()); err != nil {
				t.Fatal(err)
			}
			svc := session.InMemoryService()
			created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "lazy", UserID: "u", State: map[string]any{SessionStateKey: map[string]any{"session_id": "known"}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := testInvocationContext{Context: t.Context(), session: created.Session}
			if method == "session/resume" {
				_, err = a.recoverRemoteSession(ctx, ctx, newLogger(nil, ""), errors.New("session not found"), nil)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("resume error=%v", err)
				}
			} else {
				_, err = a.runPromptOnce(ctx, ctx, newLogger(nil, ""), "known", nil, func(*session.Event, error) bool { return true })
				if err == nil {
					t.Fatal("expected outage error")
				}
			}
			cfg, err := a.resolveSessionConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.sessionID != "known" {
				t.Fatalf("remote ID=%q", cfg.sessionID)
			}
		})
	}
}

func TestLazyIndeterminateAttachmentSurvivesRunnerReload(t *testing.T) {
	for _, response := range []string{"no response", "late"} {
		t.Run(response, func(t *testing.T) {
			path := t.TempDir() + "/methods"
			env := map[string]string{"GO_METHOD_LOG": path, "GO_HANG_METHOD": "session/new"}
			if response == "late" {
				delete(env, "GO_HANG_METHOD")
				env["GO_NEW_DELAY"] = "200ms"
				env["GO_NEW_RESPONSE_MARKER"] = path + ".response"
			}
			command := helperCommandWithEnv(t, env)
			a, err := NewWithContext(t.Context(), Config{Command: command, WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 5 * time.Second, OutputKey: "result"})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, a)
			if err := a.ensureInitialized(t.Context()); err != nil {
				t.Fatal(err)
			}
			a.setupTimeout = 100 * time.Millisecond
			svc := session.InMemoryService()
			r, err := runnerpkg.New(runnerpkg.Config{AppName: "lazy", Agent: a, SessionService: svc})
			if err != nil {
				t.Fatal(err)
			}
			created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "lazy", UserID: "u", State: map[string]any{"result": "retained output", PlanStateKey: map[string]any{"saved": "plan"}}})
			if err != nil {
				t.Fatal(err)
			}
			assertFailedTurn := func(r *runnerpkg.Runner) {
				t.Helper()
				var finals int
				for event, runErr := range r.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
					if runErr != nil {
						t.Fatalf("live indeterminate turn returned iterator error: %v", runErr)
					}
					if event.TurnComplete {
						finals++
						providerErr, ok := acperror.FromADKMetadata(event.CustomMetadata)
						if _, changed := event.Actions.StateDelta[SessionStateKey]; changed {
							t.Fatal("failed terminal overwrote the pending marker")
						}
						if event.ErrorMessage != attachmentIndeterminateMessage || providerErr != nil && providerErr.RetryAt != nil {
							t.Fatal("unsafe recovery notice or invented retry time")
						}
						if event.Partial || event.Content != nil || event.ErrorCode != "acp_session_attachment_indeterminate" || !ok || providerErr.Kind != acperror.KindUnknown || providerErr.Scope != acperror.ScopeSession {
							t.Fatalf("indeterminate final=%+v metadata=%+v", event, providerErr)
						}
					}
				}
				if finals != 1 {
					t.Fatalf("failed terminal events=%d, want1", finals)
				}
			}
			assertFailedTurn(r)
			if response == "late" {
				waitInitializeFile(t, path+".response", "response attempted")
			}
			assertFailedTurn(r)
			reloaded, err := svc.Get(t.Context(), &session.GetRequest{AppName: "lazy", UserID: "u", SessionID: created.Session.ID()})
			if err != nil {
				t.Fatal(err)
			}
			if output, err := reloaded.Session.State().Get("result"); err != nil || output != "retained output" {
				t.Fatalf("retained output=%v,%v", output, err)
			}
			if plan, err := reloaded.Session.State().Get(PlanStateKey); err != nil || plan == nil {
				t.Fatal("retained plan lost")
			}
			cfg, err := a.resolveSessionConfig(testInvocationContext{Context: t.Context(), session: reloaded.Session})
			if err != nil || !cfg.attachmentIndeterminate || cfg.sessionID != "" {
				t.Fatalf("retained fence=%+v, %v", cfg, err)
			}
			reopened, err := NewWithContext(t.Context(), Config{Command: command, WorkingDir: a.workingDir, LazyInitialization: true, SetupTimeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, reopened)
			reopenedRunner, err := runnerpkg.New(runnerpkg.Config{AppName: "lazy", Agent: reopened, SessionService: svc})
			if err != nil {
				t.Fatal(err)
			}
			assertFailedTurn(reopenedRunner)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "initialize\n") != 1 || strings.Count(string(data), "session/new\n") != 1 || strings.Contains(string(data), "session/prompt\n") || strings.Contains(string(data), "session/resume\n") {
				t.Fatalf("duplicate attachment: %s", data)
			}
		})
	}
}

func TestLazyKnownIDSurvivesConfigurationTimeout(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_HANG_METHOD": "session/set_config_option", "GO_CURRENT_SESSION_MODEL": "old", "GO_EXPECT_SESSION_MODEL": "new"}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 5 * time.Second, SessionConfig: []SessionConfigValue{SelectSessionConfigValue("model", "new")}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	if err := a.ensureInitialized(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.setupTimeout = 100 * time.Millisecond
	svc := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{AppName: "lazy", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "lazy", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	for range r.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
	}
	reloaded, err := svc.Get(t.Context(), &session.GetRequest{AppName: "lazy", UserID: "u", SessionID: created.Session.ID()})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.resolveSessionConfig(testInvocationContext{Context: t.Context(), session: reloaded.Session})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.sessionID != "session-1" || cfg.attachmentIndeterminate {
		t.Fatalf("stored attachment=%+v", cfg)
	}
}

func TestLazyCloseCancelsInitializationAndQueuedCaller(t *testing.T) {
	path := t.TempDir() + "/methods"
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_METHOD_LOG": path, "GO_HANG_METHOD": "initialize"}), LazyInitialization: true, SetupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.ensureInitialized(t.Context()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, _ := os.ReadFile(path); strings.Contains(string(data), "initialize\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initialize not dispatched")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.ensureInitialized(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued init error=%v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close during initialize=%v", err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("active init error=%v", err)
	}
	if err := a.ensureInitialized(t.Context()); err == nil {
		t.Fatal("initialization after close succeeded")
	}
}

func TestLazyConfigurationValidation(t *testing.T) {
	for _, cfg := range []Config{
		{Command: []string{"missing"}, LazyInitialization: true, SetupTimeout: -time.Second},
		{Command: []string{"missing"}, LazyInitialization: true, MCPServers: map[string]MCPServerConfig{"invalid": {Type: "invalid"}}},
	} {
		if a, err := NewWithContext(t.Context(), cfg); err == nil {
			closeTestCloser(t, a)
			t.Fatal("invalid local configuration accepted")
		}
	}
}

func TestLazyDefinitiveAttachmentFailureCanRetry(t *testing.T) {
	var logs testLogBuffer
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_FAIL_SESSION_NEW_AT": "1"}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 5 * time.Second, Logger: testSlogLogger(&logs, slog.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	if err := a.ensureInitialized(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "lazy", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testInvocationContext{Context: t.Context(), session: created.Session}
	if _, err := a.ensureRemoteSession(ctx, ctx, a.invocationLogger(ctx), nil); err == nil || errors.Is(err, ErrSessionAttachmentIndeterminate) {
		t.Fatalf("bounded non-context failure=%v", err)
	}
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil || cfg.attachmentIndeterminate {
		t.Fatal("non-context failure retained a pending marker")
	}
	remote, err := a.ensureRemoteSession(ctx, ctx, a.invocationLogger(ctx), nil)
	if err != nil || remote.id != "session-2" {
		t.Fatalf("supported retry=%+v,%v", remote, err)
	}
	cleared := false
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["phase"] == "session_new" && record["cause_class"] == "protocol_or_other" {
			cleared = record["outcome"] == "pending_cleared"
		}
	}
	if !cleared {
		t.Fatal("diagnostics did not report actual cleared protection state")
	}
}

func TestLazyInvocationReportsLaunchFailure(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{Command: []string{"/does/not/exist"}, LazyInitialization: true})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	var runErr error
	for _, err := range a.run(testInvocationContext{Context: t.Context()}) {
		runErr = err
	}
	if runErr == nil {
		t.Fatal("invocation hid process launch failure")
	}
}

func TestLazyAttachmentRequiresRecordedPendingState(t *testing.T) {
	path := t.TempDir() + "/methods"
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_METHOD_LOG": path}), WorkingDir: t.TempDir(), LazyInitialization: true})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	if err := a.ensureInitialized(t.Context()); err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("storage failed")
	ctx := testInvocationContext{Context: t.Context(), session: testStateSession{state: testState{setErr: writeErr}}}
	if _, err := a.ensureRemoteSession(ctx, ctx, newLogger(nil, ""), nil); !errors.Is(err, writeErr) {
		t.Fatalf("state failure=%v", err)
	}
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "lazy", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	ctx.session = created.Session
	if _, err := a.ensureRemoteSession(ctx, ctx, newLogger(nil, ""), func(*session.Event, error) bool { return false }); !errors.Is(err, context.Canceled) {
		t.Fatalf("consumer stop=%v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || strings.Contains(string(data), "session/new\n") {
		t.Fatalf("failed pending persistence reached new RPC: %q,%v", data, readErr)
	}
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil || !cfg.attachmentIndeterminate {
		t.Fatal("stopped consumer erased already-recorded pending intent")
	}
}
