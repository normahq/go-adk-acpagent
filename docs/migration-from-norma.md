# Migration From Norma Runtime ACP Adapter

Norma's old `pkg/runtime/acpagent` package is a deprecated compatibility
wrapper around `github.com/normahq/go-adk-acpagent`.

New code should import the standalone module directly.

## Import Path

Replace:

```go
import "github.com/normahq/norma/pkg/runtime/acpagent"
```

with:

```go
import "github.com/normahq/go-adk-acpagent"
```

## API Compatibility

The compatibility wrapper aliases the main adapter types and constants:

- `Config`
- `Agent`
- `ClientConfig`
- `Client`
- `PermissionHandler`
- `SessionStateKey`
- `PlanStateKey`
- `CWDStateKey`
- MCP server config types

Move imports first, then remove the Norma wrapper dependency once callers no
longer reference `github.com/normahq/norma/pkg/runtime/acpagent`.

## Norma Alias Mapping

Norma agent aliases normalized into generic ACP commands. In standalone
`go-adk-acpagent`, pass those commands directly through `Config.Command`.

| Norma alias | Standalone command |
| --- | --- |
| `codex_acp` | `[]string{"npx", "-y", "@normahq/codex-acp-bridge@latest"}` |
| `opencode_acp` | `[]string{"opencode", "acp"}` |
| `claude_code_acp` | `[]string{"npx", "-y", "@zed-industries/claude-code-acp@latest"}` |
| `generic_acp` | the configured `generic_acp.cmd` argv |

`go-adk-acpagent` intentionally does not implement Norma's profile, pool,
PDCA, swarm, or Beads routing layers.

## Configuration Mapping

Norma `ACPConfig` fields map to `acpagent.Config` as follows:

| Norma field | Standalone field |
| --- | --- |
| `cmd` / resolved alias command | `Config.Command` |
| `model` | `Config.Model` |
| `mode` | `Config.Mode` |
| `reasoning_effort` | `Config.ReasoningEffort` |
| `system_instructions` | `Config.Instruction` or `Config.GlobalInstruction` |
| MCP server registry entries | `Config.MCPServers` |
| working directory from build request | `Config.WorkingDir` |
| stderr writer from factory | `Config.Stderr` |
| permission callback from factory | `Config.PermissionHandler` |

## Behavior Changes To Notice

- Model selection uses ACP `session/set_config_option`.
- `session/set_model` is not used.
- ACP session identity is stored in ADK session state under `SessionStateKey`.
- ACP `session/load` is not used.
- Provider errors are exposed through `ErrorCode`, `ErrorMessage`, and
  `acperror` metadata helpers.
