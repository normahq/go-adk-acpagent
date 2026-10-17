package acpagent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

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
	// parkedAtField records when a session was parked, so the oldest parked
	// sessions can be dropped once there are more than maxParkedSessions.
	parkedAtField = "parked_at"
	// maxParkedSessions bounds the parked sessions kept in the state. Every
	// event carries the whole state, and an agent whose identity changes (a new
	// command, a rotated environment) would otherwise leave a slot behind each time.
	maxParkedSessions = 8
)

// parkNow reports the current time; tests replace it.
var parkNow = time.Now

// agentOwnerKey identifies an agent in the shared ACP session state by its name
// and the command and environment that start its backend. The name alone is not
// enough: agents that keep the default name would all look like the same owner,
// and two agents with one command but a different environment (another home
// directory, another credential) are different backends. Only a truncated hash
// is stored, never the command or the environment themselves. A changed command
// or environment is a different backend, so its stored session is not reused.
func agentOwnerKey(name string, command, environment []string) string {
	env := append([]string(nil), environment...)
	sort.Strings(env)
	input := strings.Join(command, "\x00") + "\x01" + strings.Join(env, "\x00")
	sum := sha256.Sum256([]byte(input))
	return strings.TrimSpace(name) + ":" + hex.EncodeToString(sum[:4])
}

// ownerID identifies this agent in the shared ACP session state.
func (a *Agent) ownerID() string {
	if a == nil {
		return ""
	}
	return a.ownerKey
}

// ownedState stamps state with this agent as the owner of its session id and
// carries the sessions of other agents forward as parked slots. A session id
// written without an owner predates ownership and belongs to the current agent,
// so existing sessions keep working.
func (a *Agent) ownedState(sess session.Session, state map[string]any) map[string]any {
	owner := a.ownerID()
	if owner == "" || len(state) == 0 {
		return state
	}
	state = cloneAnyMap(state)
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
			if indeterminate, _ := current["attachment_indeterminate"].(bool); indeterminate {
				slot["attachment_indeterminate"] = true
			}
			slot[parkedAtField] = parkNow().UTC().Format(time.RFC3339Nano)
			parked[currentOwner] = slot
		}
	}
	delete(parked, owner)
	dropOldestParked(parked, maxParkedSessions)
	state[ownerStateField] = owner
	if len(parked) > 0 {
		state[parkedStateField] = parked
	} else {
		delete(state, parkedStateField)
	}
	return state
}

// dropOldestParked removes the oldest parked sessions until at most limit remain.
// A slot without a timestamp counts as the oldest.
func dropOldestParked(parked map[string]any, limit int) {
	for len(parked) > limit {
		oldest, oldestAt := "", ""
		for name, raw := range parked {
			slot, _ := raw.(map[string]any)
			at, _ := slot[parkedAtField].(string)
			if oldest == "" || at < oldestAt || (at == oldestAt && name < oldest) {
				oldest, oldestAt = name, at
			}
		}
		delete(parked, oldest)
	}
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
	owner := ""
	if rawOwner, ok := state[ownerStateField]; ok {
		var isString bool
		owner, isString = rawOwner.(string)
		if !isString {
			return acpSessionConfig{}, fmt.Errorf("%s must be a string; got %T", ownerStateField, rawOwner)
		}
	}
	if owner == "" || owner == a.ownerID() {
		return cfg, nil
	}
	cfg.sessionID = ""
	cfg.configValues = nil
	cfg.attachmentIndeterminate = false
	// Parked sessions only exist next to the live session of the agent that took
	// over. A state whose own session id was cleared was reset, and a reset must
	// not bring an old session back.
	if activeID, _ := state["session_id"].(string); strings.TrimSpace(activeID) == "" {
		return cfg, nil
	}
	parked, _ := state[parkedStateField].(map[string]any)
	slot, _ := parked[a.ownerID()].(map[string]any)
	sessionID, _ := slot["session_id"].(string)
	if strings.TrimSpace(sessionID) == "" {
		return cfg, nil
	}
	var values []SessionConfigValue
	if rawValues, ok := slot["config_values"]; ok {
		parsed, err := parseSessionConfigValues(rawValues)
		if err != nil {
			// A damaged slot must not wedge the agent: start a new session.
			return cfg, nil
		}
		values = parsed
	}
	cfg.sessionID = strings.TrimSpace(sessionID)
	cfg.configValues = values
	cfg.attachmentIndeterminate, _ = slot["attachment_indeterminate"].(bool)
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
