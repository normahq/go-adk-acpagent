package acpagent

import (
	"fmt"
	"sort"

	acp "github.com/coder/acp-go-sdk"
)

// MCPServerType represents the transport type for an MCP server.
type MCPServerType string

const (
	// MCPServerTypeStdio is the stdio transport type.
	MCPServerTypeStdio MCPServerType = "stdio"
	// MCPServerTypeHTTP is the HTTP transport type.
	MCPServerTypeHTTP MCPServerType = "http"
	// MCPServerTypeSSE is the SSE (Server-Sent Events) transport type.
	MCPServerTypeSSE MCPServerType = "sse"
)

// MCPServerConfig describes how to connect to an MCP server.
type MCPServerConfig struct {
	// Type selects the MCP transport implementation.
	Type MCPServerType
	// Cmd is the stdio server executable path or argv prefix.
	Cmd []string
	// Args appends additional stdio server arguments after Cmd.
	Args []string
	// Env defines environment variables for stdio server execution.
	Env map[string]string
	// WorkingDir sets the stdio server process working directory.
	WorkingDir string
	// URL is the base endpoint for HTTP and SSE MCP transports.
	URL string
	// Headers provides additional request headers for HTTP and SSE transports.
	Headers map[string]string
}

func convertMCPServers(configs map[string]MCPServerConfig) ([]acp.McpServer, error) {
	if len(configs) == 0 {
		return nil, nil
	}
	servers := make([]acp.McpServer, 0, len(configs))
	keys := make([]string, 0, len(configs))
	for k := range configs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, name := range keys {
		cfg := configs[name]
		svr := acp.McpServer{}
		switch cfg.Type {
		case MCPServerTypeStdio:
			if len(cfg.Cmd) == 0 {
				return nil, fmt.Errorf("mcp server %q: stdio type requires command", name)
			}
			svr.Stdio = &acp.McpServerStdio{
				Name:    name,
				Command: cfg.Cmd[0],
				Env:     envToEnvVars(cfg.Env),
			}
			if len(cfg.Cmd) > 1 {
				svr.Stdio.Args = make([]string, 0, len(cfg.Cmd)-1+len(cfg.Args))
				svr.Stdio.Args = append(svr.Stdio.Args, cfg.Cmd[1:]...)
				svr.Stdio.Args = append(svr.Stdio.Args, cfg.Args...)
			} else {
				// ACP servers like OpenCode reject null for required array fields.
				svr.Stdio.Args = append(make([]string, 0, len(cfg.Args)), cfg.Args...)
			}
		case MCPServerTypeHTTP:
			svr.Http = &acp.McpServerHttpInline{
				Name:    name,
				Type:    "http",
				Url:     cfg.URL,
				Headers: headersToHttpHeaders(cfg.Headers),
			}
		case MCPServerTypeSSE:
			svr.Sse = &acp.McpServerSseInline{
				Name:    name,
				Type:    "sse",
				Url:     cfg.URL,
				Headers: headersToHttpHeaders(cfg.Headers),
			}
		default:
			return nil, fmt.Errorf("unsupported mcp server type %q", cfg.Type)
		}
		servers = append(servers, svr)
	}
	return servers, nil
}

func envToEnvVars(env map[string]string) []acp.EnvVariable {
	vars := make([]acp.EnvVariable, 0, len(env))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vars = append(vars, acp.EnvVariable{Name: k, Value: env[k]})
	}
	return vars
}

func headersToHttpHeaders(headers map[string]string) []acp.HttpHeader {
	h := make([]acp.HttpHeader, 0, len(headers))
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h = append(h, acp.HttpHeader{Name: k, Value: headers[k]})
	}
	return h
}
