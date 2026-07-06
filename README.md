# go-adk-acpagent

[![test](https://github.com/normahq/go-adk-acpagent/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/test.yml)
[![lint](https://github.com/normahq/go-adk-acpagent/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/lint.yml)
[![security](https://github.com/normahq/go-adk-acpagent/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/security.yml)
[![release](https://github.com/normahq/go-adk-acpagent/actions/workflows/release.yml/badge.svg)](https://github.com/normahq/go-adk-acpagent/actions/workflows/release.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/normahq/go-adk-acpagent.svg)](https://pkg.go.dev/github.com/normahq/go-adk-acpagent)
[![License](https://img.shields.io/github/license/normahq/go-adk-acpagent)](LICENSE)
[![Version](https://img.shields.io/github/v/tag/normahq/go-adk-acpagent?label=version)](https://github.com/normahq/go-adk-acpagent/tags)

`go-adk-acpagent` provides a Google ADK agent implementation backed by an
Agent Client Protocol (ACP) coding agent.

It starts an ACP-compatible coding-agent subprocess, talks to it as an ACP
client over stdio, maps ADK sessions to ACP sessions, and maps ACP updates back
to ADK events. ADK applications can use ACP coding agents without taking a
dependency on Norma's PDCA, swarm, Beads, or profile layers.

## Install

```sh
go get github.com/normahq/go-adk-acpagent
```

This module targets Go 1.26.4 and uses the Go version declared in `go.mod`
for CI.

## Usage

```go
package main

import (
	"io"
	"log"
	"log/slog"
	"os"

	acpagent "github.com/normahq/go-adk-acpagent"
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

`Config.Logger` accepts a standard `*slog.Logger` and is used only for adapter
diagnostics. `Config.Stderr` is optional ACP subprocess stderr forwarding; set
it to a file, buffer, `os.Stderr`, or `io.Discard` depending on how much raw
provider stderr you want to keep.

`Config.SessionConfig` optionally applies ACP session config values through
`session/set_config_option`. The lower-level client API is
`Client.SetSessionConfigOption`.

## Examples

Runnable examples are included under:

- [`examples/opencode`](examples/opencode)
- [`examples/codex`](examples/codex)

The examples show the production defaults expected by this adapter: pass a
request-scoped context to construction, configure a structured `slog.Logger`,
choose whether ACP subprocess stderr is forwarded or discarded, set a working
directory explicitly, and always call `Close`.

## Documentation

- [Documentation index](docs/README.md)
- [Concepts](docs/concepts.md): what the adapter does, why it exists, and how
  ACP sessions and config values map to ADK sessions.
- [Provider recipes](docs/provider-recipes.md): OpenCode, Codex, Claude, PI,
  and generic ACP command examples.
- [Session state](docs/session-state.md): cwd overrides, ACP session identity,
  config values, metadata, plan snapshots, and output state.
- [Troubleshooting](docs/troubleshooting.md): process startup, stderr, session
  config, permissions, provider errors, and ACP inspection.
- [Migration from Norma](docs/migration-from-norma.md): import path and config
  mapping from the deprecated Norma wrapper.

ACP provider error metadata helpers are available from:

```go
import "github.com/normahq/go-adk-acpagent/acperror"
```

ACP provider failures are projected onto ADK event `ErrorCode` and
`ErrorMessage` fields when available. ACP-specific details are also available
under `event.CustomMetadata[acperror.ProviderErrorMetadataKey]`.

## Session State

Use `acpagent.CWDStateKey` (`"cwd"`) to override the ACP session working directory per ADK session. ACP session metadata is stored under `acpagent.SessionStateKey`, including the ACP `session_id`, optional `_meta`, and optional `config_values`. ACP plan snapshots are stored under `acpagent.PlanStateKey`.

## Production Notes

- Call `Close` during shutdown so the ACP subprocess exits cleanly.
- Use `Config.Stderr` deliberately. `io.Discard` keeps application logs clean;
  `os.Stderr` or another writer is useful when diagnosing provider startup.
- Keep `Config.WorkingDir` or `CWDStateKey` pointed at an existing directory.
- Treat `SessionStateKey` as adapter-owned except for documented `_meta` and
  `config_values` overrides.
- `Config.SessionConfig` is applied through ACP `session/set_config_option`.

## Tests

```sh
go test -race ./...
go -C v2 test -race ./...
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
go -C v2 test ./... -coverprofile=coverage.out
go -C v2 tool cover -func=coverage.out
```

CI requires at least 95% total statement coverage for both modules. Security
checks run in CI through `govulncheck` for both modules. To match CI locally,
install the pinned tool versions from `tools/go.mod` and run:

```sh
GOBIN="$PWD/.bin" go -C tools install github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOBIN="$PWD/.bin" go -C tools install golang.org/x/vuln/cmd/govulncheck
./.bin/golangci-lint run ./...
./.bin/govulncheck ./...
(cd v2 && ../.bin/golangci-lint run ./...)
(cd v2 && ../.bin/govulncheck ./...)
```

Optional integration tests require the matching ACP agent binaries and build tags:

```sh
go test -tags 'integration codex' ./...
go test -tags 'integration opencode' ./...
```
