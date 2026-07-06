# Concepts

`go-adk-acpagent` provides a Google ADK agent implementation backed by an
Agent Client Protocol (ACP) coding agent.

Use it when an ACP-compatible coding agent already knows how to read code, call
tools, request permissions, and stream updates, but the rest of your
application is built around ADK runners and sessions.

## What It Does

The adapter owns one ACP coding-agent subprocess per `Agent` instance. Each ADK
invocation uses the ADK session state to find or create a remote ACP session,
sends the user prompt to that ACP session, and maps ACP updates back into ADK
events.

The package handles:

- ACP subprocess startup and shutdown.
- ACP `initialize`, `session/new`, `session/resume`, `session/prompt`, and
  prompt update dispatch.
- ADK event mapping for text, thoughts, tool calls, usage, plan updates, and
  provider errors.
- ACP permission callbacks through `PermissionHandler`.
- Optional session configuration through ACP session config options.
- Optional MCP server forwarding to ACP `session/new` and `session/resume`.

## Why It Exists

ACP coding agents and ADK applications solve different parts of the agent stack.
ACP standardizes communication between clients and coding agents. ADK
standardizes agent composition, runners, sessions, callbacks, and state.

This package keeps those concerns separate:

- provider-specific CLI behavior stays inside the ACP coding agent
- ADK orchestration code depends only on `agent.Agent`
- session identity is stored in ADK session state instead of hidden globals
- logs, stderr forwarding, permission decisions, and provider errors remain
  explicit application choices

## What It Is

- An ADK `agent.Agent` implementation.
- An ACP client for an ACP-compatible coding agent.
- A stdio subprocess lifecycle manager.
- A session and event bridge between ADK and ACP.

## What It Is Not

- It is not an ACP coding agent implementation.
- It is not a generic runtime adapter.
- It is not Norma runtime, PDCA, swarm, pool, profile, or structured-agent
  logic.

## How It Works

Create an `Agent` with `Config.Command`, `Config.WorkingDir`, and optional
process settings. Pass the returned agent to an ADK runner and call `Close`
during shutdown.

On the first invocation for an ADK session, the adapter creates an ACP session.
It stores the ACP session ID under `SessionStateKey` so later invocations in
the same ADK session can reuse or resume that ACP session.

If the ACP agent reports resume capability, the adapter uses
`session/resume` after prompt failures that indicate a stale or missing active
session. ACP `session/load` is intentionally not used because ACP load replays
history and this package does not project that replay into ADK-visible history.

## Session Configuration

`Config.SessionConfig` is applied with ACP `session/set_config_option`. ACP
session config options are session-bound and can represent model, mode, thought
level, or provider-specific controls.

For legacy ACP agents that expose modes only through `session/set_mode`, the
adapter uses that method as a fallback for `SessionConfigValue{ID: "mode"}`.
If a provider does not expose the requested session config option, the
constructor still succeeds, but session creation or prompt execution can fail
with a wrapped ACP request error. Use `Config.Logger` and `Config.Stderr` when
debugging provider capability mismatches.

## Logs And Stderr

`Config.Logger` accepts `*slog.Logger` and is used for adapter diagnostics.
ACP subprocess stderr is controlled separately by `Config.Stderr`.

Recommended defaults:

- `Stderr: io.Discard` for quiet production services.
- `Stderr: os.Stderr` or a captured buffer/file when diagnosing provider
  startup and execution failures.

## Package Boundaries

This package does not include Norma PDCA, swarm, pool agents, Beads, or profile
configuration. Those are orchestration concerns. `go-adk-acpagent` is the
ADK agent implementation that runs an ACP-compatible coding-agent command.
