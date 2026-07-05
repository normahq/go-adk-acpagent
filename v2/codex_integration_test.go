//go:build integration && codex

package acpagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	adkagent "google.golang.org/adk/v2/agent"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const codexIntegrationTimeout = 90 * time.Second

func TestCodexACPIntegration_InitializeAndNewSession(t *testing.T) {
	workingDir := requireCodexEnvironment(t)
	client, stderr := newCodexACPClient(t, workingDir)

	initResp := mustInitializeCodexACP(t, client, stderr)
	if initResp.ProtocolVersion != acp.ProtocolVersion(acp.ProtocolVersionNumber) {
		t.Fatalf("initialize protocol version = %d, want %d", initResp.ProtocolVersion, acp.ProtocolVersionNumber)
	}
	_ = mustNewCodexACPSession(t, client, stderr, workingDir)
}

func TestCodexACPIntegration_PromptRoundTrip(t *testing.T) {
	workingDir := requireCodexEnvironment(t)
	client, stderr := newCodexACPClient(t, workingDir)

	_ = mustInitializeCodexACP(t, client, stderr)
	sess := mustNewCodexACPSession(t, client, stderr, workingDir)

	ctx, cancel := context.WithTimeout(context.Background(), codexIntegrationTimeout)
	defer cancel()

	updates, resultCh, err := client.Prompt(ctx, string(sess.SessionId), "Reply with one short word.")
	if err != nil {
		maybeSkipCodexIntegration(t, err, stderr.String())
		failCodexWithDetails(t, "session/prompt failed to start", err, stderr.String())
	}

	updatesSeen := 0
	for range updates {
		updatesSeen++
	}
	result := <-resultCh
	if result.Err != nil {
		maybeSkipCodexIntegration(t, result.Err, stderr.String())
		failCodexWithDetails(t, "session/prompt returned error", result.Err, stderr.String())
	}
	if result.Response.StopReason == "" {
		failCodexWithDetails(t, "session/prompt returned empty stop_reason", nil, stderr.String())
	}
	if updatesSeen == 0 {
		failCodexWithDetails(t, "session/prompt produced no updates", nil, stderr.String())
	}
}

func TestCodexACPIntegration_ResumeSessionRoundTrip(t *testing.T) {
	workingDir := requireCodexEnvironment(t)
	client, stderr := newCodexACPClient(t, workingDir)

	initResp := mustInitializeCodexACP(t, client, stderr)
	if initResp.AgentCapabilities.SessionCapabilities.Resume == nil {
		t.Skip("codex ACP agent does not advertise session/resume capability")
	}

	sess := mustNewCodexACPSession(t, client, stderr, workingDir)

	ctx, cancel := context.WithTimeout(context.Background(), codexIntegrationTimeout)
	defer cancel()
	if _, err := client.ResumeSessionWithMeta(ctx, string(sess.SessionId), workingDir, nil, map[string]any{
		"resume_reason": "integration-test",
	}); err != nil {
		failCodexWithDetails(t, "session/resume failed", err, stderr.String())
	}

	updates, resultCh, err := client.Prompt(ctx, string(sess.SessionId), "Reply with one short word.")
	if err != nil {
		failCodexWithDetails(t, "session/prompt failed to start after session/resume", err, stderr.String())
	}

	updatesSeen := 0
	for range updates {
		updatesSeen++
	}
	result := <-resultCh
	if result.Err != nil {
		failCodexWithDetails(t, "session/prompt returned error after session/resume", result.Err, stderr.String())
	}
	if result.Response.StopReason == "" {
		failCodexWithDetails(t, "session/prompt returned empty stop_reason after session/resume", nil, stderr.String())
	}
	if updatesSeen == 0 {
		failCodexWithDetails(t, "session/prompt produced no updates after session/resume", nil, stderr.String())
	}
}

func TestCodexACPIntegration_AgentRun(t *testing.T) {
	workingDir := requireCodexEnvironment(t)

	var stderr bytes.Buffer
	agentWithCodex, err := New(Config{
		Context:    context.Background(),
		Command:    codexACPCommand(),
		WorkingDir: workingDir,
		Stderr:     &stderr,
	})
	if err != nil {
		maybeSkipCodexIntegration(t, err, stderr.String())
		failCodexWithDetails(t, "acpagent.New failed", err, stderr.String())
	}
	t.Cleanup(func() {
		_ = agentWithCodex.Close()
	})

	sessionService := session.InMemoryService()
	r, err := runnerpkg.New(runnerpkg.Config{
		AppName:        "codex-acp-integration",
		Agent:          agentWithCodex,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}

	sess, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "codex-acp-integration",
		UserID:  "integration-user",
	})
	if err != nil {
		t.Fatalf("session.Create() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexIntegrationTimeout)
	defer cancel()

	events := 0
	turnComplete := false
	finalText := ""
	for ev, runErr := range r.Run(ctx, "integration-user", sess.Session.ID(), genai.NewContentFromText("Reply with one short word.", genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			maybeSkipCodexIntegration(t, runErr, stderr.String())
			failCodexWithDetails(t, "runner.Run failed", runErr, stderr.String())
		}
		if ev == nil {
			continue
		}
		events++
		if ev.TurnComplete {
			turnComplete = true
			finalText = strings.TrimSpace(extractPromptText(ev.Content))
			if ev.Partial {
				failCodexWithDetails(t, "turn complete event was partial", nil, stderr.String())
			}
		}
	}
	if events == 0 {
		failCodexWithDetails(t, "runner.Run produced no events", nil, stderr.String())
	}
	if !turnComplete {
		failCodexWithDetails(t, "runner.Run did not produce a turn complete event", nil, stderr.String())
	}
	if finalText == "" {
		failCodexWithDetails(t, "runner.Run turn complete event had no final text", nil, stderr.String())
	}
}

func requireCodexEnvironment(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("npx"); err != nil {
		t.Skipf("npx not found in PATH: %v", err)
	}

	return findCodexWorkingDir(t)
}

func newCodexACPClient(t *testing.T, workingDir string) (*Client, *bytes.Buffer) {
	t.Helper()

	var stderr bytes.Buffer
	client, err := NewClient(context.Background(), ClientConfig{
		Command:    codexACPCommand(),
		WorkingDir: workingDir,
		Stderr:     &stderr,
	})
	if err != nil {
		maybeSkipCodexIntegration(t, err, stderr.String())
		failCodexWithDetails(t, "start ACP client failed", err, stderr.String())
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client, &stderr
}

func codexACPCommand() []string {
	return []string{"npx", "-y", "@normahq/codex-acp-bridge"}
}

func mustInitializeCodexACP(t *testing.T, client *Client, stderr *bytes.Buffer) acp.InitializeResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), codexIntegrationTimeout)
	defer cancel()

	resp, err := client.Initialize(ctx)
	if err != nil {
		maybeSkipCodexIntegration(t, err, stderr.String())
		failCodexWithDetails(t, "initialize failed", err, stderr.String())
	}
	return resp
}

func mustNewCodexACPSession(t *testing.T, client *Client, stderr *bytes.Buffer, cwd string) acp.NewSessionResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), codexIntegrationTimeout)
	defer cancel()

	resp, err := client.NewSession(ctx, cwd, nil)
	if err != nil {
		maybeSkipCodexIntegration(t, err, stderr.String())
		failCodexWithDetails(t, "session/new failed", err, stderr.String())
	}
	if strings.TrimSpace(string(resp.SessionId)) == "" {
		failCodexWithDetails(t, "session/new returned empty session id", nil, stderr.String())
	}
	return resp
}

func findCodexWorkingDir(t *testing.T) string {
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

func failCodexWithDetails(t *testing.T, heading string, err error, stderr string) {
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

func maybeSkipCodexIntegration(t *testing.T, err error, stderr string) {
	t.Helper()
	if err == nil {
		return
	}

	errText := strings.ToLower(strings.TrimSpace(err.Error()))
	stderrText := strings.ToLower(strings.TrimSpace(stderr))
	combined := errText + "\n" + stderrText

	skipMarkers := []string{
		"401",
		"429",
		"api key",
		"authentication",
		"could not determine executable to run",
		"econnrefused",
		"enotfound",
		"etimedout",
		"network",
		"not logged in",
		"npm err!",
		"npm error",
		"openai_api_key",
		"peer disconnected before response",
		"rate limit",
	}
	for _, marker := range skipMarkers {
		if strings.Contains(combined, marker) {
			t.Skipf("codex ACP bridge unavailable in this environment (%s)", marker)
		}
	}
}
