# Session State

The adapter uses ADK session state as the source of truth for ACP session
binding and per-session overrides.

## Keys

`CWDStateKey` (`"cwd"`) overrides the ACP session working directory for one ADK
session.

`SessionStateKey` (`"acp_session"`) stores ACP session data:

```go
map[string]any{
	"session_id": "provider-session-id",
	"config_values": []map[string]string{
		{"id": "model", "value": "gpt-5-codex"},
	},
	"meta": map[string]any{"codex": map[string]any{}},
}
```

`PlanStateKey` (`"acp_plan"`) stores the latest ACP plan snapshot projected from
ACP `session/update.plan` notifications.

## Lifecycle

On first use, the adapter creates an ACP session and writes
`SessionStateKey.session_id` into the live ADK session state. Later invocations
in the same ADK session reuse that ID.

If the ACP agent supports `session/resume`, the adapter uses the stored ACP
session ID to resume after recoverable prompt failures. If the stored ACP
session is stale, the adapter creates a new ACP session and updates the ADK
state.

ACP `session/load` is intentionally not used. ACP load replays prior history,
and this package does not map that replay into ADK-visible event history.

## Working Directory

`Config.WorkingDir` is the default ACP session cwd. Set `CWDStateKey` in ADK
session state to override it for a specific ADK session:

```go
state := map[string]any{
	acpagent.CWDStateKey: "/workspace/service-a",
}
```

The cwd must exist and must be a directory. Invalid cwd values fail before ACP
session creation.

## Metadata

Set `SessionStateKey.meta` to send provider metadata to ACP `session/new._meta`
and `session/resume._meta`:

```go
state := map[string]any{
	acpagent.SessionStateKey: map[string]any{
		"meta": map[string]any{
			"codex": map[string]any{
				"approvalMode": "manual",
			},
		},
	},
}
```

For new sessions, adapter-provided instructions are added under
`_meta.codex.baseInstructions` and `_meta.codex.developerInstructions` only
when those fields are not already present.

## Session Config Values

`Config.SessionConfig` is applied through ACP `session/set_config_option`.
Use it for ACP session-bound choices such as model, mode, or thought level:

```go
agentRuntime, err := acpagent.New(acpagent.Config{
	Command:    []string{"opencode", "acp"},
	WorkingDir: "/workspace",
	SessionConfig: []acpagent.SessionConfigValue{
		acpagent.SelectSessionConfigValue("model", "opencode/big-pickle"),
		acpagent.BooleanSessionConfigValue("fast_mode", true),
	},
})
```

Set `SessionStateKey.config_values` to override defaults for one ADK session.
The adapter persists current values returned from ACP `session/new`,
`session/resume`, `session/set_config_option`, and
`session/update.config_option`. Select values are stored as
`{"id":"model","value":"..."}`; boolean values are stored as
`{"id":"fast_mode","type":"boolean","value":true}`.

## Output State

Set `Config.OutputKey` to store the final visible model output in the ADK event
state delta for the invocation:

```go
agentRuntime, err := acpagent.New(acpagent.Config{
	Command:    []string{"opencode", "acp"},
	WorkingDir: "/workspace",
	OutputKey:  "last_agent_output",
})
```

Partial streaming events do not write `OutputKey`.
