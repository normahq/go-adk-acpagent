# Agent Guidelines

This repository contains `github.com/normahq/go-adk-acpagent/v2`, a Go library
that provides a Google ADK agent implementation backed by an Agent Client
Protocol (ACP) coding agent.

## Development

- Keep the public API backward compatible within a major version.
- Use the Go version declared in `go.mod`.
- Use `task` for operation tasks.
- Run `task tidy` after dependency changes.
- Run `task ci` before publishing.
- Keep examples runnable and godoc-friendly.

## Style

- Use standard Go formatting and idioms.
- Prefer small, explicit APIs over helper abstractions.
- Document exported symbols and cleanup requirements.
