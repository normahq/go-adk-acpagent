package acpagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/adk/v2/session"
)

const (
	controlStateField       = "control"
	maxControlModels        = 64
	maxControlStringBytes   = 256
	attachmentGenerationLen = 16
)

// SessionControlStatus reports whether exact-session model control is usable.
type SessionControlStatus string

const (
	// SessionControlAvailable means exact-session model control is authoritative.
	SessionControlAvailable SessionControlStatus = "available"
	// SessionControlUnavailable means the session cannot expose model control.
	SessionControlUnavailable SessionControlStatus = "unavailable"
	// SessionControlBusy means another operation currently owns the session.
	SessionControlBusy SessionControlStatus = "busy"
	// SessionControlFailed means inspection failed before an authoritative result.
	SessionControlFailed SessionControlStatus = "failed"
	// SessionControlClosed means the owning Agent has closed.
	SessionControlClosed SessionControlStatus = "closed"
)

// SessionModel describes one bounded public model choice.
type SessionModel struct {
	ID    string
	Label string
}

// SessionControlSnapshot is the authoritative model-control view for one ADK
// session. It intentionally excludes the remote ACP session ID and raw errors.
type SessionControlSnapshot struct {
	Status       SessionControlStatus
	Revision     string
	CurrentModel string
	Models       []SessionModel
	Code         string
}

// ModelValidationOutcome is the result of validating a model without applying it.
type ModelValidationOutcome string

const (
	// ModelValidationSupported means the model is valid for the exact session.
	ModelValidationSupported ModelValidationOutcome = "supported"
	// ModelValidationUnsupported means the session cannot validate models.
	ModelValidationUnsupported ModelValidationOutcome = "unsupported"
	// ModelValidationIncompatible means the model is absent from the session choices.
	ModelValidationIncompatible ModelValidationOutcome = "incompatible"
	// ModelValidationBusy means another operation currently owns the session.
	ModelValidationBusy ModelValidationOutcome = "busy"
	// ModelValidationFailed means validation failed without an authoritative result.
	ModelValidationFailed ModelValidationOutcome = "failed"
	// ModelValidationClosed means the owning Agent has closed.
	ModelValidationClosed ModelValidationOutcome = "closed"
)

// ModelValidation reports whether a model can be applied to the exact session.
type ModelValidation struct {
	Outcome  ModelValidationOutcome
	Snapshot SessionControlSnapshot
	Code     string
}

// ModelChangeOutcome is the terminal certainty class for a model mutation.
type ModelChangeOutcome string

const (
	// ModelChangeApplied means the ACP response confirms the requested model.
	ModelChangeApplied ModelChangeOutcome = "applied"
	// ModelChangeUnsupported means exact-session mutation is unavailable.
	ModelChangeUnsupported ModelChangeOutcome = "unsupported"
	// ModelChangeIncompatible means the model is absent from the session choices.
	ModelChangeIncompatible ModelChangeOutcome = "incompatible"
	// ModelChangeRejected means an authoritative response rejected the mutation.
	ModelChangeRejected ModelChangeOutcome = "rejected"
	// ModelChangeBusy means another operation currently owns the session.
	ModelChangeBusy ModelChangeOutcome = "busy"
	// ModelChangeFailed means mutation failed before dispatch.
	ModelChangeFailed ModelChangeOutcome = "failed"
	// ModelChangeAmbiguous means dispatch may have happened without confirmation.
	ModelChangeAmbiguous ModelChangeOutcome = "ambiguous"
	// ModelChangeClosed means the owning Agent has closed.
	ModelChangeClosed ModelChangeOutcome = "closed"
)

// ModelChange reports the bounded result of an exact-session model mutation.
type ModelChange struct {
	Outcome        ModelChangeOutcome
	EffectiveModel string
	Snapshot       SessionControlSnapshot
	Code           string
}

// SessionController controls the ACP session owned by one Agent.
type SessionController struct {
	agent *Agent
}

// SessionControl returns the session controller owned by this Agent.
func (a *Agent) SessionControl() *SessionController {
	return &SessionController{agent: a}
}

type sessionControlState struct {
	generation    string
	revision      string
	modelOptionID string
	currentModel  string
	models        []SessionModel
}

func newAttachmentGeneration() (string, error) {
	var raw [attachmentGenerationLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// Inspect returns an authoritative snapshot when the exact stored ACP session
// is attached to this Agent process. It may resume that existing session to
// refresh descriptors, but never creates a session or sends a prompt.
func (c *SessionController) Inspect(ctx context.Context, sess session.Session) SessionControlSnapshot {
	a := c.agent
	if a == nil || sess == nil || ctx == nil {
		return unavailableControl(SessionControlFailed, "invalid_session")
	}
	if err := a.ensureInitialized(ctx); err != nil {
		if ctx.Err() != nil {
			return unavailableControl(SessionControlFailed, "canceled")
		}
		if a.lifecycle.Err() != nil {
			return unavailableControl(SessionControlClosed, "closed")
		}
		return unavailableControl(SessionControlFailed, "initialization_failed")
	}
	cfg, err := a.resolveSessionConfigForSession(sess)
	if err != nil {
		return unavailableControl(SessionControlFailed, "invalid_state")
	}
	if authoritativeControl(cfg.control, a.attachmentGeneration) {
		return cfg.control.snapshot(SessionControlAvailable, "")
	}
	if cfg.sessionID == "" || !a.client.SupportsSessionResume() {
		return unavailableControl(SessionControlUnavailable, "attachment_unavailable")
	}

	active := &activePrompt{sessionID: acp.SessionId(cfg.sessionID), logger: a.logger.withContext(ctx)}
	if err := a.client.beginSessionOperation(active); err != nil {
		return controlSnapshotForAdmissionError(err)
	}
	defer a.client.endSessionOperation(active)

	setup, cancel := a.setupContext(ctx)
	defer cancel()
	resp, err := a.client.ResumeSessionWithMeta(setup, cfg.sessionID, cfg.cwd, a.mcpServers, cfg.meta)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return unavailableControl(SessionControlFailed, "canceled")
		}
		if isACPMethodNotFoundError(err) {
			return unavailableControl(SessionControlUnavailable, "resume_unsupported")
		}
		if isACPResumeUnknownSessionError(err) {
			return unavailableControl(SessionControlUnavailable, "unknown_session")
		}
		return unavailableControl(SessionControlUnavailable, "attachment_unavailable")
	}
	configValues := collectSessionConfigValues(resp.ConfigOptions, resp.Modes)
	if len(configValues) > 0 {
		cfg.configValues = configValues
	}
	control := newSessionControlState(resp.ConfigOptions, a.attachmentGeneration)
	if control == nil {
		return unavailableControl(SessionControlUnavailable, "model_control_unavailable")
	}
	if err := a.persistControlState(sess, cfg, control); err != nil {
		return unavailableControl(SessionControlFailed, "state_write_failed")
	}
	return control.snapshot(SessionControlAvailable, "")
}

// ValidateModel validates a model against the authoritative exact-session
// snapshot without applying it.
func (c *SessionController) ValidateModel(ctx context.Context, sess session.Session, model string) ModelValidation {
	snapshot := c.Inspect(ctx, sess)
	switch snapshot.Status {
	case SessionControlAvailable:
		model = strings.TrimSpace(model)
		for _, candidate := range snapshot.Models {
			if candidate.ID == model {
				return ModelValidation{Outcome: ModelValidationSupported, Snapshot: snapshot}
			}
		}
		return ModelValidation{Outcome: ModelValidationIncompatible, Snapshot: snapshot, Code: "unknown_model"}
	case SessionControlBusy:
		return ModelValidation{Outcome: ModelValidationBusy, Snapshot: snapshot, Code: snapshot.Code}
	case SessionControlClosed:
		return ModelValidation{Outcome: ModelValidationClosed, Snapshot: snapshot, Code: snapshot.Code}
	case SessionControlFailed:
		return ModelValidation{Outcome: ModelValidationFailed, Snapshot: snapshot, Code: snapshot.Code}
	default:
		return ModelValidation{Outcome: ModelValidationUnsupported, Snapshot: snapshot, Code: snapshot.Code}
	}
}

// ChangeModel applies a model to the exact ACP session. Any non-authoritative
// error after dispatch is reported as ambiguous.
func (c *SessionController) ChangeModel(ctx context.Context, sess session.Session, model string) ModelChange {
	validation := c.ValidateModel(ctx, sess, model)
	if validation.Outcome != ModelValidationSupported {
		return modelChangeFromValidation(validation)
	}

	a := c.agent
	cfg, err := a.resolveSessionConfigForSession(sess)
	if err != nil {
		return ModelChange{Outcome: ModelChangeFailed, Code: "invalid_state"}
	}
	active := &activePrompt{sessionID: acp.SessionId(cfg.sessionID), logger: a.logger.withContext(ctx)}
	if err := a.client.beginSessionOperation(active); err != nil {
		return modelChangeForAdmissionError(err)
	}
	defer a.client.endSessionOperation(active)

	// Re-read after admission so validation cannot race a prompt update or
	// another mutation.
	cfg, err = a.resolveSessionConfigForSession(sess)
	if err != nil || !authoritativeControl(cfg.control, a.attachmentGeneration) {
		return ModelChange{Outcome: ModelChangeFailed, Code: "stale_snapshot"}
	}
	model = strings.TrimSpace(model)
	if !cfg.control.hasModel(model) {
		return ModelChange{Outcome: ModelChangeIncompatible, Code: "unknown_model"}
	}
	if err := ctx.Err(); err != nil {
		return ModelChange{Outcome: ModelChangeFailed, Code: "canceled"}
	}

	resp, err := a.client.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(cfg.sessionID),
			ConfigId:  acp.SessionConfigId(cfg.control.modelOptionID),
			Value:     acp.SessionConfigValueId(model),
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			return ModelChange{Outcome: ModelChangeAmbiguous, Code: "response_uncertain"}
		}
		if isACPMethodNotFoundError(err) {
			return ModelChange{Outcome: ModelChangeUnsupported, Code: "method_unsupported"}
		}
		var requestErr *acp.RequestError
		if errors.As(err, &requestErr) {
			// ACP cancellation can be synthesized locally after the request was
			// written, so it does not prove that the server skipped the mutation.
			if requestErr.Code == -32800 {
				return ModelChange{Outcome: ModelChangeAmbiguous, Code: "response_uncertain"}
			}
			if requestErr.Code == -32600 || requestErr.Code == -32602 {
				return ModelChange{Outcome: ModelChangeRejected, Code: "request_rejected"}
			}
		}
		return ModelChange{Outcome: ModelChangeAmbiguous, Code: "response_uncertain"}
	}

	control := newSessionControlState(resp.ConfigOptions, a.attachmentGeneration)
	if control == nil {
		return ModelChange{Outcome: ModelChangeAmbiguous, Code: "invalid_response"}
	}
	cfg.configValues = mergeSessionConfigValues(cfg.configValues, []SessionConfigValue{SelectSessionConfigValue(control.modelOptionID, control.currentModel)})
	if err := a.persistControlState(sess, cfg, control); err != nil {
		return ModelChange{Outcome: ModelChangeAmbiguous, Code: "state_write_failed"}
	}
	snapshot := control.snapshot(SessionControlAvailable, "")
	if control.currentModel != model {
		return ModelChange{Outcome: ModelChangeRejected, EffectiveModel: control.currentModel, Snapshot: snapshot, Code: "not_applied"}
	}
	return ModelChange{Outcome: ModelChangeApplied, EffectiveModel: model, Snapshot: snapshot}
}

func (a *Agent) persistControlState(sess session.Session, cfg acpSessionConfig, control *sessionControlState) error {
	state := buildACPStateWithControl(cfg.sessionID, cfg.metaJSON, cfg.configValues, control)
	state = a.ownedState(sess, state)
	if err := sess.State().Set(SessionStateKey, state); err != nil {
		return err
	}
	return nil
}

func newSessionControlState(options []acp.SessionConfigOption, generation string) *sessionControlState {
	var selected *acp.SessionConfigOptionSelect
	for _, option := range options {
		if option.Select == nil || option.Select.Category == nil || *option.Select.Category != acp.SessionConfigOptionCategoryModel {
			continue
		}
		if selected != nil {
			return nil
		}
		selected = option.Select
	}
	if selected == nil || !boundedControlString(string(selected.Id)) || !boundedControlString(string(selected.CurrentValue)) {
		return nil
	}
	models := flattenSessionModels(selected.Options)
	if len(models) == 0 || len(models) > maxControlModels {
		return nil
	}
	current := strings.TrimSpace(string(selected.CurrentValue))
	state := &sessionControlState{
		generation:    generation,
		modelOptionID: strings.TrimSpace(string(selected.Id)),
		currentModel:  current,
		models:        models,
	}
	if !state.hasModel(current) {
		return nil
	}
	state.revision = controlRevision(state)
	return state
}

func flattenSessionModels(options acp.SessionConfigSelectOptions) []SessionModel {
	var raw []acp.SessionConfigSelectOption
	if options.Ungrouped != nil {
		if len(*options.Ungrouped) > maxControlModels {
			return nil
		}
		raw = append(raw, (*options.Ungrouped)...)
	}
	if options.Grouped != nil {
		for _, group := range *options.Grouped {
			if len(group.Options) > maxControlModels-len(raw) {
				return nil
			}
			raw = append(raw, group.Options...)
		}
	}
	models := make([]SessionModel, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, option := range raw {
		id := strings.TrimSpace(string(option.Value))
		label := strings.TrimSpace(option.Name)
		if !boundedControlString(id) || !boundedControlString(label) {
			return nil
		}
		if _, ok := seen[id]; ok {
			return nil
		}
		seen[id] = struct{}{}
		models = append(models, SessionModel{ID: id, Label: label})
	}
	return models
}

func boundedControlString(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maxControlStringBytes
}

func controlRevision(state *sessionControlState) string {
	encoded, _ := json.Marshal(struct {
		Option  string
		Current string
		Models  []SessionModel
	}{state.modelOptionID, state.currentModel, state.models})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

func authoritativeControl(state *sessionControlState, generation string) bool {
	return state != nil && generation != "" && state.generation == generation && state.modelOptionID != "" && len(state.models) > 0
}

func (s *sessionControlState) hasModel(model string) bool {
	if s == nil {
		return false
	}
	for _, candidate := range s.models {
		if candidate.ID == model {
			return true
		}
	}
	return false
}

func (s *sessionControlState) snapshot(status SessionControlStatus, code string) SessionControlSnapshot {
	if s == nil {
		return unavailableControl(status, code)
	}
	return SessionControlSnapshot{
		Status:       status,
		Revision:     s.revision,
		CurrentModel: s.currentModel,
		Models:       append([]SessionModel(nil), s.models...),
		Code:         code,
	}
}

func unavailableControl(status SessionControlStatus, code string) SessionControlSnapshot {
	return SessionControlSnapshot{Status: status, Code: code}
}

func controlSnapshotForAdmissionError(err error) SessionControlSnapshot {
	if errors.Is(err, ErrSessionOperationActive) {
		return unavailableControl(SessionControlBusy, "busy")
	}
	if errors.Is(err, ErrClientClosed) {
		return unavailableControl(SessionControlClosed, "closed")
	}
	return unavailableControl(SessionControlFailed, "admission_failed")
}

func modelChangeForAdmissionError(err error) ModelChange {
	if errors.Is(err, ErrSessionOperationActive) {
		return ModelChange{Outcome: ModelChangeBusy, Code: "busy"}
	}
	if errors.Is(err, ErrClientClosed) {
		return ModelChange{Outcome: ModelChangeClosed, Code: "closed"}
	}
	return ModelChange{Outcome: ModelChangeFailed, Code: "admission_failed"}
}

func modelChangeFromValidation(validation ModelValidation) ModelChange {
	change := ModelChange{Snapshot: validation.Snapshot, Code: validation.Code}
	switch validation.Outcome {
	case ModelValidationUnsupported:
		change.Outcome = ModelChangeUnsupported
	case ModelValidationIncompatible:
		change.Outcome = ModelChangeIncompatible
	case ModelValidationBusy:
		change.Outcome = ModelChangeBusy
	case ModelValidationClosed:
		change.Outcome = ModelChangeClosed
	default:
		change.Outcome = ModelChangeFailed
	}
	return change
}

func buildACPStateWithControl(remoteSessionID, metaJSON string, configValues []SessionConfigValue, control *sessionControlState) map[string]any {
	state := buildACPStateWithConfigValues(remoteSessionID, metaJSON, configValues)
	if control != nil {
		state[controlStateField] = control.toState()
	}
	return state
}

func (s *sessionControlState) toState() map[string]any {
	models := make([]map[string]any, 0, len(s.models))
	for _, model := range s.models {
		models = append(models, map[string]any{"id": model.ID, "label": model.Label})
	}
	return map[string]any{
		"generation":      s.generation,
		"revision":        s.revision,
		"model_option_id": s.modelOptionID,
		"current_model":   s.currentModel,
		"models":          models,
	}
}

func parseSessionControlState(raw any) *sessionControlState {
	state, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	generation, _ := state["generation"].(string)
	optionID, _ := state["model_option_id"].(string)
	current, _ := state["current_model"].(string)
	if !boundedControlString(generation) || !boundedControlString(optionID) || !boundedControlString(current) {
		return nil
	}
	models := parseSessionModels(state["models"])
	if len(models) == 0 || len(models) > maxControlModels {
		return nil
	}
	control := &sessionControlState{generation: generation, modelOptionID: optionID, currentModel: current, models: models}
	if !control.hasModel(current) {
		return nil
	}
	control.revision = controlRevision(control)
	return control
}

func parseSessionModels(raw any) []SessionModel {
	var entries []map[string]any
	switch values := raw.(type) {
	case []map[string]any:
		entries = values
	case []any:
		entries = make([]map[string]any, 0, len(values))
		for _, value := range values {
			entry, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			entries = append(entries, entry)
		}
	default:
		return nil
	}
	models := make([]SessionModel, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		id, _ := entry["id"].(string)
		label, _ := entry["label"].(string)
		if !boundedControlString(id) || !boundedControlString(label) {
			return nil
		}
		if _, ok := seen[id]; ok {
			return nil
		}
		seen[id] = struct{}{}
		models = append(models, SessionModel{ID: id, Label: label})
	}
	return models
}
