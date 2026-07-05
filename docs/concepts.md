# Concepts

`go-adk-acpagent` adapts an Agentic Computing Protocol (ACP) stdio runtime to
the Google ADK `agent.Agent` interface.

Use it when an ACP-compatible CLI already knows how to read code, call tools,
request permissions, and stream updates, but the rest of your application is
built around ADK runners and sessions.

## What It Does

The adapter owns one ACP subprocess per `Agent` instance. Each ADK invocation
uses the ADK session state to find or create a remote ACP session, sends the
user prompt to that ACP session, and maps ACP updates back into ADK events.

The package handles:

- ACP process startup and shutdown.
- ACP `initialize`, `session/new`, `session/resume`, `session/prompt`, and
  prompt update dispatch.
- ADK event mapping for text, thoughts, tool calls, usage, plan updates, and
  provider errors.
- ACP permission callbacks through `PermissionHandler`.
- Optional model and mode selection through ACP session configuration.
- Optional MCP server forwarding to ACP `session/new` and `session/resume`.

## Why It Exists

ACP runtimes and ADK applications solve different parts of the agent stack.
ACP standardizes a stdio protocol for coding agents and tool mediation. ADK
standardizes agent composition, runners, sessions, callbacks, and state.

This package keeps those concerns separate:

- provider-specific CLI behavior stays inside the ACP runtime
- ADK orchestration code depends only on `agent.Agent`
- session identity is stored in ADK session state instead of hidden globals
- logs, stderr forwarding, permission decisions, and provider errors remain
  explicit application choices

## How It Works

Create an `Agent` with `Config.Command`, `Config.WorkingDir`, and optional
runtime settings. Pass the returned agent to an ADK runner and call `Close`
during shutdown.

On the first invocation for an ADK session, the adapter creates an ACP session.
It stores the ACP session ID under `SessionStateKey` so later invocations in
the same ADK session can reuse or resume that ACP session.

If the ACP runtime reports resume capability, the adapter uses
`session/resume` after prompt failures that indicate a stale or missing active
session. ACP `session/load` is intentionally not used because ACP load replays
history and this package does not project that replay into ADK-visible history.

## Model And Mode Selection

`Config.Model` is applied with ACP `session/set_config_option`. The adapter
uses `Config.ModelConfigID` when set, otherwise it discovers a model select
option from the ACP session response.

`Config.Mode` is applied with ACP `session/set_mode` when configured.

If a provider does not expose the expected model config option or mode, the
constructor still succeeds, but session creation or prompt execution can fail
with a wrapped ACP request error. Use `Config.Logger` and `Config.Stderr` when
debugging provider capability mismatches.

## Logs And Stderr

`Config.Logger` accepts `*slog.Logger` and is used for adapter diagnostics.
ACP subprocess stderr is controlled separately by `Config.Stderr`.

Recommended defaults:

- `Stderr: io.Discard` for quiet production services.
- `Stderr: os.Stderr` or a captured buffer/file when diagnosing provider
  startup and runtime failures.

## Package Boundaries

This package does not include Norma PDCA, swarm, pool agents, Beads, or profile
configuration. Those are orchestration concerns. `go-adk-acpagent` is the
adapter layer that turns an ACP command into an ADK agent.
