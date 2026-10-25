package acpagent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func runControlSession(t *testing.T, a *Agent, svc session.Service, appName, userID string) session.Session {
	t.Helper()
	r, err := runnerpkg.New(runnerpkg.Config{AppName: appName, Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: appName, UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	for _, runErr := range r.Run(t.Context(), userID, created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	stored, err := svc.Get(t.Context(), &session.GetRequest{AppName: appName, UserID: userID, SessionID: created.Session.ID()})
	if err != nil {
		t.Fatal(err)
	}
	return stored.Session
}

func TestSessionControllerValidatesAndChangesModel(t *testing.T) {
	methodLog := t.TempDir() + "/methods.log"
	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL": "model-a",
			"GO_EXPECT_SESSION_MODEL":  "model-a",
			"GO_METHOD_LOG":            methodLog,
		}),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)

	svc := session.InMemoryService()
	stored := runControlSession(t, a, svc, "control", "u")

	control := a.SessionControl()
	snapshot := control.Inspect(t.Context(), stored)
	if snapshot.Status != SessionControlAvailable || snapshot.CurrentModel != "model-a" {
		t.Fatalf("Inspect() = %#v, want available model-a", snapshot)
	}
	if got := control.ValidateModel(t.Context(), stored, "model-a"); got.Outcome != ModelValidationSupported {
		t.Fatalf("ValidateModel(model-a) = %#v, want supported", got)
	}
	if got := control.ValidateModel(t.Context(), stored, "model-b"); got.Outcome != ModelValidationIncompatible {
		t.Fatalf("ValidateModel(model-b) = %#v, want incompatible", got)
	}
	before, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "session/set_config_option") {
		t.Fatalf("validation mutated the session; methods:\n%s", before)
	}

	change := control.ChangeModel(t.Context(), stored, "model-a")
	if change.Outcome != ModelChangeApplied || change.EffectiveModel != "model-a" {
		t.Fatalf("ChangeModel() = %#v, want applied model-a", change)
	}
	after, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "session/set_config_option") {
		t.Fatalf("ChangeModel did not dispatch session/set_config_option; methods:\n%s", after)
	}
}

func TestSessionControllerRefreshesPersistedSnapshotAfterRestart(t *testing.T) {
	workingDir := t.TempDir()
	methodLog := t.TempDir() + "/methods.log"
	env := map[string]string{
		"GO_CURRENT_SESSION_MODEL":    "model-a",
		"GO_EXPECT_SESSION_MODEL":     "model-a",
		"GO_SUPPORT_SESSION_RESUME":   "1",
		"GO_EXPECT_RESUME_SESSION_ID": "session-1",
		"GO_METHOD_LOG":               methodLog,
	}
	first, err := NewWithContext(t.Context(), Config{
		Command:      helperCommandWithEnv(t, env),
		WorkingDir:   workingDir,
		SessionOwner: "stable-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := session.InMemoryService()
	stored := runControlSession(t, first, svc, "restart", "u")
	firstGeneration := first.attachmentGeneration
	closeTestCloser(t, first)

	second, err := NewWithContext(t.Context(), Config{
		Command:      helperCommandWithEnv(t, env),
		WorkingDir:   workingDir,
		SessionOwner: "stable-owner",
		SessionConfig: []SessionConfigValue{
			SelectSessionConfigValue("model", "model-b"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, second)
	if firstGeneration == second.attachmentGeneration {
		t.Fatal("attachment generation did not change across Agent instances")
	}

	snapshot := second.SessionControl().Inspect(t.Context(), stored)
	if snapshot.Status != SessionControlAvailable || snapshot.CurrentModel != "model-a" {
		t.Fatalf("Inspect() after restart = %#v, want refreshed model-a", snapshot)
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(methods), "session/resume") {
		t.Fatalf("Inspect() did not refresh by resume; methods:\n%s", methods)
	}
	if strings.Contains(string(methods), "session/set_config_option") {
		t.Fatalf("Inspect() mutated configuration during refresh; methods:\n%s", methods)
	}
}

func TestSessionControllerReportsBusyAndPostDispatchAmbiguity(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL": "model-a",
			"GO_EXPECT_SESSION_MODEL":  "model-a",
			"GO_HANG_METHOD":           "session/set_config_option",
		}),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	stored := runControlSession(t, a, session.InMemoryService(), "outcomes", "u")
	cfg, err := a.resolveSessionConfigForSession(stored)
	if err != nil {
		t.Fatal(err)
	}
	active := &activePrompt{sessionID: acp.SessionId(cfg.sessionID), logger: a.logger}
	if err := a.client.beginSessionOperation(active); err != nil {
		t.Fatal(err)
	}
	if got := a.SessionControl().ChangeModel(t.Context(), stored, "model-a"); got.Outcome != ModelChangeBusy {
		t.Fatalf("ChangeModel() with active operation = %#v, want busy", got)
	}
	a.client.endSessionOperation(active)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if got := a.SessionControl().ChangeModel(ctx, stored, "model-a"); got.Outcome != ModelChangeAmbiguous {
		t.Fatalf("ChangeModel() after uncertain dispatch = %#v, want ambiguous", got)
	}
}

func TestSessionControlSnapshotGenerationAndBounds(t *testing.T) {
	options := helperModelConfigOptions("model-a", "model")
	state := newSessionControlState(options, "generation-a")
	if state == nil {
		t.Fatal("newSessionControlState() = nil")
	}
	if !authoritativeControl(state, "generation-a") {
		t.Fatal("current generation is not authoritative")
	}
	if authoritativeControl(state, "generation-b") {
		t.Fatal("snapshot from another process generation is authoritative")
	}
	parsed := parseSessionControlState(state.toState())
	if diff := cmp.Diff(state.snapshot(SessionControlAvailable, ""), parsed.snapshot(SessionControlAvailable, "")); diff != "" {
		t.Fatalf("state round trip mismatch (-want +got):\n%s", diff)
	}

	tooMany := make([]SessionModel, maxControlModels+1)
	for i := range tooMany {
		tooMany[i] = SessionModel{ID: "id", Label: "label"}
	}
	invalid := state.toState()
	models := make([]map[string]any, 0, len(tooMany))
	for _, model := range tooMany {
		models = append(models, map[string]any{"id": model.ID, "label": model.Label})
	}
	invalid["models"] = models
	if got := parseSessionControlState(invalid); got != nil {
		t.Fatalf("oversized snapshot parsed as %#v", got)
	}
}

func TestParkedOwnerCarriesControlSnapshot(t *testing.T) {
	first := newOwnedTestAgent(t, "first-control", nil)
	defer closeTestCloser(t, first)
	second := newOwnedTestAgent(t, "second-control", nil)
	defer closeTestCloser(t, second)
	control := newSessionControlState(helperModelConfigOptions("model-a", "model"), first.attachmentGeneration)
	ctx := newStateContext(t, map[string]any{SessionStateKey: map[string]any{
		"session_id":      "first-1",
		"owner":           first.ownerID(),
		controlStateField: control.toState(),
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
	if cfg.sessionID != "first-1" || !authoritativeControl(cfg.control, first.attachmentGeneration) {
		t.Fatalf("restored cfg = %#v, want first session with authoritative control", cfg)
	}
}

func TestSessionControlValueValidation(t *testing.T) {
	modelCategory := acp.SessionConfigOptionCategoryModel
	otherCategory := acp.SessionConfigOptionCategoryMode
	modelOption := func(id, current string, options acp.SessionConfigSelectOptions) acp.SessionConfigOption {
		option := acp.NewSessionConfigOptionSelect(acp.SessionConfigValueId(current), options)
		option.Select.Id = acp.SessionConfigId(id)
		option.Select.Category = &modelCategory
		return option
	}
	flat := func(values ...string) acp.SessionConfigSelectOptions {
		options := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(values))
		for _, value := range values {
			options = append(options, acp.SessionConfigSelectOption{Name: "Model " + value, Value: acp.SessionConfigValueId(value)})
		}
		return acp.SessionConfigSelectOptions{Ungrouped: &options}
	}

	grouped := acp.SessionConfigSelectOptionsGrouped{{
		Group: "stable",
		Name:  "Stable",
		Options: []acp.SessionConfigSelectOption{
			{Name: "Model A", Value: "model-a"},
			{Name: "Model B", Value: "model-b"},
		},
	}}
	state := newSessionControlState([]acp.SessionConfigOption{modelOption("model", "model-b", acp.SessionConfigSelectOptions{Grouped: &grouped})}, "generation")
	if state == nil || !state.hasModel("model-a") || state.currentModel != "model-b" {
		t.Fatalf("grouped model state = %#v", state)
	}
	snapshot := state.snapshot(SessionControlAvailable, "")
	snapshot.Models[0].ID = "mutated"
	if state.models[0].ID == "mutated" {
		t.Fatal("snapshot models alias internal state")
	}

	nonModel := modelOption("mode", "model-a", flat("model-a"))
	nonModel.Select.Category = &otherCategory
	duplicate := modelOption("model-2", "model-a", flat("model-a"))
	long := strings.Repeat("x", maxControlStringBytes+1)
	tooManyOptions := make(acp.SessionConfigSelectOptionsUngrouped, maxControlModels+1)
	for i := range tooManyOptions {
		tooManyOptions[i] = acp.SessionConfigSelectOption{Name: "Model", Value: acp.SessionConfigValueId(string(rune('a' + i)))}
	}
	testCases := []struct {
		name    string
		options []acp.SessionConfigOption
	}{
		{name: "missing model"},
		{name: "non-model category", options: []acp.SessionConfigOption{nonModel}},
		{name: "duplicate model options", options: []acp.SessionConfigOption{modelOption("model", "model-a", flat("model-a")), duplicate}},
		{name: "empty option id", options: []acp.SessionConfigOption{modelOption("", "model-a", flat("model-a"))}},
		{name: "long current", options: []acp.SessionConfigOption{modelOption("model", long, flat(long))}},
		{name: "current absent", options: []acp.SessionConfigOption{modelOption("model", "model-b", flat("model-a"))}},
		{name: "duplicate values", options: []acp.SessionConfigOption{modelOption("model", "model-a", flat("model-a", "model-a"))}},
		{name: "empty label", options: []acp.SessionConfigOption{modelOption("model", "model-a", acp.SessionConfigSelectOptions{Ungrouped: &acp.SessionConfigSelectOptionsUngrouped{{Value: "model-a"}}})}},
		{name: "too many values", options: []acp.SessionConfigOption{modelOption("model", "a", acp.SessionConfigSelectOptions{Ungrouped: &tooManyOptions})}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newSessionControlState(tc.options, "generation"); got != nil {
				t.Fatalf("newSessionControlState() = %#v, want nil", got)
			}
		})
	}
	if boundedControlString("") || boundedControlString(long) || !boundedControlString(" value ") {
		t.Fatal("boundedControlString accepted invalid bounds")
	}
	if authoritativeControl(nil, "generation") || authoritativeControl(state, "") || state.hasModel("missing") || (*sessionControlState)(nil).hasModel("model-a") {
		t.Fatal("control authority/model membership accepted invalid input")
	}
	if got := (*sessionControlState)(nil).snapshot(SessionControlUnavailable, "missing"); got.Status != SessionControlUnavailable || got.Code != "missing" {
		t.Fatalf("nil snapshot = %#v", got)
	}
}

func TestSessionControlStateParsingRejectsMalformedData(t *testing.T) {
	valid := map[string]any{
		"generation":      "generation",
		"model_option_id": "model",
		"current_model":   "model-a",
		"models":          []any{map[string]any{"id": "model-a", "label": "Model A"}},
	}
	if got := parseSessionControlState(valid); got == nil || got.revision == "" {
		t.Fatalf("parseSessionControlState(valid) = %#v", got)
	}

	testCases := []struct {
		name string
		raw  any
	}{
		{name: "not object", raw: "invalid"},
		{name: "missing generation", raw: map[string]any{"model_option_id": "model", "current_model": "model-a", "models": valid["models"]}},
		{name: "models not list", raw: map[string]any{"generation": "g", "model_option_id": "model", "current_model": "model-a", "models": "invalid"}},
		{name: "entry not object", raw: map[string]any{"generation": "g", "model_option_id": "model", "current_model": "model-a", "models": []any{"invalid"}}},
		{name: "empty model id", raw: map[string]any{"generation": "g", "model_option_id": "model", "current_model": "model-a", "models": []map[string]any{{"id": "", "label": "Model A"}}}},
		{name: "duplicate model id", raw: map[string]any{"generation": "g", "model_option_id": "model", "current_model": "model-a", "models": []map[string]any{{"id": "model-a", "label": "A"}, {"id": "model-a", "label": "Again"}}}},
		{name: "current absent", raw: map[string]any{"generation": "g", "model_option_id": "model", "current_model": "model-b", "models": valid["models"]}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSessionControlState(tc.raw); got != nil {
				t.Fatalf("parseSessionControlState() = %#v, want nil", got)
			}
		})
	}
}

func TestSessionControlOutcomeMappings(t *testing.T) {
	if got := controlSnapshotForAdmissionError(ErrSessionOperationActive); got.Status != SessionControlBusy {
		t.Fatalf("busy snapshot = %#v", got)
	}
	if got := controlSnapshotForAdmissionError(ErrClientClosed); got.Status != SessionControlClosed {
		t.Fatalf("closed snapshot = %#v", got)
	}
	if got := controlSnapshotForAdmissionError(errors.New("other")); got.Status != SessionControlFailed {
		t.Fatalf("failed snapshot = %#v", got)
	}
	admissionCases := []struct {
		err  error
		want ModelChangeOutcome
	}{
		{ErrSessionOperationActive, ModelChangeBusy},
		{ErrClientClosed, ModelChangeClosed},
		{errors.New("other"), ModelChangeFailed},
	}
	for _, tc := range admissionCases {
		if got := modelChangeForAdmissionError(tc.err); got.Outcome != tc.want {
			t.Fatalf("modelChangeForAdmissionError(%v) = %#v, want %s", tc.err, got, tc.want)
		}
	}
	validationCases := []struct {
		in   ModelValidationOutcome
		want ModelChangeOutcome
	}{
		{ModelValidationUnsupported, ModelChangeUnsupported},
		{ModelValidationIncompatible, ModelChangeIncompatible},
		{ModelValidationBusy, ModelChangeBusy},
		{ModelValidationClosed, ModelChangeClosed},
		{ModelValidationFailed, ModelChangeFailed},
		{ModelValidationSupported, ModelChangeFailed},
	}
	for _, tc := range validationCases {
		validation := ModelValidation{Outcome: tc.in, Code: "code", Snapshot: SessionControlSnapshot{Status: SessionControlFailed}}
		got := modelChangeFromValidation(validation)
		if got.Outcome != tc.want || got.Code != "code" || got.Snapshot.Status != SessionControlFailed {
			t.Fatalf("modelChangeFromValidation(%s) = %#v, want %s", tc.in, got, tc.want)
		}
	}
}

func TestSessionControllerInvalidAndUnavailableSessions(t *testing.T) {
	if got := (&SessionController{}).Inspect(t.Context(), nil); got.Status != SessionControlFailed || got.Code != "invalid_session" {
		t.Fatalf("Inspect(nil) = %#v", got)
	}
	a := newOwnedTestAgent(t, "unavailable", nil)
	defer closeTestCloser(t, a)
	ctx := newStateContext(t, map[string]any{SessionStateKey: map[string]any{"session_id": "remote-1"}})
	if got := a.SessionControl().Inspect(t.Context(), ctx.session); got.Status != SessionControlUnavailable || got.Code != "attachment_unavailable" {
		t.Fatalf("Inspect(unattached) = %#v", got)
	}
	if got := a.SessionControl().ValidateModel(t.Context(), ctx.session, "model-a"); got.Outcome != ModelValidationUnsupported {
		t.Fatalf("ValidateModel(unattached) = %#v", got)
	}
	if got := a.SessionControl().ChangeModel(t.Context(), ctx.session, "model-a"); got.Outcome != ModelChangeUnsupported {
		t.Fatalf("ChangeModel(unattached) = %#v", got)
	}
}

func TestSessionControllerMutationErrorCertainty(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		want ModelChangeOutcome
		code string
	}{
		{name: "unsupported", env: map[string]string{"GO_DISABLE_SET_CONFIG_OPTION": "1"}, want: ModelChangeUnsupported},
		{name: "ambiguous", env: map[string]string{"GO_FAIL_METHOD": "session/set_config_option"}, want: ModelChangeAmbiguous},
		{name: "request canceled", env: map[string]string{"GO_SET_CONFIG_OPTION_ERROR": "canceled"}, want: ModelChangeAmbiguous},
		{name: "rejected", env: map[string]string{"GO_SET_CONFIG_OPTION_ERROR": "invalid_params"}, want: ModelChangeRejected},
		{name: "empty response", env: map[string]string{"GO_EMPTY_SET_CONFIG_OPTION_RESPONSE": "1"}, want: ModelChangeAmbiguous, code: "invalid_response"},
		{name: "invalid response", env: map[string]string{"GO_SET_CONFIG_OPTION_RESPONSE_MODEL": "none"}, want: ModelChangeRejected, code: "not_applied"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GO_CURRENT_SESSION_MODEL": "model-a", "GO_EXPECT_SESSION_MODEL": "model-a"}
			for key, value := range tc.env {
				env[key] = value
			}
			a, err := NewWithContext(t.Context(), Config{Command: helperCommandWithEnv(t, env), WorkingDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, a)
			stored := runControlSession(t, a, session.InMemoryService(), "certainty-"+tc.name, "u")
			got := a.SessionControl().ChangeModel(t.Context(), stored, "model-a")
			if got.Outcome != tc.want || tc.code != "" && got.Code != tc.code {
				t.Fatalf("ChangeModel() = %#v, want %s code %q", got, tc.want, tc.code)
			}
		})
	}
}

func TestSessionControllerInspectRefreshOutcomes(t *testing.T) {
	testCases := []struct {
		name     string
		env      map[string]string
		cancel   bool
		wantCode string
	}{
		{name: "resume unsupported", env: map[string]string{"GO_FAIL_RESUME_METHOD_NOT_FOUND": "1"}, wantCode: "resume_unsupported"},
		{name: "unknown session", env: map[string]string{"GO_FAIL_RESUME_ENTITY_NOT_FOUND": "1"}, wantCode: "unknown_session"},
		{name: "resume failed", env: map[string]string{"GO_FAIL_METHOD": "session/resume"}, wantCode: "attachment_unavailable"},
		{name: "model control absent", wantCode: "model_control_unavailable"},
		{name: "canceled", cancel: true, wantCode: "canceled"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"GO_SUPPORT_SESSION_RESUME":   "1",
				"GO_EXPECT_RESUME_SESSION_ID": "remote-1",
			}
			if tc.name != "model control absent" {
				env["GO_CURRENT_SESSION_MODEL"] = "model-a"
				env["GO_EXPECT_SESSION_MODEL"] = "model-a"
			}
			for key, value := range tc.env {
				env[key] = value
			}
			a, err := NewWithContext(t.Context(), Config{
				Command:    helperCommandWithEnv(t, env),
				WorkingDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, a)
			oldControl := newSessionControlState(helperModelConfigOptions("model-a", "model"), "old-generation")
			state := map[string]any{"session_id": "remote-1"}
			if oldControl != nil {
				state[controlStateField] = oldControl.toState()
			}
			stored := newStateContext(t, map[string]any{SessionStateKey: state}).session
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got := a.SessionControl().Inspect(ctx, stored)
			if got.Code != tc.wantCode {
				t.Fatalf("Inspect() = %#v, want code %q", got, tc.wantCode)
			}
		})
	}
}

func TestSessionControllerInspectBusyClosedAndInvalidState(t *testing.T) {
	a := newOwnedTestAgent(t, "inspect-errors", map[string]string{
		"GO_SUPPORT_SESSION_RESUME":   "1",
		"GO_EXPECT_RESUME_SESSION_ID": "remote-1",
		"GO_CURRENT_SESSION_MODEL":    "model-a",
	})
	control := newSessionControlState(helperModelConfigOptions("model-a", "model"), "old-generation")
	stored := newStateContext(t, map[string]any{SessionStateKey: map[string]any{
		"session_id":      "remote-1",
		controlStateField: control.toState(),
	}}).session
	active := &activePrompt{sessionID: "remote-1", logger: a.logger}
	if err := a.client.beginSessionOperation(active); err != nil {
		t.Fatal(err)
	}
	if got := a.SessionControl().Inspect(t.Context(), stored); got.Status != SessionControlBusy || got.Code != "busy" {
		t.Fatalf("Inspect(busy) = %#v", got)
	}
	if got := a.SessionControl().ValidateModel(t.Context(), stored, "model-a"); got.Outcome != ModelValidationBusy {
		t.Fatalf("ValidateModel(busy) = %#v", got)
	}
	a.client.endSessionOperation(active)
	closeTestCloser(t, a)
	if got := a.SessionControl().Inspect(t.Context(), stored); got.Status != SessionControlClosed || got.Code != "closed" {
		t.Fatalf("Inspect(closed) = %#v", got)
	}
	if got := a.SessionControl().ValidateModel(t.Context(), stored, "model-a"); got.Outcome != ModelValidationClosed {
		t.Fatalf("ValidateModel(closed) = %#v", got)
	}
	if got := a.SessionControl().ChangeModel(t.Context(), stored, "model-a"); got.Outcome != ModelChangeClosed {
		t.Fatalf("ChangeModel(closed) = %#v", got)
	}

	invalid := newStateContext(t, map[string]any{SessionStateKey: "invalid"}).session
	b := newOwnedTestAgent(t, "inspect-invalid", nil)
	defer closeTestCloser(t, b)
	if got := b.SessionControl().Inspect(t.Context(), invalid); got.Status != SessionControlFailed || got.Code != "invalid_state" {
		t.Fatalf("Inspect(invalid state) = %#v", got)
	}
	if got := b.SessionControl().ValidateModel(t.Context(), invalid, "model-a"); got.Outcome != ModelValidationFailed {
		t.Fatalf("ValidateModel(invalid state) = %#v", got)
	}
}

func TestSessionAdmissionDefensiveBranches(t *testing.T) {
	a := newOwnedTestAgent(t, "admission", nil)
	client := a.client
	_ = client.PromptCapabilities()
	if err := client.beginSessionOperation(nil); !errors.Is(err, errSessionIDRequired) {
		t.Fatalf("beginSessionOperation(nil) = %v", err)
	}
	client.endSessionOperation(nil)
	closeActivePrompt(nil)
	updates := make(chan ExtendedSessionNotification)
	active := &activePrompt{sessionID: "not-registered", updates: updates}
	client.finishSessionUpdates(active)
	select {
	case _, ok := <-updates:
		if ok {
			t.Fatal("updates channel remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("updates channel was not closed")
	}
	lateUpdates := make(chan ExtendedSessionNotification)
	late := &activePrompt{sessionID: "late-update", updates: lateUpdates}
	if err := client.beginSessionOperation(late); err != nil {
		t.Fatal(err)
	}
	client.finishSessionUpdates(late)
	select {
	case <-lateUpdates:
	case <-time.After(time.Second):
		t.Fatal("late update stream was not closed")
	}
	client.dispatchSessionUpdate(ExtendedSessionNotification{SessionNotification: acp.SessionNotification{
		SessionId: "late-update",
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			Content: acp.TextBlock("late"),
		}},
	}})
	client.endSessionOperation(late)
	closeTestCloser(t, a)
	if err := client.beginSessionOperation(&activePrompt{sessionID: "closed"}); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("beginSessionOperation(closed) = %v", err)
	}
	var remote *remoteSession
	remote.applyPromptControl(&sessionControlState{})
	remote = &remoteSession{}
	remote.applyPromptControl(nil)
	control := &sessionControlState{modelOptionID: "model", currentModel: "model-a"}
	remote.applyPromptControl(control)
	if remote.control != control || len(remote.configValues) != 1 || remote.configValues[0].Value != "model-a" {
		t.Fatalf("applyPromptControl() = %#v", remote)
	}
}

func TestBuildSetSessionConfigOptionRequestValidation(t *testing.T) {
	booleanOption := acp.SessionConfigOption{Boolean: &acp.SessionConfigOptionBoolean{Id: "fast"}}
	if _, err := buildSetSessionConfigOptionRequest("session", "fast", SessionConfigValue{}, booleanOption); err == nil {
		t.Fatal("boolean option accepted missing value")
	}
	value := true
	request, err := buildSetSessionConfigOptionRequest("session", "fast", SessionConfigValue{BoolValue: &value}, booleanOption)
	if err != nil || request.Boolean == nil || !request.Boolean.Value {
		t.Fatalf("boolean request = %#v, %v", request, err)
	}
	selectOption := helperModelConfigOptions("model-a", "model")[0]
	if _, err := buildSetSessionConfigOptionRequest("session", "model", SessionConfigValue{}, selectOption); err == nil {
		t.Fatal("select option accepted missing value")
	}
	if _, err := buildSetSessionConfigOptionRequest("session", "unknown", SessionConfigValue{}, acp.SessionConfigOption{}); err == nil {
		t.Fatal("unknown option type was accepted")
	}
}

func TestPromptRefreshesStaleControlAttachment(t *testing.T) {
	testCases := []struct {
		name        string
		secondEnv   map[string]string
		wantNewCall bool
	}{
		{name: "resume existing"},
		{name: "replace unknown", secondEnv: map[string]string{"GO_FAIL_RESUME_ENTITY_NOT_FOUND": "1"}, wantNewCall: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			workingDir := t.TempDir()
			svc := session.InMemoryService()
			firstEnv := map[string]string{
				"GO_CURRENT_SESSION_MODEL":  "model-a",
				"GO_EXPECT_SESSION_MODEL":   "model-a",
				"GO_SUPPORT_SESSION_RESUME": "1",
			}
			first, err := NewWithContext(t.Context(), Config{Name: "stable", SessionOwner: "stable", Command: helperCommandWithEnv(t, firstEnv), WorkingDir: workingDir})
			if err != nil {
				t.Fatal(err)
			}
			stored := runControlSession(t, first, svc, "prompt-refresh-"+tc.name, "u")
			closeTestCloser(t, first)

			methodLog := t.TempDir() + "/methods.log"
			secondEnv := map[string]string{
				"GO_CURRENT_SESSION_MODEL":    "model-a",
				"GO_EXPECT_SESSION_MODEL":     "model-a",
				"GO_SUPPORT_SESSION_RESUME":   "1",
				"GO_EXPECT_RESUME_SESSION_ID": "session-1",
				"GO_METHOD_LOG":               methodLog,
			}
			for key, value := range tc.secondEnv {
				secondEnv[key] = value
			}
			second, err := NewWithContext(t.Context(), Config{Name: "stable", SessionOwner: "stable", Command: helperCommandWithEnv(t, secondEnv), WorkingDir: workingDir})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, second)
			runner, err := runnerpkg.New(runnerpkg.Config{AppName: "prompt-refresh-" + tc.name, Agent: second, SessionService: svc})
			if err != nil {
				t.Fatal(err)
			}
			for _, runErr := range runner.Run(t.Context(), "u", stored.ID(), genai.NewContentFromText("again", genai.RoleUser), agent.RunConfig{}) {
				if runErr != nil {
					t.Fatal(runErr)
				}
			}
			methods, err := os.ReadFile(methodLog)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(methods), "session/resume") {
				t.Fatalf("stale attachment was not resumed; methods:\n%s", methods)
			}
			if gotNew := strings.Contains(string(methods), "session/new"); gotNew != tc.wantNewCall {
				t.Fatalf("session/new called = %v, want %v; methods:\n%s", gotNew, tc.wantNewCall, methods)
			}
		})
	}
}

func TestPromptAdmissionHeldThroughFinalStateProjection(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL": "model-a",
			"GO_EXPECT_SESSION_MODEL":  "model-a",
		}),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	svc := session.InMemoryService()
	runner, err := runnerpkg.New(runnerpkg.Config{AppName: "held-admission", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "held-admission", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	sawFinal := false
	for event, runErr := range runner.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
		if event == nil || !event.TurnComplete {
			continue
		}
		sawFinal = true
		stored, getErr := svc.Get(t.Context(), &session.GetRequest{AppName: "held-admission", UserID: "u", SessionID: created.Session.ID()})
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got := a.SessionControl().ChangeModel(t.Context(), stored.Session, "model-a"); got.Outcome != ModelChangeBusy {
			t.Fatalf("ChangeModel() during final projection = %#v, want busy", got)
		}
	}
	if !sawFinal {
		t.Fatal("runner emitted no final event")
	}
	stored, err := svc.Get(t.Context(), &session.GetRequest{AppName: "held-admission", UserID: "u", SessionID: created.Session.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.SessionControl().ChangeModel(t.Context(), stored.Session, "model-a"); got.Outcome != ModelChangeApplied {
		t.Fatalf("ChangeModel() after prompt = %#v, want applied", got)
	}
}

func TestCloseFromFinalProjectionCallbackDoesNotDeadlock(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL": "model-a",
			"GO_EXPECT_SESSION_MODEL":  "model-a",
		}),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := session.InMemoryService()
	runner, err := runnerpkg.New(runnerpkg.Config{AppName: "close-admission", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "close-admission", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	sawFinal := false
	for event, runErr := range runner.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
		if event == nil || !event.TurnComplete {
			continue
		}
		sawFinal = true
		if closeErr := a.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !sawFinal {
		t.Fatal("runner emitted no final event")
	}
}

func TestSessionControllerInitializationFailure(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command:            []string{t.TempDir() + "/missing-acp-agent"},
		WorkingDir:         t.TempDir(),
		LazyInitialization: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	stored := newStateContext(t, nil).session
	if got := a.SessionControl().Inspect(t.Context(), stored); got.Status != SessionControlFailed || got.Code != "initialization_failed" {
		t.Fatalf("Inspect(initialization failure) = %#v", got)
	}
}

func TestSessionControllerCanceledMutation(t *testing.T) {
	a, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL": "model-a",
			"GO_EXPECT_SESSION_MODEL":  "model-a",
		}),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, a)
	stored := runControlSession(t, a, session.InMemoryService(), "canceled-change", "u")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := a.SessionControl().ChangeModel(ctx, stored, "model-a"); got.Outcome != ModelChangeFailed || got.Code != "canceled" {
		t.Fatalf("ChangeModel(canceled) = %#v", got)
	}
}
