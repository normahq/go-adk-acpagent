package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/normahq/go-adk-acpagent/v2/acperror"
	adkagent "google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestAttachmentDiagnosticsAreStructural(t *testing.T) {
	const argSecret = "synthetic-private-argv"
	const cwdSecret = "synthetic-private-cwd"
	var logs testLogBuffer
	cwd := filepath.Join(t.TempDir(), cwdSecret)
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_PRIVATE_VALUE": argSecret}), WorkingDir: cwd, LazyInitialization: true, SetupTimeout: 5 * time.Second, Logger: testSlogLogger(&logs, slog.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	if err := a.ensureInitialized(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "diagnostics", UserID: "u", State: map[string]any{SessionStateKey: map[string]any{"meta": map[string]any{"private": "synthetic-private-meta"}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testInvocationContext{Context: t.Context(), session: created.Session}
	if _, err := a.ensureRemoteSession(ctx, ctx, a.invocationLogger(ctx), nil); err != nil {
		t.Fatal(err)
	}
	text := logs.String()
	for _, secret := range []string{argSecret, cwdSecret, "synthetic-private-meta"} {
		if strings.Contains(text, secret) {
			t.Errorf("diagnostics disclosed protected fixture value %q", secret)
		}
	}
	phases := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] != "acp setup phase completed" {
			continue
		}
		phase, _ := record["phase"].(string)
		phases[phase] = true
		if record["cause_class"] != "none" || record["configured_budget_ms"] != float64(5000) {
			t.Fatalf("phase classification=%v", record)
		}
		elapsed, ok := record["elapsed_ms"].(float64)
		effective, hasBudget := record["effective_budget_ms"].(float64)
		if !ok || elapsed < 0 || !hasBudget || effective < 0 || effective > 5000 {
			t.Fatalf("phase timing=%v", record)
		}
	}
	for _, phase := range []string{"initialization_gate", "process_launch", "initialize", "attachment_preflight", "attachment_pending", "session_new", "post_attach_config"} {
		if !phases[phase] {
			t.Errorf("missing actual phase %q", phase)
		}
	}
}

func TestAttachmentCancellationKeepsContextAndFence(t *testing.T) {
	for _, mode := range []string{"caller cancel", "caller deadline", "lifecycle", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "methods")
			var logs testLogBuffer
			life, cancelLife := context.WithCancelCause(t.Context())
			privateCause := errors.New("synthetic private cancellation cause")
			defer cancelLife(privateCause)
			a, err := NewWithContext(life, Config{Command: helperCommandWithEnv(t, map[string]string{"GO_HANG_METHOD": "session/new", "GO_METHOD_LOG": path}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 5 * time.Second, SessionOwner: "fixture-owner", Logger: testSlogLogger(&logs, slog.LevelDebug)})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := a.Close(); err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("close canceled fixture: %v", err)
				}
			}()
			if err := a.ensureInitialized(t.Context()); err != nil {
				t.Fatal(err)
			}
			svc := session.InMemoryService()
			created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "cancel", UserID: "u"})
			if err != nil {
				t.Fatal(err)
			}
			r, err := runnerpkg.New(runnerpkg.Config{AppName: "cancel", Agent: a, SessionService: svc})
			if err != nil {
				t.Fatal(err)
			}
			caller, cancelCause := context.WithCancelCause(t.Context())
			cancel := func() { cancelCause(privateCause) }
			defer cancel()
			want := context.Canceled
			if mode == "caller deadline" {
				caller, cancel = context.WithTimeoutCause(t.Context(), 300*time.Millisecond, privateCause)
				defer cancel()
				want = context.DeadlineExceeded
			}
			type outcome struct {
				err    error
				finals int
			}
			done := make(chan outcome, 1)
			go func() {
				var result outcome
				for event, err := range r.Run(caller, "u", created.Session.ID(), genai.NewContentFromText("private prompt", genai.RoleUser), adkagent.RunConfig{}) {
					if err != nil {
						result.err = err
					}
					if event != nil && event.TurnComplete {
						result.finals++
					}
				}
				done <- result
			}()
			waitInitializeFile(t, path, "session/new\n")
			switch mode {
			case "caller cancel":
				cancel()
			case "lifecycle":
				cancelLife(privateCause)
			case "shutdown":
				if err := a.Close(); err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			result := <-done
			if mode != "shutdown" && !errors.Is(result.err, privateCause) {
				t.Fatalf("original cancellation cause lost: %v", result.err)
			}
			if !errors.Is(result.err, want) || result.finals != 0 {
				t.Fatalf("canceled outcome=%+v, want %v/no final", result, want)
			}
			stored, err := svc.Get(t.Context(), &session.GetRequest{AppName: "cancel", UserID: "u", SessionID: created.Session.ID()})
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := a.resolveSessionConfig(testInvocationContext{Context: t.Context(), session: stored.Session})
			if err != nil || !cfg.attachmentIndeterminate || cfg.sessionID != "" {
				t.Fatalf("canceled fence=%+v,%v", cfg, err)
			}
			logText := logs.String()
			if strings.Contains(logText, privateCause.Error()) || strings.Contains(logText, "private prompt") {
				t.Fatal("cancellation diagnostics disclosed private text")
			}
			expectedCause := "caller_canceled"
			if mode == "caller deadline" {
				expectedCause = "caller_deadline"
			}
			if mode == "lifecycle" || mode == "shutdown" {
				expectedCause = "lifecycle_canceled"
			}
			matched := false
			for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["phase"] == "session_new" {
					matched = true
					if record["cause_class"] != expectedCause || record["outcome"] != "attempted_unknown" {
						t.Fatalf("attempt diagnostic=%v", record)
					}
				}
			}
			if !matched {
				t.Fatal("missing attempted attachment diagnostic")
			}
			methods, err := os.ReadFile(path)
			if err != nil || strings.Count(string(methods), "session/new\n") != 1 || strings.Contains(string(methods), "session/prompt\n") {
				t.Fatalf("canceled methods=%q,%v", methods, err)
			}
		})
	}
}

func TestPreEndedInvocationDoesNotCreateAmbiguity(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "methods")
			a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_METHOD_LOG": path}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, a)
			caller, cancel := context.WithCancel(t.Context())
			cancel()
			want := context.Canceled
			if mode == "deadline" {
				caller, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			svc := session.InMemoryService()
			created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "pre-ended", UserID: "u"})
			if err != nil {
				t.Fatal(err)
			}
			invocation := testInvocationContext{Context: caller, session: created.Session}
			var got error
			for event, err := range a.run(invocation) {
				if event != nil {
					t.Fatal("pre-ended invocation emitted event")
				}
				got = err
			}
			if !errors.Is(got, want) {
				t.Fatalf("pre-ended error=%v, want %v", got, want)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pre-ended protocol work=%v", err)
			}
			cfg, err := a.resolveSessionConfig(testInvocationContext{Context: t.Context(), session: created.Session})
			if err != nil || cfg.attachmentIndeterminate || cfg.sessionID != "" {
				t.Fatalf("pre-ended state=%+v,%v", cfg, err)
			}
		})
	}
}

func TestAttachmentConfigFailureDoesNotInitialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "methods")
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_METHOD_LOG": path}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "config", UserID: "u", State: map[string]any{SessionStateKey: "invalid configuration"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runnerpkg.New(runnerpkg.Config{AppName: "config", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for event, err := range r.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			runErr = err
		}
		if event != nil && event.TurnComplete {
			t.Fatal("invalid local config became a handled attachment outcome")
		}
	}
	if runErr == nil {
		t.Fatal("invalid local session config accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid config initialized a process: %v", err)
	}
}

func TestMissingSessionRecoveryKeepsIndeterminateFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "methods")
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_METHOD_LOG": path, "GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND": "1", "GO_HANG_SESSION_NEW_AT": "2"}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 100 * time.Millisecond, InitializeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "recovery", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runnerpkg.New(runnerpkg.Config{AppName: "recovery", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		finals := 0
		for event, err := range r.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("uncertain recovery returned iterator error: %v", err)
			}
			if event.TurnComplete {
				finals++
				typed, ok := acperror.FromADKMetadata(event.CustomMetadata)
				if event.ErrorCode != attachmentIndeterminateCode || event.Content != nil || !ok || typed.Scope != acperror.ScopeSession {
					t.Fatal("recovery did not return the fixed session-scoped failure")
				}
			}
		}
		if finals != 1 {
			t.Fatalf("recovery failed finals=%d", finals)
		}
	}
	stored, err := svc.Get(t.Context(), &session.GetRequest{AppName: "recovery", UserID: "u", SessionID: created.Session.ID()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testInvocationContext{Context: t.Context(), session: stored.Session}
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil || !cfg.attachmentIndeterminate || cfg.sessionID != "session-1" {
		t.Fatalf("recovery lost known predecessor/fence: %+v,%v", cfg, err)
	}
	if _, err := a.recoverRemoteSession(ctx, ctx, newLogger(nil, ""), errors.New("session not found"), nil); !errors.Is(err, ErrSessionAttachmentIndeterminate) {
		t.Fatalf("lower recovery bypassed fence: %v", err)
	}
	methods, err := os.ReadFile(path)
	if err != nil || strings.Count(string(methods), "session/new\n") != 2 || strings.Count(string(methods), "session/prompt\n") != 1 || strings.Contains(string(methods), "session/resume\n") {
		t.Fatalf("recovery replayed remote work: %q,%v", methods, err)
	}
}

func TestPendingIntentCancellationPreventsNewCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "methods")
	a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, map[string]string{"GO_METHOD_LOG": path}), WorkingDir: t.TempDir(), LazyInitialization: true, SetupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	if err := a.ensureInitialized(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "pending", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	caller, cancel := context.WithCancelCause(t.Context())
	privateCause := errors.New("private cancellation while persisting pending intent")
	defer cancel(privateCause)
	ctx := testInvocationContext{Context: caller, session: created.Session}
	_, err = a.ensureRemoteSession(ctx, ctx, newLogger(nil, ""), func(*session.Event, error) bool { cancel(privateCause); return true })
	if !errors.Is(err, context.Canceled) || !errors.Is(err, privateCause) || errors.Is(err, ErrSessionAttachmentIndeterminate) {
		t.Fatalf("pre-call cancellation lost provenance: %v", err)
	}
	cfg, err := a.resolveSessionConfig(testInvocationContext{Context: t.Context(), session: created.Session})
	if err != nil || !cfg.attachmentIndeterminate || cfg.sessionID != "" {
		t.Fatal("recorded pending intent was erased")
	}
	methods, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(methods), "session/new\n") {
		t.Fatalf("pre-call cancellation dispatched new: %q,%v", methods, err)
	}
}
