# Concepts

`go-adk-acpagent` provides a Google ADK agent implementation backed by an
Agent Client Protocol (ACP) coding agent.

Use it when an ACP-compatible coding agent already knows how to read code, call
tools, request permissions, and stream updates, but your application uses ADK
runners, sessions, callbacks, and state.

## What It Does

The adapter owns one ACP coding-agent subprocess per `Agent` instance. Each ADK
invocation uses ADK session state to find or create a remote ACP session, sends
the user prompt to that ACP session, and maps ACP updates back into ADK events.

The package handles:

- ACP subprocess startup and shutdown.
- ACP `initialize`, `session/new`, `session/resume`, `session/prompt`, and
  prompt update dispatch.
- ADK event mapping for text, thoughts, tool calls, usage, plan updates, and
  provider errors.
- ACP permission callbacks through `PermissionHandler`.
- Optional session configuration through ACP session config options.
- Optional MCP server forwarding to ACP `session/new` and `session/resume`.

See [Event mapping](event-mapping.md) for the exact ACP update to ADK event
contract.

## Why It Exists

ACP standardizes communication between clients and coding agents. ADK
standardizes agent composition, runners, sessions, callbacks, and state.

This package connects those layers while keeping provider-specific behavior in
the ACP coding agent and ADK orchestration in the application.

## How It Works

Create an `Agent` with `Config.Command`, `Config.WorkingDir`, and optional
process settings. Pass the returned agent to an ADK runner and call `Close`
during shutdown.

On the first invocation for an ADK session, the adapter creates an ACP session.
It stores the ACP session ID under `SessionStateKey` so later invocations in
the same ADK session can reuse or resume that ACP session.

If the ACP agent reports resume capability, the adapter uses `session/resume`
after prompt failures that indicate a stale or missing active session. ACP
`session/load` is not used because load replays prior history and this package
does not project that replay into ADK-visible history.

## Session Configuration

`Config.SessionConfig` is applied with ACP `session/set_config_option`. ACP
session config options are session-bound and can represent model, mode, thought
level, or provider-specific controls.

For ACP agents that expose modes only through `session/set_mode`, the adapter
uses that method as a fallback for `SessionConfigValue{ID: "mode"}`. If a
provider does not expose the requested session config option, session creation
or prompt execution can fail with a wrapped ACP request error.

## Logs And Stderr

`Config.Logger` accepts `*slog.Logger` and is used for adapter diagnostics. ACP
subprocess stderr is controlled separately by `Config.Stderr`.

Recommended defaults:

- `Stderr: io.Discard` for quiet production services.
- `Stderr: os.Stderr` or a captured buffer/file when diagnosing provider
  startup and execution failures.
