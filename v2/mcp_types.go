package acpagent

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
