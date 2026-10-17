package acpagent

import (
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func newOwnedTestAgent(t *testing.T, name string, env map[string]string) *Agent {
	t.Helper()
	a, err := NewWithContext(t.Context(), Config{
		Name:       name,
		Command:    helperCommandWithEnv(t, env),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewWithContext(%s) error = %v", name, err)
	}
	return a
}

func newStateContext(t *testing.T, state map[string]any) testInvocationContext {
	t.Helper()
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "owner", UserID: "u", State: state})
	if err != nil {
		t.Fatal(err)
	}
	return testInvocationContext{Context: t.Context(), session: created.Session}
}

func TestResolveSessionConfigHonorsSessionOwner(t *testing.T) {
	a := newOwnedTestAgent(t, "claude", nil)
	defer closeTestCloser(t, a)
	testCases := []struct {
		name      string
		state     map[string]any
		wantID    string
		wantMeta  string
		wantValue int
	}{
		{
			name:   "legacy state without owner belongs to the current agent",
			state:  map[string]any{"session_id": "legacy-1"},
			wantID: "legacy-1",
		},
		{
			name:   "own session",
			state:  map[string]any{"session_id": "own-1", "owner": "claude"},
			wantID: "own-1",
		},
		{
			name: "foreign session is not reused and its config values are dropped",
			state: map[string]any{
				"session_id":    "codex-1",
				"owner":         "codex",
				"config_values": []any{map[string]any{"id": "model", "value": "gpt"}},
				"meta":          map[string]any{"codex": map[string]any{"x": "y"}},
			},
			wantID:   "",
			wantMeta: `{"codex":{"x":"y"}}`,
		},
		{
			name: "session parked for the current agent is restored",
			state: map[string]any{
				"session_id": "codex-1",
				"owner":      "codex",
				"parked": map[string]any{
					"claude": map[string]any{
						"session_id":    "claude-7",
						"config_values": []any{map[string]any{"id": "model", "value": "sonnet"}},
					},
				},
			},
			wantID:    "claude-7",
			wantValue: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := a.resolveSessionConfig(newStateContext(t, map[string]any{SessionStateKey: tc.state, CWDStateKey: t.TempDir()}))
			if err != nil {
				t.Fatalf("resolveSessionConfig() error = %v", err)
			}
			if cfg.sessionID != tc.wantID {
				t.Fatalf("sessionID = %q, want %q", cfg.sessionID, tc.wantID)
			}
			if len(cfg.configValues) != tc.wantValue {
				t.Fatalf("configValues = %v, want %d", cfg.configValues, tc.wantValue)
			}
			if tc.wantMeta != "" && cfg.metaJSON != tc.wantMeta {
				t.Fatalf("metaJSON = %q, want %q (meta stays shared)", cfg.metaJSON, tc.wantMeta)
			}
		})
	}
}

func TestOwnedStateParksForeignSessionAndRestoresOwn(t *testing.T) {
	claude := newOwnedTestAgent(t, "claude", nil)
	defer closeTestCloser(t, claude)
	codex := newOwnedTestAgent(t, "codex", nil)
	defer closeTestCloser(t, codex)
	ctx := newStateContext(t, map[string]any{SessionStateKey: map[string]any{"session_id": "codex-1", "owner": "codex"}})

	got := claude.ownedState(ctx.session, map[string]any{"session_id": "claude-1"})
	want := map[string]any{
		"session_id": "claude-1",
		"owner":      "claude",
		"parked":     map[string]any{"codex": map[string]any{"session_id": "codex-1"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("claude takes over (-want +got):\n%s", diff)
	}
	if err := ctx.session.State().Set(SessionStateKey, got); err != nil {
		t.Fatal(err)
	}

	back := codex.ownedState(ctx.session, map[string]any{"session_id": "codex-1"})
	wantBack := map[string]any{
		"session_id": "codex-1",
		"owner":      "codex",
		"parked":     map[string]any{"claude": map[string]any{"session_id": "claude-1"}},
	}
	if diff := cmp.Diff(wantBack, back); diff != "" {
		t.Fatalf("codex takes back its session (-want +got):\n%s", diff)
	}
	again := codex.ownedState(ctx.session, map[string]any{"session_id": "codex-1"})
	if diff := cmp.Diff(back, again); diff != "" {
		t.Fatalf("stamping is not idempotent (-want +got):\n%s", diff)
	}
}

func TestAgentsSharingASessionNeverResumeEachOthersSession(t *testing.T) {
	logDir := t.TempDir()
	logA, logB := logDir+"/a", logDir+"/b"
	envFor := func(path string) map[string]string {
		return map[string]string{"GO_SUPPORT_SESSION_RESUME": "1", "GO_METHOD_LOG": path}
	}
	agentA := newOwnedTestAgent(t, "agent-a", envFor(logA))
	defer closeTestCloser(t, agentA)
	agentB := newOwnedTestAgent(t, "agent-b", envFor(logB))
	defer closeTestCloser(t, agentB)

	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "owner", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(a agent.Agent, text string) map[string]any {
		r, err := runnerpkg.New(runnerpkg.Config{AppName: "owner", Agent: a, SessionService: svc})
		if err != nil {
			t.Fatal(err)
		}
		_, state := collectFinalTextAndSessionState(t, r.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText(text, genai.RoleUser), agent.RunConfig{}))
		return state
	}

	run(agentA, "one")
	stateB := run(agentB, "two")
	if stateB["owner"] != "agent-b" {
		t.Fatalf("owner after takeover = %v, want agent-b", stateB["owner"])
	}
	parked, _ := stateB["parked"].(map[string]any)
	if slot, _ := parked["agent-a"].(map[string]any); slot["session_id"] == nil {
		t.Fatalf("agent-a's session must be parked, got %v", stateB)
	}
	run(agentA, "three")

	count := func(path, method string) int {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), method+"\n")
	}
	if count(logB, "session/new") != 1 || count(logB, "session/resume") != 0 || count(logB, "session/prompt") != 1 {
		t.Fatalf("agent-b must start its own session and never resume a foreign one:\n%s", mustRead(t, logB))
	}
	if count(logA, "session/new") != 1 || count(logA, "session/prompt") != 2 {
		t.Fatalf("agent-a must continue its own session after agent-b took over:\n%s", mustRead(t, logA))
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
