# Provider Recipes

`go-adk-acpagent` is provider-agnostic. Provider support comes from the ACP
command you run.

Each recipe below shows the `Config.Command` shape used to start the ACP stdio
agent. Add `SessionConfig`, `ReasoningEffort`, `MCPServers`, `Logger`, and
`Stderr` as needed for your application.

## OpenCode

```go
agentRuntime, err := acpagent.NewWithContext(ctx, acpagent.Config{
	Command:    []string{"opencode", "acp"},
	WorkingDir: "/workspace",
	Stderr:     os.Stderr,
	Logger:     logger,
})
```

OpenCode supports ACP directly through `opencode acp`. Optional model or mode
selection should use ACP session config values exposed by your installed
OpenCode ACP agent.

## Codex

```go
agentRuntime, err := acpagent.NewWithContext(ctx, acpagent.Config{
	Command:    []string{"npx", "-y", "@normahq/codex-acp-bridge@latest"},
	WorkingDir: "/workspace",
	Stderr:     os.Stderr,
	Logger:     logger,
})
```

Use `ReasoningEffort` for Codex ACP agents that support Codex reasoning
metadata. The adapter sends reasoning effort through
`session/new._meta.codex.config`.

## Claude Code

```go
agentRuntime, err := acpagent.NewWithContext(ctx, acpagent.Config{
	Command:    []string{"npx", "-y", "@zed-industries/claude-code-acp@latest"},
	WorkingDir: "/workspace",
	Stderr:     os.Stderr,
	Logger:     logger,
})
```

Claude Code ACP is started through the Zed ACP wrapper. Keep the npm package
version pinned in production if reproducibility matters.

## Pi

```go
agentRuntime, err := acpagent.NewWithContext(ctx, acpagent.Config{
	Command:    []string{"npx", "-y", "pi-acp"},
	WorkingDir: "/workspace",
	Stderr:     os.Stderr,
	Logger:     logger,
})
```

`pi-acp` is the ACP wrapper for the Pi coding agent. Install and configure Pi
separately with `@earendil-works/pi-coding-agent`, then run the wrapper with
`npx -y pi-acp`. If `pi-acp` is globally installed, `Command` can be
`[]string{"pi-acp"}` instead.

Pin the npm package version in production if reproducibility matters. Pi
session model selection uses the ACP `model` config option with values
advertised as `provider/model`. Pi thinking level uses the ACP `thought_level`
config option with values `off`, `minimal`, `low`, `medium`, `high`, or
`xhigh`.

## Generic ACP

Use this for any local executable or script that implements ACP over stdio.

```go
agentRuntime, err := acpagent.NewWithContext(ctx, acpagent.Config{
	Command:    []string{"/usr/local/bin/my-acp-agent", "--stdio"},
	WorkingDir: "/workspace",
	Stderr:     os.Stderr,
	Logger:     logger,
})
```

The command must speak ACP on stdin/stdout. Keep provider logs on stderr so they
do not corrupt the JSON-RPC stream.

## MCP Servers

ACP agents that accept MCP server definitions can receive them through
`Config.MCPServers`:

```go
agentRuntime, err := acpagent.NewWithContext(ctx, acpagent.Config{
	Command:    []string{"opencode", "acp"},
	WorkingDir: "/workspace",
	MCPServers: map[string]acpagent.MCPServerConfig{
		"tools": {
			Type: acpagent.MCPServerTypeStdio,
			Cmd:  []string{"my-mcp-server"},
			Args: []string{"--stdio"},
		},
	},
})
```

Supported MCP transports are stdio, HTTP, and SSE.
