// Package acpagent provides a Google ADK agent implementation backed by an
// Agent Client Protocol (ACP) coding agent.
//
// The package starts an ACP-compatible coding-agent subprocess, talks to it as
// an ACP client over stdio, and maps each ADK session to a remote ACP session.
// By default, ACP session creation uses [Config.WorkingDir] as the ACP session
// cwd.
//
// # Per-session overrides via ADK state
//
// Callers can override ACP session creation per ADK session by setting
// [CWDStateKey] before the first invocation in that ADK session:
//
//	map[string]any{
//	  CWDStateKey: "/absolute/path", // optional
//	}
//
// ACP-specific session/new metadata may be provided under [SessionStateKey]:
//
//	map[string]any{
//	  acpagent.SessionStateKey: map[string]any{
//	    "meta": map[string]any{ // optional; forwarded to ACP session/new _meta
//	      "codex": map[string]any{"approvalMode": "manual"},
//	    },
//	  },
//	}
//
// Behavior:
//   - If `state[CWDStateKey]` is set, it overrides [Config.WorkingDir]
//     for ACP session creation.
//   - The adapter persists the canonical ACP session ID under
//     `state[SessionStateKey].session_id` and uses that value for
//     ACP session/resume when the ACP agent advertises resume capability.
//     When [Config.Model] is set, the adapter also persists the discovered
//     ACP model configuration option ID under
//     `state[SessionStateKey].model_config_id`.
//   - As soon as the adapter binds a remote ACP session, it stores the
//     canonical ACP session ID in the live ADK session state under
//     `state[SessionStateKey]`.
//   - If `state[SessionStateKey].session_id` is absent, the package creates a
//     new ACP session.
//   - For newly created ACP sessions, resolved startup instructions are passed
//     through two channels: `session/new._meta.codex` receives
//     `baseInstructions` and `developerInstructions` when those keys are not
//     already set, and the first real user `session/prompt` is prepended with
//     the combined instructions. The adapter does not send a separate
//     instruction-only prompt.
//   - The adapter does not use ACP `session/load`; ACP v1 load replays prior
//     history, and this adapter does not yet map that replay into ADK-visible
//     history.
//   - If `state[SessionStateKey].meta` is set, it is passed through to ACP
//     session/new._meta and session/resume._meta.
//   - If `state[SessionStateKey].model_config_id` is set, it is used to apply
//     [Config.Model] through ACP session/set_config_option for an existing ACP
//     session. Otherwise, [Config.ModelConfigID] or a model config option
//     discovered from the ACP session response is used.
//   - Overrides are read when the ACP session is first created for the ADK
//     session. Subsequent changes do not rebind that existing ACP session.
//
// Invalid override values (for example, non-string `state[CWDStateKey]`,
// non-object `state[SessionStateKey]`, non-object `state[SessionStateKey].meta`,
// non-string `state[SessionStateKey].session_id`,
// non-string `state[SessionStateKey].model_config_id`,
// or a cwd that is not a valid existing directory) cause invocation failure
// before ACP session creation.
//
// # ACP plan updates
//
// ACP `session/update.plan` notifications are projected into ADK event state
// under [PlanStateKey]. Each event carries the full replacement snapshot at
// `event.Actions.StateDelta[PlanStateKey]` with an `entries` field containing
// the current ACP plan entries. Plan updates do not appear as content parts.
package acpagent
