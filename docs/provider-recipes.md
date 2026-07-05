# Provider Recipes

`go-adk-acpagent` is provider-agnostic. Provider support comes from the ACP
command you run.

Each recipe below shows the `Config.Command` shape used to start the ACP stdio
agent. Add `Model`, `Mode`, `ReasoningEffort`, `MCPServers`, `Logger`, and
`Stderr` as needed for your application.

## Codex

Norma's Codex ACP integration uses the standalone Codex ACP bridge package.

```go
agentRuntime, err := acpagent.New(acpagent.Config{
	Command:       []string{"npx", "-y", "@normahq/codex-acp-bridge@latest"},
	WorkingDir:    "/workspace",
	Model:         "gpt-5-codex",
	ModelConfigID: "model",
	Stderr:        os.Stderr,
	Logger:        logger,
})
```

Use `ReasoningEffort` for Codex ACP agents that support Codex reasoning metadata.
The adapter sends reasoning effort through `session/new._meta.codex.config`.

## OpenCode

```go
agentRuntime, err := acpagent.New(acpagent.Config{
	Command:       []string{"opencode", "acp"},
	WorkingDir:    "/workspace",
	Model:         "opencode/big-pickle",
	ModelConfigID: "model",
	Mode:          "coding",
	Stderr:        os.Stderr,
	Logger:        logger,
})
```

OpenCode supports ACP directly through `opencode acp`. Use the model and mode
identifiers exposed by your installed OpenCode ACP agent.

## Claude Code

```go
agentRuntime, err := acpagent.New(acpagent.Config{
	Command:       []string{"npx", "-y", "@zed-industries/claude-code-acp@latest"},
	WorkingDir:    "/workspace",
	Model:         "claude-sonnet-4-20250514",
	ModelConfigID: "model",
	Stderr:        os.Stderr,
	Logger:        logger,
})
```

Claude Code ACP is started through the Zed ACP wrapper. Keep the npm package
version pinned in production if reproducibility matters.

## PI

No concrete PI ACP command is defined in the old Norma runtime configuration.
Treat PI as a generic ACP-compatible coding agent until the command is
standardized:

```go
agentRuntime, err := acpagent.New(acpagent.Config{
	Command:       []string{"pi-acp"},
	WorkingDir:    "/workspace",
	Model:         "pi-model-id",
	ModelConfigID: "model",
	Stderr:        os.Stderr,
	Logger:        logger,
})
```

Replace `pi-acp` and model identifiers with the actual PI ACP stdio command and
capability names.

## Generic ACP

Use this for any local executable or script that implements ACP over stdio.

```go
agentRuntime, err := acpagent.New(acpagent.Config{
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
agentRuntime, err := acpagent.New(acpagent.Config{
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
