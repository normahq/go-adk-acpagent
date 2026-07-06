# go-adk-acpagent

[![test](https://github.com/normahq/go-adk-acpagent/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/test.yml)
[![lint](https://github.com/normahq/go-adk-acpagent/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/lint.yml)
[![security](https://github.com/normahq/go-adk-acpagent/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/security.yml)
[![release](https://github.com/normahq/go-adk-acpagent/actions/workflows/release.yml/badge.svg)](https://github.com/normahq/go-adk-acpagent/actions/workflows/release.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/normahq/go-adk-acpagent/v2.svg)](https://pkg.go.dev/github.com/normahq/go-adk-acpagent/v2)
[![License](https://img.shields.io/github/license/normahq/go-adk-acpagent)](../LICENSE)
[![Version](https://img.shields.io/github/v/tag/normahq/go-adk-acpagent?label=version)](https://github.com/normahq/go-adk-acpagent/tags)

`go-adk-acpagent/v2` lets Google ADK applications run an Agent Client Protocol
(ACP) coding agent as an ADK `agent.Agent`.

The package starts an ACP-compatible subprocess, communicates with it over
stdio, maps ADK sessions to ACP sessions, and converts ACP updates into ADK
events.

## Install

```sh
go get github.com/normahq/go-adk-acpagent/v2
```

## Usage

```go
package main

import (
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

	_ = agentRuntime
}
```

`Config.Logger` accepts a standard `*slog.Logger` for adapter diagnostics.
`Config.Stderr` controls ACP subprocess stderr forwarding. `Config.SessionConfig`
applies ACP session config values through `session/set_config_option`.

ACP provider error metadata helpers are available from:

```go
import "github.com/normahq/go-adk-acpagent/v2/acperror"
```

ACP provider failures are projected onto ADK event `ErrorCode` and
`ErrorMessage` fields. ACP-specific details are also available under
`event.CustomMetadata[acperror.ProviderErrorMetadataKey]`.

## Examples

- [`examples/opencode`](examples/opencode)
- [`examples/codex`](examples/codex)

## Documentation

- [Concepts](../docs/concepts.md)
- [Provider recipes](../docs/provider-recipes.md)
- [Session state](../docs/session-state.md)
- [Troubleshooting](../docs/troubleshooting.md)

## Production Notes

- Call `Close` during shutdown so the ACP subprocess exits cleanly.
- Use `Config.Stderr` deliberately. `io.Discard` keeps application logs clean;
  `os.Stderr` or another writer is useful when diagnosing provider startup.
- Keep `Config.WorkingDir` or `CWDStateKey` pointed at an existing directory.
- Treat `SessionStateKey` as adapter-owned except for documented `_meta` and
  `config_values` overrides.

## Tests

```sh
go test -race ./...
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
```

CI requires at least 95% total statement coverage for both modules.
