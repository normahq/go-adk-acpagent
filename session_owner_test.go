package acpagent

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// fixedParkTime makes the parked_at stamp predictable for the test.
func fixedParkTime(t *testing.T) string {
	t.Helper()
	at := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	previous := parkNow
	parkNow = func() time.Time { return at }
	t.Cleanup(func() { parkNow = previous })
	return at.Format(time.RFC3339Nano)
}

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
	me := a.ownerID()
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
			state:  map[string]any{"session_id": "own-1", "owner": me},
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
					me: map[string]any{
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
	ctx := newStateContext(t, map[string]any{SessionStateKey: map[string]any{"session_id": "codex-1", "owner": codex.ownerID()}})

	at := fixedParkTime(t)
	input := map[string]any{"session_id": "claude-1"}
	got := claude.ownedState(ctx.session, input)
	want := map[string]any{
		"session_id": "claude-1",
		"owner":      claude.ownerID(),
		"parked":     map[string]any{codex.ownerID(): map[string]any{"session_id": "codex-1", "parked_at": at}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("claude takes over (-want +got):\n%s", diff)
	}
	if _, stamped := input["owner"]; stamped {
		t.Fatal("ownedState must not mutate its input")
	}
	if err := ctx.session.State().Set(SessionStateKey, got); err != nil {
		t.Fatal(err)
	}

	back := codex.ownedState(ctx.session, map[string]any{"session_id": "codex-1"})
	wantBack := map[string]any{
		"session_id": "codex-1",
		"owner":      codex.ownerID(),
		"parked":     map[string]any{claude.ownerID(): map[string]any{"session_id": "claude-1", "parked_at": at}},
	}
	if diff := cmp.Diff(wantBack, back); diff != "" {
		t.Fatalf("codex takes back its session (-want +got):\n%s", diff)
	}
	again := codex.ownedState(ctx.session, map[string]any{"session_id": "codex-1"})
	if diff := cmp.Diff(back, again); diff != "" {
		t.Fatalf("stamping is not idempotent (-want +got):\n%s", diff)
	}
}

func TestUnnamedAgentsWithDifferentBackendsAreDistinctOwners(t *testing.T) {
	first := newOwnedTestAgent(t, "", map[string]string{"GO_SUPPORT_SESSION_RESUME": "1"})
	defer closeTestCloser(t, first)
	second := newOwnedTestAgent(t, "", map[string]string{"GO_SUPPORT_SESSION_RESUME": "1", "GO_FAIL_RESUME_QUERY_CLOSED": "1"})
	defer closeTestCloser(t, second)
	if first.Name() != second.Name() {
		t.Fatalf("names differ (%q, %q): the test needs the default name", first.Name(), second.Name())
	}
	if first.ownerID() == second.ownerID() {
		t.Fatalf("agents with different backends share the owner %q", first.ownerID())
	}
	state := map[string]any{"session_id": "x", "owner": first.ownerID(), "attachment_indeterminate": true}
	cfg, err := second.resolveSessionConfig(newStateContext(t, map[string]any{SessionStateKey: state, CWDStateKey: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.sessionID != "" || cfg.attachmentIndeterminate {
		t.Fatalf("a different backend adopted session %q (indeterminate=%v)", cfg.sessionID, cfg.attachmentIndeterminate)
	}
}

func TestParkedSlotKeepsAttachmentIndeterminate(t *testing.T) {
	first := newOwnedTestAgent(t, "first", nil)
	defer closeTestCloser(t, first)
	second := newOwnedTestAgent(t, "second", nil)
	defer closeTestCloser(t, second)
	ctx := newStateContext(t, map[string]any{SessionStateKey: map[string]any{
		"session_id": "first-1", "owner": first.ownerID(), "attachment_indeterminate": true,
	}})

	parked := second.ownedState(ctx.session, map[string]any{"session_id": "second-1"})
	if err := ctx.session.State().Set(SessionStateKey, parked); err != nil {
		t.Fatal(err)
	}
	if err := ctx.session.State().Set(CWDStateKey, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cfg, err := first.resolveSessionConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.sessionID != "first-1" || !cfg.attachmentIndeterminate {
		t.Fatalf("restored slot lost its in-doubt flag: id=%q indeterminate=%v", cfg.sessionID, cfg.attachmentIndeterminate)
	}
}

func TestDamagedOwnershipStateIsHandled(t *testing.T) {
	a := newOwnedTestAgent(t, "me", nil)
	defer closeTestCloser(t, a)
	resolve := func(state map[string]any) (acpSessionConfig, error) {
		return a.resolveSessionConfig(newStateContext(t, map[string]any{SessionStateKey: state, CWDStateKey: t.TempDir()}))
	}
	if _, err := resolve(map[string]any{"session_id": "x", "owner": 5}); err == nil {
		t.Fatal("a non-string owner must be an error, not silently adopted")
	}
	cfg, err := resolve(map[string]any{
		"session_id": "other-1",
		"owner":      "other:00000000",
		"parked":     map[string]any{a.ownerID(): map[string]any{"session_id": "mine-1", "config_values": "broken"}},
	})
	if err != nil {
		t.Fatalf("a damaged parked slot must not wedge the agent: %v", err)
	}
	if cfg.sessionID != "" {
		t.Fatalf("a damaged slot must lead to a new session, got %q", cfg.sessionID)
	}
}

func TestStampOwnershipCoversPartialConfigAndModeEvents(t *testing.T) {
	first := newOwnedTestAgent(t, "first", nil)
	defer closeTestCloser(t, first)
	second := newOwnedTestAgent(t, "second", nil)
	defer closeTestCloser(t, second)
	ctx := newStateContext(t, map[string]any{SessionStateKey: map[string]any{"session_id": "first-1", "owner": first.ownerID()}})

	var got *session.Event
	yield := second.stampOwnership(ctx, func(ev *session.Event, _ error) bool { got = ev; return true })
	update := session.NewEvent(ctx, "inv")
	update.Partial = true
	update.Actions.StateDelta[SessionStateKey] = buildACPStateWithConfigValues("second-1", "", []SessionConfigValue{{ID: "mode", Value: "build"}})
	yield(update, nil)

	state, _ := got.Actions.StateDelta[SessionStateKey].(map[string]any)
	if state["owner"] != second.ownerID() {
		t.Fatalf("partial update event is not stamped: %v", state)
	}
	parked, _ := state["parked"].(map[string]any)
	if _, ok := parked[first.ownerID()]; !ok {
		t.Fatalf("partial update event dropped the parked slot: %v", state)
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
	meta := map[string]any{"codex": map[string]any{"approvalMode": "manual"}}
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "owner", UserID: "u", State: map[string]any{SessionStateKey: map[string]any{"meta": meta}}})
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
	if stateB["owner"] != agentB.ownerID() {
		t.Fatalf("owner after takeover = %v, want %s", stateB["owner"], agentB.ownerID())
	}
	parked, _ := stateB["parked"].(map[string]any)
	if slot, _ := parked[agentA.ownerID()].(map[string]any); slot["session_id"] == nil {
		t.Fatalf("agent-a's session must be parked, got %v", stateB)
	}
	if diff := cmp.Diff(meta, stateB["meta"]); diff != "" {
		t.Fatalf("meta must stay shared across the takeover (-want +got):\n%s", diff)
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

func TestOwnedStateLeavesAnEmptyStateAlone(t *testing.T) {
	a := newOwnedTestAgent(t, "me", nil)
	defer closeTestCloser(t, a)
	ctx := newStateContext(t, map[string]any{})
	got := a.ownedState(ctx.session, map[string]any{})
	if len(got) != 0 {
		t.Fatalf("an empty state must stay empty, got %v", got)
	}
}

func TestResetDoesNotBringBackAParkedSession(t *testing.T) {
	first := newOwnedTestAgent(t, "first", nil)
	defer closeTestCloser(t, first)
	second := newOwnedTestAgent(t, "second", nil)
	defer closeTestCloser(t, second)
	// The second agent took over and the session id was then cleared by a reset.
	state := map[string]any{
		"session_id": "",
		"owner":      second.ownerID(),
		"parked":     map[string]any{first.ownerID(): map[string]any{"session_id": "first-old"}},
	}
	cfg, err := first.resolveSessionConfig(newStateContext(t, map[string]any{SessionStateKey: state, CWDStateKey: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.sessionID != "" {
		t.Fatalf("a reset resumed the old session %q", cfg.sessionID)
	}
}

func TestParkedSessionsAreBounded(t *testing.T) {
	tick := time.Date(2026, time.October, 17, 0, 0, 0, 0, time.UTC)
	previous := parkNow
	parkNow = func() time.Time { tick = tick.Add(time.Minute); return tick }
	t.Cleanup(func() { parkNow = previous })

	ctx := newStateContext(t, map[string]any{})
	var firstOwner string
	for i := 0; i < maxParkedSessions+5; i++ {
		a := newOwnedTestAgent(t, fmt.Sprintf("agent-%02d", i), nil)
		defer closeTestCloser(t, a)
		if i == 0 {
			firstOwner = a.ownerID()
		}
		state := a.ownedState(ctx.session, map[string]any{"session_id": fmt.Sprintf("s-%02d", i)})
		if err := ctx.session.State().Set(SessionStateKey, state); err != nil {
			t.Fatal(err)
		}
	}
	live := liveACPState(ctx.session)
	parked, _ := live["parked"].(map[string]any)
	if len(parked) != maxParkedSessions {
		t.Fatalf("parked sessions = %d, want at most %d", len(parked), maxParkedSessions)
	}
	if _, kept := parked[firstOwner]; kept {
		t.Fatal("the oldest parked session must be dropped first")
	}
}

func TestOwnerIdentityCoversNameCommandAndEnvironment(t *testing.T) {
	command := []string{"agent", "acp"}
	base := agentOwnerKey("same", command, []string{"HOME=/a"}, "/work")
	if base != agentOwnerKey("same", command, []string{"HOME=/a"}, "/work") {
		t.Fatal("the identity must be stable")
	}
	if base == agentOwnerKey("same", command, []string{"HOME=/b"}, "/work") {
		t.Fatal("agents with another environment must be distinct owners")
	}
	if base == agentOwnerKey("same", command, []string{"HOME=/a"}, "/other") {
		t.Fatal("agents with another working directory must be distinct owners")
	}
	if base == agentOwnerKey("same", []string{"agent", "acp", "--x"}, []string{"HOME=/a"}, "/work") {
		t.Fatal("agents with another command must be distinct owners")
	}
	if base == agentOwnerKey("other", command, []string{"HOME=/a"}, "/work") {
		t.Fatal("agents with another name must be distinct owners")
	}
	if agentOwnerKey("same", command, []string{"B=2", "A=1"}, "/work") != agentOwnerKey("same", command, []string{"A=1", "B=2"}, "/work") {
		t.Fatal("the order of environment entries must not matter")
	}
	if strings.Contains(base, "/a") {
		t.Fatalf("the identity must not contain the environment: %q", base)
	}
}

func TestNonStringOwnerErrorNamesTheOwnerField(t *testing.T) {
	a := newOwnedTestAgent(t, "me", nil)
	defer closeTestCloser(t, a)
	_, err := a.resolveSessionConfig(newStateContext(t, map[string]any{SessionStateKey: map[string]any{"session_id": "x", "owner": 5}, CWDStateKey: t.TempDir()}))
	if err == nil || !strings.Contains(err.Error(), "owner must be a string") {
		t.Fatalf("error = %v, want one naming the owner field", err)
	}
	if strings.Contains(err.Error(), "parked") {
		t.Fatalf("error blames the parked field: %v", err)
	}
}

func newBackendAgent(t *testing.T, workingDir string, environment []string) *Agent {
	t.Helper()
	a, err := NewWithContext(t.Context(), Config{
		Name:        "provider",
		Command:     helperCommandWithEnv(t, nil),
		Environment: environment,
		WorkingDir:  workingDir,
	})
	if err != nil {
		t.Fatalf("NewWithContext() error = %v", err)
	}
	return a
}

func TestSameNameAndCommandWithAnotherProfileIsADifferentOwner(t *testing.T) {
	dir := t.TempDir()
	profileA := newBackendAgent(t, dir, []string{"PATH=" + os.Getenv("PATH"), "CODEX_HOME=/profiles/a"})
	defer closeTestCloser(t, profileA)
	profileB := newBackendAgent(t, dir, []string{"PATH=" + os.Getenv("PATH"), "CODEX_HOME=/profiles/b"})
	defer closeTestCloser(t, profileB)
	if profileA.Name() != profileB.Name() {
		t.Fatalf("names differ: %q, %q", profileA.Name(), profileB.Name())
	}
	if profileA.ownerID() == profileB.ownerID() {
		t.Fatalf("identical name and command with another CODEX_HOME share the owner %q", profileA.ownerID())
	}

	state := map[string]any{"session_id": "profile-a-session", "owner": profileA.ownerID()}
	cfg, err := profileB.resolveSessionConfig(newStateContext(t, map[string]any{SessionStateKey: state, CWDStateKey: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.sessionID != "" {
		t.Fatalf("profile B resumed the session %q of profile A", cfg.sessionID)
	}
}

func TestSameNameAndCommandInAnotherWorkingDirectoryIsADifferentOwner(t *testing.T) {
	first := newBackendAgent(t, t.TempDir(), nil)
	defer closeTestCloser(t, first)
	second := newBackendAgent(t, t.TempDir(), nil)
	defer closeTestCloser(t, second)
	if first.ownerID() == second.ownerID() {
		t.Fatalf("identical name and command in another working directory share the owner %q", first.ownerID())
	}
}

func TestInheritedEnvironmentIsPartOfTheOwnerIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", "/profiles/a")
	profileA := newBackendAgent(t, dir, nil)
	defer closeTestCloser(t, profileA)
	t.Setenv("CODEX_HOME", "/profiles/b")
	profileB := newBackendAgent(t, dir, nil)
	defer closeTestCloser(t, profileB)

	if profileA.Name() != profileB.Name() {
		t.Fatalf("names differ: %q, %q", profileA.Name(), profileB.Name())
	}
	if profileA.ownerID() == profileB.ownerID() {
		t.Fatalf("agents inheriting different CODEX_HOME values share owner %q", profileA.ownerID())
	}
	state := map[string]any{"session_id": "profile-a-session", "owner": profileA.ownerID()}
	cfg, err := profileB.resolveSessionConfig(newStateContext(t, map[string]any{SessionStateKey: state, CWDStateKey: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.sessionID != "" {
		t.Fatalf("the profile inheriting another CODEX_HOME resumed the session %q", cfg.sessionID)
	}
}

func TestLaunchSpecificVariablesDoNotChangeTheOwnerIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOSTNAME", "container-1")
	t.Setenv("PWD", "/somewhere")
	first := newBackendAgent(t, dir, nil)
	defer closeTestCloser(t, first)
	t.Setenv("HOSTNAME", "container-2")
	t.Setenv("PWD", "/elsewhere")
	second := newBackendAgent(t, dir, nil)
	defer closeTestCloser(t, second)
	if first.ownerID() != second.ownerID() {
		t.Fatalf("a restart in another container changed the owner: %q vs %q", first.ownerID(), second.ownerID())
	}
}

func TestOneEnvironmentSnapshotFeedsTheSubprocessAndTheIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", "/profiles/snapshot")
	inherited := newBackendAgent(t, dir, nil)
	defer closeTestCloser(t, inherited)
	if inherited.clientConfig.Environment == nil {
		t.Fatal("an agent without an explicit environment must hand its subprocess the snapshot")
	}
	if !slices.Contains(inherited.clientConfig.Environment, "CODEX_HOME=/profiles/snapshot") {
		t.Fatal("the subprocess environment is not the snapshot the identity was computed from")
	}
	want := agentOwnerKey(inherited.Name(), inherited.clientConfig.Command, inherited.clientConfig.Environment, dir)
	if inherited.ownerID() != want {
		t.Fatalf("the identity %q was not computed from the subprocess environment (%q)", inherited.ownerID(), want)
	}

	explicit := []string{"ONLY=this"}
	exact := newBackendAgent(t, dir, explicit)
	defer closeTestCloser(t, exact)
	if !slices.Equal(exact.clientConfig.Environment, explicit) {
		t.Fatalf("an explicit environment must stay exact, got %v", exact.clientConfig.Environment)
	}
}

func TestInheritedEnvironmentSnapshotCarriesThePWDOfTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PWD", "/the/parent/directory")
	inherited := newBackendAgent(t, dir, nil)
	defer closeTestCloser(t, inherited)

	var pwd []string
	for _, entry := range inherited.clientConfig.Environment {
		if strings.HasPrefix(entry, "PWD=") {
			pwd = append(pwd, entry)
		}
	}
	if !slices.Equal(pwd, []string{"PWD=" + dir}) {
		t.Fatalf("the subprocess must get PWD of its working directory, got %v", pwd)
	}

	explicit := newBackendAgent(t, dir, []string{"ONLY=this"})
	defer closeTestCloser(t, explicit)
	if !slices.Equal(explicit.clientConfig.Environment, []string{"ONLY=this"}) {
		t.Fatalf("an explicit environment must not get a PWD, got %v", explicit.clientConfig.Environment)
	}
}

func TestOwnerIdentityIgnoresOnlyLaunchSpecificVariables(t *testing.T) {
	command := []string{"agent", "acp"}
	base := []string{"CODEX_HOME=/a", "HOME=/home/me"}
	want := agentOwnerKey("n", command, base, "/w")
	for _, volatile := range []string{
		"HOSTNAME=x", "PWD=/y", "SHLVL=3", "TERM=xterm", "TMUX=/tmp/tmux-1/default,1,0", "INVOCATION_ID=abc",
		"JOURNAL_STREAM=8:1", "XPC_SERVICE_NAME=foo", "XPC_FLAGS=0x0", "SSH_AUTH_SOCK=/tmp/agent", "TERM_SESSION_ID=w0",
	} {
		if got := agentOwnerKey("n", command, append(slices.Clone(base), volatile), "/w"); got != want {
			t.Errorf("%s changed the identity", volatile)
		}
	}
	// Names are case sensitive, so these are other variables and count.
	if agentOwnerKey("n", command, append(slices.Clone(base), "hostname=x"), "/w") == want {
		t.Error("a variable that only looks like a launch-specific one was ignored")
	}
	if agentOwnerKey("n", command, append(slices.Clone(base), "CODEX_PROFILE=work"), "/w") == want {
		t.Error("an ordinary variable was ignored")
	}
}

func TestOwnerIdentityUsesTheLastValueOfARepeatedVariable(t *testing.T) {
	command := []string{"agent", "acp"}
	first := agentOwnerKey("n", command, []string{"A=1", "A=2"}, "/w")
	second := agentOwnerKey("n", command, []string{"A=2", "A=1"}, "/w")
	if first == second {
		t.Fatal("environments that end up with a different value for A must be different owners")
	}
	if agentOwnerKey("n", command, []string{"A=0", "A=2"}, "/w") != first {
		t.Fatal("only the last value of a repeated variable counts")
	}
}

func TestSessionOwnerPinsTheIdentity(t *testing.T) {
	dir := t.TempDir()
	build := func(environment []string, owner string) *Agent {
		a, err := NewWithContext(t.Context(), Config{
			Name:         "provider",
			Command:      helperCommandWithEnv(t, nil),
			Environment:  environment,
			WorkingDir:   dir,
			SessionOwner: owner,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := build([]string{"BUILD_ID=1"}, "  codex-work  ")
	defer closeTestCloser(t, first)
	second := build([]string{"BUILD_ID=2"}, "codex-work")
	defer closeTestCloser(t, second)
	if first.ownerID() != "codex-work" || second.ownerID() != "codex-work" {
		t.Fatalf("a configured identity must be used as given (trimmed): %q, %q", first.ownerID(), second.ownerID())
	}
	other := build([]string{"BUILD_ID=2"}, "codex-personal")
	defer closeTestCloser(t, other)
	if other.ownerID() == first.ownerID() {
		t.Fatal("different configured identities must stay different owners")
	}
	derived := build([]string{"BUILD_ID=2"}, "")
	defer closeTestCloser(t, derived)
	if derived.ownerID() == "" || derived.ownerID() == "codex-work" {
		t.Fatalf("without SessionOwner the identity is derived, got %q", derived.ownerID())
	}
}
