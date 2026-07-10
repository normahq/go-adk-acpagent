package acpagent

import (
	"encoding/json"
	"iter"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

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

	a, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
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
		r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if finalText != sessionID+":hello" {
		t.Fatalf("final text = %q, want %q", finalText, sessionID+":hello")
	}
	if got := finalSessionState["session_id"]; got != sessionID {
		t.Fatalf("final %s.session_id = %v, want %q", SessionStateKey, got, sessionID)
	}
	if diff := cmp.Diff(meta, finalSessionState["meta"]); diff != "" {
		t.Errorf("final %s.meta mismatch (-want +got):\n%s", SessionStateKey, diff)
	}
}

func TestAgentUsesStateSessionFromSessionID(t *testing.T) {
	workingDir := t.TempDir()
	callerSessionID := "caller-provided-session"
	expectedPromptsRaw, err := json.Marshal([]string{"hello"})
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}

	a, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
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
		r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
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

	a, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
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
		r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
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

	a, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
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
		r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if finalText != sessionID+":hello" {
		t.Fatalf("final text = %q, want %s:hello", finalText, sessionID)
	}
	if got := finalSessionState["session_id"]; got != sessionID {
		t.Fatalf("final %s.session_id = %v, want %q", SessionStateKey, got, sessionID)
	}
	wantMeta := map[string]any{
		"codex": map[string]any{
			"approvalMode": "manual",
			"config": map[string]any{
				"model_reasoning_effort": "high",
			},
		},
	}
	if diff := cmp.Diff(wantMeta, finalSessionState["meta"]); diff != "" {
		t.Errorf("final %s.meta mismatch (-want +got):\n%s", SessionStateKey, diff)
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

			a, err := NewWithContext(t.Context(), Config{
				Command:    helperCommandWithEnv(t, env),
				WorkingDir: workingDir,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer closeTestCloser(t, a)

			sessionService := session.InMemoryService()
			r, err := runnerpkg.New(runnerpkg.Config{
				AppName:        "test-app",
				Agent:          a,
				SessionService: sessionService,
			})
			if err != nil {
				t.Fatalf("runner.New() error = %v", err)
			}
			sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
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
				r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
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

	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
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
	firstAgent, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, firstAgent)

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
		firstRunner.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if firstText != testSessionOneHello {
		t.Fatalf("first final text = %q, want %q", firstText, testSessionOneHello)
	}
	if got := firstSessionState["session_id"]; got != testSessionOneID {
		t.Fatalf("first final %s.session_id = %v, want %s", SessionStateKey, got, testSessionOneID)
	}

	stored, err := sessionService.Get(t.Context(), &session.GetRequest{
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
	secondAgent, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, secondAgent)

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
		secondRunner.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("again", genai.RoleUser), agent.RunConfig{}),
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

	a, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
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

	first := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if first != sessionID+":hello" {
		t.Fatalf("first final text = %q, want %q", first, sessionID+":hello")
	}

	second := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("again", genai.RoleUser), agent.RunConfig{}))
	if second != sessionID+":again" {
		t.Fatalf("second final text = %q, want %q", second, sessionID+":again")
	}
}

func TestAgentUsesStateSessionWhenSessionConfigChanges(t *testing.T) {
	defaultWorkingDir := t.TempDir()
	overrideWorkingDir := t.TempDir()
	var bootstrapBuf testLogBuffer
	bootstrapLogger := testSlogLogger(&bootstrapBuf, slog.LevelDebug)

	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": defaultWorkingDir,
		}),
		WorkingDir: defaultWorkingDir,
		Logger:     bootstrapLogger,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var invocationBuf testLogBuffer
	invocationLogger := testSlogLogger(&invocationBuf, slog.LevelDebug).With("source", "invocation")
	invocationCtx := contextWithLogger(t.Context(), newLogger(invocationLogger, ""))

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

	a, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first, firstSessionState := collectFinalTextAndSessionState(
		t,
		r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}),
	)
	if first != "session-2:hello" {
		t.Fatalf("first final text = %q, want session-2:hello", first)
	}
	if got := firstSessionState["session_id"]; got != "session-2" {
		t.Fatalf("first final %s.session_id = %v, want session-2", SessionStateKey, got)
	}

	second := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("again", genai.RoleUser), agent.RunConfig{}))
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
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %q", got, testSessionOneHello)
	}
}

func TestAgentRunTurnCompleteIncludesFinalContent(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	partialText := ""
	finalText := ""
	turnCompleteCount := 0
	for ev, err := range r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
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
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalEvent := collectFinalEvent(t, r.Run(
		t.Context(),
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

func TestAgentRunTurnCompleteIncludesPromptMetaTerminalErrorWithoutVisibleReply(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalEvent := collectFinalEvent(t, r.Run(
		t.Context(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("terminal-meta-error", genai.RoleUser),
		agent.RunConfig{},
	))

	if finalEvent.Content != nil {
		t.Fatalf("final content = %v, want nil", finalEvent.Content)
	}
	if got := finalEvent.ErrorMessage; got != "quota exceeded for this account" {
		t.Fatalf("error message = %q, want prompt meta terminal error", got)
	}
	if got := finalEvent.ErrorCode; got != "quota_exceeded" {
		t.Fatalf("error code = %q, want quota_exceeded", got)
	}
}

func TestAgentRunIgnoresRetryOnlyErrorsWhenTurnLaterSucceeds(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	finalEvent := collectFinalEvent(t, r.Run(
		t.Context(),
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

	a, err := NewWithContext(t.Context(), Config{
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
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var invocationBuf testLogBuffer
	invocationLogger := testSlogLogger(&invocationBuf, slog.LevelDebug).With("source", "invocation")
	invocationCtx := contextWithLogger(t.Context(), newLogger(invocationLogger, ""))

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
	} {
		if !strings.Contains(invocationLogs, mustContain) {
			t.Fatalf("invocation log missing %q: %q", mustContain, invocationLogs)
		}
	}
	if strings.Contains(invocationLogs, `"session_id":"`+testSessionOneID+`"`) {
		t.Fatalf("invocation log reused ACP session id as session_id: %q", invocationLogs)
	}
	for _, sensitive := range []string{`"prompt":"hello"`, `"text":"hello"`} {
		if strings.Contains(invocationLogs, sensitive) {
			t.Fatalf("debug log contains sensitive payload %q: %q", sensitive, invocationLogs)
		}
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
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	seenCall := false
	seenUpdate := false
	seenMessage := false
	seenTurnComplete := false

	for ev, err := range r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("tooling", genai.RoleUser), agent.RunConfig{}) {
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
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
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
		t.Context(),
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

	stored, err := sessionService.Get(t.Context(), &session.GetRequest{
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
	if diff := cmp.Diff(streamedPlanSnapshots[1], finalPlanSnapshot); diff != "" {
		t.Errorf("final plan snapshot mismatch (-want +got):\n%s", diff)
	}
	storedSnapshot := planSnapshotFromValue(t, storedSnapshotValue)
	if diff := cmp.Diff(streamedPlanSnapshots[1], storedSnapshot); diff != "" {
		t.Errorf("stored plan snapshot mismatch (-want +got):\n%s", diff)
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

	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_MCP_SERVERS": string(expectedJSON),
		}),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer closeTestCloser(t, client)

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	sess, err := client.NewSession(t.Context(), t.TempDir(), expectedServers)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("NewSession() returned empty session id")
	}
}

func TestClientNewSessionSendsEmptyMCPServersArrayWhenNil(t *testing.T) {
	client, err := NewClient(t.Context(), ClientConfig{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_MCP_SERVERS_RAW": "[]",
		}),
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
	if got := strings.TrimSpace(string(sess.SessionId)); got == "" {
		t.Fatal("NewSession() returned empty session id")
	}
}

func TestAgentConfigMCPServersUseEmptyArraysNotNull(t *testing.T) {
	expectedRaw := `[{"headers":[],"name":"http_server","type":"http","url":"http://localhost:9999/mcp"},{"headers":[],"name":"sse_server","type":"sse","url":"http://localhost:9998/sse"},{"args":[],"command":"echo","env":[],"name":"stdio_server"}]`

	a, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	for _, runErr := range r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("ping", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("runner event error = %v", runErr)
		}
	}
}

func TestAgentRunStoresOutputKeyInFinalStateDelta(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
		OutputKey:  "result",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closeTestCloser(t, a)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "test-app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var finalOutput string
	for ev, runErr := range r.Run(
		t.Context(),
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

	stored, err := sessionService.Get(t.Context(), &session.GetRequest{
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
