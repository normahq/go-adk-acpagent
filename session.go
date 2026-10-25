package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// SessionConfigValue is an ACP session configuration value to apply to each
// bound ACP session.
type SessionConfigValue struct {
	// ID is the ACP session config option ID.
	ID string `json:"id"`
	// Value is the ACP session config value ID for select options.
	Value string `json:"value"`
	// BoolValue is the ACP session config value for boolean options.
	BoolValue *bool `json:"bool_value,omitempty"`
}

// SelectSessionConfigValue returns an ACP select session config value.
func SelectSessionConfigValue(id, value string) SessionConfigValue {
	return SessionConfigValue{ID: id, Value: value}
}

// BooleanSessionConfigValue returns an ACP boolean session config value.
func BooleanSessionConfigValue(id string, value bool) SessionConfigValue {
	return SessionConfigValue{ID: id, BoolValue: &value}
}

// ErrSessionAttachmentIndeterminate means a canceled session/new may have
// created a remote session. Reset the ADK session before attaching again.
var ErrSessionAttachmentIndeterminate = errors.New("acp session attachment outcome is indeterminate; reset the session")

type acpSessionConfig struct {
	attachmentIndeterminate bool
	sessionID               string
	configValues            []SessionConfigValue
	desiredConfigValues     []SessionConfigValue
	cwd                     string
	meta                    map[string]any
	metaJSON                string
	control                 *sessionControlState
}

type remoteSession struct {
	id                      string
	configValues            []SessionConfigValue
	metaJSON                string
	fresh                   bool
	firstPromptInstructions string
	configError             error
	control                 *sessionControlState
}

func (r *remoteSession) applyPromptControl(control *sessionControlState) {
	if r == nil || control == nil {
		return
	}
	r.control = control
	r.configValues = mergeSessionConfigValues(r.configValues, []SessionConfigValue{
		SelectSessionConfigValue(control.modelOptionID, control.currentModel),
	})
}

const (
	// SessionStateKey is the reserved ADK session-state key for ACP-specific
	// per-session settings.
	//
	// The value at this key must be an object with optional fields:
	//   - "meta" (object): forwarded to ACP session/new._meta
	//   - "session_id" (string): canonical ACP session id returned by the ACP
	//     agent and used for ACP session/resume
	//
	// The agent also records itself under "owner" and keeps the sessions of
	// other agents that shared this ADK session under "parked". An agent never
	// resumes a session id that another agent created; it resumes the one parked
	// for it, or starts a new session. A state without "owner" belongs to the
	// current agent.
	//
	// This state is the source of truth for ACP session identity. If the ADK
	// session is deleted, this state is deleted with it and no in-memory ACP
	// session binding is reused.
	SessionStateKey = "acp_session"
	// PlanStateKey is the ADK session-state key used for ACP plan snapshots.
	//
	// Each ACP session/update.plan notification is projected into
	// event.Actions.StateDelta[PlanStateKey] as the authoritative full plan
	// replacement snapshot.
	PlanStateKey = "acp_plan"
	// CWDStateKey is the ADK session-state key used to override the ACP
	// session working directory for a single ADK session.
	CWDStateKey = "cwd"
)

func (a *Agent) maybeSaveOutputToState(event *session.Event, output string) {
	if a.outputKey == "" || event == nil || event.Partial || output == "" {
		return
	}
	if event.Actions.StateDelta == nil {
		event.Actions.StateDelta = make(map[string]any)
	}
	event.Actions.StateDelta[a.outputKey] = output
}

func (a *Agent) persistSessionStateDelta(event *session.Event, remoteSessionID, metaJSON string, configValues []SessionConfigValue, controls ...*sessionControlState) {
	if event == nil || event.Partial || strings.TrimSpace(remoteSessionID) == "" {
		return
	}

	if event.Actions.StateDelta == nil {
		event.Actions.StateDelta = make(map[string]any)
	}
	var control *sessionControlState
	if len(controls) > 0 {
		control = controls[0]
	}
	event.Actions.StateDelta[SessionStateKey] = buildACPStateWithControl(remoteSessionID, metaJSON, configValues, control)
}

func (a *Agent) ensureRemoteSession(ctx adkagent.InvocationContext, logCtx context.Context, logger logger, yield func(*session.Event, error) bool) (remoteSession, error) {
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil {
		return remoteSession{}, err
	}

	if cfg.attachmentIndeterminate {
		return remoteSession{}, ErrSessionAttachmentIndeterminate
	}
	if cfg.sessionID != "" {
		if cfg.control != nil && !authoritativeControl(cfg.control, a.attachmentGeneration) && a.client.SupportsSessionResume() {
			// Refresh an existing persisted attachment before the first prompt in
			// this process. Prompt success alone does not make old descriptors
			// authoritative.
			snapshot := a.SessionControl().Inspect(logCtx, ctx.Session())
			if snapshot.Code == "unknown_session" {
				cfg.sessionID = ""
				cfg.configValues = nil
				cfg.control = nil
			} else {
				cfg, err = a.resolveSessionConfig(ctx)
				if err != nil {
					return remoteSession{}, err
				}
			}
		}
		if cfg.sessionID != "" {
			logger.Debug().
				Str("acp_session_id", cfg.sessionID).
				Msg("using acp session id from adk session state")
			if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.configValues, cfg.control); err != nil {
				return remoteSession{}, err
			}
			return remoteSession{id: cfg.sessionID, configValues: cfg.configValues, metaJSON: cfg.metaJSON, control: cfg.control}, nil
		}
	}
	instructions, err := a.resolveInstructionParts(ctx)
	if err != nil {
		return remoteSession{}, err
	}
	cfg, err = addInstructionMetaToSessionConfig(cfg, instructions)
	if err != nil {
		return remoteSession{}, err
	}
	return a.createRemoteSession(ctx, logCtx, logger, cfg, instructions.combined(), yield)
}

func (a *Agent) recoverRemoteSession(ctx adkagent.InvocationContext, logCtx context.Context, logger logger, promptErr error, yield func(*session.Event, error) bool) (remoteSession, error) {
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil {
		return remoteSession{}, err
	}
	if cfg.sessionID != "" && a.client.SupportsSessionResume() {
		active := &activePrompt{sessionID: acp.SessionId(cfg.sessionID), logger: logger}
		if err := a.client.beginSessionOperation(active); err != nil {
			return remoteSession{}, err
		}
		defer a.client.endSessionOperation(active)
		setup, cancel := a.setupContext(logCtx)
		defer cancel()
		resumeResp, err := a.client.ResumeSessionWithMeta(setup, cfg.sessionID, cfg.cwd, a.mcpServers, cfg.meta)
		if err != nil && setup.Err() != nil {
			err = setup.Err()
		}
		if err == nil {
			configValues, finalOptions, configErr := a.client.applySessionConfigRequiredWithOptions(
				setup,
				cfg.sessionID,
				cfg.desiredConfigValues,
				resumeResp.ConfigOptions,
				resumeResp.Modes,
				cfg.desiredConfigValues,
			)
			if len(configValues) > 0 {
				cfg.configValues = configValues
			}
			control := newSessionControlState(finalOptions, a.attachmentGeneration)
			if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.configValues, control); err != nil {
				return remoteSession{}, err
			}
			remote := remoteSession{id: cfg.sessionID, configValues: cfg.configValues, metaJSON: cfg.metaJSON, control: control}
			if configErr != nil {
				if errors.Is(configErr, context.Canceled) || errors.Is(configErr, context.DeadlineExceeded) {
					return remoteSession{}, configErr
				}
				remote.configError = configErr
				return remote, nil
			}
			a.logBoundRemoteSession(logger, "resumed acp session after prompt failure", cfg.sessionID, cfg.cwd, cfg.metaJSON)
			return remote, nil
		}
		if isACPSessionAlreadyExistsError(err) {
			logger.Debug().
				Err(err).
				Str("acp_session_id", cfg.sessionID).
				Msg("acp session already active after prompt failure")
			if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.configValues, cfg.control); err != nil {
				return remoteSession{}, err
			}
			return remoteSession{id: cfg.sessionID, configValues: cfg.configValues, metaJSON: cfg.metaJSON, control: cfg.control}, nil
		}
		if !isACPResumeUnknownSessionError(err) {
			return remoteSession{}, fmt.Errorf("resume acp session %q after prompt failure: %w", cfg.sessionID, err)
		}
		// The fallback may reuse the same remote ID, so release the failed
		// attachment admission before session/new.
		a.client.endSessionOperation(active)
		logger.Warn().
			Err(err).
			Str("acp_session_id", cfg.sessionID).
			Msg("acp session resume unavailable after prompt failure; falling back to session/new")
	}
	instructions, err := a.resolveInstructionParts(ctx)
	if err != nil {
		return remoteSession{}, err
	}
	cfg, err = addInstructionMetaToSessionConfig(cfg, instructions)
	if err != nil {
		return remoteSession{}, err
	}
	recovered, err := a.createRemoteSession(ctx, logCtx, logger, cfg, instructions.combined(), yield)
	if err != nil {
		return remoteSession{}, fmt.Errorf("recover acp session after prompt failure %q: %w", promptErr, err)
	}
	return recovered, nil
}

func (a *Agent) createRemoteSession(ctx adkagent.InvocationContext, logCtx context.Context, logger logger, cfg acpSessionConfig, firstPromptInstructions string, yield func(*session.Event, error) bool) (remoteSession, error) {
	setup, cancel := a.setupContext(logCtx)
	defer cancel()
	if err := setup.Err(); err != nil {
		return remoteSession{}, err
	}
	// Persist the pending outcome before dispatch, while the invocation is live.
	// Cancellation may prevent the runner from persisting any later event.
	if a.protectAttachments {
		if err := a.persistAttachmentOutcome(ctx, cfg, true, yield); err != nil {
			return remoteSession{}, err
		}
	}
	resp, err := a.client.NewSessionWithMeta(setup, cfg.cwd, a.mcpServers, cfg.meta)
	if err != nil {
		if setup.Err() != nil {
			return remoteSession{}, setup.Err()
		}
		if a.protectAttachments {
			if stateErr := a.persistAttachmentOutcome(ctx, cfg, false, yield); stateErr != nil {
				return remoteSession{}, errors.Join(err, stateErr)
			}
		}
		return remoteSession{}, err
	}

	sessionID := string(resp.SessionId)
	active := &activePrompt{sessionID: acp.SessionId(sessionID), logger: logger}
	if err := a.client.beginSessionOperation(active); err != nil {
		return remoteSession{}, err
	}
	defer a.client.endSessionOperation(active)
	if a.protectAttachments {
		bound := cfg
		bound.sessionID = sessionID
		if err := a.persistAttachmentOutcome(ctx, bound, false, yield); err != nil {
			return remoteSession{}, err
		}
	}
	valuesToApply := mergeSessionConfigValues(cfg.configValues, cfg.desiredConfigValues)
	configValues, finalOptions, configErr := a.client.applySessionConfigRequiredWithOptions(
		setup,
		sessionID,
		valuesToApply,
		resp.ConfigOptions,
		resp.Modes,
		cfg.desiredConfigValues,
	)
	if len(configValues) > 0 {
		cfg.configValues = configValues
	}
	control := newSessionControlState(finalOptions, a.attachmentGeneration)
	if err := a.persistRemoteSessionBinding(ctx, sessionID, cfg.metaJSON, cfg.configValues, control); err != nil {
		return remoteSession{}, err
	}
	remote := remoteSession{
		id:                      sessionID,
		configValues:            cfg.configValues,
		metaJSON:                cfg.metaJSON,
		fresh:                   true,
		firstPromptInstructions: strings.TrimSpace(firstPromptInstructions),
		control:                 control,
	}
	if configErr != nil {
		if errors.Is(configErr, context.Canceled) || errors.Is(configErr, context.DeadlineExceeded) {
			return remoteSession{}, configErr
		}
		remote.configError = configErr
		return remote, nil
	}
	a.logBoundRemoteSession(logger, "created new acp session for adk session", sessionID, cfg.cwd, cfg.metaJSON)
	return remote, nil
}

func (a *Agent) logBoundRemoteSession(logger logger, message, remoteSessionID, cwd, metaJSON string) {
	event := logger.Debug().
		Str("acp_session_id", remoteSessionID).
		Str("cwd", cwd).
		Bool("has_meta", strings.TrimSpace(metaJSON) != "" && strings.TrimSpace(metaJSON) != "{}")
	if len(a.sessionConfig) > 0 {
		event = event.Int("session_config_values", len(a.sessionConfig))
	}
	event.Msg(message)
	if logger.enabled(levelTrace) {
		logger.Trace().
			Str("acp_session_id", remoteSessionID).
			Int("meta_bytes", len(metaJSON)).
			Msg(message + " metadata")
	}
}

func buildACPState(remoteSessionID, metaJSON string) map[string]any {
	return buildACPStateWithConfigValues(remoteSessionID, metaJSON, nil)
}

func buildACPStateWithConfigValues(remoteSessionID, metaJSON string, configValues []SessionConfigValue) map[string]any {
	acpState := map[string]any{
		"session_id": remoteSessionID,
	}
	if normalized := normalizeSessionConfigValues(configValues); len(normalized) > 0 {
		acpState["config_values"] = sessionConfigValuesToState(normalized)
	}
	if strings.TrimSpace(metaJSON) == "" || metaJSON == "{}" {
		return acpState
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(metaJSON), &meta); err == nil && len(meta) > 0 {
		acpState["meta"] = meta
	}
	return acpState
}

func normalizeSessionConfigValues(values []SessionConfigValue) []SessionConfigValue {
	normalized := make([]SessionConfigValue, 0, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value.ID)
		if id == "" {
			continue
		}
		if value.BoolValue != nil {
			boolValue := *value.BoolValue
			normalized = append(normalized, BooleanSessionConfigValue(id, boolValue))
			continue
		}
		configValue := strings.TrimSpace(value.Value)
		if configValue == "" {
			continue
		}
		normalized = append(normalized, SelectSessionConfigValue(id, configValue))
	}
	return normalized
}

func cloneSessionConfigValues(values []SessionConfigValue) []SessionConfigValue {
	return append([]SessionConfigValue(nil), normalizeSessionConfigValues(values)...)
}

func mergeSessionConfigValues(defaults, overrides []SessionConfigValue) []SessionConfigValue {
	merged := cloneSessionConfigValues(defaults)
	indexByID := make(map[string]int, len(merged))
	for i, value := range merged {
		indexByID[value.ID] = i
	}
	for _, value := range normalizeSessionConfigValues(overrides) {
		if index, ok := indexByID[value.ID]; ok {
			merged[index] = value
			continue
		}
		indexByID[value.ID] = len(merged)
		merged = append(merged, value)
	}
	return merged
}

func sessionConfigValuesToState(values []SessionConfigValue) []map[string]any {
	normalized := normalizeSessionConfigValues(values)
	stateValues := make([]map[string]any, 0, len(normalized))
	for _, value := range normalized {
		stateValue := map[string]any{"id": value.ID}
		if value.BoolValue != nil {
			stateValue["type"] = "boolean"
			stateValue["value"] = *value.BoolValue
		} else {
			stateValue["value"] = value.Value
		}
		stateValues = append(stateValues, stateValue)
	}
	return stateValues
}

func parseSessionConfigValues(raw any) ([]SessionConfigValue, error) {
	switch values := raw.(type) {
	case []SessionConfigValue:
		return normalizeSessionConfigValues(values), nil
	case []map[string]string:
		parsed := make([]SessionConfigValue, 0, len(values))
		for _, value := range values {
			parsed = append(parsed, SessionConfigValue{ID: value["id"], Value: value["value"]})
		}
		return normalizeSessionConfigValues(parsed), nil
	case []map[string]any:
		return parseAnySessionConfigValueMaps(values)
	case []any:
		parsed := make([]map[string]any, 0, len(values))
		for _, rawValue := range values {
			value, ok := rawValue.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("must be an array of objects; got element %T", rawValue)
			}
			parsed = append(parsed, value)
		}
		return parseAnySessionConfigValueMaps(parsed)
	default:
		return nil, fmt.Errorf("must be an array; got %T", raw)
	}
}

func parseAnySessionConfigValueMaps(values []map[string]any) ([]SessionConfigValue, error) {
	parsed := make([]SessionConfigValue, 0, len(values))
	for _, value := range values {
		rawID, ok := value["id"]
		if !ok {
			return nil, errors.New(`entry missing "id"`)
		}
		id, ok := rawID.(string)
		if !ok {
			return nil, fmt.Errorf(`entry "id" must be a string; got %T`, rawID)
		}
		rawValue, ok := value["value"]
		if !ok {
			return nil, errors.New(`entry missing "value"`)
		}
		if configType, _ := value["type"].(string); configType == "boolean" {
			configValue, ok := rawValue.(bool)
			if !ok {
				return nil, fmt.Errorf(`entry "value" must be a boolean; got %T`, rawValue)
			}
			parsed = append(parsed, BooleanSessionConfigValue(id, configValue))
			continue
		}
		configValue, ok := rawValue.(string)
		if !ok {
			return nil, fmt.Errorf(`entry "value" must be a string; got %T`, rawValue)
		}
		parsed = append(parsed, SelectSessionConfigValue(id, configValue))
	}
	return normalizeSessionConfigValues(parsed), nil
}

func (a *Agent) persistRemoteSessionBinding(ctx adkagent.InvocationContext, remoteSessionID, metaJSON string, configValues []SessionConfigValue, controls ...*sessionControlState) error {
	if ctx == nil || ctx.Session() == nil || strings.TrimSpace(remoteSessionID) == "" {
		return nil
	}

	var control *sessionControlState
	if len(controls) > 0 {
		control = controls[0]
	}
	acpState := a.ownedState(ctx.Session(), buildACPStateWithControl(remoteSessionID, metaJSON, configValues, control))
	if currentACPStateMatches(ctx.Session(), remoteSessionID, metaJSON) {
		if err := ctx.Session().State().Set(SessionStateKey, cloneAnyMap(acpState)); err != nil {
			return fmt.Errorf("set live acp session state: %w", err)
		}
		return nil
	}

	if err := ctx.Session().State().Set(SessionStateKey, cloneAnyMap(acpState)); err != nil {
		return fmt.Errorf("set live acp session state: %w", err)
	}
	return nil
}

func currentACPStateMatches(sess session.Session, remoteSessionID, metaJSON string) bool {
	if sess == nil {
		return false
	}
	rawState, err := sess.State().Get(SessionStateKey)
	if err != nil {
		return false
	}
	state, ok := rawState.(map[string]any)
	if !ok {
		return false
	}
	storedSessionID, _ := state["session_id"].(string)
	if strings.TrimSpace(storedSessionID) != strings.TrimSpace(remoteSessionID) {
		return false
	}
	return normalizeACPStateMetaJSON(state["meta"]) == normalizeACPStateMetaJSONFromRaw(metaJSON)
}

func normalizeACPStateMetaJSON(raw any) string {
	if raw == nil {
		return "{}"
	}
	switch value := raw.(type) {
	case map[string]any:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "{}"
		}
		return string(encoded)
	default:
		return "{}"
	}
}

func normalizeACPStateMetaJSONFromRaw(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return "{}"
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(trimmed), &meta); err != nil {
		return "{}"
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func (a *Agent) resolveSessionConfig(ctx adkagent.InvocationContext) (acpSessionConfig, error) {
	if ctx == nil {
		return acpSessionConfig{}, errors.New("adk invocation context is required")
	}
	return a.resolveSessionConfigForSession(ctx.Session())
}

func (a *Agent) resolveSessionConfigForSession(sess session.Session) (acpSessionConfig, error) {
	if sess == nil {
		return acpSessionConfig{}, errors.New("adk session is required")
	}
	cfg := acpSessionConfig{
		cwd:                 strings.TrimSpace(a.workingDir),
		desiredConfigValues: cloneSessionConfigValues(a.sessionConfig),
	}
	rawCWD, err := sess.State().Get(CWDStateKey)
	if err != nil {
		if !errors.Is(err, session.ErrStateKeyNotExist) {
			return acpSessionConfig{}, fmt.Errorf("read %q from adk session state: %w", CWDStateKey, err)
		}
	} else {
		cwd, ok := rawCWD.(string)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("adk session state %q must be a string; got %T", CWDStateKey, rawCWD)
		}
		cfg.cwd = strings.TrimSpace(cwd)
	}

	rawState, err := sess.State().Get(SessionStateKey)
	if err != nil {
		if errors.Is(err, session.ErrStateKeyNotExist) {
			cfg, cfgErr := addReasoningEffortToSessionConfig(cfg, a.reasoningEffort)
			if cfgErr != nil {
				return acpSessionConfig{}, cfgErr
			}
			return normalizeACPConfigCWD(cfg)
		}
		return acpSessionConfig{}, fmt.Errorf("read %q from adk session state: %w", SessionStateKey, err)
	}

	state, ok := rawState.(map[string]any)
	if !ok {
		return acpSessionConfig{}, fmt.Errorf("adk session state %q must be an object; got %T", SessionStateKey, rawState)
	}
	cfg.attachmentIndeterminate, _ = state["attachment_indeterminate"].(bool)
	if rawMeta, ok := state["meta"]; ok {
		meta, ok := rawMeta.(map[string]any)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("adk session state %q.meta must be an object; got %T", SessionStateKey, rawMeta)
		}
		cfg.meta = cloneAnyMap(meta)
	}
	if rawSessionID, ok := state["session_id"]; ok {
		sessionID, ok := rawSessionID.(string)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("adk session state %q.session_id must be a string; got %T", SessionStateKey, rawSessionID)
		}
		cfg.sessionID = strings.TrimSpace(sessionID)
	}
	if rawConfigValues, ok := state["config_values"]; ok {
		configValues, err := parseSessionConfigValues(rawConfigValues)
		if err != nil {
			return acpSessionConfig{}, fmt.Errorf("adk session state %q.config_values: %w", SessionStateKey, err)
		}
		cfg.configValues = configValues
	}
	cfg.control = parseSessionControlState(state[controlStateField])
	cfg, err = a.applyForeignOwnership(cfg, state)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("adk session state %q ownership: %w", SessionStateKey, err)
	}
	cfg, err = addReasoningEffortToSessionConfig(cfg, a.reasoningEffort)
	if err != nil {
		return acpSessionConfig{}, err
	}
	return normalizeACPConfigCWD(cfg)
}

func normalizeACPConfigCWD(cfg acpSessionConfig) (acpSessionConfig, error) {
	if cfg.meta == nil {
		cfg.metaJSON = "{}"
	} else {
		metaJSON, err := json.Marshal(cfg.meta)
		if err != nil {
			return acpSessionConfig{}, fmt.Errorf("marshal acp session meta: %w", err)
		}
		cfg.metaJSON = string(metaJSON)
	}

	if cfg.cwd == "" {
		return acpSessionConfig{}, fmt.Errorf("acp session cwd is empty")
	}
	absCWD, err := filepath.Abs(cfg.cwd)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("resolve acp session cwd %q: %w", cfg.cwd, err)
	}
	info, err := os.Stat(absCWD)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("stat acp session cwd %q: %w", absCWD, err)
	}
	if !info.IsDir() {
		return acpSessionConfig{}, fmt.Errorf("acp session cwd %q is not a directory", absCWD)
	}
	cfg.cwd = absCWD
	return cfg, nil
}

func (a *Agent) persistAttachmentOutcome(ctx adkagent.InvocationContext, cfg acpSessionConfig, pending bool, yield func(*session.Event, error) bool) error {
	state := a.ownedState(ctx.Session(), buildACPStateWithConfigValues(cfg.sessionID, cfg.metaJSON, cfg.configValues))
	if pending {
		state["attachment_indeterminate"] = true
	}
	if err := ctx.Session().State().Set(SessionStateKey, state); err != nil {
		return fmt.Errorf("persist attachment outcome: %w", err)
	}
	if yield != nil {
		event := session.NewEvent(ctx, ctx.InvocationID())
		event.Actions.StateDelta[SessionStateKey] = state
		if !yield(event, nil) {
			return context.Canceled
		}
	}
	return nil
}
