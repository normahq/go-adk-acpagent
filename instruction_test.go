package acpagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestAgentReusesRemoteSession(t *testing.T) {
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

	first := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("one", genai.RoleUser), agent.RunConfig{}))
	second := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("two", genai.RoleUser), agent.RunConfig{}))

	if first != testSessionOneOne {
		t.Fatalf("first final text = %q, want session-1:one", first)
	}
	if second != testSessionOneTwo {
		t.Fatalf("second final text = %q, want session-1:two", second)
	}
}

func TestAgentAppliesModelConfigOption(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command:       helperCommandWithEnv(t, map[string]string{"GO_EXPECT_SESSION_MODEL": "openai/gpt-5.4"}),
		SessionConfig: []SessionConfigValue{{ID: "model", Value: "openai/gpt-5.4"}},
		WorkingDir:    t.TempDir(),
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

	finalText, finalSessionState := collectFinalTextAndSessionState(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if finalText != testSessionOneHello {
		t.Fatalf("final text = %q, want %q", finalText, testSessionOneHello)
	}
	wantConfigValues := []map[string]any{{"id": "model", "value": "openai/gpt-5.4"}}
	if diff := cmp.Diff(wantConfigValues, finalSessionState["config_values"]); diff != "" {
		t.Errorf("final %s.config_values mismatch (-want +got):\n%s", SessionStateKey, diff)
	}
}

func TestAgentReusesConfiguredSessionWithoutReapplyingUnavailableConfigOptions(t *testing.T) {
	promptsRaw, err := json.Marshal([]string{"one", "two"})
	if err != nil {
		t.Fatalf("json.Marshal(prompts) error = %v", err)
	}
	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_PROMPTS":       string(promptsRaw),
			"GO_EXPECT_SESSION_MODEL": "openai/gpt-5.4",
		}),
		SessionConfig: []SessionConfigValue{{ID: "model", Value: "openai/gpt-5.4"}},
		WorkingDir:    t.TempDir(),
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

	first := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("one", genai.RoleUser), agent.RunConfig{}))
	second := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("two", genai.RoleUser), agent.RunConfig{}))

	if first != testSessionOneOne {
		t.Fatalf("first final text = %q, want %q", first, testSessionOneOne)
	}
	if second != testSessionOneTwo {
		t.Fatalf("second final text = %q, want %q", second, testSessionOneTwo)
	}
}

func TestAgentBeforeAgentCallbacksShortCircuitACPPrompt(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
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

	got := collectEventTexts(t, r.Run(
		t.Context(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("hello", genai.RoleUser),
		agent.RunConfig{},
	))
	if diff := cmp.Diff([]string{"before-callback"}, got); diff != "" {
		t.Errorf("event texts mismatch (-want +got):\n%s", diff)
	}
}

func TestAgentAfterAgentCallbacksEmitPostRunEvent(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
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

	got := collectEventTexts(t, r.Run(
		t.Context(),
		"test-user",
		sess.Session.ID(),
		genai.NewContentFromText("hello", genai.RoleUser),
		agent.RunConfig{},
	))
	want := []string{"session-1:", "hello", testSessionOneHello, "after-callback"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("event texts mismatch (-want +got):\n%s", diff)
	}
}

func TestAgentUsesWorkingDirAsDefaultSessionCWD(t *testing.T) {
	workingDir := t.TempDir()
	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": workingDir,
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
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{AppName: "test-app", UserID: "test-user"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentUsesSessionStateCWDOverride(t *testing.T) {
	defaultWorkingDir := t.TempDir()
	overrideWorkingDir := t.TempDir()
	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": overrideWorkingDir,
		}),
		WorkingDir: defaultWorkingDir,
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
			"cwd": overrideWorkingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentInjectsSessionStateIntoInstruction(t *testing.T) {
	workingDir := t.TempDir()
	a, err := NewWithContext(t.Context(), Config{
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
			"cwd":     workingDir,
			"project": "relay",
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	want := testSessionOneHello
	if got != want {
		t.Fatalf("final text = %q, want %q", got, want)
	}
}

func TestAgentInstructionProviderSkipsTemplateInjection(t *testing.T) {
	workingDir := t.TempDir()
	a, err := NewWithContext(t.Context(), Config{
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
			"cwd":     workingDir,
			"project": "relay",
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	want := "session-1:hello"
	if got != want {
		t.Fatalf("final text = %q, want %q", got, want)
	}
}

func TestAgentInstructionProviderReceivesReadonlyContext(t *testing.T) {
	seen := false
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
		InstructionProvider: func(ctx agent.ReadonlyContext) (string, error) {
			seen = true
			if ctx.UserContent() == nil || extractPromptText(ctx.UserContent()) != "hello" {
				return "", fmt.Errorf("readonly user content = %#v, want hello", ctx.UserContent())
			}
			if ctx.InvocationID() == "" {
				return "", fmt.Errorf("readonly invocation id is empty")
			}
			if ctx.AgentName() != defaultAgentName {
				return "", fmt.Errorf("readonly agent name = %q, want %q", ctx.AgentName(), defaultAgentName)
			}
			if ctx.UserID() != "test-user" || ctx.AppName() != "test-app" {
				return "", fmt.Errorf("readonly identity = (%q, %q), want test-user/test-app", ctx.UserID(), ctx.AppName())
			}
			if ctx.SessionID() == "" {
				return "", fmt.Errorf("readonly session id is empty")
			}
			_ = ctx.Branch()
			if got, err := ctx.ReadonlyState().Get("topic"); err != nil || got != "docs" {
				return "", fmt.Errorf("readonly state topic = (%v, %v), want docs nil", got, err)
			}
			if _, ok := ctx.Deadline(); ok {
				return "", fmt.Errorf("readonly deadline ok = true, want false")
			}
			_ = ctx.Done()
			if ctx.Err() != nil {
				return "", fmt.Errorf("readonly Err() = %v, want nil", ctx.Err())
			}
			if got := ctx.Value("missing"); got != nil {
				return "", fmt.Errorf("readonly Value(missing) = %#v, want nil", got)
			}
			return "", nil
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
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State:   map[string]any{"topic": "docs"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	for _, runErr := range r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("runner event error = %v", runErr)
		}
	}
	if !seen {
		t.Fatal("instruction provider was not called")
	}
}

func TestAgentPrependsInstructionsOnlyOncePerADKSession(t *testing.T) {
	workingDir := t.TempDir()
	expectedPrompts := expectedPromptsJSON(
		t,
		instructionPrompt("project=relay cwd="+workingDir, "one"),
		"two",
	)

	a, err := NewWithContext(t.Context(), Config{
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
			"cwd":     workingDir,
			"project": "relay",
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("one", genai.RoleUser), agent.RunConfig{}))
	if first != testSessionOneOne {
		t.Fatalf("first final text = %q, want session-1:one", first)
	}
	second := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("two", genai.RoleUser), agent.RunConfig{}))
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

	a, err := NewWithContext(t.Context(), Config{
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
	firstSession, err := sessionService.Create(t.Context(), &session.CreateRequest{
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
	secondSession, err := sessionService.Create(t.Context(), &session.CreateRequest{
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

	first := collectFinalText(t, r.Run(t.Context(), "test-user", firstSession.Session.ID(), genai.NewContentFromText("one", genai.RoleUser), agent.RunConfig{}))
	if first != testSessionOneOne {
		t.Fatalf("first final text = %q, want session-1:one", first)
	}
	second := collectFinalText(t, r.Run(t.Context(), "test-user", secondSession.Session.ID(), genai.NewContentFromText("two", genai.RoleUser), agent.RunConfig{}))
	if second != testSessionTwoTwo {
		t.Fatalf("second final text = %q, want session-2:two", second)
	}
}

func TestAgentInstructionTemplates(t *testing.T) {
	tests := []struct {
		name             string
		instruction      string
		state            map[string]any
		wantInstruction  string
		wantErrSubstring string
	}{
		{
			name:             "missing required state",
			instruction:      "missing={not_set}",
			wantErrSubstring: "inject session state into instruction",
		},
		{
			name:            "optional missing state",
			instruction:     "optional={missing?}",
			wantInstruction: "optional=",
		},
		{
			name:            "invalid state names remain literal",
			instruction:     "invalid={invalid-key} prefix={invalid:key}",
			wantInstruction: "invalid={invalid-key} prefix={invalid:key}",
		},
		{
			name:            "prefixed state",
			instruction:     "prefixed={app:user_name}",
			state:           map[string]any{"app:user_name": "Foo"},
			wantInstruction: "prefixed=Foo",
		},
		{
			name:             "artifact service missing",
			instruction:      "artifact={artifact.my_file}",
			wantErrSubstring: "artifact service is not initialized",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expectedPrompts := ""
			if test.wantInstruction != "" {
				expectedPrompts = expectedPromptsJSON(t, instructionPrompt(test.wantInstruction, "hello"))
			}
			got, err := runInstructionTemplate(t, test.instruction, expectedPrompts, test.state)
			if test.wantErrSubstring != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrSubstring) {
					t.Fatalf("runInstructionTemplate() error = %v, want substring %q", err, test.wantErrSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("runInstructionTemplate() error = %v", err)
			}
			if got != testSessionOneHello {
				t.Errorf("runInstructionTemplate() = %q, want %q", got, testSessionOneHello)
			}
		})
	}
}

func TestAgentInjectsArtifactsIntoInstruction(t *testing.T) {
	workingDir := t.TempDir()
	expectedPrompts := expectedPromptsJSON(t, instructionPrompt("artifact=artifact-content optional=", "hello"))

	a, err := NewWithContext(t.Context(), Config{
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
	defer closeTestCloser(t, a)

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
	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			"cwd": workingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err = artifactService.Save(t.Context(), &artifact.SaveRequest{
		AppName:   "test-app",
		UserID:    "test-user",
		SessionID: sess.Session.ID(),
		FileName:  "my_file",
		Part:      &genai.Part{Text: "artifact-content"},
	})
	if err != nil {
		t.Fatalf("artifact.Save() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func runInstructionTemplate(t *testing.T, instruction, expectedPrompts string, extraState map[string]any) (string, error) {
	t.Helper()

	workingDir := t.TempDir()
	env := map[string]string{"GO_EXPECT_SESSION_CWD": workingDir}
	if expectedPrompts != "" {
		env["GO_EXPECT_PROMPTS"] = expectedPrompts
	}
	a, err := NewWithContext(t.Context(), Config{
		Command:     helperCommandWithEnv(t, env),
		WorkingDir:  workingDir,
		Instruction: instruction,
	})
	if err != nil {
		t.Fatalf("NewWithContext() error = %v", err)
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

	state := map[string]any{"cwd": workingDir}
	for k, v := range extraState {
		state[k] = v
	}

	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State:   state,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var finalText string
	// collectFinalText is not used here because this helper must return run
	// errors to the table test instead of failing it directly.
	for ev, err := range r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			return "", err
		}
		if ev != nil && ev.TurnComplete && !ev.Partial {
			finalText = extractPromptText(ev.Content)
		}
	}
	return finalText, nil
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

	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD": overrideWorkingDir,
		}),
		WorkingDir: defaultWorkingDir,
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
			"cwd": relativeOverride,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentFailsOnInvalidSessionStateCWDOverride(t *testing.T) {
	defaultWorkingDir := t.TempDir()
	missingWorkingDir := filepath.Join(t.TempDir(), "missing")
	a, err := NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: defaultWorkingDir,
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
			"cwd": missingWorkingDir,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var runErr error
	for _, err := range r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
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

	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_EXPECT_SESSION_CWD":          workingDir,
			"GO_EXPECT_NEW_SESSION_META_RAW": string(metaJSON),
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
				"meta": meta,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
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

	a, err := NewWithContext(t.Context(), Config{
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
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
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

	a, err := NewWithContext(t.Context(), Config{
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
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
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

	a, err := NewWithContext(t.Context(), Config{
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
				"meta": meta,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
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

	a, err := NewWithContext(t.Context(), Config{
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
				"meta": meta,
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := collectFinalText(t, r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}))
	if got != testSessionOneHello {
		t.Fatalf("final text = %q, want %s", got, testSessionOneHello)
	}
}

func TestAgentFailsWhenCodexInstructionMetaIsNotObject(t *testing.T) {
	workingDir := t.TempDir()
	a, err := NewWithContext(t.Context(), Config{
		Command:     helperCommandWithEnv(t, map[string]string{"GO_EXPECT_SESSION_CWD": workingDir}),
		WorkingDir:  workingDir,
		Instruction: "developer guide",
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
	for _, err := range r.Run(t.Context(), "test-user", sess.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
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
