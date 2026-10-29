package acpagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func createRegistrySession(t *testing.T, service session.Service, id string) session.Session {
	t.Helper()
	created, err := service.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	return created.Session
}

func TestModelRegistryAdmissionAndTokens(t *testing.T) {
	service := session.InMemoryService()
	first := createRegistrySession(t, service, "first")
	second := createRegistrySession(t, service, "second")
	registry := newModelRegistry()
	if _, err := registry.acquire(t.Context(), nil); err == nil {
		t.Fatal("nil session admission succeeded")
	}

	admission, err := registry.acquire(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.acquire(t.Context(), first); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("same-key acquire error = %v, want busy", err)
	}
	other, err := registry.acquire(t.Context(), second)
	if err != nil {
		t.Fatalf("cross-key acquire: %v", err)
	}
	other.release()

	oldEntry := admission.entry
	oldToken := admission.token
	firstAttachment := admission.replaceAttachment("remote-1")
	secondAttachment := admission.replaceAttachment("remote-2")
	control := newSessionControlState(helperModelConfigOptions("model-a", "provider.model"), "generation")
	if admission.confirmCurrentAttachment(control, firstAttachment) {
		t.Fatal("stale attachment token was accepted")
	}
	if !admission.confirmCurrentAttachment(control, secondAttachment) {
		t.Fatal("current attachment token was rejected")
	}
	if admission.confirm("model-a", "model-a", firstAttachment) {
		t.Fatal("stale attachment token confirmed a model")
	}
	tokenBeforeConfirm := admission.currentAttachmentToken()
	if !admission.confirm("model-a", "model-a", tokenBeforeConfirm) || admission.currentAttachmentToken() != tokenBeforeConfirm {
		t.Fatal("model confirmation changed attachment token")
	}
	if got := admission.observed(); got != control {
		t.Fatalf("observed control = %#v, want %#v", got, control)
	}
	admission.observeCurrent(control)
	admission.release()
	if !registry.evictOneLocked() {
		t.Fatal("default zero-reference entry was not evictable")
	}
	if _, ok := registry.entries[admission.key]; ok {
		t.Fatal("default zero-reference entry was not evicted")
	}

	replacement, err := registry.acquire(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.token == oldToken {
		t.Fatal("replacement reused immutable entry token")
	}
	late := &modelAdmission{registry: registry, key: replacement.key, entry: oldEntry, token: oldToken}
	late.release()
	if !replacement.entry.busy || replacement.entry.refs != 1 {
		t.Fatal("late old-token release changed replacement")
	}
	registry.close()
	replacement.replaceAttachment("remote-3")
	replacement.release()
	if len(registry.entries) != 0 {
		t.Fatal("last exact release after close did not finalize registry entry")
	}
}

func TestModelRegistryBoundedAdmissionErrors(t *testing.T) {
	if got := boundedModelAdmissionError(ErrSessionBusy); !errors.Is(got, ErrSessionBusy) {
		t.Fatalf("busy = %v", got)
	}
	if got := boundedModelAdmissionError(ErrClientClosed); !errors.Is(got, ErrClientClosed) {
		t.Fatalf("closed = %v", got)
	}
	if got := boundedModelAdmissionError(ErrModelRegistryFull); !errors.Is(got, ErrModelRegistryFull) {
		t.Fatalf("full = %v", got)
	}
	if got := boundedModelAdmissionError(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("canceled = %v", got)
	}
	if got := boundedModelAdmissionError(errors.New("secret")); got.Error() != "acp adk session admission failed" {
		t.Fatalf("unknown = %v", got)
	}

	registry := newModelRegistry()
	if _, err := registry.acquire(nil, nil); !errors.Is(err, context.Canceled) { //nolint:staticcheck // Verify the public nil-context guard.
		t.Fatalf("nil context error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := registry.acquire(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v", err)
	}
	registry.close()
	service := session.InMemoryService()
	if _, err := registry.acquire(t.Context(), createRegistrySession(t, service, "closed")); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("closed registry error = %v", err)
	}
	(&modelAdmission{}).release()
}

func TestModelRegistryEvictsOldestDefaultEntryAtCapacity(t *testing.T) {
	registry := newModelRegistry()
	for i := 0; i < maxModelRegistryEntries; i++ {
		registry.nextEntryToken++
		registry.nextSequence++
		key := adkSessionKey{appName: "app", userID: "user", sessionID: fmt.Sprintf("old-%d", i)}
		entry := &modelRegistryEntry{entryToken: registry.nextEntryToken, insertSequence: registry.nextSequence}
		if i > 0 {
			entry.liveDesired = "sticky"
		}
		registry.entries[key] = entry
	}
	service := session.InMemoryService()
	extra := createRegistrySession(t, service, "replacement")
	admission, err := registry.acquire(t.Context(), extra)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.release()
	if _, ok := registry.entries[adkSessionKey{appName: "app", userID: "user", sessionID: "old-0"}]; ok {
		t.Fatal("oldest evictable entry was retained")
	}
}

func TestModelRegistryStickyCapacityFailsClosed(t *testing.T) {
	service := session.InMemoryService()
	registry := newModelRegistry()
	for i := 0; i < maxModelRegistryEntries; i++ {
		sess := createRegistrySession(t, service, fmt.Sprintf("session-%d", i))
		admission, err := registry.acquire(t.Context(), sess)
		if err != nil {
			t.Fatal(err)
		}
		attachment := admission.replaceAttachment("remote")
		admission.confirm("override", "default", attachment)
		admission.release()
	}
	extra := createRegistrySession(t, service, "extra")
	if _, err := registry.acquire(t.Context(), extra); !errors.Is(err, ErrModelRegistryFull) {
		t.Fatalf("capacity acquire error = %v, want registry full", err)
	}

	first, err := service.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session-0"})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := registry.acquire(t.Context(), first.Session)
	if err != nil {
		t.Fatal(err)
	}
	if got := admission.desired("default"); got != "override" {
		t.Fatalf("sticky desired = %q, want override", got)
	}
	attachment := admission.replaceAttachment("remote")
	admission.confirm("default", "default", attachment)
	admission.release()
	if _, err := registry.acquire(t.Context(), extra); err != nil {
		t.Fatalf("clearing sticky override did not free capacity: %v", err)
	}
}

func TestAgentCloseDoesNotWaitForADKSessionAdmission(t *testing.T) {
	lifecycle, cancel := context.WithCancel(t.Context())
	agent := &Agent{
		lifecycle:          lifecycle,
		cancelLifecycle:    cancel,
		initializationGate: make(chan struct{}, 1),
		modelRegistry:      newModelRegistry(),
	}
	service := session.InMemoryService()
	sess := createRegistrySession(t, service, "close")
	admission, err := agent.modelRegistry.acquire(t.Context(), sess)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- agent.Close() }()
	go func() { done <- agent.Close() }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Agent.Close waited for an admitted ADK-session operation")
		}
	}
	admission.release()
	admission.release()
	if len(agent.modelRegistry.entries) != 0 {
		t.Fatal("deferred exact-token release did not finalize closed registry")
	}
}

func TestAgentCloseFromPartialMessageCallbackDoesNotDeadlock(t *testing.T) {
	agent, err := NewWithContext(t.Context(), Config{Command: helperCommand(t), WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	service := session.InMemoryService()
	created := createRegistrySession(t, service, "partial-close")
	r, err := runner.New(runner.Config{AppName: "app", Agent: agent, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	closed := false
	for event, _ := range r.Run(ctx, "user", created.ID(), genai.NewContentFromText("hello", genai.RoleUser), adkagent.RunConfig{}) {
		if event == nil || !event.Partial || closed {
			continue
		}
		if err := agent.Close(); err != nil {
			t.Fatalf("Close from partial callback error = %v", err)
		}
		closed = true
	}
	if !closed {
		t.Fatal("partial message callback was not observed")
	}
	if len(agent.modelRegistry.entries) != 0 {
		t.Fatal("partial callback Close did not reach eventual zero refs")
	}
}

func TestAgentCloseFromPermissionCallbackDoesNotDeadlock(t *testing.T) {
	callbackReturned := make(chan error, 1)
	var agent *Agent
	var err error
	agent, err = NewWithContext(t.Context(), Config{
		Command:    helperCommand(t),
		WorkingDir: t.TempDir(),
		PermissionHandler: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			closeErr := agent.Close()
			callbackReturned <- closeErr
			return PermissionDecision{OptionID: "reject"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := session.InMemoryService()
	created := createRegistrySession(t, service, "callback-close")
	r, err := runner.New(runner.Config{AppName: "app", Agent: agent, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for range r.Run(ctx, "user", created.ID(), genai.NewContentFromText("permission", genai.RoleUser), adkagent.RunConfig{}) {
	}
	select {
	case closeErr := <-callbackReturned:
		if closeErr != nil {
			t.Fatalf("callback Close error = %v", closeErr)
		}
	case <-ctx.Done():
		t.Fatal("Agent.Close deadlocked inside permission callback")
	}
}

func TestDesiredModelDescriptorInvalidIsPreDispatch(t *testing.T) {
	service := session.InMemoryService()
	sess := createRegistrySession(t, service, "descriptor")
	registry := newModelRegistry()
	admission, err := registry.acquire(t.Context(), sess)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.release()
	desired := "model-b"
	agent := &Agent{desiredModel: desired}
	if _, err := agent.applyDesiredModelHeld(t.Context(), admission, "remote", nil); !errors.Is(err, ErrModelDescriptorInvalid) {
		t.Fatalf("applyDesiredModelHeld error = %v, want descriptor invalid", err)
	}
}

func TestModelDescriptorClassificationAndDurableFiltering(t *testing.T) {
	category := acp.SessionConfigOptionCategoryModel
	valid := helperModelConfigOptions("model-a", "provider.model")
	duplicate := append([]acp.SessionConfigOption(nil), valid...)
	duplicateOption := valid[0]
	duplicateSelect := *valid[0].Select
	duplicateSelect.Id = "provider.model.second"
	duplicateOption.Select = &duplicateSelect
	duplicate = append(duplicate, duplicateOption)
	malformed := append([]acp.SessionConfigOption(nil), valid...)
	malformedSelect := *valid[0].Select
	malformedSelect.CurrentValue = "not-advertised"
	malformed[0].Select = &malformedSelect
	booleanModel := append([]acp.SessionConfigOption(nil), valid...)
	booleanModel = append(booleanModel, acp.SessionConfigOption{Boolean: &acp.SessionConfigOptionBoolean{
		Id: "provider.model.boolean", Name: "bad", Category: &category, CurrentValue: true, Type: "boolean",
	}})

	for name, options := range map[string][]acp.SessionConfigOption{
		"missing":       nil,
		"duplicate":     duplicate,
		"malformed":     malformed,
		"mixed boolean": booleanModel,
	} {
		t.Run(name, func(t *testing.T) {
			if control, valid := classifyModelDescriptor(options, "generation"); valid || control != nil {
				t.Fatalf("classification = %#v, %v", control, valid)
			}
		})
	}
	values := []SessionConfigValue{
		SelectSessionConfigValue("provider.model", "a"),
		SelectSessionConfigValue("provider.model.second", "b"),
		BooleanSessionConfigValue("provider.model.boolean", true),
		SelectSessionConfigValue("mode", "code"),
	}
	filtered := withoutModelConfigOptions(values, append(duplicate, booleanModel[len(booleanModel)-1]))
	if len(filtered) != 1 || filtered[0].ID != "mode" {
		t.Fatalf("filtered config values = %#v, want mode only", filtered)
	}
}

func TestInvalidDesiredDescriptorHasNoMutationOrDurableProjection(t *testing.T) {
	for name, flag := range map[string]string{
		"missing":       "GO_OMIT_MODEL_DESCRIPTOR",
		"duplicate":     "GO_DUPLICATE_MODEL_DESCRIPTOR",
		"malformed":     "GO_MALFORMED_MODEL_DESCRIPTOR",
		"boolean model": "GO_BOOLEAN_MODEL_DESCRIPTOR",
	} {
		t.Run(name, func(t *testing.T) {
			methodLog := t.TempDir() + "/methods.log"
			desired := "model-a"
			agent, err := NewWithContext(t.Context(), Config{
				Command: helperCommandWithEnv(t, map[string]string{
					"GO_CURRENT_SESSION_MODEL": "model-a",
					"GO_EXPECT_SESSION_MODEL":  "model-a",
					"GO_METHOD_LOG":            methodLog,
					flag:                       "1",
				}),
				WorkingDir:   t.TempDir(),
				DesiredModel: &desired,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, agent)
			service := session.InMemoryService()
			created := createRegistrySession(t, service, "invalid-descriptor")
			r, err := runner.New(runner.Config{AppName: "app", Agent: agent, SessionService: service})
			if err != nil {
				t.Fatal(err)
			}
			var gotErr error
			for _, runErr := range r.Run(t.Context(), "user", created.ID(), genai.NewContentFromText("hello", genai.RoleUser), adkagent.RunConfig{}) {
				if runErr != nil {
					gotErr = runErr
				}
			}
			if !errors.Is(gotErr, ErrModelDescriptorInvalid) {
				t.Fatalf("run error = %v", gotErr)
			}
			methods, err := os.ReadFile(methodLog)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(methods), "session/set_config_option") || strings.Contains(string(methods), "session/prompt") {
				t.Fatalf("invalid descriptor crossed mutation boundary:\n%s", methods)
			}
			stored, err := service.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: created.ID()})
			if err != nil {
				t.Fatal(err)
			}
			if raw, stateErr := stored.Session.State().Get(SessionStateKey); stateErr == nil {
				state, _ := raw.(map[string]any)
				if _, ok := state[controlStateField]; ok {
					t.Fatalf("invalid descriptor persisted control: %v", state)
				}
				if _, ok := state["config_values"]; ok {
					t.Fatalf("invalid descriptor persisted model values: %v", state)
				}
			}
		})
	}
}

func TestDesiredModelLocalPreDispatchOutcomes(t *testing.T) {
	service := session.InMemoryService()
	sess := createRegistrySession(t, service, "local-outcomes")
	options := helperModelConfigOptions("model-a", "provider.model")

	t.Run("automatic selection", func(t *testing.T) {
		registry := newModelRegistry()
		admission, err := registry.acquire(t.Context(), sess)
		if err != nil {
			t.Fatal(err)
		}
		defer admission.release()
		agent := &Agent{}
		control, err := agent.applyDesiredModelHeld(t.Context(), admission, "remote", options)
		if err != nil || control == nil || control.currentModel != "model-a" {
			t.Fatalf("apply = %#v, %v", control, err)
		}
	})

	t.Run("already selected", func(t *testing.T) {
		registry := newModelRegistry()
		admission, err := registry.acquire(t.Context(), sess)
		if err != nil {
			t.Fatal(err)
		}
		defer admission.release()
		agent := &Agent{desiredModel: "model-a"}
		control, err := agent.applyDesiredModelHeld(t.Context(), admission, "remote", options)
		if err != nil || control.currentModel != "model-a" {
			t.Fatalf("apply = %#v, %v", control, err)
		}
	})

	t.Run("already selected on stale attachment", func(t *testing.T) {
		registry := newModelRegistry()
		admission, err := registry.acquire(t.Context(), sess)
		if err != nil {
			t.Fatal(err)
		}
		defer admission.release()
		stale := admission.replaceAttachment("remote-old")
		current := admission.replaceAttachment("remote-current")
		controlBefore := admission.entry.control
		agent := &Agent{desiredModel: "model-a"}
		if _, err := agent.applyDesiredModelCurrentHeld(t.Context(), admission, "remote-old", options, stale); !errors.Is(err, ErrDesiredModelUnavailable) {
			t.Fatalf("apply error = %v, want desired model unavailable", err)
		}
		if admission.entry.attachmentToken != current || admission.entry.attachmentID != "remote-current" || admission.entry.control != controlBefore {
			t.Fatalf("stale rejection changed current attachment: %#v", admission.entry)
		}
	})

	t.Run("unknown target", func(t *testing.T) {
		registry := newModelRegistry()
		admission, err := registry.acquire(t.Context(), sess)
		if err != nil {
			t.Fatal(err)
		}
		defer admission.release()
		agent := &Agent{desiredModel: "model-b"}
		if _, err := agent.applyDesiredModelHeld(t.Context(), admission, "remote", options); !errors.Is(err, ErrDesiredModelRejected) {
			t.Fatalf("apply error = %v", err)
		}
	})

	t.Run("canceled before dispatch", func(t *testing.T) {
		registry := newModelRegistry()
		admission, err := registry.acquire(t.Context(), sess)
		if err != nil {
			t.Fatal(err)
		}
		defer admission.release()
		agent := &Agent{desiredModel: "model-b"}
		withTarget := helperModelConfigOptions("model-a", "provider.model")
		modelCategory := acp.SessionConfigOptionCategoryModel
		withTarget[0].Select.Options.Ungrouped = &acp.SessionConfigSelectOptionsUngrouped{
			{Name: "model-a", Value: "model-a"},
			{Name: "model-b", Value: "model-b"},
		}
		withTarget[0].Select.Category = &modelCategory
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := agent.applyDesiredModelHeld(ctx, admission, "remote", withTarget); !errors.Is(err, context.Canceled) {
			t.Fatalf("apply error = %v", err)
		}
	})
}

func TestDesiredModelUsesSemanticDescriptorBeforePrompt(t *testing.T) {
	methodLog := t.TempDir() + "/methods.log"
	desired := "model-b"
	agent, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL": "model-a",
			"GO_EXPECT_SESSION_MODEL":  desired,
			"GO_EXPECT_CONFIG_ID":      "provider.model.choice",
			"GO_SESSION_MODELS":        desired,
			"GO_METHOD_LOG":            methodLog,
		}),
		WorkingDir:   t.TempDir(),
		DesiredModel: &desired,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, agent)
	service := session.InMemoryService()
	created := createRegistrySession(t, service, "desired")
	r, err := runner.New(runner.Config{AppName: "app", Agent: agent, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	for _, runErr := range r.Run(t.Context(), "user", created.ID(), genai.NewContentFromText("hello", genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(methods)
	set := strings.Index(text, "session/set_config_option")
	prompt := strings.Index(text, "session/prompt")
	if set < 0 || prompt < 0 || set > prompt {
		t.Fatalf("desired model was not verified before prompt:\n%s", text)
	}
	stored, err := service.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: created.ID()})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := stored.Session.State().Get(SessionStateKey)
	if err != nil {
		t.Fatal(err)
	}
	state, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("ACP state type = %T", raw)
	}
	if _, ok := state[controlStateField]; ok {
		t.Fatalf("model control leaked into durable state: %v", state)
	}
	if _, ok := state["config_values"]; ok {
		t.Fatalf("desired model leaked into durable config values: %v", state)
	}
}

func TestDesiredModelAmbiguityPreventsPromptAndRedactsTarget(t *testing.T) {
	methodLog := t.TempDir() + "/methods.log"
	desired := "secret-model-target"
	agent, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL": "model-a",
			"GO_EXPECT_SESSION_MODEL":  desired,
			"GO_SESSION_MODELS":        desired,
			"GO_FAIL_METHOD":           "session/set_config_option",
			"GO_METHOD_LOG":            methodLog,
		}),
		WorkingDir:   t.TempDir(),
		DesiredModel: &desired,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, agent)
	service := session.InMemoryService()
	created := createRegistrySession(t, service, "ambiguous")
	r, err := runner.New(runner.Config{AppName: "app", Agent: agent, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	for _, runErr := range r.Run(context.Background(), "user", created.ID(), genai.NewContentFromText("hello", genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			gotErr = runErr
		}
	}
	if gotErr == nil || !errors.Is(gotErr, ErrDesiredModelUncertain) {
		t.Fatalf("run error = %v, want uncertain", gotErr)
	}
	if strings.Contains(gotErr.Error(), desired) {
		t.Fatalf("error leaked desired model: %v", gotErr)
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(methods), "session/prompt") {
		t.Fatalf("ambiguous model application reached prompt:\n%s", methods)
	}
}

func TestMayHaveMutatedInitialDesiredRetriesInspectFirstWithoutSecondSet(t *testing.T) {
	methodLog := t.TempDir() + "/methods.log"
	desired := "model-b"
	agent, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL":      "model-a",
			"GO_EXPECT_SESSION_MODEL":       desired,
			"GO_SESSION_MODELS":             desired,
			"GO_SUPPORT_SESSION_RESUME":     "1",
			"GO_EXPECT_RESUME_SESSION_ID":   "session-1",
			"GO_APPLY_FIRST_SET_THEN_ERROR": "1",
			"GO_METHOD_LOG":                 methodLog,
		}),
		WorkingDir:   t.TempDir(),
		DesiredModel: &desired,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, agent)
	service := session.InMemoryService()
	created := createRegistrySession(t, service, "retry")
	r, err := runner.New(runner.Config{AppName: "app", Agent: agent, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	run := func() error {
		var gotErr error
		for _, runErr := range r.Run(t.Context(), "user", created.ID(), genai.NewContentFromText("hello", genai.RoleUser), adkagent.RunConfig{}) {
			if runErr != nil {
				gotErr = runErr
			}
		}
		return gotErr
	}
	if err := run(); !errors.Is(err, ErrDesiredModelUncertain) {
		t.Fatalf("first run error = %v", err)
	}
	if err := run(); err != nil {
		t.Fatalf("retry error = %v", err)
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(methods)
	if strings.Count(text, "session/set_config_option") != 1 || strings.Count(text, "session/prompt") != 1 {
		t.Fatalf("unexpected retry protocol sequence:\n%s", text)
	}
	resume := strings.Index(text, "session/resume")
	prompt := strings.Index(text, "session/prompt")
	if resume < 0 || resume > prompt {
		t.Fatalf("retry did not Inspect before prompt:\n%s", text)
	}
}

func TestDesiredModelBoundedFailureMatrix(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		want error
	}{
		{name: "method unsupported", env: map[string]string{"GO_DISABLE_SET_CONFIG_OPTION": "1"}, want: ErrDesiredModelRejected},
		{name: "invalid params", env: map[string]string{"GO_SET_CONFIG_OPTION_ERROR": "invalid_params"}, want: ErrDesiredModelRejected},
		{name: "empty response", env: map[string]string{"GO_EMPTY_SET_CONFIG_OPTION_RESPONSE": "1"}, want: ErrDesiredModelUncertain},
		{name: "different confirmed model", env: map[string]string{"GO_SET_CONFIG_OPTION_RESPONSE_MODEL": "model-c"}, want: ErrDesiredModelRejected},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			desired := "model-b"
			env := map[string]string{
				"GO_CURRENT_SESSION_MODEL": "model-a",
				"GO_EXPECT_SESSION_MODEL":  desired,
				"GO_SESSION_MODELS":        desired,
			}
			for key, value := range tc.env {
				env[key] = value
			}
			agent, err := NewWithContext(t.Context(), Config{
				Command:      helperCommandWithEnv(t, env),
				WorkingDir:   t.TempDir(),
				DesiredModel: &desired,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestCloser(t, agent)
			service := session.InMemoryService()
			created := createRegistrySession(t, service, "failure")
			r, err := runner.New(runner.Config{AppName: "app", Agent: agent, SessionService: service})
			if err != nil {
				t.Fatal(err)
			}
			var gotErr error
			for _, runErr := range r.Run(t.Context(), "user", created.ID(), genai.NewContentFromText("hello", genai.RoleUser), adkagent.RunConfig{}) {
				if runErr != nil {
					gotErr = runErr
				}
			}
			if !errors.Is(gotErr, tc.want) {
				t.Fatalf("run error = %v, want %v", gotErr, tc.want)
			}
		})
	}
}

func TestSessionControlModelRegistryAdmissionMapping(t *testing.T) {
	service := session.InMemoryService()
	sess := createRegistrySession(t, service, "mapping")
	agent := &Agent{modelRegistry: newModelRegistry()}
	admission, err := agent.modelRegistry.acquire(t.Context(), sess)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.release()
	control := agent.SessionControl()
	if got := control.Inspect(t.Context(), sess); got.Status != SessionControlBusy || got.Code != "busy" {
		t.Fatalf("Inspect = %#v", got)
	}
	if got := control.ValidateModel(t.Context(), sess, "model"); got.Outcome != ModelValidationBusy {
		t.Fatalf("ValidateModel = %#v", got)
	}
	if got := control.ChangeModel(t.Context(), sess, "model"); got.Outcome != ModelChangeBusy {
		t.Fatalf("ChangeModel = %#v", got)
	}
	admission.release()
	agent.modelRegistry.close()
	if got := control.Inspect(t.Context(), sess); got.Status != SessionControlClosed || got.Code != "closed" {
		t.Fatalf("Inspect after close = %#v", got)
	}
	if got := controlSnapshotForModelAdmissionError(context.Canceled); got.Status != SessionControlFailed || got.Code != "canceled" {
		t.Fatalf("canceled snapshot = %#v", got)
	}
	if got := controlSnapshotForModelAdmissionError(errors.New("secret")); got.Status != SessionControlFailed || got.Code != "invalid_session" {
		t.Fatalf("unknown snapshot = %#v", got)
	}
	if got := modelValidationForAdmissionError(ErrClientClosed); got.Outcome != ModelValidationClosed {
		t.Fatalf("closed validation = %#v", got)
	}
	if got := modelValidationForAdmissionError(context.Canceled); got.Outcome != ModelValidationFailed {
		t.Fatalf("failed validation = %#v", got)
	}
}

func TestModelControlInvalidInputsAndEventHelpers(t *testing.T) {
	var nilResult *promptRunResult
	nilResult.release()
	(&promptRunResult{}).release()

	control := (&Agent{modelRegistry: newModelRegistry()}).SessionControl()
	if got := control.Inspect(nil, nil); got.Status != SessionControlFailed || got.Code != "invalid_session" { //nolint:staticcheck // Verify bounded invalid input.
		t.Fatalf("Inspect invalid = %#v", got)
	}
	if got := control.ValidateModel(nil, nil, "model"); got.Outcome != ModelValidationFailed || got.Code != "invalid_session" { //nolint:staticcheck // Verify bounded invalid input.
		t.Fatalf("ValidateModel invalid = %#v", got)
	}
	if got := control.ChangeModel(nil, nil, "model"); got.Outcome != ModelChangeFailed || got.Code != "invalid_session" { //nolint:staticcheck // Verify bounded invalid input.
		t.Fatalf("ChangeModel invalid = %#v", got)
	}

	agent := &Agent{outputKey: "answer"}
	event := &session.Event{}
	agent.maybeSaveOutputToState(event, "done")
	if got := event.Actions.StateDelta["answer"]; got != "done" {
		t.Fatalf("saved output = %v", got)
	}
	var logs testLogBuffer
	event.InvocationID = "invocation"
	event.Partial = true
	event.TurnComplete = true
	event.FinishReason = genai.FinishReasonStop
	event.Content = genai.NewContentFromText("content", genai.RoleModel)
	agent.logADKEvent(testLogger(&logs, levelTrace), event, "event")
	text := logs.String()
	for _, want := range []string{"invocation", "finish_reason", "has_content"} {
		if !strings.Contains(text, want) {
			t.Fatalf("event log missing %q: %s", want, text)
		}
	}
}

func TestEnsureInitializedRejectsClosedAndCanceledAgent(t *testing.T) {
	lifecycle, cancel := context.WithCancel(t.Context())
	agent := &Agent{lifecycle: lifecycle, initializationGate: make(chan struct{}, 1), closed: true}
	if err := agent.ensureInitialized(t.Context()); err == nil || err.Error() != "acp agent is closed" {
		t.Fatalf("closed ensureInitialized error = %v", err)
	}
	agent.closed = false
	cancel()
	if err := agent.ensureInitialized(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ensureInitialized error = %v", err)
	}
}

func TestDesiredModelExistingAttachmentUnavailablePreventsPrompt(t *testing.T) {
	workingDir := t.TempDir()
	methodLog := t.TempDir() + "/methods.log"
	service := session.InMemoryService()
	first, err := NewWithContext(t.Context(), Config{
		Command:      helperCommandWithEnv(t, map[string]string{"GO_CURRENT_SESSION_MODEL": "model-a", "GO_EXPECT_SESSION_MODEL": "model-a"}),
		WorkingDir:   workingDir,
		SessionOwner: "stable-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := runControlSession(t, first, service, "unavailable-model", "user")
	closeTestCloser(t, first)

	desired := "model-b"
	second, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL":    "model-a",
			"GO_EXPECT_SESSION_MODEL":     desired,
			"GO_SESSION_MODELS":           desired,
			"GO_SUPPORT_SESSION_RESUME":   "1",
			"GO_EXPECT_RESUME_SESSION_ID": "session-1",
			"GO_FAIL_METHOD":              "session/resume",
			"GO_METHOD_LOG":               methodLog,
		}),
		WorkingDir:   workingDir,
		SessionOwner: "stable-owner",
		DesiredModel: &desired,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, second)
	admission, err := second.modelRegistry.acquire(t.Context(), stored)
	if err != nil {
		t.Fatal(err)
	}
	tokenBefore := admission.replaceAttachment("session-1")
	idBefore := admission.entry.attachmentID
	controlBefore := admission.entry.control
	admission.release()
	r, err := runner.New(runner.Config{AppName: "unavailable-model", Agent: second, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	for _, runErr := range r.Run(t.Context(), "user", stored.ID(), genai.NewContentFromText("again", genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			gotErr = runErr
		}
	}
	if !errors.Is(gotErr, ErrDesiredModelUnavailable) {
		t.Fatalf("run error = %v, want model unavailable", gotErr)
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(methods)
	if strings.Count(text, "session/resume") != 1 {
		t.Fatalf("method log resume count = %d, want 1:\n%s", strings.Count(text, "session/resume"), text)
	}
	for _, forbidden := range []string{"session/prompt", "session/set_config_option"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("method log unexpectedly contains %q:\n%s", forbidden, text)
		}
	}
	key, err := sessionKey(stored)
	if err != nil {
		t.Fatal(err)
	}
	entry := second.modelRegistry.entries[key]
	if entry.attachmentToken != tokenBefore || entry.attachmentID != idBefore || entry.control != controlBefore {
		t.Fatalf("failed resume changed incumbent attachment: before token=%d id=%q control=%p; after=%#v", tokenBefore, idBefore, controlBefore, entry)
	}
}

func TestClosedRegistryRejectsInternalSessionEntryPoints(t *testing.T) {
	agent := &Agent{modelRegistry: newModelRegistry()}
	agent.modelRegistry.close()
	ctx := newStateContext(t, map[string]any{})
	if _, err := agent.ensureRemoteSession(ctx, t.Context(), newLogger(nil, ""), nil); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("ensureRemoteSession error = %v", err)
	}
	if _, err := agent.recoverRemoteSession(ctx, t.Context(), newLogger(nil, ""), errors.New("lost"), nil); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("recoverRemoteSession error = %v", err)
	}
}

func TestDesiredModelBlankRejectedBeforeProcessStart(t *testing.T) {
	blank := "  "
	_, err := NewWithContext(t.Context(), Config{Command: []string{"does-not-exist"}, DesiredModel: &blank})
	if err == nil || err.Error() != "desired model must not be blank" {
		t.Fatalf("NewWithContext error = %v", err)
	}
}

func TestDesiredModelReconstructedAfterProductRestart(t *testing.T) {
	workingDir := t.TempDir()
	service := session.InMemoryService()
	first, err := NewWithContext(t.Context(), Config{
		Command:      helperCommandWithEnv(t, map[string]string{"GO_CURRENT_SESSION_MODEL": "model-a", "GO_EXPECT_SESSION_MODEL": "model-a"}),
		WorkingDir:   workingDir,
		SessionOwner: "stable-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := runControlSession(t, first, service, "restart-model", "user")
	closeTestCloser(t, first)

	methodLog := t.TempDir() + "/methods.log"
	desired := "model-b"
	second, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL":    "model-a",
			"GO_EXPECT_SESSION_MODEL":     desired,
			"GO_SESSION_MODELS":           desired,
			"GO_SUPPORT_SESSION_RESUME":   "1",
			"GO_EXPECT_RESUME_SESSION_ID": "session-1",
			"GO_METHOD_LOG":               methodLog,
		}),
		WorkingDir:   workingDir,
		SessionOwner: "stable-owner",
		DesiredModel: &desired,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, second)
	r, err := runner.New(runner.Config{AppName: "restart-model", Agent: second, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	for _, runErr := range r.Run(t.Context(), "user", stored.ID(), genai.NewContentFromText("again", genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(methods)
	resume := strings.Index(text, "session/resume")
	set := strings.Index(text, "session/set_config_option")
	prompt := strings.Index(text, "session/prompt")
	if resume < 0 || set < resume || prompt < set {
		t.Fatalf("restart did not reconstruct desired model before prompt:\n%s", text)
	}
}

func TestConfirmedLiveDesiredSurvivesSessionNotFoundRecovery(t *testing.T) {
	methodLog := t.TempDir() + "/methods.log"
	agent, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL":           "model-a",
			"GO_EXPECT_SESSION_MODEL":            "model-b",
			"GO_SESSION_MODELS":                  "model-b",
			"GO_SUPPORT_SESSION_RESUME":          "1",
			"GO_EXPECT_RESUME_SESSION_ID":        "session-1",
			"GO_FAIL_RESUME_ENTITY_NOT_FOUND_AT": "2",
			"GO_FAIL_PROMPT_ENTITY_NOT_FOUND_AT": "2",
			"GO_METHOD_LOG":                      methodLog,
		}),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, agent)
	service := session.InMemoryService()
	stored := runControlSession(t, agent, service, "not-found-model", "user")
	if got := agent.SessionControl().ChangeModel(t.Context(), stored, "model-b"); got.Outcome != ModelChangeApplied {
		t.Fatalf("ChangeModel = %#v", got)
	}
	r, err := runner.New(runner.Config{AppName: "not-found-model", Agent: agent, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	for _, runErr := range r.Run(t.Context(), "user", stored.ID(), genai.NewContentFromText("recover", genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(methods)
	if strings.Count(text, "session/set_config_option") < 2 || strings.Count(text, "session/new") < 2 {
		t.Fatalf("confirmed live model was not reapplied after session-not-found:\n%s", text)
	}
}

func TestSessionNotFoundInvalidatesAttachmentBeforeFailedFallback(t *testing.T) {
	agent, err := NewWithContext(t.Context(), Config{
		Command: helperCommandWithEnv(t, map[string]string{
			"GO_CURRENT_SESSION_MODEL":           "model-a",
			"GO_EXPECT_SESSION_MODEL":            "model-b",
			"GO_SESSION_MODELS":                  "model-b",
			"GO_SUPPORT_SESSION_RESUME":          "1",
			"GO_EXPECT_RESUME_SESSION_ID":        "session-1",
			"GO_FAIL_RESUME_ENTITY_NOT_FOUND_AT": "2",
			"GO_FAIL_PROMPT_ENTITY_NOT_FOUND_AT": "2",
			"GO_FAIL_SESSION_NEW_AT":             "2",
		}),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestCloser(t, agent)
	service := session.InMemoryService()
	stored := runControlSession(t, agent, service, "failed-fallback", "user")
	if got := agent.SessionControl().ChangeModel(t.Context(), stored, "model-b"); got.Outcome != ModelChangeApplied {
		t.Fatalf("ChangeModel = %#v", got)
	}
	key, err := sessionKey(stored)
	if err != nil {
		t.Fatal(err)
	}
	tokenBefore := agent.modelRegistry.entries[key].attachmentToken
	r, err := runner.New(runner.Config{AppName: "failed-fallback", Agent: agent, SessionService: service})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	for _, runErr := range r.Run(t.Context(), "user", stored.ID(), genai.NewContentFromText("recover", genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			gotErr = runErr
		}
	}
	if gotErr == nil {
		t.Fatal("failed fallback unexpectedly succeeded")
	}
	entry := agent.modelRegistry.entries[key]
	if entry.attachmentToken <= tokenBefore {
		t.Fatalf("attachment token did not advance: before=%d after=%d", tokenBefore, entry.attachmentToken)
	}
	if entry.attachmentID != "" {
		t.Fatalf("failed fallback attachment ID = %q, want empty", entry.attachmentID)
	}
	if entry.control != nil {
		t.Fatalf("failed fallback retained stale verification: %#v", entry)
	}
}
