# go-adk-acpagent

`go-adk-acpagent` adapts Agentic Computing Protocol (ACP) runtimes to the Google ADK `agent.Agent` interface.

## Install

```sh
go get github.com/normahq/go-adk-acpagent
```

This module targets Go 1.26.4 and uses the Go version declared in `go.mod`
for CI.

## Usage

```go
import acpagent "github.com/normahq/go-adk-acpagent"

agentRuntime, err := acpagent.New(acpagent.Config{
	Command:    []string{"codex-acp"},
	WorkingDir: "/workspace",
})
if err != nil {
	return err
}
defer agentRuntime.Close()
```

Provider error metadata helpers are available from:

```go
import "github.com/normahq/go-adk-acpagent/providererror"
```

## Session State

Use `acpagent.CWDStateKey` (`"cwd"`) to override the ACP session working directory per ADK session. ACP session metadata is stored under `acpagent.SessionStateKey`, and ACP plan snapshots are stored under `acpagent.PlanStateKey`.

## Tests

```sh
go test -race ./...
```

Optional integration tests require the matching ACP runtime binaries and build tags:

```sh
go test -tags 'integration codex' ./...
go test -tags 'integration opencode' ./...
```
