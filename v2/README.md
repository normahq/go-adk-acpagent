# go-adk-acpagent

[![test](https://github.com/normahq/go-adk-acpagent/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/test.yml)
[![lint](https://github.com/normahq/go-adk-acpagent/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/lint.yml)
[![security](https://github.com/normahq/go-adk-acpagent/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/security.yml)
[![release](https://github.com/normahq/go-adk-acpagent/actions/workflows/release.yml/badge.svg)](https://github.com/normahq/go-adk-acpagent/actions/workflows/release.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/normahq/go-adk-acpagent/v2.svg)](https://pkg.go.dev/github.com/normahq/go-adk-acpagent/v2)
[![License](https://img.shields.io/github/license/normahq/go-adk-acpagent)](../LICENSE)
[![Version](https://img.shields.io/github/v/tag/normahq/go-adk-acpagent?label=version)](https://github.com/normahq/go-adk-acpagent/tags)

**Run ACP-compatible coding agents as Google ADK `agent.Agent`
implementations.**

`go-adk-acpagent/v2` starts a coding-agent subprocess, talks to it with Agent
Client Protocol (ACP) over stdio, binds ACP sessions to ADK sessions, and emits
ADK events for streamed text, thoughts, tool calls, usage, plan updates, and
provider errors.

Use it when your application is built on Google ADK v2 but the coding agent you
want to run is exposed as an ACP command.

## What You Get

| Capability | Behavior |
| --- | --- |
| ADK agent | Implements the ADK `agent.Agent` interface. |
| ACP lifecycle | Starts one ACP subprocess per agent instance and closes it on shutdown. |
| Session binding | Creates, stores, reuses, and resumes ACP sessions through ADK session state. |
| Event mapping | Converts ACP updates into ADK events and state deltas. |
| Permissions | Routes ACP permission requests through `PermissionHandler`. |
| Session config | Applies ACP session-bound values such as model, mode, or thought level. |
| MCP forwarding | Sends configured MCP servers to ACP session creation and resume calls. |
| Diagnostics | Uses `*slog.Logger` for adapter logs and a separate writer for provider stderr. |

## Try It With OpenCode

```sh
go get github.com/normahq/go-adk-acpagent/v2
```

```go
package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"

	acpagent "github.com/normahq/go-adk-acpagent/v2"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	agentRuntime, err := acpagent.New(acpagent.Config{
		Context:    context.Background(),
		Command:    []string{"opencode", "acp"},
		WorkingDir: "/workspace",
		Logger:     logger,
		Stderr:     io.Discard,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := agentRuntime.Close(); err != nil {
			log.Printf("close ACP agent: %v", err)
		}
	}()

	// Pass agentRuntime to an ADK runner.
}
```

## Provider Commands

| Provider | Command |
| --- | --- |
| OpenCode | `[]string{"opencode", "acp"}` |
| Codex | `[]string{"npx", "-y", "@normahq/codex-acp-bridge@latest"}` |
| Claude Code | `[]string{"npx", "-y", "@zed-industries/claude-code-acp@latest"}` |
| Generic ACP | Any executable that speaks ACP on stdin/stdout. |

Runnable examples are available for [OpenCode](examples/opencode) and
[Codex](examples/codex).

## Configuration Cheatsheet

| Field | Purpose |
| --- | --- |
| `Command` | ACP subprocess argv. |
| `WorkingDir` | Process directory and default ACP session cwd. |
| `Logger` | Adapter diagnostics through `*slog.Logger`. |
| `Stderr` | ACP subprocess stderr forwarding. |
| `PermissionHandler` | Application decision point for ACP permission requests. |
| `SessionConfig` | ACP session config values applied through `session/set_config_option`. |
| `MCPServers` | MCP servers forwarded to ACP sessions. |
| `Instruction` / `GlobalInstruction` | ADK-style instructions prepended to prompts. |
| `ReasoningEffort` | Provider reasoning effort metadata when supported. |
| `OutputKey` | ADK state key for the final visible model output. |

ACP provider error metadata helpers are available from:

```go
import "github.com/normahq/go-adk-acpagent/v2/acperror"
```

## Documentation By Task

| Task | Start here |
| --- | --- |
| Understand lifecycle and event mapping | [Concepts](../docs/concepts.md) |
| Choose a provider command | [Provider recipes](../docs/provider-recipes.md) |
| Manage cwd, session metadata, config values, plans, and output state | [Session state](../docs/session-state.md) |
| Debug startup, JSON-RPC streams, permissions, and provider errors | [Troubleshooting](../docs/troubleshooting.md) |

## Production Notes

- Call `Close` during shutdown so the ACP subprocess exits cleanly.
- Keep ACP protocol messages on stdout and provider logs on stderr.
- Use `SessionConfig` for session-bound model, mode, thought-level, or
  provider-specific choices.
- Treat `SessionStateKey` as adapter-owned except for documented `_meta`,
  `config_values`, and cwd overrides.

## Tests

```sh
go test -race ./...
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
```

CI requires at least 95% total statement coverage for both modules.
