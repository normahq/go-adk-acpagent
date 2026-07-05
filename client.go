package acpagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

var (
	// ErrPromptAlreadyActive is returned when a prompt is already in progress for
	// the same ACP session ID.
	ErrPromptAlreadyActive = errors.New("acp prompt already active")

	errSessionIDRequired = errors.New("acp session id is required")
	errPromptRequired    = errors.New("acp prompt is required")
	errPromptContentReq  = errors.New("acp prompt content is required")
	errModeRequired      = errors.New("acp mode is required")
)

const (
	defaultClientName    = "runtime-acpagent"
	defaultClientVersion = "dev"
	unknownValue         = "unknown"

	// idleUpdateWindow is the duration to wait for further updates before considering
	// a series of ACP updates complete.
	idleUpdateWindow = 20 * time.Millisecond
)

type promptContextKey string

const suppressLastChunkLogContextKey promptContextKey = "acpagent.suppress_last_chunk_log"

// PermissionHandler decides how ACP permission requests should be handled.
// It returns a response with the selected outcome or an error if the request
// could not be processed.
type PermissionHandler func(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)

// ClientConfig configures an ACP subprocess client.
type ClientConfig struct {
	// Command is the argv array used to start the ACP subprocess.
	Command []string
	// WorkingDir is the directory where the ACP subprocess is executed
	// (cmd.Dir). It is independent from ACP session/new.cwd, which is provided
	// per session creation call.
	WorkingDir string
	// ClientName is the name reported to the ACP server. Defaults to "runtime-acpagent".
	ClientName string
	// ClientVersion is the version reported to the ACP server. Defaults to "dev".
	ClientVersion string
	// Stderr is an optional writer for the ACP subprocess's standard error.
	Stderr io.Writer
	// PermissionHandler decides how to respond to ACP permission requests.
	PermissionHandler PermissionHandler
	// Logger is the slog logger to use for this client.
	Logger *slog.Logger
}

// ExtendedSessionNotification wraps an ACP notification with its raw JSON representation
// to allow access to fields not yet supported by the SDK.
type ExtendedSessionNotification struct {
	acp.SessionNotification
	Raw    json.RawMessage
	Method string
}

// Client manages a single Agent Client Protocol (ACP) subprocess and its
// communication over standard input/output. It implements the acp.Client interface
// to handle protocol-level callbacks and manages multiple concurrent prompt sessions.
type Client struct {
	ctx               context.Context
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	conn              *acp.ClientSideConnection
	permissionHandler PermissionHandler
	clientName        string
	clientVersion     string
	logger            logger

	stateMu         sync.Mutex
	activeBySession map[acp.SessionId]*activePrompt
	updates         chan ExtendedSessionNotification
	deactivate      chan acp.SessionId
	agentCaps       acp.AgentCapabilities

	closed       chan struct{}
	dispatchDone chan struct{}
	closeOnce    sync.Once
	closeErr     error
	closing      atomic.Bool
}

type activePrompt struct {
	sessionID acp.SessionId
	updates   chan ExtendedSessionNotification
	signal    chan struct{}
	logger    logger
	lastChunk *loggedACPChunk
	closeOnce sync.Once
}

type loggedACPChunk struct {
	kind         string
	contentBlock map[string]any
	partial      bool
	thought      bool
}

// PromptResult contains the terminal Prompt RPC response, usage metadata, or an error.
type PromptResult struct {
	Response acp.PromptResponse
	Usage    *acp.Usage
	Raw      json.RawMessage
	Err      error
}

var _ acp.Client = (*Client)(nil)

// NewClient starts an ACP subprocess and returns a protocol client over stdio.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("acp command is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	l := newLogger(cfg.Logger, "acpagent.client")
	clientName := strings.TrimSpace(cfg.ClientName)
	if clientName == "" {
		clientName = defaultClientName
	}
	clientVersion := strings.TrimSpace(cfg.ClientVersion)
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}

	stderr := cfg.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.WorkingDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("acp stdout pipe: %w", err)
	}
	cmd.Stderr = stderr

	l.Debug().
		Str("binary", cfg.Command[0]).
		Strs("args", cfg.Command[1:]).
		Str("cwd", cfg.WorkingDir).
		Msg("starting acp process")
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start acp process: %w", err)
	}
	if cmd.Process != nil {
		l.Debug().Int("pid", cmd.Process.Pid).Msg("acp process started")
	}

	c := &Client{
		ctx:               ctx,
		cmd:               cmd,
		stdin:             stdin,
		permissionHandler: cfg.PermissionHandler,
		clientName:        clientName,
		clientVersion:     clientVersion,
		logger:            l,
		activeBySession:   make(map[acp.SessionId]*activePrompt),
		updates:           make(chan ExtendedSessionNotification, 256),
		deactivate:        make(chan acp.SessionId, 256),
		closed:            make(chan struct{}),
		dispatchDone:      make(chan struct{}),
	}

	wireWriter := newWireLoggingWriter(stdin, l)
	wireReader := newWireLoggingReader(stdout, l, c.enqueueUpdateFromWire)
	c.conn = acp.NewClientSideConnection(c, wireWriter, wireReader)
	c.conn.SetLogger(l.slog())

	go c.dispatchUpdates()
	go c.waitLoop()
	return c, nil
}

func (c *Client) loggerForContext(ctx context.Context) logger {
	return loggerFromContext(ctx, c.logger, "acpagent.client")
}

// Initialize performs ACP protocol initialization and validates protocol
// compatibility.
func (c *Client) Initialize(ctx context.Context) (acp.InitializeResponse, error) {
	l := c.loggerForContext(ctx)
	l.Debug().Msg("sending acp initialize")
	resp, err := c.conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    c.clientName,
			Version: c.clientVersion,
		},
	})
	if err != nil {
		return acp.InitializeResponse{}, err
	}
	if resp.ProtocolVersion != acp.ProtocolVersion(acp.ProtocolVersionNumber) {
		return acp.InitializeResponse{}, fmt.Errorf("unsupported acp protocol version %d", resp.ProtocolVersion)
	}
	c.stateMu.Lock()
	c.agentCaps = resp.AgentCapabilities
	c.stateMu.Unlock()
	l.Debug().Int("protocol_version", int(resp.ProtocolVersion)).Msg("acp initialize succeeded")
	return resp, nil
}

// SupportsSessionLoad reports whether the initialized ACP server advertises
// session/load support.
func (c *Client) SupportsSessionLoad() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.agentCaps.LoadSession
}

// SupportsSessionResume reports whether the initialized ACP server advertises
// session/resume support.
func (c *Client) SupportsSessionResume() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.agentCaps.SessionCapabilities.Resume != nil
}

// Authenticate requests ACP authentication for a specific method.
func (c *Client) Authenticate(ctx context.Context, methodID string) error {
	if strings.TrimSpace(methodID) == "" {
		return nil
	}
	l := c.loggerForContext(ctx)
	l.Debug().Str("method_id", methodID).Msg("sending acp authenticate")
	_, err := c.conn.Authenticate(ctx, acp.AuthenticateRequest{MethodId: methodID})
	if err != nil {
		return err
	}
	l.Debug().Str("method_id", methodID).Msg("acp authenticate succeeded")
	return nil
}

// NewSession creates a new ACP session in the provided working directory.
//
// The ACP protocol requires cwd to be an absolute path.
func (c *Client) NewSession(ctx context.Context, cwd string, mcpServers []acp.McpServer) (acp.NewSessionResponse, error) {
	return c.NewSessionWithMeta(ctx, cwd, mcpServers, nil)
}

// NewSessionWithMeta creates a new ACP session in the provided working
// directory and sends optional _meta extensions with the session request.
//
// The ACP protocol requires cwd to be an absolute path.
func (c *Client) NewSessionWithMeta(
	ctx context.Context,
	cwd string,
	mcpServers []acp.McpServer,
	meta map[string]any,
) (acp.NewSessionResponse, error) {
	if mcpServers == nil {
		mcpServers = []acp.McpServer{}
	}

	req := acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: mcpServers,
	}
	if reqMeta := cloneAnyMap(meta); len(reqMeta) > 0 {
		req.Meta = reqMeta
	}

	l := c.loggerForContext(ctx)
	logEvent := l.Debug().
		Str("cwd", cwd).
		Int("mcp_servers", len(mcpServers))
	logEvent.Msg("sending acp session/new")

	resp, err := c.conn.NewSession(ctx, req)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	acpSessionID := strings.TrimSpace(string(resp.SessionId))
	if acpSessionID == "" {
		return acp.NewSessionResponse{}, fmt.Errorf("acp session id is empty")
	}
	l.Debug().Str("acp_session_id", acpSessionID).Msg("acp session/new succeeded")
	return resp, nil
}

func cloneAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

// CreateSession creates a new ACP session and applies configured session
// model/mode when requested.
func (c *Client) CreateSession(ctx context.Context, cwd, model, mode string, mcpServers []acp.McpServer) (acp.NewSessionResponse, error) {
	return c.CreateSessionWithMeta(ctx, cwd, model, mode, mcpServers, nil)
}

// ResumeSession resumes an existing ACP session in the provided working
// directory.
//
// The ACP protocol requires cwd to be an absolute path.
func (c *Client) ResumeSession(ctx context.Context, sessionID, cwd string, mcpServers []acp.McpServer) (acp.ResumeSessionResponse, error) {
	return c.ResumeSessionWithMeta(ctx, sessionID, cwd, mcpServers, nil)
}

// ResumeSessionWithMeta resumes an existing ACP session in the provided working
// directory and sends optional _meta extensions with the resume request.
//
// The ACP protocol requires cwd to be an absolute path.
func (c *Client) ResumeSessionWithMeta(
	ctx context.Context,
	sessionID,
	cwd string,
	mcpServers []acp.McpServer,
	meta map[string]any,
) (acp.ResumeSessionResponse, error) {
	trimmedSessionID, normalizedMCP, reqMeta, err := normalizeSessionRestoreInput(sessionID, mcpServers, meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	c.logSessionRestoreStart(ctx, "session/resume", trimmedSessionID, cwd, normalizedMCP)

	req := acp.ResumeSessionRequest{
		SessionId:  acp.SessionId(trimmedSessionID),
		Cwd:        cwd,
		McpServers: normalizedMCP,
	}
	if len(reqMeta) > 0 {
		req.Meta = reqMeta
	}
	resp, err := c.conn.ResumeSession(ctx, req)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	c.logSessionRestoreSuccess(ctx, "session/resume", trimmedSessionID)
	return resp, nil
}

// LoadSession loads an existing ACP session in the provided working directory.
//
// The ACP protocol requires cwd to be an absolute path.
func (c *Client) LoadSession(ctx context.Context, sessionID, cwd string, mcpServers []acp.McpServer) (acp.LoadSessionResponse, error) {
	return c.LoadSessionWithMeta(ctx, sessionID, cwd, mcpServers, nil)
}

// LoadSessionWithMeta loads an existing ACP session in the provided working
// directory and sends optional _meta extensions with the load request.
//
// The ACP protocol requires cwd to be an absolute path.
func (c *Client) LoadSessionWithMeta(
	ctx context.Context,
	sessionID,
	cwd string,
	mcpServers []acp.McpServer,
	meta map[string]any,
) (acp.LoadSessionResponse, error) {
	trimmedSessionID, normalizedMCP, reqMeta, err := normalizeSessionRestoreInput(sessionID, mcpServers, meta)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	c.logSessionRestoreStart(ctx, "session/load", trimmedSessionID, cwd, normalizedMCP)

	req := acp.LoadSessionRequest{
		SessionId:  acp.SessionId(trimmedSessionID),
		Cwd:        cwd,
		McpServers: normalizedMCP,
	}
	if len(reqMeta) > 0 {
		req.Meta = reqMeta
	}
	resp, err := c.conn.LoadSession(ctx, req)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	c.logSessionRestoreSuccess(ctx, "session/load", trimmedSessionID)
	return resp, nil
}

func normalizeSessionRestoreInput(
	sessionID string,
	mcpServers []acp.McpServer,
	meta map[string]any,
) (trimmedSessionID string, normalizedMCP []acp.McpServer, reqMeta map[string]any, err error) {
	trimmedSessionID = strings.TrimSpace(sessionID)
	if trimmedSessionID == "" {
		return "", nil, nil, errSessionIDRequired
	}
	normalizedMCP = mcpServers
	if normalizedMCP == nil {
		normalizedMCP = []acp.McpServer{}
	}
	reqMeta = cloneAnyMap(meta)
	return trimmedSessionID, normalizedMCP, reqMeta, nil
}

func (c *Client) logSessionRestoreStart(
	ctx context.Context,
	method string,
	sessionID string,
	cwd string,
	mcpServers []acp.McpServer,
) {
	l := c.loggerForContext(ctx)
	l.Debug().
		Str("acp_session_id", sessionID).
		Str("cwd", cwd).
		Int("mcp_servers", len(mcpServers)).
		Msg("sending acp " + method)
}

func (c *Client) logSessionRestoreSuccess(ctx context.Context, method, sessionID string) {
	l := c.loggerForContext(ctx)
	l.Debug().
		Str("acp_session_id", sessionID).
		Msg("acp " + method + " succeeded")
}

// CreateSessionWithMeta creates a new ACP session with optional session/new
// _meta and applies configured session model/mode when requested.
//
// This helper is equivalent to:
//  1. NewSessionWithMeta(ctx, cwd, mcpServers, meta)
//  2. optionally SetSessionConfigOption(...) for the ACP model option
//  3. optionally SetSessionMode(...)
//
// The ACP protocol requires cwd to be an absolute path.
func (c *Client) CreateSessionWithMeta(
	ctx context.Context,
	cwd,
	model,
	mode string,
	mcpServers []acp.McpServer,
	meta map[string]any,
) (acp.NewSessionResponse, error) {
	resp, err := c.NewSessionWithMeta(ctx, cwd, mcpServers, meta)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	if _, err := c.applySessionModelAndMode(ctx, string(resp.SessionId), model, "", mode, resp.ConfigOptions, resp.Modes); err != nil {
		return acp.NewSessionResponse{}, err
	}
	return resp, nil
}

func (c *Client) applySessionModelAndMode(
	ctx context.Context,
	sessionID,
	model,
	modelConfigID,
	mode string,
	configOptions []acp.SessionConfigOption,
	modes *acp.SessionModeState,
) (string, error) {
	l := c.loggerForContext(ctx)
	appliedModelConfigID := ""
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel != "" {
		resolvedModelConfigID, err := resolveModelConfigID(modelConfigID, configOptions)
		if err != nil {
			return "", err
		}
		_, err = c.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
			ValueId: &acp.SetSessionConfigOptionValueId{
				SessionId: acp.SessionId(sessionID),
				ConfigId:  acp.SessionConfigId(resolvedModelConfigID),
				Value:     acp.SessionConfigValueId(trimmedModel),
			},
		})
		if err != nil {
			return "", fmt.Errorf("set acp session model config option %q: %w", resolvedModelConfigID, err)
		}
		appliedModelConfigID = resolvedModelConfigID
	}

	trimmedMode := strings.TrimSpace(mode)
	if trimmedMode != "" {
		if err := c.SetSessionMode(ctx, sessionID, trimmedMode); err != nil {
			if isACPMethodNotFoundError(err) {
				l.Debug().
					Str("acp_session_id", sessionID).
					Str("mode", trimmedMode).
					Msg("acp session/set_mode unsupported; continuing")
			} else {
				return "", fmt.Errorf("set acp session mode: %w", err)
			}
		} else if modes != nil {
			modes.CurrentModeId = acp.SessionModeId(trimmedMode)
		}
	}
	return appliedModelConfigID, nil
}

func resolveModelConfigID(configID string, options []acp.SessionConfigOption) (string, error) {
	if trimmed := strings.TrimSpace(configID); trimmed != "" {
		return trimmed, nil
	}
	for _, option := range options {
		if option.Select == nil || option.Select.Category == nil {
			continue
		}
		if *option.Select.Category == acp.SessionConfigOptionCategoryModel && strings.TrimSpace(string(option.Select.Id)) != "" {
			return strings.TrimSpace(string(option.Select.Id)), nil
		}
	}
	for _, option := range options {
		if option.Select == nil {
			continue
		}
		if strings.TrimSpace(string(option.Select.Id)) == "model" {
			return "model", nil
		}
	}
	return "", errors.New("acp model config option is not available")
}

// SetSessionConfigOption sets an ACP session configuration option.
func (c *Client) SetSessionConfigOption(
	ctx context.Context,
	req acp.SetSessionConfigOptionRequest,
) (acp.SetSessionConfigOptionResponse, error) {
	l := c.loggerForContext(ctx)
	l.Debug().Msg("sending acp session/set_config_option")
	resp, err := c.conn.SetSessionConfigOption(ctx, req)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	l.Debug().Msg("acp session/set_config_option succeeded")
	return resp, nil
}

// SetSessionMode selects the active mode for an ACP session.
func (c *Client) SetSessionMode(ctx context.Context, sessionID, mode string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errSessionIDRequired
	}
	trimmedMode := strings.TrimSpace(mode)
	if trimmedMode == "" {
		return errModeRequired
	}

	l := c.loggerForContext(ctx)
	l.Debug().
		Str("acp_session_id", sessionID).
		Str("mode", trimmedMode).
		Msg("sending acp session/set_mode")
	_, err := c.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
		SessionId: acp.SessionId(sessionID),
		ModeId:    acp.SessionModeId(trimmedMode),
	})
	if err != nil {
		return err
	}
	l.Debug().
		Str("acp_session_id", sessionID).
		Str("mode", trimmedMode).
		Msg("acp session/set_mode succeeded")
	return nil
}

func isACPMethodNotFoundError(err error) bool {
	var reqErr *acp.RequestError
	return errors.As(err, &reqErr) && reqErr.Code == -32601
}

func isACPSessionNotFoundError(err error) bool {
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		return false
	}
	message := strings.ToLower(reqErr.Message)
	data := strings.ToLower(acpRequestErrorDataString(reqErr.Data))
	if strings.Contains(message, "not found") {
		return true
	}
	if strings.Contains(data, "session not found") {
		return true
	}
	// Codex ACP bridge now validates thread IDs as UUIDs during thread/resume.
	// Legacy persisted ACP session IDs like "session-1" should be treated as
	// stale restore state and replaced with a fresh session/new binding.
	return strings.Contains(message, "invalid thread id") ||
		strings.Contains(data, "invalid thread id") ||
		strings.Contains(message, "invalid session id") ||
		strings.Contains(data, "invalid session id")
}

func isACPSessionAlreadyExistsError(err error) bool {
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		return false
	}
	message := strings.ToLower(reqErr.Message)
	data := strings.ToLower(acpRequestErrorDataString(reqErr.Data))
	return strings.Contains(message, "already exists") ||
		strings.Contains(data, "already exists")
}

func acpRequestErrorDataString(data any) string {
	switch value := data.(type) {
	case nil:
		return ""
	case string:
		return value
	case []byte:
		return string(value)
	default:
		encoded, err := json.Marshal(value)
		if err == nil {
			return string(encoded)
		}
		return fmt.Sprint(value)
	}
}

// Prompt sends a prompt to an ACP session and streams session updates.
func (c *Client) Prompt(ctx context.Context, sessionID, prompt string) (<-chan ExtendedSessionNotification, <-chan PromptResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, errPromptRequired
	}
	return c.promptWithBlocks(ctx, sessionID, []acp.ContentBlock{acp.TextBlock(prompt)}, len(prompt))
}

// PromptWithContent sends a prompt composed of ACP content blocks and streams
// session updates.
func (c *Client) PromptWithContent(ctx context.Context, sessionID string, prompt []acp.ContentBlock) (<-chan ExtendedSessionNotification, <-chan PromptResult, error) {
	if len(prompt) == 0 {
		return nil, nil, errPromptContentReq
	}
	return c.promptWithBlocks(ctx, sessionID, prompt, 0)
}

func (c *Client) promptWithBlocks(
	ctx context.Context,
	sessionID string,
	prompt []acp.ContentBlock,
	promptLen int,
) (<-chan ExtendedSessionNotification, <-chan PromptResult, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil, errSessionIDRequired
	}

	c.stateMu.Lock()
	activeSessionID := acp.SessionId(sessionID)
	if c.activeBySession[activeSessionID] != nil {
		c.stateMu.Unlock()
		return nil, nil, ErrPromptAlreadyActive
	}
	updates := make(chan ExtendedSessionNotification, 64)
	l := c.loggerForContext(ctx)
	active := &activePrompt{sessionID: activeSessionID, updates: updates, signal: make(chan struct{}, 1), logger: l}
	c.activeBySession[activeSessionID] = active
	c.stateMu.Unlock()

	promptBlocks := append([]acp.ContentBlock(nil), prompt...)
	logEvent := l.Debug().
		Str("acp_session_id", sessionID).
		Str("prompt", renderACPContentBlocks(promptBlocks)).
		Int("prompt_blocks", len(promptBlocks))
	if promptLen > 0 {
		logEvent = logEvent.Int("prompt_len", promptLen)
	}
	logEvent.Msg("sending acp session/prompt")

	resultCh := make(chan PromptResult, 1)
	go func() {
		defer close(resultCh)
		defer c.clearActive(activeSessionID)

		resp, err := c.conn.Prompt(ctx, acp.PromptRequest{
			SessionId: activeSessionID,
			Prompt:    promptBlocks,
		})
		waitForUpdateIdle(ctx, active.signal)
		if !suppressLastChunkLogFromContext(ctx) {
			c.logLastChunkInSeries(activeSessionID)
		}
		if err != nil {
			resultCh <- PromptResult{Err: err}
			return
		}

		l.Debug().
			Str("acp_session_id", sessionID).
			Str("stop_reason", string(resp.StopReason)).
			Interface("usage", resp.Usage).
			Msg("acp session/prompt completed")
		resultCh <- PromptResult{Response: resp, Usage: resp.Usage, Raw: mustMarshalJSON(resp)}
	}()

	return updates, resultCh, nil
}

func suppressLastChunkLogFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	suppress, ok := ctx.Value(suppressLastChunkLogContextKey).(bool)
	return ok && suppress
}

func renderACPContentBlocks(blocks []acp.ContentBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		text := strings.TrimSpace(acpContentBlockLogText(block))
		if text != "" && text != unknownValue {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Close stops the ACP subprocess and waits for cleanup to finish.
func (c *Client) Close() error {
	c.closing.Store(true)
	if err := c.stdin.Close(); err != nil && !isBenignStdinCloseErr(err) {
		c.logger.Warn().Err(err).Msg("failed to close stdin")
	}
	select {
	case <-c.closed:
		return c.finalizeCloseErr()
	case <-time.After(200 * time.Millisecond):
	}
	if c.cmd.Process != nil {
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			c.logger.Warn().Err(err).Msg("failed to kill acp process")
		}
	}
	<-c.closed
	return c.finalizeCloseErr()
}

func (c *Client) waitLoop() {
	err := c.cmd.Wait()
	if err != nil {
		if c.closing.Load() {
			c.logger.Debug().Err(err).Msg("acp process exited during close")
			c.failAll(io.EOF)
			return
		}
		// If the context was cancelled, this is an expected termination.
		if c.ctx.Err() != nil {
			c.logger.Debug().Err(err).Msg("acp process exited due to context cancellation")
			c.failAll(c.ctx.Err())
			return
		}
		c.logger.Warn().Err(err).Msg("acp process exited with error")
		c.failAll(fmt.Errorf("acp process exit: %w", err))
		return
	}
	c.logger.Debug().Msg("acp process exited")
	c.failAll(io.EOF)
}

func (c *Client) finalizeCloseErr() error {
	if c.closeErr != nil && !errors.Is(c.closeErr, io.EOF) {
		return fmt.Errorf("acp client close: %w", c.closeErr)
	}
	return nil
}

func isBenignStdinCloseErr(err error) bool {
	return errors.Is(err, os.ErrClosed) || strings.Contains(strings.ToLower(err.Error()), "file already closed")
}

// RequestPermission handles ACP permission callbacks.
func (c *Client) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	l := c.loggerForContext(ctx)
	title := ""
	if params.ToolCall.Title != nil {
		title = *params.ToolCall.Title
	}
	l.Debug().
		Str("acp_session_id", string(params.SessionId)).
		Str("title", title).
		Int("option_count", len(params.Options)).
		Msg("received acp permission request")

	if c.permissionHandler != nil {
		resp, err := c.permissionHandler(ctx, params)
		if err != nil {
			l.Error().Err(err).Str("acp_session_id", string(params.SessionId)).Msg("permission handler failed")
			return acp.RequestPermissionResponse{}, err
		}
		l.Debug().
			Str("acp_session_id", string(params.SessionId)).
			Str("outcome", permissionOutcomeLabel(resp.Outcome)).
			Msg("permission handler returned")
		return resp, nil
	}

	for _, option := range params.Options {
		if option.Kind == acp.PermissionOptionKindRejectOnce || option.Kind == acp.PermissionOptionKindRejectAlways {
			resp := acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}
			l.Debug().
				Str("acp_session_id", string(params.SessionId)).
				Str("option_id", string(option.OptionId)).
				Str("option_kind", string(option.Kind)).
				Msg("permission auto-selected reject option")
			return resp, nil
		}
	}

	resp := acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}
	l.Debug().Str("acp_session_id", string(params.SessionId)).Msg("permission auto-cancelled")
	return resp, nil
}

// SessionUpdate is part of the ACP client callback contract.
func (c *Client) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	l := c.loggerForContext(ctx)
	logEvent := l.Trace().
		Str("acp_session_id", string(params.SessionId)).
		Str("update_kind", sessionUpdateKind(params.Update))
	logACPUpdateContentFields(logEvent, params.Update)
	logACPUpdateChunkFields(logEvent, params.Update)
	logEvent.Msg("received acp session update callback")
	return nil
}

// ReadTextFile reports unsupported file read for this ACP client.
func (c *Client) ReadTextFile(_ context.Context, _ acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}

// WriteTextFile reports unsupported file write for this ACP client.
func (c *Client) WriteTextFile(_ context.Context, _ acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}

// CreateTerminal reports unsupported terminal creation for this ACP client.
func (c *Client) CreateTerminal(_ context.Context, _ acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

// KillTerminal reports unsupported terminal command control for this ACP client.
func (c *Client) KillTerminal(_ context.Context, _ acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

// TerminalOutput reports unsupported terminal output streaming for this ACP client.
func (c *Client) TerminalOutput(_ context.Context, _ acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

// ReleaseTerminal reports unsupported terminal release for this ACP client.
func (c *Client) ReleaseTerminal(_ context.Context, _ acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

// WaitForTerminalExit reports unsupported terminal wait operations for this ACP client.
func (c *Client) WaitForTerminalExit(_ context.Context, _ acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

func (c *Client) clearActive(sessionID acp.SessionId) {
	select {
	case c.deactivate <- sessionID:
	case <-c.closed:
		<-c.dispatchDone
		c.closeActiveSession(sessionID)
	}
}

func (c *Client) logLastChunkInSeries(sessionID acp.SessionId) {
	c.stateMu.Lock()
	active := c.activeBySession[sessionID]
	var last *loggedACPChunk
	sessionLogger := c.logger
	if active != nil && active.lastChunk != nil {
		chunkCopy := *active.lastChunk
		last = &chunkCopy
	}
	if active != nil {
		sessionLogger = active.logger
	}
	c.stateMu.Unlock()
	if last == nil {
		return
	}

	logEvent := sessionLogger.Debug().
		Str("acp_session_id", string(sessionID)).
		Str("update_kind", last.kind).
		Bool("partial", last.partial).
		Bool("thought", last.thought).
		Bool("last_in_series", true)
	if last.contentBlock != nil {
		logEvent = logEvent.Interface("acp_content_block", last.contentBlock)
	}
	logEvent.Msg("received last acp chunk in series")
}

func (c *Client) failAll(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		close(c.closed)
		<-c.dispatchDone
		c.closeAllActiveSessions()
	})
}

func (c *Client) enqueueUpdateFromWire(ext ExtendedSessionNotification) {
	select {
	case c.updates <- ext:
	default:
		c.logger.Warn().Str("acp_session_id", string(ext.SessionId)).Msg("dropping ordered wire update due to full buffer")
	}
}

func (c *Client) dispatchUpdates() {
	defer close(c.dispatchDone)
	for {
		select {
		case <-c.closed:
			return
		case sessionID := <-c.deactivate:
			c.closeActiveSession(sessionID)
		case ext := <-c.updates:
			c.dispatchSessionUpdate(ext)
		}
	}
}

func (c *Client) closeActiveSession(sessionID acp.SessionId) {
	c.stateMu.Lock()
	active := c.activeBySession[sessionID]
	delete(c.activeBySession, sessionID)
	c.stateMu.Unlock()
	if active != nil {
		active.closeOnce.Do(func() {
			close(active.updates)
		})
	}
}

func (c *Client) closeAllActiveSessions() {
	c.stateMu.Lock()
	active := make([]*activePrompt, 0, len(c.activeBySession))
	for sessionID, prompt := range c.activeBySession {
		active = append(active, prompt)
		delete(c.activeBySession, sessionID)
	}
	c.stateMu.Unlock()
	for _, prompt := range active {
		if prompt == nil {
			continue
		}
		prompt.closeOnce.Do(func() {
			close(prompt.updates)
		})
	}
}

func (c *Client) dispatchSessionUpdate(ext ExtendedSessionNotification) {
	updateType := sessionUpdateKind(ext.Update)
	c.stateMu.Lock()
	active := c.activeBySession[ext.SessionId]
	if active != nil {
		active.lastChunk = loggedACPChunkFromUpdate(ext.Update)
	}
	c.stateMu.Unlock()

	sessionLogger := c.logger
	if active != nil {
		sessionLogger = active.logger
	}
	logEvent := sessionLogger.Trace().
		Str("acp_session_id", string(ext.SessionId)).
		Str("update_kind", updateType)

	if updateType == unknownValue {
		logEvent = logEvent.RawJSON("raw_update", ext.Raw)
	}

	logACPUpdateContentFields(logEvent, ext.Update)
	logACPUpdateChunkFields(logEvent, ext.Update)
	logEvent.Msg("received acp session update")

	if active == nil {
		return
	}
	select {
	case active.updates <- ext:
		select {
		case active.signal <- struct{}{}:
		default:
		}
	case <-c.closed:
	}
}

func permissionOutcomeLabel(outcome acp.RequestPermissionOutcome) string {
	switch {
	case outcome.Selected != nil:
		return "selected"
	case outcome.Cancelled != nil:
		return "cancelled"
	default:
		return unknownValue
	}
}

func sessionUpdateKind(update acp.SessionUpdate) string {
	switch {
	case update.AgentMessageChunk != nil:
		return "agent_message_chunk"
	case update.UserMessageChunk != nil:
		return "user_message_chunk"
	case update.AgentThoughtChunk != nil:
		return "agent_thought_chunk"
	case update.ToolCall != nil:
		return "tool_call"
	case update.ToolCallUpdate != nil:
		return "tool_call_update"
	case update.Plan != nil:
		return "plan"
	case update.AvailableCommandsUpdate != nil:
		return "available_commands_update"
	case update.CurrentModeUpdate != nil:
		return "current_mode_update"
	case update.ConfigOptionUpdate != nil:
		return "config_option_update"
	case update.SessionInfoUpdate != nil:
		return "session_info_update"
	case update.UsageUpdate != nil:
		return "usage_update"
	default:
		return unknownValue
	}
}

func logACPUpdateContentFields(event *logEvent, update acp.SessionUpdate) {
	if event == nil {
		return
	}
	switch {
	case update.AgentMessageChunk != nil:
		event.Interface("acp_content_block", acpContentBlockLogValue(update.AgentMessageChunk.Content))
	case update.UserMessageChunk != nil:
		event.Interface("acp_content_block", acpContentBlockLogValue(update.UserMessageChunk.Content))
	case update.AgentThoughtChunk != nil:
		event.Interface("acp_content_block", acpContentBlockLogValue(update.AgentThoughtChunk.Content))
	}
}

func logACPUpdateChunkFields(event *logEvent, update acp.SessionUpdate) {
	if event == nil {
		return
	}
	switch {
	case update.AgentMessageChunk != nil:
		event.Bool("partial", true).Bool("thought", false)
	case update.AgentThoughtChunk != nil:
		event.Bool("partial", true).Bool("thought", true)
	case update.UserMessageChunk != nil:
		event.Bool("partial", true).Bool("thought", false)
	}
}

func loggedACPChunkFromUpdate(update acp.SessionUpdate) *loggedACPChunk {
	switch {
	case update.AgentMessageChunk != nil:
		return &loggedACPChunk{
			kind:         "agent_message_chunk",
			contentBlock: acpContentBlockLogValue(update.AgentMessageChunk.Content),
			partial:      true,
			thought:      false,
		}
	case update.AgentThoughtChunk != nil:
		return &loggedACPChunk{
			kind:         "agent_thought_chunk",
			contentBlock: acpContentBlockLogValue(update.AgentThoughtChunk.Content),
			partial:      true,
			thought:      true,
		}
	case update.UserMessageChunk != nil:
		return &loggedACPChunk{
			kind:         "user_message_chunk",
			contentBlock: acpContentBlockLogValue(update.UserMessageChunk.Content),
			partial:      true,
			thought:      false,
		}
	default:
		return nil
	}
}

func waitForUpdateIdle(ctx context.Context, signal <-chan struct{}) {
	timer := time.NewTimer(idleUpdateWindow)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-signal:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleUpdateWindow)
		case <-timer.C:
			return
		}
	}
}

type wireLoggingWriter struct {
	writer io.Writer
	buffer *wireLogBuffer
}

func newWireLoggingWriter(writer io.Writer, logger logger) io.Writer {
	return &wireLoggingWriter{writer: writer, buffer: newWireLogBuffer("send", logger, nil)}
}

func (w *wireLoggingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.buffer.append(p[:n])
	}
	if err != nil {
		w.buffer.logger.Warn().Err(err).Msg("failed to write acp stream")
	}
	return n, err
}

type wireLoggingReader struct {
	reader io.Reader
	buffer *wireLogBuffer
}

func newWireLoggingReader(reader io.Reader, logger logger, onSessionUpdate func(ExtendedSessionNotification)) io.Reader {
	return &wireLoggingReader{reader: reader, buffer: newWireLogBuffer("recv", logger, onSessionUpdate)}
}

func (r *wireLoggingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.buffer.append(p[:n])
	}
	if err != nil && !errors.Is(err, io.EOF) {
		r.buffer.logger.Warn().Err(err).Msg("failed to read acp stream")
	}
	return n, err
}

type wireLogBuffer struct {
	direction string
	logger    logger
	onUpdate  func(ExtendedSessionNotification)

	mu  sync.Mutex
	buf []byte
}

func newWireLogBuffer(direction string, logger logger, onUpdate func(ExtendedSessionNotification)) *wireLogBuffer {
	return &wireLogBuffer{direction: direction, logger: logger, onUpdate: onUpdate}
}

func (b *wireLogBuffer) append(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, chunk...)
	for {
		idx := bytes.IndexByte(b.buf, '\n')
		if idx < 0 {
			return
		}
		line := bytes.TrimSpace(b.buf[:idx])
		b.buf = b.buf[idx+1:]
		if len(line) == 0 {
			continue
		}
		b.logLine(line)
	}
}

func (b *wireLogBuffer) logLine(line []byte) {
	type wireEnvelope struct {
		Method string          `json:"method,omitempty"`
		ID     json.RawMessage `json:"id,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	var env wireEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		b.logger.Warn().
			Str("direction", b.direction).
			Err(err).
			Msg("failed to decode acp wire payload")
		return
	}

	if b.direction == "recv" && b.onUpdate != nil && len(env.Params) > 0 {
		switch env.Method {
		case acp.ClientMethodSessionUpdate:
			var note acp.SessionNotification
			if err := json.Unmarshal(env.Params, &note); err == nil {
				b.onUpdate(ExtendedSessionNotification{
					SessionNotification: note,
					Raw:                 env.Params,
					Method:              env.Method,
				})
			} else {
				b.logger.Warn().Err(err).Msg("failed to decode ordered session update")
			}
		default:
			if sessionID, ok := extractPromptScopedSessionID(env.Params); ok {
				b.onUpdate(ExtendedSessionNotification{
					SessionNotification: acp.SessionNotification{SessionId: acp.SessionId(sessionID)},
					Raw:                 env.Params,
					Method:              env.Method,
				})
			}
		}
	}

	kind := unknownValue
	switch {
	case env.Method != "" && len(env.ID) > 0:
		kind = "request"
	case env.Method != "":
		kind = "notification"
	case len(env.ID) > 0:
		kind = "response"
	}

	evt := b.logger.Trace().
		Str("direction", b.direction).
		Str("rpc_kind", kind)
	if env.Method != "" {
		evt = evt.Str("method", env.Method)
	}
	if len(env.ID) > 0 {
		evt = evt.Str("id", strings.TrimSpace(string(env.ID)))
	}
	if len(env.Params) > 0 {
		evt = evt.RawJSON("params", env.Params)
	}
	if len(env.Result) > 0 {
		evt = evt.RawJSON("result", env.Result)
	}
	if env.Error != nil {
		evt = evt.Int("error_code", env.Error.Code).Str("error_message", env.Error.Message)
	}
	evt.Msg("acp wire")
}

func extractPromptScopedSessionID(raw json.RawMessage) (string, bool) {
	var params struct {
		SessionID string `json:"sessionId"`
		ThreadID  string `json:"threadId"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", false
	}
	switch {
	case strings.TrimSpace(params.SessionID) != "":
		return strings.TrimSpace(params.SessionID), true
	case strings.TrimSpace(params.ThreadID) != "":
		return strings.TrimSpace(params.ThreadID), true
	default:
		return "", false
	}
}

func mustMarshalJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
