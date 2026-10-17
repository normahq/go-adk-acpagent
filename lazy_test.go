package acpagent

import (
	"context"
	"errors"
	"google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"os"
	"strings"
	"testing"
	"time"
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
	path := t.TempDir() + "/methods"
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_METHOD_LOG": path, "GO_HANG_METHOD": "session/new"}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 5 * time.Second})
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
	for i := range 2 {
		var runErr error
		for _, err := range r.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
			if err != nil {
				runErr = err
			}
		}
		if i == 0 && !errors.Is(runErr, context.DeadlineExceeded) {
			t.Fatalf("first run: %v", runErr)
		}
		if i == 1 && !errors.Is(runErr, ErrSessionAttachmentIndeterminate) {
			t.Fatalf("second run: %v", runErr)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "session/new\n") != 1 {
		t.Fatalf("duplicate attachment: %s", data)
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
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_FAIL_METHOD": "session/new"}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 5 * time.Second})
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
	for range 2 {
		_, err := a.ensureRemoteSession(ctx, ctx, newLogger(nil, ""), nil)
		if err == nil || errors.Is(err, ErrSessionAttachmentIndeterminate) {
			t.Fatalf("definitive failure=%v", err)
		}
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
	a, err := NewWithContext(t.Context(), Config{Command: helperCommand(t), WorkingDir: t.TempDir(), LazyInitialization: true})
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
}
