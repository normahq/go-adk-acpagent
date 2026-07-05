# go-adk-acpagent

[![test](https://github.com/normahq/go-adk-acpagent/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/test.yml)
[![lint](https://github.com/normahq/go-adk-acpagent/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/lint.yml)
[![security](https://github.com/normahq/go-adk-acpagent/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/normahq/go-adk-acpagent/actions/workflows/security.yml)
[![release](https://github.com/normahq/go-adk-acpagent/actions/workflows/release.yml/badge.svg)](https://github.com/normahq/go-adk-acpagent/actions/workflows/release.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/normahq/go-adk-acpagent.svg)](https://pkg.go.dev/github.com/normahq/go-adk-acpagent)
[![License](https://img.shields.io/github/license/normahq/go-adk-acpagent)](LICENSE)
[![Version](https://img.shields.io/github/v/tag/normahq/go-adk-acpagent?label=version)](https://github.com/normahq/go-adk-acpagent/tags)

`go-adk-acpagent` adapts Agentic Computing Protocol (ACP) runtimes to the Google ADK `agent.Agent` interface.

## Install

```sh
go get github.com/normahq/go-adk-acpagent
```

This module targets Go 1.26.4 and uses the Go version declared in `go.mod`
for CI.

Use `github.com/normahq/go-adk-acpagent/v2` with `google.golang.org/adk/v2`.

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
		Command:    []string{"codex-acp"},
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

`Config.Model` selects an ACP session model through `session/set_config_option`.
By default the adapter discovers a select config option with category `model`
from `session/new` or `session/resume`; set `Config.ModelConfigID` when a
provider uses a known custom config option id. The lower-level client API is
`Client.SetSessionConfigOption`.

For ADK v2, import the major-version module and keep the rest of the adapter
contract the same:

```go
import acpagent "github.com/normahq/go-adk-acpagent/v2"
```

ACP provider error metadata helpers are available from:

```go
import "github.com/normahq/go-adk-acpagent/acperror"
```

For ADK v2, use:

```go
import "github.com/normahq/go-adk-acpagent/v2/acperror"
```

ACP provider failures are projected onto ADK event `ErrorCode` and
`ErrorMessage` fields when available. ACP-specific details are also available
under `event.CustomMetadata[acperror.ProviderErrorMetadataKey]`.

## Session State

Use `acpagent.CWDStateKey` (`"cwd"`) to override the ACP session working directory per ADK session. ACP session metadata is stored under `acpagent.SessionStateKey`, including the ACP `session_id`, optional `_meta`, and optional `model_config_id`. ACP plan snapshots are stored under `acpagent.PlanStateKey`.

## Tests

```sh
go test -race ./...
go -C v2 test -race ./...
```

Security checks run in CI through `govulncheck` for both modules. To match CI
locally, install the pinned tool versions from `tools/go.mod` and run:

```sh
GOBIN="$PWD/.bin" go -C tools install github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOBIN="$PWD/.bin" go -C tools install golang.org/x/vuln/cmd/govulncheck
./.bin/golangci-lint run ./...
./.bin/govulncheck ./...
cd v2 && ../.bin/govulncheck ./...
```

Optional integration tests require the matching ACP runtime binaries and build tags:

```sh
go test -tags 'integration codex' ./...
go test -tags 'integration opencode' ./...
```
