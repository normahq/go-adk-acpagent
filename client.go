package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// ProtocolPermissionHandler handles raw ACP permission callbacks for the
// low-level Client API.
// During an active prompt, ctx is the context passed to Prompt or
// PromptWithContent for the request's ACP session.
type ProtocolPermissionHandler func(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)

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
	PermissionHandler ProtocolPermissionHandler
	// Logger is the slog logger to use for this client.
	// Trace-level records can contain complete ACP payloads and other sensitive
	// content.
	Logger *slog.Logger
}

// ExtendedSessionNotification wraps an ACP notification with its raw JSON representation
// to allow access to fields not yet supported by the SDK.
type ExtendedSessionNotification struct {
	acp.SessionNotification
	// Raw is the original JSON notification payload.
	Raw json.RawMessage
	// Method is the JSON-RPC notification method.
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
	permissionHandler ProtocolPermissionHandler
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
	// Response is the terminal ACP prompt response when Err is nil.
	Response acp.PromptResponse
	// Usage is the token usage reported with Response, when available.
	Usage *acp.Usage
	// Raw is the JSON representation of Response.
	Raw json.RawMessage
	// Err is an asynchronous prompt error. Synchronous validation errors are
	// returned directly from Prompt or PromptWithContent.
	Err error
}

var _ acp.Client = (*Client)(nil)

// NewClient starts an ACP subprocess and returns a protocol client over stdio.
// The caller must call [Client.Close] to release the subprocess and associated
// resources.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("acp command is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	l := newLogger(cfg.Logger, "acpagent.client").withContext(ctx)
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
	gatedReader, releaseReader := newConnectionStartReader(wireReader)
	c.conn = acp.NewClientSideConnection(c, wireWriter, gatedReader)
	c.conn.SetLogger(l.slog())
	releaseReader()

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
func (c *Client) NewSessionWithMeta(ctx context.Context, cwd string, mcpServers []acp.McpServer, meta map[string]any) (acp.NewSessionResponse, error) {
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
// config values when requested.
func (c *Client) CreateSession(ctx context.Context, cwd string, configValues []SessionConfigValue, mcpServers []acp.McpServer) (acp.NewSessionResponse, error) {
	return c.CreateSessionWithMeta(ctx, cwd, configValues, mcpServers, nil)
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
func (c *Client) ResumeSessionWithMeta(ctx context.Context, sessionID, cwd string, mcpServers []acp.McpServer, meta map[string]any) (acp.ResumeSessionResponse, error) {
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
func (c *Client) LoadSessionWithMeta(ctx context.Context, sessionID, cwd string, mcpServers []acp.McpServer, meta map[string]any) (acp.LoadSessionResponse, error) {
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

func normalizeSessionRestoreInput(sessionID string, mcpServers []acp.McpServer, meta map[string]any) (trimmedSessionID string, normalizedMCP []acp.McpServer, reqMeta map[string]any, err error) {
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

func (c *Client) logSessionRestoreStart(ctx context.Context, method, sessionID, cwd string, mcpServers []acp.McpServer) {
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
// _meta and applies configured session config values when requested.
//
// This helper is equivalent to:
//  1. NewSessionWithMeta(ctx, cwd, mcpServers, meta)
//  2. optionally SetSessionConfigOption(...) for ACP session config values
//
// The ACP protocol requires cwd to be an absolute path.
func (c *Client) CreateSessionWithMeta(ctx context.Context, cwd string, configValues []SessionConfigValue, mcpServers []acp.McpServer, meta map[string]any) (acp.NewSessionResponse, error) {
	resp, err := c.NewSessionWithMeta(ctx, cwd, mcpServers, meta)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	if _, err := c.applySessionConfig(ctx, string(resp.SessionId), configValues, resp.ConfigOptions, resp.Modes); err != nil {
		return acp.NewSessionResponse{}, err
	}
	return resp, nil
}

func (c *Client) applySessionConfig(ctx context.Context, sessionID string, values []SessionConfigValue, configOptions []acp.SessionConfigOption, modes *acp.SessionModeState) ([]SessionConfigValue, error) {
	l := c.loggerForContext(ctx)
	currentOptions := configOptions
	for _, value := range normalizeSessionConfigValues(values) {
		optionID := strings.TrimSpace(value.ID)
		option := findSessionConfigOption(currentOptions, optionID)
		if option != nil {
			req, err := buildSetSessionConfigOptionRequest(sessionID, optionID, value, *option)
			if err != nil {
				return nil, err
			}
			resp, err := c.SetSessionConfigOption(ctx, req)
			if err != nil {
				if isACPMethodNotFoundError(err) {
					l.Warn().
						Str("acp_session_id", sessionID).
						Str("config_option", optionID).
						Msg("acp session/set_config_option unsupported; continuing")
					continue
				}
				return nil, fmt.Errorf("set acp session config option %q: %w", optionID, err)
			}
			currentOptions = resp.ConfigOptions
			continue
		}
		if optionID == "mode" && modes != nil {
			optionValue := strings.TrimSpace(value.Value)
			if optionValue == "" {
				return nil, fmt.Errorf("acp session mode value is required")
			}
			if err := c.SetSessionMode(ctx, sessionID, optionValue); err != nil {
				if isACPMethodNotFoundError(err) {
					l.Warn().
						Str("acp_session_id", sessionID).
						Str("mode", optionValue).
						Msg("acp session/set_mode unsupported; continuing")
					continue
				}
				return nil, fmt.Errorf("set acp session mode: %w", err)
			}
			modes.CurrentModeId = acp.SessionModeId(optionValue)
			continue
		}
		l.Warn().
			Str("acp_session_id", sessionID).
			Str("config_option", optionID).
			Msg("acp session config option unavailable; continuing")
	}
	return collectSessionConfigValues(currentOptions, modes), nil
}

func hasSessionConfigOption(options []acp.SessionConfigOption, id string) bool {
	return findSessionConfigOption(options, id) != nil
}

func findSessionConfigOption(options []acp.SessionConfigOption, id string) *acp.SessionConfigOption {
	for _, option := range options {
		if option.Select != nil && strings.TrimSpace(string(option.Select.Id)) == strings.TrimSpace(id) {
			return &option
		}
		if option.Boolean != nil && strings.TrimSpace(string(option.Boolean.Id)) == strings.TrimSpace(id) {
			return &option
		}
	}
	return nil
}

func buildSetSessionConfigOptionRequest(sessionID, optionID string, value SessionConfigValue, option acp.SessionConfigOption) (acp.SetSessionConfigOptionRequest, error) {
	if option.Boolean != nil {
		if value.BoolValue == nil {
			return acp.SetSessionConfigOptionRequest{}, fmt.Errorf("acp session config option %q requires a boolean value", optionID)
		}
		return acp.SetSessionConfigOptionRequest{
			Boolean: &acp.SetSessionConfigOptionBoolean{
				SessionId: acp.SessionId(sessionID),
				ConfigId:  acp.SessionConfigId(optionID),
				Type:      "boolean",
				Value:     *value.BoolValue,
			},
		}, nil
	}
	if option.Select != nil {
		optionValue := strings.TrimSpace(value.Value)
		if optionValue == "" {
			return acp.SetSessionConfigOptionRequest{}, fmt.Errorf("acp session config option %q requires a select value", optionID)
		}
		return acp.SetSessionConfigOptionRequest{
			ValueId: &acp.SetSessionConfigOptionValueId{
				SessionId: acp.SessionId(sessionID),
				ConfigId:  acp.SessionConfigId(optionID),
				Value:     acp.SessionConfigValueId(optionValue),
			},
		}, nil
	}
	return acp.SetSessionConfigOptionRequest{}, fmt.Errorf("acp session config option %q has unsupported type", optionID)
}

func collectSessionConfigValues(options []acp.SessionConfigOption, modes *acp.SessionModeState) []SessionConfigValue {
	values := make([]SessionConfigValue, 0, len(options)+1)
	seen := make(map[string]struct{}, len(options)+1)
	for _, option := range options {
		switch {
		case option.Select != nil:
			id := strings.TrimSpace(string(option.Select.Id))
			value := strings.TrimSpace(string(option.Select.CurrentValue))
			if id == "" || value == "" {
				continue
			}
			values = append(values, SelectSessionConfigValue(id, value))
			seen[id] = struct{}{}
		case option.Boolean != nil:
			id := strings.TrimSpace(string(option.Boolean.Id))
			if id == "" {
				continue
			}
			values = append(values, BooleanSessionConfigValue(id, option.Boolean.CurrentValue))
			seen[id] = struct{}{}
		default:
			continue
		}
	}
	if modes != nil {
		mode := strings.TrimSpace(string(modes.CurrentModeId))
		if mode != "" {
			if _, ok := seen["mode"]; !ok {
				values = append(values, SessionConfigValue{ID: "mode", Value: mode})
			}
		}
	}
	return values
}

// SetSessionConfigOption sets an ACP session configuration option.
func (c *Client) SetSessionConfigOption(ctx context.Context, req acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
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
//
// The caller must continuously receive from both returned channels until they
// close. The update channel preserves ACP notification order. The result
// channel sends exactly one PromptResult and then closes. Canceling ctx stops
// the prompt. Starting another prompt for the same session before both streams
// finish returns [ErrPromptAlreadyActive].
func (c *Client) Prompt(ctx context.Context, sessionID, prompt string) (<-chan ExtendedSessionNotification, <-chan PromptResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, errPromptRequired
	}
	return c.promptWithBlocks(ctx, sessionID, []acp.ContentBlock{acp.TextBlock(prompt)}, len(prompt))
}

// PromptWithContent sends a prompt composed of ACP content blocks and streams
// session updates. Its channel and cancellation contract is the same as
// [Client.Prompt].
func (c *Client) PromptWithContent(ctx context.Context, sessionID string, prompt []acp.ContentBlock) (<-chan ExtendedSessionNotification, <-chan PromptResult, error) {
	if len(prompt) == 0 {
		return nil, nil, errPromptContentReq
	}
	return c.promptWithBlocks(ctx, sessionID, prompt, 0)
}

func (c *Client) promptWithBlocks(ctx context.Context, sessionID string, prompt []acp.ContentBlock, promptLen int) (<-chan ExtendedSessionNotification, <-chan PromptResult, error) {
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
		Int("prompt_blocks", len(promptBlocks))
	if promptLen > 0 {
		logEvent = logEvent.Int("prompt_len", promptLen)
	}
	logEvent.Msg("sending acp session/prompt")
	if l.enabled(levelTrace) {
		l.Trace().
			Str("acp_session_id", sessionID).
			Str("prompt", renderACPContentBlocks(promptBlocks)).
			Msg("sending acp session/prompt payload")
	}

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
