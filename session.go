package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

type acpSessionConfig struct {
	sessionID    string
	configValues []SessionConfigValue
	cwd          string
	meta         map[string]any
	metaJSON     string
}

type remoteSession struct {
	id                      string
	configValues            []SessionConfigValue
	metaJSON                string
	fresh                   bool
	firstPromptInstructions string
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

func (a *Agent) persistSessionStateDelta(event *session.Event, remoteSessionID, metaJSON string, configValues []SessionConfigValue) {
	if event == nil || event.Partial || strings.TrimSpace(remoteSessionID) == "" {
		return
	}

	if event.Actions.StateDelta == nil {
		event.Actions.StateDelta = make(map[string]any)
	}
	event.Actions.StateDelta[SessionStateKey] = buildACPStateWithConfigValues(remoteSessionID, metaJSON, configValues)
}

func (a *Agent) ensureRemoteSession(ctx adkagent.InvocationContext, logCtx context.Context, logger logger) (remoteSession, error) {
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil {
		return remoteSession{}, err
	}

	if cfg.sessionID != "" {
		logger.Debug().
			Str("acp_session_id", cfg.sessionID).
			Msg("using acp session id from adk session state")
		configValues, err := a.client.applySessionConfig(
			logCtx,
			cfg.sessionID,
			cfg.configValues,
			nil,
			nil,
		)
		if err != nil {
			return remoteSession{}, err
		}
		if len(configValues) > 0 {
			cfg.configValues = configValues
		}
		if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.configValues); err != nil {
			return remoteSession{}, err
		}
		return remoteSession{id: cfg.sessionID, configValues: cfg.configValues, metaJSON: cfg.metaJSON}, nil
	}
	instructions, err := a.resolveInstructionParts(ctx)
	if err != nil {
		return remoteSession{}, err
	}
	cfg, err = addInstructionMetaToSessionConfig(cfg, instructions)
	if err != nil {
		return remoteSession{}, err
	}
	return a.createRemoteSession(ctx, logCtx, logger, cfg, instructions.combined())
}

func (a *Agent) recoverRemoteSession(
	ctx adkagent.InvocationContext,
	logCtx context.Context,
	logger logger,
	promptErr error,
) (remoteSession, error) {
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil {
		return remoteSession{}, err
	}
	if cfg.sessionID != "" && a.client.SupportsSessionResume() {
		resumeResp, err := a.client.ResumeSessionWithMeta(logCtx, cfg.sessionID, cfg.cwd, a.mcpServers, cfg.meta)
		if err == nil {
			configValues, err := a.client.applySessionConfig(
				logCtx,
				cfg.sessionID,
				cfg.configValues,
				resumeResp.ConfigOptions,
				resumeResp.Modes,
			)
			if err != nil {
				return remoteSession{}, err
			}
			if len(configValues) > 0 {
				cfg.configValues = configValues
			}
			a.logBoundRemoteSession(logger, "resumed acp session after prompt failure", cfg.sessionID, cfg.cwd, cfg.metaJSON)
			if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.configValues); err != nil {
				return remoteSession{}, err
			}
			return remoteSession{id: cfg.sessionID, configValues: cfg.configValues, metaJSON: cfg.metaJSON}, nil
		}
		if isACPSessionAlreadyExistsError(err) {
			logger.Debug().
				Err(err).
				Str("acp_session_id", cfg.sessionID).
				Msg("acp session already active after prompt failure")
			if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.configValues); err != nil {
				return remoteSession{}, err
			}
			return remoteSession{id: cfg.sessionID, configValues: cfg.configValues, metaJSON: cfg.metaJSON}, nil
		}
		if !isACPSessionNotFoundError(err) {
			return remoteSession{}, fmt.Errorf("resume acp session %q after prompt failure: %w", cfg.sessionID, err)
		}
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
	recovered, err := a.createRemoteSession(ctx, logCtx, logger, cfg, instructions.combined())
	if err != nil {
		return remoteSession{}, fmt.Errorf("recover acp session after prompt failure %q: %w", promptErr, err)
	}
	return recovered, nil
}

func (a *Agent) createRemoteSession(
	ctx adkagent.InvocationContext,
	logCtx context.Context,
	logger logger,
	cfg acpSessionConfig,
	firstPromptInstructions string,
) (remoteSession, error) {
	resp, err := a.client.NewSessionWithMeta(logCtx, cfg.cwd, a.mcpServers, cfg.meta)
	if err != nil {
		return remoteSession{}, err
	}
	sessionID := string(resp.SessionId)
	configValues, err := a.client.applySessionConfig(
		logCtx,
		sessionID,
		cfg.configValues,
		resp.ConfigOptions,
		resp.Modes,
	)
	if err != nil {
		return remoteSession{}, err
	}
	if len(configValues) > 0 {
		cfg.configValues = configValues
	}
	a.logBoundRemoteSession(logger, "created new acp session for adk session", sessionID, cfg.cwd, cfg.metaJSON)
	if err := a.persistRemoteSessionBinding(ctx, sessionID, cfg.metaJSON, cfg.configValues); err != nil {
		return remoteSession{}, err
	}
	return remoteSession{
		id:                      sessionID,
		configValues:            cfg.configValues,
		metaJSON:                cfg.metaJSON,
		fresh:                   true,
		firstPromptInstructions: strings.TrimSpace(firstPromptInstructions),
	}, nil
}

func (a *Agent) logBoundRemoteSession(
	logger logger,
	message string,
	remoteSessionID string,
	cwd string,
	metaJSON string,
) {
	event := logger.Debug().
		Str("acp_session_id", remoteSessionID).
		Str("cwd", cwd).
		RawJSON("meta", []byte(metaJSON))
	if len(a.sessionConfig) > 0 {
		event = event.Int("session_config_values", len(a.sessionConfig))
	}
	event.Msg(message)
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

func (a *Agent) persistRemoteSessionBinding(
	ctx adkagent.InvocationContext,
	remoteSessionID string,
	metaJSON string,
	configValues []SessionConfigValue,
) error {
	if ctx == nil || ctx.Session() == nil || strings.TrimSpace(remoteSessionID) == "" {
		return nil
	}

	acpState := buildACPStateWithConfigValues(remoteSessionID, metaJSON, configValues)
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
	cfg := acpSessionConfig{
		cwd:          strings.TrimSpace(a.workingDir),
		configValues: cloneSessionConfigValues(a.sessionConfig),
	}
	rawCWD, err := ctx.Session().State().Get(CWDStateKey)
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

	rawState, err := ctx.Session().State().Get(SessionStateKey)
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
		cfg.configValues = mergeSessionConfigValues(cfg.configValues, configValues)
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
