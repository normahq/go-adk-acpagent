package acpagent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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

// effectiveEnvironment returns the environment the subprocess runs with: the
// configured one as is, or, when none is set, a snapshot of the process
// environment taken now. Like os/exec does for an inherited environment, the
// snapshot gets PWD set to the directory the subprocess starts in, otherwise it
// would keep the PWD of this process.
func effectiveEnvironment(configured []string, workingDir string) []string {
	if configured != nil {
		return configured
	}
	snapshot := os.Environ()
	if workingDir == "" {
		return snapshot
	}
	dir, err := filepath.Abs(workingDir)
	if err != nil {
		dir = workingDir
	}
	environment := make([]string, 0, len(snapshot)+1)
	for _, entry := range snapshot {
		if name, _, _ := strings.Cut(entry, "="); name != "PWD" {
			environment = append(environment, entry)
		}
	}
	return append(environment, "PWD="+dir)
}

// volatileEnvironment lists variables that describe a process, a terminal, a
// login session or a service manager launch and say nothing about which
// accounts or credentials the backend uses (the container id in HOSTNAME, the
// working directory, terminal and multiplexer ids, systemd invocation ids,
// window-system ids). They are left out of the owner identity, otherwise every
// restart would look like another backend and forget its stored sessions.
//
// Variables that route credentials stay in the identity even though some of
// them change between launches: SSH_AUTH_SOCK and SSH_AGENT_PID (which ssh
// agent and so which keys), DBUS_SESSION_BUS_ADDRESS and XDG_RUNTIME_DIR (which
// user's secret service and runtime sockets), cloud and API credentials. A
// restart that changes them forgets the stored session, which is safe; the
// alternative would resume the session of another account. A deployment whose
// credential variables change between launches pins the identity with
// Config.SessionOwner. The list cannot be complete for the same reason.
var volatileEnvironment = map[string]struct{}{
	"_": {}, "PWD": {}, "OLDPWD": {}, "SHLVL": {}, "HOSTNAME": {}, "LINES": {}, "COLUMNS": {},
	"TERM": {}, "COLORTERM": {}, "TERM_PROGRAM_VERSION": {}, "TERM_SESSION_ID": {}, "ITERM_SESSION_ID": {},
	"ITERM_PROFILE": {}, "TMUX": {}, "TMUX_PANE": {}, "STY": {}, "GPG_TTY": {}, "WT_SESSION": {},
	"SECURITYSESSIONID": {}, "COMMAND_MODE": {}, "__CF_USER_TEXT_ENCODING": {}, "LaunchInstanceID": {},
	"SSH_CONNECTION": {}, "SSH_CLIENT": {}, "SSH_TTY": {},
	"TMPDIR": {}, "WINDOWID": {}, "DISPLAY": {}, "VSCODE_IPC_HOOK_CLI": {},
	"SESSION_MANAGER": {}, "XDG_SESSION_ID": {},
	"INVOCATION_ID": {}, "JOURNAL_STREAM": {}, "SYSTEMD_EXEC_PID": {}, "NOTIFY_SOCKET": {},
	"MAINPID": {}, "LISTEN_PID": {}, "LISTEN_FDS": {}, "LISTEN_FDNAMES": {},
}

// stableEnvironment returns the entries of the effective environment that
// identify a backend: launch-specific variables are dropped and, for a name
// that appears several times, only the last value counts, as it does when the
// environment is applied. The result is sorted.
func stableEnvironment(environment []string) []string {
	last := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, volatile := volatileEnvironment[name]; volatile || strings.HasPrefix(name, "XPC_") {
			continue
		}
		last[name] = entry
	}
	stable := make([]string, 0, len(last))
	for _, entry := range last {
		stable = append(stable, entry)
	}
	sort.Strings(stable)
	return stable
}

// agentOwnerKey identifies an agent in the shared ACP session state by its name
// and by what starts its backend: the command, the effective environment and
// the working directory of the subprocess. The name alone is not enough: agents
// that keep the default name would all look like the same owner, and two agents
// with one name and command but another environment (a different CODEX_HOME or
// provider profile, another credential, whether set explicitly or inherited) or
// another working directory are different backends whose sessions must not be
// resumed by each other. Variables that only describe a launch are ignored, see
// volatileEnvironment. Only a truncated hash is stored, never the command, the
// environment or the directory themselves. A changed backend is a different
// owner, so its stored session is not reused.
func agentOwnerKey(name string, command, environment []string, workingDir string) string {
	commandPart := strings.Join(command, "\x00")
	environmentPart := strings.Join(stableEnvironment(environment), "\x00")
	sum := sha256.Sum256([]byte(commandPart + "\x01" + environmentPart + "\x02" + workingDir))
	return strings.TrimSpace(name) + ":" + hex.EncodeToString(sum[:4])
}

// resolveOwnerKey returns the identity configured by the caller, or derives it
// from the backend. A configured identity must be stable across launches and
// must not contain secrets: it is stored in the session state as given.
func resolveOwnerKey(configured, name string, command, environment []string, workingDir string) string {
	if owner := strings.TrimSpace(configured); owner != "" {
		return owner
	}
	return agentOwnerKey(name, command, environment, workingDir)
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
			if control, ok := current[controlStateField]; ok {
				slot[controlStateField] = control
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
	cfg.control = nil
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
	cfg.control = parseSessionControlState(slot[controlStateField])
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
