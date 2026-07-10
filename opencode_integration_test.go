//go:build integration && opencode

package acpagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	adkagent "google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const opencodeIntegrationTimeout = 90 * time.Second

func TestOpenCodeACPIntegration_InitializeAndNewSession(t *testing.T) {
	workingDir := requireOpenCodeEnvironment(t)
	client, stderr := newOpenCodeACPClient(t, workingDir, "acp")

	initResp := mustInitializeACP(t, client, stderr)
	if initResp.ProtocolVersion != acp.ProtocolVersion(acp.ProtocolVersionNumber) {
		t.Fatalf("initialize protocol version = %d, want %d", initResp.ProtocolVersion, acp.ProtocolVersionNumber)
	}
	_ = mustNewACPSession(t, client, stderr, workingDir)
}

func TestOpenCodeACPIntegration_NewSessionWithMCPServers(t *testing.T) {
	workingDir := requireOpenCodeEnvironment(t)
	client, stderr := newOpenCodeACPClient(t, workingDir, "acp")

	_ = mustInitializeACP(t, client, stderr)
	mcpCommand := opencodeMCPHelperCommand(t)
	mcpServers := []acp.McpServer{
		{
			Stdio: &acp.McpServerStdio{
				Name:    "runtime-opencode-mcp-helper",
				Command: mcpCommand[0],
				Args:    append([]string(nil), mcpCommand[1:]...),
				Env:     []acp.EnvVariable{},
			},
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), opencodeIntegrationTimeout)
	defer cancel()

	if _, err := client.NewSession(ctx, workingDir, mcpServers); err != nil {
		maybeSkipOpenCodeIntegration(t, err, stderr.String())
		failWithDetails(t, "session/new failed with mcpServers", err, stderr.String())
	}
}

func TestOpenCodeACPIntegration_AgentWithMCPServers(t *testing.T) {
	workingDir := requireOpenCodeEnvironment(t)
	configureOpenCodeWritableEnv(t)
	mcpCommand := opencodeMCPHelperCommand(t)

	var stderr bytes.Buffer
	agentWithMCP, err := NewWithContext(t.Context(), Config{
		Command:    []string{"opencode", "acp"},
		WorkingDir: workingDir,
		Stderr:     &stderr,
		MCPServers: map[string]MCPServerConfig{
			"runtime-opencode-mcp-helper": {
				Type: MCPServerTypeStdio,
				Cmd:  append([]string(nil), mcpCommand...),
			},
		},
	})
	if err != nil {
		maybeSkipOpenCodeIntegration(t, err, stderr.String())
		failWithDetails(t, "acpagent.New failed", err, stderr.String())
	}
	defer closeTestCloser(t, agentWithMCP)

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "opencode-acp-mcp-integration",
		Agent:          agentWithMCP,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}

	sess, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "opencode-acp-mcp-integration",
		UserID:  "integration-user",
	})
	if err != nil {
		t.Fatalf("session.Create() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), opencodeIntegrationTimeout)
	defer cancel()

	events := 0
	for _, runErr := range r.Run(ctx, "integration-user", sess.Session.ID(), genai.NewContentFromText("Reply with one short word.", genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			maybeSkipOpenCodeIntegration(t, runErr, stderr.String())
			failWithDetails(t, "runner.Run failed", runErr, stderr.String())
		}
		events++
	}
	if events == 0 {
		failWithDetails(t, "runner.Run produced no events", nil, stderr.String())
	}
}

func TestOpenCodeACPIntegration_PromptRoundTrip(t *testing.T) {
	workingDir := requireOpenCodeEnvironment(t)
	client, stderr := newOpenCodeACPClient(t, workingDir, "acp")

	_ = mustInitializeACP(t, client, stderr)
	sess := mustNewACPSession(t, client, stderr, workingDir)

	ctx, cancel := context.WithTimeout(t.Context(), opencodeIntegrationTimeout)
	defer cancel()

	updates, resultCh, err := client.Prompt(ctx, string(sess.SessionId), "Reply with one short word.")
	if err != nil {
		failWithDetails(t, "session/prompt failed to start", err, stderr.String())
	}

	updatesSeen := 0
	for range updates {
		updatesSeen++
	}
	result := <-resultCh
	if result.Err != nil {
		failWithDetails(t, "session/prompt returned error", result.Err, stderr.String())
	}
	if result.Response.StopReason == "" {
		failWithDetails(t, "session/prompt returned empty stop_reason", nil, stderr.String())
	}
	if updatesSeen == 0 {
		failWithDetails(t, "session/prompt produced no updates", nil, stderr.String())
	}
}

func TestOpenCodeACPIntegration_InvalidArgFailsInitialize(t *testing.T) {
	workingDir := requireOpenCodeEnvironment(t)
	client, stderr := newOpenCodeACPClient(t, workingDir, "--definitely-invalid-flag", "acp")

	ctx, cancel := context.WithTimeout(t.Context(), opencodeIntegrationTimeout)
	defer cancel()
	if _, err := client.Initialize(ctx); err == nil {
		failWithDetails(t, "initialize unexpectedly succeeded with invalid opencode arg", nil, stderr.String())
	}
}

func requireOpenCodeEnvironment(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("opencode"); err != nil {
		t.Fatalf("opencode binary not found in PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, "opencode", "acp", "--help")
	var helpOut bytes.Buffer
	helpCmd.Stdout = &helpOut
	helpCmd.Stderr = &helpOut
	if err := helpCmd.Run(); err != nil {
		t.Fatalf("opencode acp --help failed: %v | output=%s", err, strings.TrimSpace(helpOut.String()))
	}

	return findWorkingDir(t)
}

func findWorkingDir(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat go.mod failed in %q: %v", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate working directory containing go.mod (started from %q)", dir)
		}
		dir = parent
	}
}

func newOpenCodeACPClient(t *testing.T, workingDir string, commandArgs ...string) (*Client, *bytes.Buffer) {
	t.Helper()

	configureOpenCodeWritableEnv(t)

	command := append([]string{"opencode"}, commandArgs...)
	var stderr bytes.Buffer
	client, err := NewClient(t.Context(), ClientConfig{
		Command:    command,
		WorkingDir: workingDir,
		Stderr:     &stderr,
	})
	if err != nil {
		failWithDetails(t, "start ACP client failed", err, stderr.String())
	}
	defer closeTestCloser(t, client)
	return client, &stderr
}

func configureOpenCodeWritableEnv(t *testing.T) {
	t.Helper()

	xdgRoot := t.TempDir()
	xdgData := filepath.Join(xdgRoot, "data")
	xdgState := filepath.Join(xdgRoot, "state")
	xdgCache := filepath.Join(xdgRoot, "cache")
	for _, dir := range []string{xdgData, xdgState, xdgCache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create dir %q: %v", dir, err)
		}
	}

	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("XDG_STATE_HOME", xdgState)
	t.Setenv("XDG_CACHE_HOME", xdgCache)
}

func mustInitializeACP(t *testing.T, client *Client, stderr *bytes.Buffer) acp.InitializeResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), opencodeIntegrationTimeout)
	defer cancel()

	resp, err := client.Initialize(ctx)
	if err != nil {
		maybeSkipOpenCodeIntegration(t, err, stderr.String())
		failWithDetails(t, "initialize failed", err, stderr.String())
	}
	return resp
}

func mustNewACPSession(t *testing.T, client *Client, stderr *bytes.Buffer, cwd string) acp.NewSessionResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), opencodeIntegrationTimeout)
	defer cancel()

	resp, err := client.NewSession(ctx, cwd, nil)
	if err != nil {
		failWithDetails(t, "session/new failed", err, stderr.String())
	}
	if strings.TrimSpace(string(resp.SessionId)) == "" {
		failWithDetails(t, "session/new returned empty session id", nil, stderr.String())
	}
	return resp
}

func failWithDetails(t *testing.T, heading string, err error, stderr string) {
	t.Helper()

	errText := ""
	if err != nil {
		errText = strings.TrimSpace(err.Error())
	}
	stderrText := strings.TrimSpace(stderr)

	message := heading
	if errText != "" {
		message += ": " + errText
	}
	if stderrText != "" && (errText == "" || !strings.Contains(stderrText, errText)) {
		message += " | stderr: " + stderrText
	}
	t.Fatal(message)
}

func maybeSkipOpenCodeIntegration(t *testing.T, err error, stderr string) {
	t.Helper()

	errText := strings.ToLower(strings.TrimSpace(err.Error()))
	stderrText := strings.ToLower(strings.TrimSpace(stderr))
	combined := errText + "\n" + stderrText

	skipMarkers := []string{
		"peer disconnected before response",
		"failed to start server on port 0",
		"unable to connect. is the computer able to access the url?",
		"service=models.dev error=unable to connect",
	}
	for _, marker := range skipMarkers {
		if strings.Contains(combined, marker) {
			t.Skipf("opencode ACP unavailable in this environment (%s)", marker)
		}
	}
}

func opencodeMCPHelperCommand(t *testing.T) []string {
	t.Helper()
	return []string{"env", "GO_WANT_OPENCODE_MCP_HELPER=1", os.Args[0], "-test.run=TestOpenCodeMCPHelperProcess", "--"}
}

type openCodeMCPPingInput struct {
	Message string `json:"message"`
}

type openCodeMCPPingOutput struct {
	Message string `json:"message"`
}

func TestOpenCodeMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_OPENCODE_MCP_HELPER") != "1" {
		return
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "runtime-opencode-mcp-helper", Version: "v1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ping",
		Description: "Echoes ping messages for integration tests.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, input openCodeMCPPingInput) (*mcp.CallToolResult, openCodeMCPPingOutput, error) {
		msg := strings.TrimSpace(input.Message)
		if msg == "" {
			msg = "ping"
		}
		reply := "pong: " + msg
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: reply}},
		}, openCodeMCPPingOutput{Message: reply}, nil
	})

	if err := server.Run(t.Context(), &mcp.StdioTransport{}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "opencode mcp helper failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
