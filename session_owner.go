package acpagent

import (
	"strings"

	"google.golang.org/adk/v2/session"
)

const (
	// ownerStateField names the agent that created the ACP session id stored
	// under [SessionStateKey].
	ownerStateField = "owner"
	// parkedStateField holds the sessions of other agents that shared this ADK
	// session, keyed by agent name, so an agent that takes over again resumes
	// its own ACP session instead of a foreign one.
	parkedStateField = "parked"
)

// ownerID identifies this agent in the shared ACP session state.
func (a *Agent) ownerID() string {
	if a == nil || a.Agent == nil {
		return ""
	}
	return strings.TrimSpace(a.Name())
}

// ownedState stamps state with this agent as the owner of its session id and
// carries the sessions of other agents forward as parked slots. A session id
// written without an owner predates ownership and belongs to the current agent,
// so existing sessions keep working.
func (a *Agent) ownedState(sess session.Session, state map[string]any) map[string]any {
	owner := a.ownerID()
	if owner == "" || state == nil {
		return state
	}
	parked := map[string]any{}
	if current := liveACPState(sess); current != nil {
		if rawParked, ok := current[parkedStateField].(map[string]any); ok {
			for name, slot := range rawParked {
				parked[name] = slot
			}
		}
		currentOwner, _ := current[ownerStateField].(string)
		currentID, _ := current["session_id"].(string)
		if currentOwner != "" && currentOwner != owner && strings.TrimSpace(currentID) != "" {
			slot := map[string]any{"session_id": currentID}
			if values, ok := current["config_values"]; ok {
				slot["config_values"] = values
			}
			parked[currentOwner] = slot
		}
	}
	delete(parked, owner)
	state[ownerStateField] = owner
	if len(parked) > 0 {
		state[parkedStateField] = parked
	} else {
		delete(state, parkedStateField)
	}
	return state
}

func liveACPState(sess session.Session) map[string]any {
	if sess == nil {
		return nil
	}
	raw, err := sess.State().Get(SessionStateKey)
	if err != nil {
		return nil
	}
	state, _ := raw.(map[string]any)
	return state
}

// applyForeignOwnership drops the session id of another agent from cfg. The
// ACP session id and its config values only make sense to the agent that
// created them; the meta stays shared. A slot parked for this agent is
// restored.
func (a *Agent) applyForeignOwnership(cfg acpSessionConfig, state map[string]any) (acpSessionConfig, error) {
	owner, _ := state[ownerStateField].(string)
	if owner == "" || owner == a.ownerID() {
		return cfg, nil
	}
	cfg.sessionID = ""
	cfg.configValues = nil
	cfg.attachmentIndeterminate = false
	parked, _ := state[parkedStateField].(map[string]any)
	slot, _ := parked[a.ownerID()].(map[string]any)
	sessionID, _ := slot["session_id"].(string)
	if strings.TrimSpace(sessionID) == "" {
		return cfg, nil
	}
	cfg.sessionID = strings.TrimSpace(sessionID)
	if rawValues, ok := slot["config_values"]; ok {
		values, err := parseSessionConfigValues(rawValues)
		if err != nil {
			return acpSessionConfig{}, err
		}
		cfg.configValues = values
	}
	return cfg, nil
}

// stampOwnership stamps every ACP session state event with this agent as owner.
func (a *Agent) stampOwnership(ctx interface{ Session() session.Session }, yield func(*session.Event, error) bool) func(*session.Event, error) bool {
	return func(ev *session.Event, err error) bool {
		if ev != nil {
			if state, ok := ev.Actions.StateDelta[SessionStateKey].(map[string]any); ok {
				ev.Actions.StateDelta[SessionStateKey] = a.ownedState(ctx.Session(), state)
			}
		}
		return yield(ev, err)
	}
}
