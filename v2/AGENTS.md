# Agent Guidelines

This repository contains `github.com/normahq/go-adk-acpagent`, a Go library
that adapts ACP runtimes to Google ADK agents.

## Development

- Keep the public API backward compatible within a major version.
- Use the Go version declared in `go.mod`.
- Run `go mod tidy` after dependency changes.
- Run `go test ./...`, `go test -race ./...`,
  `GOBIN="$PWD/../.bin" go -C ../tools install github.com/golangci/golangci-lint/v2/cmd/golangci-lint`,
  `GOBIN="$PWD/../.bin" go -C ../tools install golang.org/x/vuln/cmd/govulncheck`,
  `../.bin/golangci-lint run ./...`, and `../.bin/govulncheck ./...`
  before publishing.
- Keep examples runnable and godoc-friendly.

## Style

- Use standard Go formatting and idioms.
- Prefer small, explicit APIs over helper abstractions.
- Document exported symbols and cleanup requirements.
