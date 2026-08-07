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
- permission callbacks mapped into the ADK-facing `PermissionRequest` and
  `PermissionDecision` contract used by `PermissionHandler`.
- Optional session configuration through ACP session config options.
- Optional MCP server forwarding to ACP `session/new` and `session/resume`.
- Ordered ADK user content conversion into ACP text, image, audio, embedded
  resource, and resource-link prompt blocks.

See [Event mapping](event-mapping.md) for the exact ACP update to ADK event
contract.

## Why It Exists

ACP standardizes communication between clients and coding agents. ADK
standardizes agent composition, runners, sessions, callbacks, and state.

This package connects those layers while keeping provider-specific behavior in
the ACP coding agent and ADK orchestration in the application.

## How It Works

Create an `Agent` with `NewWithContext`, `Config.Command`,
`Config.WorkingDir`, and optional process settings. Pass the returned agent to
an ADK runner and call `Close` during shutdown.

On the first invocation for an ADK session, the adapter creates an ACP session.
It stores the ACP session ID under `SessionStateKey` so later invocations in
the same ADK session can reuse or resume that ACP session.

If the ACP agent reports resume capability, the adapter uses `session/resume`
after prompt failures that indicate a stale or missing active session. ACP
`session/load` is not used because load replays prior history and this package
does not project that replay into ADK-visible history.

## Structured User Content

The adapter preserves the order of supported ADK user parts and selects
optional ACP blocks from the capabilities returned by `initialize`. Text and
`ResourceLink` are ACP baseline content and require no capability flag.

For the optional prompt blocks, conversion is deterministic:

- inline image bytes become an ACP image block when `image` is advertised;
  otherwise they become an embedded resource when `embeddedContext` is
  advertised, or fail with a typed capability error;
- inline audio bytes follow the same matrix using `audio`;
- other inline bytes require `embeddedContext` and become an embedded blob
  resource with a stable content-derived URI;
- image file data becomes an ACP image URI block when `image` is advertised,
  otherwise it becomes a baseline resource link;
- other file data, including audio/voice files, becomes a baseline resource
  link with its display name and MIME type.

The adapter does not dereference file URIs or create temporary files. Callers
that own durable attachment storage should prefer `FileData` so an agent with
only baseline capabilities can consume the local reference.

Unsupported function, tool, executable-code, or result parts fail explicitly
instead of being silently discarded. First-turn instructions are prepended as
a separate text block, so media and resource parts remain structured during
initial and recovered prompts. Before `session/prompt`, the adapter rejects
image, audio, and embedded resource blocks unless the ACP agent advertised the
corresponding optional capability. Text and resource links remain baseline ACP
content and do not require capability flags.

## Session Configuration

`Config.SessionConfig` is applied with ACP `session/set_config_option`. ACP
session config options are session-bound and can represent model, mode, thought
level, or provider-specific controls. Select options use
`SessionConfigValue{ID: "...", Value: "..."}`; boolean options use
`BooleanSessionConfigValue("...", true)`.

For ACP agents that expose modes only through `session/set_mode`, the adapter
uses that method as a fallback for `SessionConfigValue{ID: "mode"}`. If a
provider does not expose the requested session config option, session creation
or prompt execution can fail with a wrapped ACP request error.

## Logs And Stderr

`Config.Logger` accepts `*slog.Logger` and is used for adapter diagnostics. ACP
subprocess stderr is controlled separately by `Config.Stderr`.

Debug and trace records contain structural fields such as session IDs, update
types, content-block types, counts, and byte lengths. Prompt text, encoded
media, resource URIs, and raw JSON-RPC payloads are omitted. Provider stderr
may still contain provider-owned diagnostics and remains controlled separately.

Recommended defaults:

- `Stderr: io.Discard` for quiet production services.
- `Stderr: os.Stderr` or a captured buffer/file when diagnosing provider
  startup and execution failures.
