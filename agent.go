package acpagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	acp "github.com/coder/acp-go-sdk"
	"github.com/normahq/go-adk-acpagent/acperror"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// InstructionProvider allows ACP instructions to be created dynamically using
// invocation context, mirroring llmagent semantics.
type InstructionProvider func(ctx adkagent.ReadonlyContext) (string, error)

// Config configures an ACP-backed ADK agent.
type Config struct {
	// Context is the base context for the agent's lifecycle.
	Context context.Context
	// Name is the display name of the agent. Defaults to "ACPAgent".
	Name string
	// Description describes the agent's purpose.
	Description string
	// BeforeAgentCallbacks are standard ADK lifecycle callbacks invoked before
	// the ACP-backed run starts.
	BeforeAgentCallbacks []adkagent.BeforeAgentCallback
	// AfterAgentCallbacks are standard ADK lifecycle callbacks invoked after
	// the ACP-backed run completes.
	AfterAgentCallbacks []adkagent.AfterAgentCallback
	// Model is the specific LLM model identifier to use.
	Model string
	// ModelConfigID is the ACP session config option id used to select Model.
	// When empty, the adapter discovers a select option with category "model"
	// from the ACP session response.
	ModelConfigID string
	// Mode is the ACP session mode identifier to use.
	Mode string
	// Instruction is the optional instruction applied to each invocation.
	Instruction string
	// GlobalInstruction is the optional global instruction applied before
	// Instruction.
	GlobalInstruction string
	// ReasoningEffort selects the provider reasoning effort when supported.
	ReasoningEffort string
	// InstructionProvider dynamically provides [Config.Instruction] content.
	// When set, this takes precedence over [Config.Instruction].
	InstructionProvider InstructionProvider
	// GlobalInstructionProvider dynamically provides
	// [Config.GlobalInstruction] content. When set, this takes precedence over
	// [Config.GlobalInstruction].
	GlobalInstructionProvider InstructionProvider
	// SystemInstructions is deprecated and kept for backward compatibility.
	// Use Instruction instead.
	SystemInstructions string
	// ClientName is the name reported to the ACP server during initialization.
	ClientName string
	// ClientVersion is the version reported to the ACP server during initialization.
	ClientVersion string
	// Command is the argv array used to start the ACP subprocess.
	Command []string
	// WorkingDir is the default directory for ACP execution:
	//   - the ACP subprocess is started with this directory as cmd.Dir.
	//   - ACP session/new uses this as cwd unless overridden per ADK session
	//     via session state key [CWDStateKey].
	//
	// When session state override is present, the override takes precedence for
	// ACP session cwd selection.
	WorkingDir string
	// Stderr is an optional writer for the ACP subprocess's standard error.
	Stderr io.Writer
	// PermissionHandler decides how to respond to ACP permission requests.
	PermissionHandler PermissionHandler
	// Logger is the slog logger to use for this agent.
	Logger *slog.Logger
	// MCPServers is the map of MCP server configurations.
	MCPServers map[string]MCPServerConfig
	// SessionService is ignored. ACP session bindings are recorded through the
	// ADK session state exposed by the invocation context.
	SessionService session.Service
	// OutputKey stores the final visible model output in session state delta for this invocation.
	// When set, the final non-partial turn-complete event includes
	// event.Actions.StateDelta[OutputKey] = final visible output text.
	OutputKey string
}

// Agent is an ADK agent implementation backed by an Agent Client Protocol (ACP)
// coding-agent subprocess.
// It manages the subprocess lifecycle and maps ACP sessions to ADK sessions.
type Agent struct {
	adkagent.Agent

	client                    *Client
	workingDir                string
	sessionModel              string
	sessionModelConfigID      string
	sessionMode               string
	reasoningEffort           string
	outputKey                 string
	instruction               string
	globalInstruction         string
	instructionProvider       InstructionProvider
	globalInstructionProvider InstructionProvider
	logger                    logger
	mcpServers                []acp.McpServer
}

type acpSessionConfig struct {
	sessionID     string
	modelConfigID string
	cwd           string
	meta          map[string]any
	metaJSON      string
}

type remoteSession struct {
	id                      string
	modelConfigID           string
	metaJSON                string
	fresh                   bool
	firstPromptInstructions string
}

type promptRunResult struct {
	events             []*session.Event
	promptResult       *PromptResult
	finalOutput        string
	latestPlanSnapshot map[string]any
	terminalError      *terminalPromptError
}

type terminalPromptError struct {
	Message string
	Code    string
}

type resolvedInstructionParts struct {
	global      string
	instruction string
}

func (r resolvedInstructionParts) combined() string {
	instructions := make([]string, 0, 2)
	if strings.TrimSpace(r.global) != "" {
		instructions = append(instructions, strings.TrimSpace(r.global))
	}
	if strings.TrimSpace(r.instruction) != "" {
		instructions = append(instructions, strings.TrimSpace(r.instruction))
	}
	return strings.Join(instructions, "\n\n")
}

const (
	defaultAgentName        = "ACPAgent"
	defaultAgentDescription = "ACP coding agent exposed through ADK"

	acpTypeText       = "text"
	acpTypeImage      = "image"
	acpTypeAudio      = "audio"
	acpTypeResource   = "resource"
	acpUsageUpdate    = "usage_update"
	acpPlanEntriesKey = "entries"
)

const (
	// SessionStateKey is the reserved ADK session-state key for ACP-specific
	// per-session settings.
	//
	// The value at this key must be an object with optional fields:
	//   - "meta" (object): forwarded to ACP session/new._meta
	//   - "session_id" (string): canonical ACP session id returned by the ACP
	//     agent and used for ACP session/resume
	//
	// This state is the source of truth for ACP session identity. If the ADK
	// session is deleted, this state is deleted with it and no in-memory ACP
	// session binding is reused.
	SessionStateKey = "acp_session"
	// PlanStateKey is the ADK session-state key used for ACP plan snapshots.
	//
	// Each ACP session/update.plan notification is projected into
	// event.Actions.StateDelta[PlanStateKey] as the authoritative full plan
	// replacement snapshot.
	PlanStateKey = "acp_plan"
	// CWDStateKey is the ADK session-state key used to override the ACP
	// session working directory for a single ADK session.
	CWDStateKey = "cwd"
)

var _ adkagent.Agent = (*Agent)(nil)

var placeholderRegex = regexp.MustCompile(`{+[^{}]*}+`)

const (
	appPrefix  = "app:"
	userPrefix = "user:"
	tempPrefix = "temp:"
)

// New creates an ADK agent backed by an ACP client process.
//
// It starts the ACP process, performs ACP initialization, and creates ACP
// sessions lazily per ADK session.
//
// Per ADK session, callers may provide state overrides:
//   - [CWDStateKey] (string): override ACP session/new cwd
//   - [SessionStateKey].meta (object): forwarded to ACP session/new._meta and
//     session/resume._meta
//
// ACP session IDs are agent-owned. The adapter persists the canonical ACP
// session id under [SessionStateKey].session_id and uses that value for
// session/resume when the ACP agent advertises resume capability.
//
// ACP session/load is not used by this adapter. Per ACP v1, load replays prior
// history; until that replay is intentionally mapped into ADK-visible history,
// the adapter restores only through session/resume or creates a new ACP
// session.
//
// If no override is provided, Config.WorkingDir is used as ACP session cwd.
// The first ACP session created for an ADK session is reused for subsequent
// invocations in that same ADK session.
// For newly created ACP sessions, resolved instructions are sent in
// session/new._meta.codex and prepended to the first real user prompt. The
// adapter does not send a separate instruction-only prompt.
//
// The caller is responsible for calling Close() to shut down the subprocess.
func New(cfg Config) (*Agent, error) {
	ctx := cfg.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = defaultAgentName
	}
	if strings.TrimSpace(cfg.Description) == "" {
		cfg.Description = defaultAgentDescription
	}

	l := newLogger(cfg.Logger, "acpagent.agent")

	client, err := NewClient(ctx, ClientConfig{
		Command:           cfg.Command,
		WorkingDir:        cfg.WorkingDir,
		ClientName:        cfg.ClientName,
		ClientVersion:     cfg.ClientVersion,
		Stderr:            cfg.Stderr,
		PermissionHandler: cfg.PermissionHandler,
		Logger:            cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	if _, err := client.Initialize(ctx); err != nil {
		err = fmt.Errorf("initialize acp client: %w", err)
		return nil, closeClientAfterError(client, err, "close acp client after initialize failure")
	}

	mcpServers, err := convertMCPServers(cfg.MCPServers)
	if err != nil {
		err = fmt.Errorf("convert mcp servers: %w", err)
		return nil, closeClientAfterError(client, err, "close acp client after mcp config conversion failure")
	}

	a := &Agent{
		client:                    client,
		workingDir:                cfg.WorkingDir,
		sessionModel:              strings.TrimSpace(cfg.Model),
		sessionModelConfigID:      strings.TrimSpace(cfg.ModelConfigID),
		sessionMode:               strings.TrimSpace(cfg.Mode),
		reasoningEffort:           strings.TrimSpace(cfg.ReasoningEffort),
		outputKey:                 strings.TrimSpace(cfg.OutputKey),
		instruction:               normalizeInstruction(cfg.Instruction, cfg.SystemInstructions),
		globalInstruction:         strings.TrimSpace(cfg.GlobalInstruction),
		instructionProvider:       cfg.InstructionProvider,
		globalInstructionProvider: cfg.GlobalInstructionProvider,
		logger:                    l,
		mcpServers:                mcpServers,
	}
	base, err := adkagent.New(adkagent.Config{
		Name:                 cfg.Name,
		Description:          cfg.Description,
		BeforeAgentCallbacks: cfg.BeforeAgentCallbacks,
		Run:                  a.run,
		AfterAgentCallbacks:  cfg.AfterAgentCallbacks,
	})
	if err != nil {
		err = fmt.Errorf("create adk acp agent: %w", err)
		return nil, closeClientAfterError(client, err, "close acp client after adk agent creation failure")
	}
	a.Agent = base
	return a, nil
}

func closeClientAfterError(client *Client, err error, closeMsg string) error {
	if closeErr := client.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("%s: %w", closeMsg, closeErr))
	}
	return err
}

// Close shuts down the underlying ACP client process.
func (a *Agent) Close() error {
	if err := a.client.Close(); err != nil {
		return fmt.Errorf("close acp client: %w", err)
	}
	return nil
}

func (a *Agent) run(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		baseLogger := a.invocationLogger(ctx)

		prompt := extractPromptText(ctx.UserContent())
		if strings.TrimSpace(prompt) == "" {
			yield(nil, errors.New("prompt is empty"))
			return
		}

		adkSessionID := ctx.Session().ID()
		logCtx, logger := a.sessionLogger(ctx, baseLogger, adkSessionID)

		remote, err := a.ensureRemoteSession(ctx, logCtx, logger)
		if err != nil {
			yield(nil, err)
			return
		}
		promptForRun := prompt
		if remote.fresh {
			promptForRun = prependInstructionsToPrompt(remote.firstPromptInstructions, prompt)
		}
		stateEvent := session.NewEvent(ctx.InvocationID())
		a.persistSessionStateDelta(stateEvent, remote.id, remote.metaJSON, remote.modelConfigID)
		if len(stateEvent.Actions.StateDelta) > 0 {
			a.logADKEvent(logger, stateEvent, "yielding acp session state event")
			if !yield(stateEvent, nil) {
				return
			}
		}

		result, err := a.runPromptOnce(ctx, logCtx, logger, remote.id, promptForRun)
		if err != nil && isACPSessionNotFoundError(err) {
			recovered, recoverErr := a.recoverRemoteSession(ctx, logCtx, logger, err)
			if recoverErr != nil {
				yield(nil, recoverErr)
				return
			}
			remote = recovered
			promptForRun = prompt
			if remote.fresh {
				promptForRun = prependInstructionsToPrompt(remote.firstPromptInstructions, prompt)
			}
			stateEvent := session.NewEvent(ctx.InvocationID())
			a.persistSessionStateDelta(stateEvent, remote.id, remote.metaJSON, remote.modelConfigID)
			if len(stateEvent.Actions.StateDelta) > 0 {
				a.logADKEvent(logger, stateEvent, "yielding recovered acp session state event")
				if !yield(stateEvent, nil) {
					return
				}
			}
			result, err = a.runPromptOnce(ctx, logCtx, logger, remote.id, promptForRun)
		}
		if err != nil {
			yield(nil, err)
			return
		}
		for _, ev := range result.events {
			if !yield(ev, nil) {
				return
			}
		}

		ev := session.NewEvent(ctx.InvocationID())
		if result.promptResult != nil {
			ev.FinishReason = mapACPStopReasonToFinishReason(result.promptResult.Response.StopReason)
			ev.UsageMetadata = mapACPUsageToUsageMetadata(result.promptResult.Usage)
			copyACPProviderErrorMetadata(ev, result.promptResult.Response.Meta)
		}
		if result.finalOutput != "" {
			ev.Content = genai.NewContentFromText(result.finalOutput, genai.RoleModel)
		} else if result.terminalError != nil {
			ev.ErrorMessage = result.terminalError.Message
			ev.ErrorCode = result.terminalError.Code
		}
		if result.latestPlanSnapshot != nil {
			ev.Actions.StateDelta[PlanStateKey] = result.latestPlanSnapshot
		}
		a.persistSessionStateDelta(ev, remote.id, remote.metaJSON, remote.modelConfigID)
		a.maybeSaveOutputToState(ev, result.finalOutput)
		ev.TurnComplete = true
		a.logADKEvent(logger, ev, "yielding final turn complete event")
		if !yield(ev, nil) {
			return
		}
	}
}

func prependInstructionsToPrompt(instructions string, prompt string) string {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return prompt
	}
	return instructions + "\n\nUser message:\n" + prompt
}

func (a *Agent) runPromptOnce(
	ctx adkagent.InvocationContext,
	logCtx context.Context,
	logger logger,
	remoteSessionID string,
	prompt string,
) (promptRunResult, error) {
	var out promptRunResult

	logger.Debug().
		Str("acp_session_id", remoteSessionID).
		Str("prompt", prompt).
		Int("prompt_len", len(prompt)).
		Msg("starting adk invocation")

	updates, resultCh, err := a.client.Prompt(logCtx, remoteSessionID, prompt)
	if err != nil {
		return promptRunResult{}, err
	}

	var finalText strings.Builder
	for updates != nil || resultCh != nil {
		select {
		case <-ctx.Done():
			return promptRunResult{}, ctx.Err()
		case ext, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if terminalErr, ok := terminalPromptErrorFromNotification(ext); ok {
				out.terminalError = terminalErr
			}
			ev, ok := mapACPUpdateToEvent(logger, ctx.InvocationID(), ext)
			if !ok {
				continue
			}
			if ext.Update.AgentMessageChunk != nil {
				finalText.WriteString(contentVisibleText(ev.Content))
			}
			if planSnapshot, ok := ev.Actions.StateDelta[PlanStateKey].(map[string]any); ok {
				out.latestPlanSnapshot = planSnapshot
			}
			a.logADKEvent(logger, ev, "yielding adk event")
			out.events = append(out.events, ev)
		case result, ok := <-resultCh:
			if !ok {
				resultCh = nil
				continue
			}
			out.promptResult = &result
			resultCh = nil
		}
	}

	if out.promptResult != nil && out.promptResult.Err != nil {
		return promptRunResult{}, out.promptResult.Err
	}

	logger.Debug().
		Str("acp_session_id", remoteSessionID).
		Msg("completed adk invocation")

	out.finalOutput = finalText.String()
	return out, nil
}

func terminalPromptErrorFromNotification(ext ExtendedSessionNotification) (*terminalPromptError, bool) {
	switch ext.Method {
	case "error":
		return parsePromptErrorNotification(ext.Raw)
	case "turn/completed":
		return parseTurnCompletedTerminalError(ext.Raw)
	default:
		return nil, false
	}
}

func parsePromptErrorNotification(raw json.RawMessage) (*terminalPromptError, bool) {
	var payload struct {
		Error struct {
			Message           string `json:"message"`
			CodexErrorInfo    any    `json:"codexErrorInfo"`
			AdditionalDetails string `json:"additionalDetails"`
		} `json:"error"`
		WillRetry bool `json:"willRetry"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false
	}
	if payload.WillRetry {
		return nil, false
	}
	return newTerminalPromptError(payload.Error.Message, payload.Error.CodexErrorInfo, payload.Error.AdditionalDetails)
}

func parseTurnCompletedTerminalError(raw json.RawMessage) (*terminalPromptError, bool) {
	var payload struct {
		Turn struct {
			Status string `json:"status"`
			Error  struct {
				Message           string `json:"message"`
				CodexErrorInfo    any    `json:"codexErrorInfo"`
				AdditionalDetails string `json:"additionalDetails"`
			} `json:"error"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(payload.Turn.Status), "failed") {
		return nil, false
	}
	return newTerminalPromptError(payload.Turn.Error.Message, payload.Turn.Error.CodexErrorInfo, payload.Turn.Error.AdditionalDetails)
}

func newTerminalPromptError(message string, code any, additionalDetails string) (*terminalPromptError, bool) {
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = strings.TrimSpace(additionalDetails)
	}
	if msg == "" {
		return nil, false
	}
	errCode := "provider_error"
	if codeText := strings.TrimSpace(stringifyTerminalErrorCode(code)); codeText != "" {
		errCode = codeText
	}
	return &terminalPromptError{
		Message: msg,
		Code:    errCode,
	}, true
}

func stringifyTerminalErrorCode(code any) string {
	switch value := code.(type) {
	case nil:
		return ""
	case string:
		return value
	case map[string]any:
		if len(value) != 1 {
			return ""
		}
		for key := range value {
			return key
		}
		return ""
	default:
		return fmt.Sprint(value)
	}
}

func (a *Agent) maybeSaveOutputToState(event *session.Event, output string) {
	if a.outputKey == "" || event == nil || event.Partial || output == "" {
		return
	}
	if event.Actions.StateDelta == nil {
		event.Actions.StateDelta = make(map[string]any)
	}
	event.Actions.StateDelta[a.outputKey] = output
}

func (a *Agent) persistSessionStateDelta(event *session.Event, remoteSessionID, metaJSON, modelConfigID string) {
	if event == nil || event.Partial || strings.TrimSpace(remoteSessionID) == "" {
		return
	}

	if event.Actions.StateDelta == nil {
		event.Actions.StateDelta = make(map[string]any)
	}
	event.Actions.StateDelta[SessionStateKey] = buildACPStateWithModelConfigID(remoteSessionID, metaJSON, modelConfigID)
}

func (a *Agent) invocationLogger(ctx context.Context) logger {
	return loggerFromContext(ctx, a.logger, "acpagent.agent")
}

func (a *Agent) sessionLogger(ctx context.Context, base logger, adkSessionID string) (context.Context, logger) {
	logger := base.with(
		"session_id", adkSessionID,
		"adk_session_id", adkSessionID,
	)
	return contextWithLogger(ctx, logger), logger
}

func (a *Agent) logADKEvent(logger logger, ev *session.Event, msg string) {
	if ev == nil {
		return
	}
	logEvent := logger.Trace().
		Str("invocation_id", ev.InvocationID).
		Bool("partial", ev.Partial).
		Bool("turn_complete", ev.TurnComplete)

	if ev.FinishReason != "" {
		logEvent = logEvent.Str("finish_reason", string(ev.FinishReason))
	}
	if ev.Content != nil {
		logEvent = logEvent.Bool("has_content", true)
	}
	logEvent.Msg(msg)
}

func mapACPStopReasonToFinishReason(reason acp.StopReason) genai.FinishReason {
	switch reason {
	case acp.StopReasonEndTurn:
		return genai.FinishReasonStop
	case acp.StopReasonMaxTokens:
		return genai.FinishReasonMaxTokens
	case acp.StopReasonRefusal:
		return genai.FinishReasonProhibitedContent
	case acp.StopReasonCancelled, acp.StopReasonMaxTurnRequests:
		return genai.FinishReasonOther // No direct match for cancelled in genai.FinishReason
	default:
		return genai.FinishReasonUnspecified
	}
}

func mapACPUsageToUsageMetadata(usage *acp.Usage) *genai.GenerateContentResponseUsageMetadata {
	if usage == nil {
		return nil
	}
	m := &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     int32(usage.InputTokens),
		CandidatesTokenCount: int32(usage.OutputTokens),
		TotalTokenCount:      int32(usage.TotalTokens),
	}
	if usage.CachedReadTokens != nil {
		m.CachedContentTokenCount = int32(*usage.CachedReadTokens)
	}
	return m
}

func mapACPLegacyUsageToUsageMetadata(usage map[string]any) *genai.GenerateContentResponseUsageMetadata {
	if usage == nil {
		return nil
	}
	m := &genai.GenerateContentResponseUsageMetadata{}
	found := false
	if val, ok := usage["inputTokens"].(float64); ok {
		m.PromptTokenCount = int32(val)
		found = true
	}
	if val, ok := usage["outputTokens"].(float64); ok {
		m.CandidatesTokenCount = int32(val)
		found = true
	}
	if val, ok := usage["totalTokens"].(float64); ok {
		m.TotalTokenCount = int32(val)
		found = true
	}
	if val, ok := usage["cachedReadTokens"].(float64); ok {
		m.CachedContentTokenCount = int32(val)
		found = true
	}
	if !found {
		return nil
	}
	return m
}

func (a *Agent) ensureRemoteSession(ctx adkagent.InvocationContext, logCtx context.Context, logger logger) (remoteSession, error) {
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil {
		return remoteSession{}, err
	}

	if cfg.sessionID != "" {
		logger.Debug().
			Str("acp_session_id", cfg.sessionID).
			Msg("using acp session id from adk session state")
		appliedModelConfigID, err := a.client.applySessionModelAndMode(
			logCtx,
			cfg.sessionID,
			a.sessionModel,
			cfg.modelConfigID,
			a.sessionMode,
			nil,
			nil,
		)
		if err != nil {
			return remoteSession{}, err
		}
		if appliedModelConfigID != "" {
			cfg.modelConfigID = appliedModelConfigID
		}
		if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.modelConfigID); err != nil {
			return remoteSession{}, err
		}
		return remoteSession{id: cfg.sessionID, modelConfigID: cfg.modelConfigID, metaJSON: cfg.metaJSON}, nil
	}
	instructions, err := a.resolveInstructionParts(ctx)
	if err != nil {
		return remoteSession{}, err
	}
	cfg, err = addInstructionMetaToSessionConfig(cfg, instructions)
	if err != nil {
		return remoteSession{}, err
	}
	return a.createRemoteSession(ctx, logCtx, logger, cfg, instructions.combined())
}

func (a *Agent) recoverRemoteSession(
	ctx adkagent.InvocationContext,
	logCtx context.Context,
	logger logger,
	promptErr error,
) (remoteSession, error) {
	cfg, err := a.resolveSessionConfig(ctx)
	if err != nil {
		return remoteSession{}, err
	}
	if cfg.sessionID != "" && a.client.SupportsSessionResume() {
		resumeResp, err := a.client.ResumeSessionWithMeta(logCtx, cfg.sessionID, cfg.cwd, a.mcpServers, cfg.meta)
		if err == nil {
			appliedModelConfigID, err := a.client.applySessionModelAndMode(
				logCtx,
				cfg.sessionID,
				a.sessionModel,
				cfg.modelConfigID,
				a.sessionMode,
				resumeResp.ConfigOptions,
				resumeResp.Modes,
			)
			if err != nil {
				return remoteSession{}, err
			}
			if appliedModelConfigID != "" {
				cfg.modelConfigID = appliedModelConfigID
			}
			a.logBoundRemoteSession(logger, "resumed acp session after prompt failure", cfg.sessionID, cfg.cwd, cfg.metaJSON)
			if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.modelConfigID); err != nil {
				return remoteSession{}, err
			}
			return remoteSession{id: cfg.sessionID, modelConfigID: cfg.modelConfigID, metaJSON: cfg.metaJSON}, nil
		}
		if isACPSessionAlreadyExistsError(err) {
			logger.Debug().
				Err(err).
				Str("acp_session_id", cfg.sessionID).
				Msg("acp session already active after prompt failure")
			if err := a.persistRemoteSessionBinding(ctx, cfg.sessionID, cfg.metaJSON, cfg.modelConfigID); err != nil {
				return remoteSession{}, err
			}
			return remoteSession{id: cfg.sessionID, modelConfigID: cfg.modelConfigID, metaJSON: cfg.metaJSON}, nil
		}
		if !isACPSessionNotFoundError(err) {
			return remoteSession{}, fmt.Errorf("resume acp session %q after prompt failure: %w", cfg.sessionID, err)
		}
		logger.Warn().
			Err(err).
			Str("acp_session_id", cfg.sessionID).
			Msg("acp session resume unavailable after prompt failure; falling back to session/new")
	}
	instructions, err := a.resolveInstructionParts(ctx)
	if err != nil {
		return remoteSession{}, err
	}
	cfg, err = addInstructionMetaToSessionConfig(cfg, instructions)
	if err != nil {
		return remoteSession{}, err
	}
	recovered, err := a.createRemoteSession(ctx, logCtx, logger, cfg, instructions.combined())
	if err != nil {
		return remoteSession{}, fmt.Errorf("recover acp session after prompt failure %q: %w", promptErr, err)
	}
	return recovered, nil
}

func (a *Agent) createRemoteSession(
	ctx adkagent.InvocationContext,
	logCtx context.Context,
	logger logger,
	cfg acpSessionConfig,
	firstPromptInstructions string,
) (remoteSession, error) {
	resp, err := a.client.NewSessionWithMeta(logCtx, cfg.cwd, a.mcpServers, cfg.meta)
	if err != nil {
		return remoteSession{}, err
	}
	sessionID := string(resp.SessionId)
	appliedModelConfigID, err := a.client.applySessionModelAndMode(
		logCtx,
		sessionID,
		a.sessionModel,
		cfg.modelConfigID,
		a.sessionMode,
		resp.ConfigOptions,
		resp.Modes,
	)
	if err != nil {
		return remoteSession{}, err
	}
	if appliedModelConfigID != "" {
		cfg.modelConfigID = appliedModelConfigID
	}
	a.logBoundRemoteSession(logger, "created new acp session for adk session", sessionID, cfg.cwd, cfg.metaJSON)
	if err := a.persistRemoteSessionBinding(ctx, sessionID, cfg.metaJSON, cfg.modelConfigID); err != nil {
		return remoteSession{}, err
	}
	return remoteSession{
		id:                      sessionID,
		modelConfigID:           cfg.modelConfigID,
		metaJSON:                cfg.metaJSON,
		fresh:                   true,
		firstPromptInstructions: strings.TrimSpace(firstPromptInstructions),
	}, nil
}

func (a *Agent) logBoundRemoteSession(
	logger logger,
	message string,
	remoteSessionID string,
	cwd string,
	metaJSON string,
) {
	event := logger.Debug().
		Str("acp_session_id", remoteSessionID).
		Str("cwd", cwd).
		RawJSON("meta", []byte(metaJSON))
	if a.sessionModel != "" {
		event = event.Str("model", a.sessionModel)
	}
	if a.sessionMode != "" {
		event = event.Str("mode", a.sessionMode)
	}
	event.Msg(message)
}

func buildACPState(remoteSessionID, metaJSON string) map[string]any {
	return buildACPStateWithModelConfigID(remoteSessionID, metaJSON, "")
}

func buildACPStateWithModelConfigID(remoteSessionID, metaJSON, modelConfigID string) map[string]any {
	acpState := map[string]any{
		"session_id": remoteSessionID,
	}
	if strings.TrimSpace(modelConfigID) != "" {
		acpState["model_config_id"] = strings.TrimSpace(modelConfigID)
	}
	if strings.TrimSpace(metaJSON) == "" || metaJSON == "{}" {
		return acpState
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(metaJSON), &meta); err == nil && len(meta) > 0 {
		acpState["meta"] = meta
	}
	return acpState
}

func (a *Agent) persistRemoteSessionBinding(
	ctx adkagent.InvocationContext,
	remoteSessionID string,
	metaJSON string,
	modelConfigID string,
) error {
	if ctx == nil || ctx.Session() == nil || strings.TrimSpace(remoteSessionID) == "" {
		return nil
	}

	acpState := buildACPStateWithModelConfigID(remoteSessionID, metaJSON, modelConfigID)
	if currentACPStateMatches(ctx.Session(), remoteSessionID, metaJSON) {
		if err := ctx.Session().State().Set(SessionStateKey, cloneAnyMap(acpState)); err != nil {
			return fmt.Errorf("set live acp session state: %w", err)
		}
		return nil
	}

	if err := ctx.Session().State().Set(SessionStateKey, cloneAnyMap(acpState)); err != nil {
		return fmt.Errorf("set live acp session state: %w", err)
	}
	return nil
}

func currentACPStateMatches(sess session.Session, remoteSessionID, metaJSON string) bool {
	if sess == nil {
		return false
	}
	rawState, err := sess.State().Get(SessionStateKey)
	if err != nil {
		return false
	}
	state, ok := rawState.(map[string]any)
	if !ok {
		return false
	}
	storedSessionID, _ := state["session_id"].(string)
	if strings.TrimSpace(storedSessionID) != strings.TrimSpace(remoteSessionID) {
		return false
	}
	return normalizeACPStateMetaJSON(state["meta"]) == normalizeACPStateMetaJSONFromRaw(metaJSON)
}

func normalizeACPStateMetaJSON(raw any) string {
	if raw == nil {
		return "{}"
	}
	switch value := raw.(type) {
	case map[string]any:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "{}"
		}
		return string(encoded)
	default:
		return "{}"
	}
}

func normalizeACPStateMetaJSONFromRaw(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return "{}"
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(trimmed), &meta); err != nil {
		return "{}"
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func (a *Agent) resolveSessionConfig(ctx adkagent.InvocationContext) (acpSessionConfig, error) {
	cfg := acpSessionConfig{
		cwd:           strings.TrimSpace(a.workingDir),
		modelConfigID: strings.TrimSpace(a.sessionModelConfigID),
	}
	rawCWD, err := ctx.Session().State().Get(CWDStateKey)
	if err != nil {
		if !errors.Is(err, session.ErrStateKeyNotExist) {
			return acpSessionConfig{}, fmt.Errorf("read %q from adk session state: %w", CWDStateKey, err)
		}
	} else {
		cwd, ok := rawCWD.(string)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("adk session state %q must be a string; got %T", CWDStateKey, rawCWD)
		}
		cfg.cwd = strings.TrimSpace(cwd)
	}

	rawState, err := ctx.Session().State().Get(SessionStateKey)
	if err != nil {
		if errors.Is(err, session.ErrStateKeyNotExist) {
			cfg, cfgErr := addReasoningEffortToSessionConfig(cfg, a.reasoningEffort)
			if cfgErr != nil {
				return acpSessionConfig{}, cfgErr
			}
			return normalizeACPConfigCWD(cfg)
		}
		return acpSessionConfig{}, fmt.Errorf("read %q from adk session state: %w", SessionStateKey, err)
	}

	state, ok := rawState.(map[string]any)
	if !ok {
		return acpSessionConfig{}, fmt.Errorf("adk session state %q must be an object; got %T", SessionStateKey, rawState)
	}
	if rawMeta, ok := state["meta"]; ok {
		meta, ok := rawMeta.(map[string]any)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("adk session state %q.meta must be an object; got %T", SessionStateKey, rawMeta)
		}
		cfg.meta = cloneAnyMap(meta)
	}
	if rawSessionID, ok := state["session_id"]; ok {
		sessionID, ok := rawSessionID.(string)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("adk session state %q.session_id must be a string; got %T", SessionStateKey, rawSessionID)
		}
		cfg.sessionID = strings.TrimSpace(sessionID)
	}
	if rawModelConfigID, ok := state["model_config_id"]; ok {
		modelConfigID, ok := rawModelConfigID.(string)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("adk session state %q.model_config_id must be a string; got %T", SessionStateKey, rawModelConfigID)
		}
		if cfg.modelConfigID == "" {
			cfg.modelConfigID = strings.TrimSpace(modelConfigID)
		}
	}
	cfg, err = addReasoningEffortToSessionConfig(cfg, a.reasoningEffort)
	if err != nil {
		return acpSessionConfig{}, err
	}
	return normalizeACPConfigCWD(cfg)
}

func normalizeACPConfigCWD(cfg acpSessionConfig) (acpSessionConfig, error) {
	if cfg.meta == nil {
		cfg.metaJSON = "{}"
	} else {
		metaJSON, err := json.Marshal(cfg.meta)
		if err != nil {
			return acpSessionConfig{}, fmt.Errorf("marshal acp session meta: %w", err)
		}
		cfg.metaJSON = string(metaJSON)
	}

	if cfg.cwd == "" {
		return acpSessionConfig{}, fmt.Errorf("acp session cwd is empty")
	}
	absCWD, err := filepath.Abs(cfg.cwd)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("resolve acp session cwd %q: %w", cfg.cwd, err)
	}
	info, err := os.Stat(absCWD)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("stat acp session cwd %q: %w", absCWD, err)
	}
	if !info.IsDir() {
		return acpSessionConfig{}, fmt.Errorf("acp session cwd %q is not a directory", absCWD)
	}
	cfg.cwd = absCWD
	return cfg, nil
}

func normalizeInstruction(primary, deprecated string) string {
	inst := strings.TrimSpace(primary)
	if inst != "" {
		return inst
	}
	return strings.TrimSpace(deprecated)
}

func (a *Agent) resolveInstructionParts(ctx adkagent.InvocationContext) (resolvedInstructionParts, error) {
	readonlyCtx := readonlyInvocationContext{invocation: ctx}

	globalInstruction, err := a.resolveSingleInstruction(
		ctx,
		readonlyCtx,
		a.globalInstruction,
		a.globalInstructionProvider,
		"global instruction",
	)
	if err != nil {
		return resolvedInstructionParts{}, err
	}

	instruction, err := a.resolveSingleInstruction(
		ctx,
		readonlyCtx,
		a.instruction,
		a.instructionProvider,
		"instruction",
	)
	if err != nil {
		return resolvedInstructionParts{}, err
	}

	return resolvedInstructionParts{
		global:      strings.TrimSpace(globalInstruction),
		instruction: strings.TrimSpace(instruction),
	}, nil
}

func addInstructionMetaToSessionConfig(cfg acpSessionConfig, instructions resolvedInstructionParts) (acpSessionConfig, error) {
	if instructions.combined() == "" {
		return cfg, nil
	}

	if cfg.meta == nil {
		cfg.meta = map[string]any{}
	}
	codexMeta := map[string]any{}
	if rawCodexMeta, ok := cfg.meta["codex"]; ok {
		existingCodexMeta, ok := rawCodexMeta.(map[string]any)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("acp session meta codex must be an object; got %T", rawCodexMeta)
		}
		codexMeta = cloneAnyMap(existingCodexMeta)
	}

	setInstructionMetaIfEmpty(codexMeta, "baseInstructions", instructions.global)
	setInstructionMetaIfEmpty(codexMeta, "developerInstructions", instructions.instruction)
	if len(codexMeta) > 0 {
		cfg.meta["codex"] = codexMeta
	}

	metaJSON, err := json.Marshal(cfg.meta)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("marshal acp session meta: %w", err)
	}
	cfg.metaJSON = string(metaJSON)
	return cfg, nil
}

func addReasoningEffortToSessionConfig(cfg acpSessionConfig, reasoningEffort string) (acpSessionConfig, error) {
	trimmedEffort := strings.TrimSpace(reasoningEffort)
	if trimmedEffort == "" {
		return cfg, nil
	}
	if cfg.meta == nil {
		cfg.meta = map[string]any{}
	}

	codexMeta, err := codexMetaObject(cfg.meta)
	if err != nil {
		return acpSessionConfig{}, err
	}
	configMeta, err := codexConfigObject(codexMeta)
	if err != nil {
		return acpSessionConfig{}, err
	}
	configMeta["model_reasoning_effort"] = trimmedEffort
	codexMeta["config"] = configMeta
	cfg.meta["codex"] = codexMeta

	metaJSON, err := json.Marshal(cfg.meta)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("marshal acp session meta: %w", err)
	}
	cfg.metaJSON = string(metaJSON)
	return cfg, nil
}

func codexMetaObject(meta map[string]any) (map[string]any, error) {
	codexMeta := map[string]any{}
	if rawCodexMeta, ok := meta["codex"]; ok {
		existingCodexMeta, ok := rawCodexMeta.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("acp session meta codex must be an object; got %T", rawCodexMeta)
		}
		codexMeta = cloneAnyMap(existingCodexMeta)
	}
	return codexMeta, nil
}

func codexConfigObject(codexMeta map[string]any) (map[string]any, error) {
	configMeta := map[string]any{}
	if rawConfigMeta, ok := codexMeta["config"]; ok {
		existingConfigMeta, ok := rawConfigMeta.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("acp session meta codex.config must be an object; got %T", rawConfigMeta)
		}
		configMeta = cloneAnyMap(existingConfigMeta)
	}
	return configMeta, nil
}

func setInstructionMetaIfEmpty(meta map[string]any, key string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if existing, ok := meta[key]; ok && isNonEmptyMetaValue(existing) {
		return
	}
	meta[key] = value
}

func isNonEmptyMetaValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return true
	}
}

func (a *Agent) resolveSingleInstruction(
	invocationCtx adkagent.InvocationContext,
	ctx adkagent.ReadonlyContext,
	templateInstruction string,
	provider InstructionProvider,
	kind string,
) (string, error) {
	if provider != nil {
		instruction, err := provider(ctx)
		if err != nil {
			return "", fmt.Errorf("evaluate %s provider: %w", kind, err)
		}
		return instruction, nil
	}

	templateInstruction = strings.TrimSpace(templateInstruction)
	if templateInstruction == "" {
		return "", nil
	}

	instruction, err := injectSessionState(invocationCtx, templateInstruction)
	if err != nil {
		return "", fmt.Errorf("inject session state into %s: %w", kind, err)
	}
	return instruction, nil
}

func injectSessionState(ctx adkagent.InvocationContext, templateInstruction string) (string, error) {
	var result strings.Builder
	lastIndex := 0
	matches := placeholderRegex.FindAllStringIndex(templateInstruction, -1)

	for _, matchIndexes := range matches {
		startIndex, endIndex := matchIndexes[0], matchIndexes[1]
		result.WriteString(templateInstruction[lastIndex:startIndex])

		replacement, err := replaceTemplateMatch(ctx, templateInstruction[startIndex:endIndex])
		if err != nil {
			return "", err
		}
		result.WriteString(replacement)

		lastIndex = endIndex
	}

	result.WriteString(templateInstruction[lastIndex:])
	return result.String(), nil
}

func replaceTemplateMatch(ctx adkagent.InvocationContext, match string) (string, error) {
	varName := strings.TrimSpace(strings.Trim(match, "{}"))
	optional := false
	if strings.HasSuffix(varName, "?") {
		optional = true
		varName = strings.TrimSuffix(varName, "?")
	}

	if after, ok := strings.CutPrefix(varName, "artifact."); ok {
		if ctx.Artifacts() == nil {
			return "", fmt.Errorf("artifact service is not initialized")
		}
		resp, err := ctx.Artifacts().Load(ctx, after)
		if err != nil {
			if optional {
				return "", nil
			}
			return "", fmt.Errorf("failed to load artifact %s: %w", after, err)
		}
		return resp.Part.Text, nil
	}

	if !isValidStateName(varName) {
		return match, nil
	}

	value, err := ctx.Session().State().Get(varName)
	if err != nil {
		if optional {
			return "", nil
		}
		return "", err
	}
	if value == nil {
		return "", nil
	}
	return fmt.Sprintf("%v", value), nil
}

func isValidStateName(varName string) bool {
	parts := strings.Split(varName, ":")
	if len(parts) == 1 {
		return isIdentifier(varName)
	}

	if len(parts) == 2 {
		prefix := parts[0] + ":"
		validPrefixes := []string{appPrefix, userPrefix, tempPrefix}
		if slices.Contains(validPrefixes, prefix) {
			return isIdentifier(parts[1])
		}
	}
	return false
}

func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

type readonlyInvocationContext struct {
	invocation adkagent.InvocationContext
}

func (c readonlyInvocationContext) Deadline() (time.Time, bool) {
	if c.invocation == nil {
		return time.Time{}, false
	}
	return c.invocation.Deadline()
}

func (c readonlyInvocationContext) Done() <-chan struct{} {
	if c.invocation == nil {
		return nil
	}
	return c.invocation.Done()
}

func (c readonlyInvocationContext) Err() error {
	if c.invocation == nil {
		return nil
	}
	return c.invocation.Err()
}

func (c readonlyInvocationContext) Value(key any) any {
	if c.invocation == nil {
		return nil
	}
	return c.invocation.Value(key)
}

func (c readonlyInvocationContext) UserContent() *genai.Content {
	if c.invocation == nil {
		return nil
	}
	return c.invocation.UserContent()
}

func (c readonlyInvocationContext) InvocationID() string {
	if c.invocation == nil {
		return ""
	}
	return c.invocation.InvocationID()
}

func (c readonlyInvocationContext) AgentName() string {
	if c.invocation == nil || c.invocation.Agent() == nil {
		return ""
	}
	return c.invocation.Agent().Name()
}

func (c readonlyInvocationContext) ReadonlyState() session.ReadonlyState {
	if c.invocation == nil || c.invocation.Session() == nil {
		return emptyReadonlyState{}
	}
	return c.invocation.Session().State()
}

func (c readonlyInvocationContext) UserID() string {
	if c.invocation == nil || c.invocation.Session() == nil {
		return ""
	}
	return c.invocation.Session().UserID()
}

func (c readonlyInvocationContext) AppName() string {
	if c.invocation == nil || c.invocation.Session() == nil {
		return ""
	}
	return c.invocation.Session().AppName()
}

func (c readonlyInvocationContext) SessionID() string {
	if c.invocation == nil || c.invocation.Session() == nil {
		return ""
	}
	return c.invocation.Session().ID()
}

func (c readonlyInvocationContext) Branch() string {
	if c.invocation == nil {
		return ""
	}
	return c.invocation.Branch()
}

type emptyReadonlyState struct{}

func (emptyReadonlyState) Get(string) (any, error) {
	return nil, session.ErrStateKeyNotExist
}

func (emptyReadonlyState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {}
}

func extractPromptText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range content.Parts {
		if part == nil || part.Text == "" {
			continue
		}
		builder.WriteString(part.Text)
	}
	return strings.TrimSpace(builder.String())
}

func contentVisibleText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range content.Parts {
		if part == nil || part.Text == "" || part.Thought {
			continue
		}
		builder.WriteString(part.Text)
	}
	return builder.String()
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

func mapACPUpdateToEvent(logger logger, invocationID string, ext ExtendedSessionNotification) (*session.Event, bool) {
	update := ext.Update
	switch {
	case update.UserMessageChunk != nil:
		return mapACPUserMessageChunk(logger, invocationID, update.UserMessageChunk)
	case update.AgentMessageChunk != nil:
		return mapACPAgentMessageChunk(logger, invocationID, update.AgentMessageChunk)
	case update.AgentThoughtChunk != nil:
		return mapACPAgentThoughtChunk(logger, invocationID, update.AgentThoughtChunk)
	case update.ToolCall != nil:
		return mapACPToolCall(invocationID, update.ToolCall)
	case update.ToolCallUpdate != nil:
		return mapACPToolCallUpdate(invocationID, update.ToolCallUpdate)
	case update.Plan != nil:
		return mapACPPlanUpdate(logger, invocationID, update.Plan)
	case update.AvailableCommandsUpdate != nil:
		logIgnoredACPUpdate(logger, "available_commands_update", map[string]any{
			"availableCommands": update.AvailableCommandsUpdate.AvailableCommands,
		})
		return nil, false
	case update.CurrentModeUpdate != nil:
		logIgnoredACPUpdate(logger, "current_mode_update", map[string]any{
			"currentModeId": update.CurrentModeUpdate.CurrentModeId,
		})
		return nil, false
	case update.ConfigOptionUpdate != nil:
		logIgnoredACPUpdate(logger, "config_option_update", map[string]any{
			"configOptions": update.ConfigOptionUpdate.ConfigOptions,
		})
		return nil, false
	case update.SessionInfoUpdate != nil:
		logIgnoredACPUpdate(logger, "session_info_update", map[string]any{
			"title":     update.SessionInfoUpdate.Title,
			"updatedAt": update.SessionInfoUpdate.UpdatedAt,
		})
		return nil, false
	case update.UsageUpdate != nil:
		logIgnoredACPUpdate(logger, acpUsageUpdate, map[string]any{
			"size": update.UsageUpdate.Size,
			"used": update.UsageUpdate.Used,
			"cost": update.UsageUpdate.Cost,
		})
		return nil, false
	default:
		// Check for recognized discriminators in raw JSON that are not in the SDK struct.
		var raw map[string]any
		if err := json.Unmarshal(ext.Raw, &raw); err == nil {
			if u, ok := raw["update"].(map[string]any); ok {
				if disc, ok := u["sessionUpdate"].(string); ok && disc == acpUsageUpdate {
					return mapACPLegacyUsageUpdate(logger, invocationID, u)
				}
			}
		}

		logUnsupportedACPUpdate(logger, ext)
		return nil, false
	}
}

func mapACPLegacyUsageUpdate(logger logger, invocationID string, update map[string]any) (*session.Event, bool) {
	usage := mapACPLegacyUsageToUsageMetadata(update)
	if usage == nil {
		logger.Debug().Interface("update", update).Msg("ignoring usage_update with no token counts")
		return nil, false
	}
	ev := session.NewEvent(invocationID)
	ev.UsageMetadata = usage
	ev.Partial = true
	return ev, true
}

func mapACPAgentMessageChunk(logger logger, invocationID string, chunk *acp.SessionUpdateAgentMessageChunk) (*session.Event, bool) {
	part, ok := mapACPContentBlockToPart(logger, chunk.Content)
	if !ok {
		return nil, false
	}
	ev := session.NewEvent(invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleModel)
	ev.Partial = true

	if id, ok := chunk.Meta["messageId"]; ok {
		ev.CustomMetadata = map[string]any{"acp_message_id": id}
	}
	if chunk.MessageId != nil && *chunk.MessageId != "" {
		if ev.CustomMetadata == nil {
			ev.CustomMetadata = map[string]any{}
		}
		ev.CustomMetadata["acp_message_id"] = *chunk.MessageId
	}
	copyACPProviderErrorMetadata(ev, chunk.Meta)
	return ev, true
}

func copyACPProviderErrorMetadata(ev *session.Event, meta map[string]any) {
	if ev == nil {
		return
	}
	providerErr, ok := acperror.FromMetadata(meta)
	if !ok {
		return
	}
	if ev.CustomMetadata == nil {
		ev.CustomMetadata = map[string]any{}
	}
	ev.ErrorCode = "acp_provider_error:" + string(providerErr.Kind)
	ev.ErrorMessage = providerErr.Error()
	ev.CustomMetadata[acperror.ProviderErrorMetadataKey] = providerErr.Metadata()
}

func mapACPUserMessageChunk(logger logger, invocationID string, chunk *acp.SessionUpdateUserMessageChunk) (*session.Event, bool) {
	part, ok := mapACPContentBlockToPart(logger, chunk.Content)
	if !ok {
		return nil, false
	}
	ev := session.NewEvent(invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleUser)
	ev.Partial = true
	return ev, true
}

func mapACPAgentThoughtChunk(logger logger, invocationID string, chunk *acp.SessionUpdateAgentThoughtChunk) (*session.Event, bool) {
	part, ok := mapACPContentBlockToPart(logger, chunk.Content)
	if !ok {
		return nil, false
	}
	part.Thought = true
	ev := session.NewEvent(invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleModel)
	ev.Partial = true
	return ev, true
}

func mapACPToolCall(invocationID string, tool *acp.SessionUpdateToolCall) (*session.Event, bool) {
	args := map[string]any{
		"kind":      tool.Kind,
		"status":    tool.Status,
		"title":     tool.Title,
		"locations": tool.Locations,
		"rawInput":  tool.RawInput,
		"rawOutput": tool.RawOutput,
	}
	part := &genai.Part{
		FunctionCall: &genai.FunctionCall{
			ID:   string(tool.ToolCallId),
			Name: "acp_tool_call",
			Args: args,
		},
	}
	ev := session.NewEvent(invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleModel)
	if isACPToolStatusLongRunning(tool.Status) {
		ev.LongRunningToolIDs = []string{string(tool.ToolCallId)}
	}
	return ev, true
}

func mapACPToolCallUpdate(invocationID string, tool *acp.SessionToolCallUpdate) (*session.Event, bool) {
	response := map[string]any{
		"status":    tool.Status,
		"title":     tool.Title,
		"kind":      tool.Kind,
		"locations": tool.Locations,
		"rawInput":  tool.RawInput,
		"rawOutput": tool.RawOutput,
	}
	part := &genai.Part{
		FunctionResponse: &genai.FunctionResponse{
			ID:       string(tool.ToolCallId),
			Name:     "acp_tool_call_update",
			Response: response,
		},
	}
	ev := session.NewEvent(invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleModel)
	if tool.Status != nil && isACPToolStatusLongRunning(*tool.Status) {
		ev.LongRunningToolIDs = []string{string(tool.ToolCallId)}
	}
	return ev, true
}

func mapACPPlanUpdate(_ logger, invocationID string, plan *acp.SessionUpdatePlan) (*session.Event, bool) {
	if plan == nil || len(plan.Entries) == 0 {
		return nil, false
	}
	entries := make([]map[string]any, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		entries = append(entries, map[string]any{
			"content":  entry.Content,
			"status":   entry.Status,
			"priority": entry.Priority,
		})
	}
	ev := session.NewEvent(invocationID)
	ev.Actions.StateDelta[PlanStateKey] = map[string]any{
		acpPlanEntriesKey: entries,
	}
	ev.Partial = true
	return ev, true
}

func mapACPContentBlockToPart(logger logger, block acp.ContentBlock) (*genai.Part, bool) {
	if block.Text != nil {
		if block.Text.Text == "" {
			return nil, false
		}
		return genai.NewPartFromText(block.Text.Text), true
	}
	if block.Image != nil {
		part := mapACPImageToPart(block.Image)
		if part != nil {
			return part, true
		}
	}
	if block.Audio != nil {
		part := mapACPAudioToPart(block.Audio)
		if part != nil {
			return part, true
		}
	}
	if block.ResourceLink != nil {
		part := mapACPResourceLinkToPart(block.ResourceLink)
		if part != nil {
			return part, true
		}
	}
	logIgnoredACPContentBlock(logger, block)
	return nil, false
}

func mapACPImageToPart(img *acp.ContentBlockImage) *genai.Part {
	if img == nil {
		return nil
	}
	mimeType := "image/jpeg"
	if img.MimeType != "" {
		mimeType = img.MimeType
	}
	if img.Data != "" {
		imgBytes, err := decodeBase64(img.Data)
		if err != nil {
			return nil
		}
		return genai.NewPartFromBytes(imgBytes, mimeType)
	}
	if img.Uri != nil && *img.Uri != "" {
		return genai.NewPartFromURI(*img.Uri, mimeType)
	}
	return nil
}

func mapACPAudioToPart(audio *acp.ContentBlockAudio) *genai.Part {
	if audio == nil {
		return nil
	}
	mimeType := "audio/wav"
	if audio.MimeType != "" {
		mimeType = audio.MimeType
	}
	if audio.Data != "" {
		audioBytes, err := decodeBase64(audio.Data)
		if err != nil {
			return nil
		}
		return genai.NewPartFromBytes(audioBytes, mimeType)
	}
	return nil
}

func mapACPResourceLinkToPart(link *acp.ContentBlockResourceLink) *genai.Part {
	if link == nil {
		return nil
	}
	if link.Uri != "" {
		mimeType := "application/octet-stream"
		if link.MimeType != nil && *link.MimeType != "" {
			mimeType = *link.MimeType
		}
		return genai.NewPartFromURI(link.Uri, mimeType)
	}
	return nil
}

func decodeBase64(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("empty")
	}
	return base64.StdEncoding.DecodeString(s)
}

func marshalACPUpdatePayload(logger logger, payloadType string, v any) (string, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		logger.Debug().
			Err(err).
			Str("acp_payload_type", payloadType).
			Msg("ignoring acp payload that failed to marshal")
		return "", false
	}
	return string(raw), true
}

func isACPToolStatusLongRunning(status acp.ToolCallStatus) bool {
	return status == acp.ToolCallStatusPending || status == acp.ToolCallStatusInProgress
}

func logUnsupportedACPUpdate(logger logger, ext ExtendedSessionNotification) {
	updateType := extendedSessionUpdateType(ext)
	logEvent := logger.Debug().
		Str("acp_update_type", updateType)

	if updateType == unknownValue {
		logEvent = logEvent.RawJSON("acp_update_payload", ext.Raw)
	} else if payload, ok := marshalACPUpdatePayload(logger, "session_update_"+updateType, ext.Update); ok {
		logEvent = logEvent.Str("acp_update_payload", payload)
	}
	logEvent.Msg("ignoring unsupported acp session update")
}

func logIgnoredACPUpdate(logger logger, updateType string, payload any) {
	logEvent := logger.Debug().
		Str("acp_update_type", updateType)

	if marshaled, ok := marshalACPUpdatePayload(logger, "session_update_"+updateType, payload); ok {
		logEvent = logEvent.Str("acp_update_payload", marshaled)
	}
	logEvent.Msg("ignoring non-user-visible acp session update")
}

func extendedSessionUpdateType(ext ExtendedSessionNotification) string {
	if disc := sessionUpdateType(ext.Update); disc != unknownValue {
		return disc
	}

	var raw map[string]any
	if err := json.Unmarshal(ext.Raw, &raw); err == nil {
		if u, ok := raw["update"].(map[string]any); ok {
			if disc, ok := u["sessionUpdate"].(string); ok {
				return disc
			}
		}
	}
	return unknownValue
}

func logIgnoredACPContentBlock(logger logger, block acp.ContentBlock) {
	blockType := contentBlockType(block)
	logEvent := logger.Debug().
		Str("acp_content_block_type", blockType).
		Str("acp_content_block_text", acpContentBlockLogText(block)).
		Interface("acp_content_block", acpContentBlockLogValue(block))

	if blockType == unknownValue {
		logEvent.Msg("ignoring unsupported acp content block")
		return
	}
	logEvent.Msg("ignoring non-text acp content block")
}

func acpContentBlockLogText(block acp.ContentBlock) string {
	switch {
	case block.Text != nil:
		return strings.TrimSpace(block.Text.Text)
	case block.Image != nil:
		return acpTypeImage
	case block.Audio != nil:
		return acpTypeAudio
	case block.ResourceLink != nil:
		return fmt.Sprintf("resource_link name=%q uri=%q", block.ResourceLink.Name, block.ResourceLink.Uri)
	case block.Resource != nil:
		return acpTypeResource
	default:
		return unknownValue
	}
}

func acpContentBlockLogValue(block acp.ContentBlock) map[string]any {
	switch {
	case block.Text != nil:
		return map[string]any{
			"type": acpTypeText,
			"text": block.Text.Text,
		}
	case block.Image != nil:
		return logACPImageBlockValue(block.Image)
	case block.Audio != nil:
		return logACPAudioBlockValue(block.Audio)
	case block.ResourceLink != nil:
		return logACPResourceLinkBlockValue(block.ResourceLink)
	case block.Resource != nil:
		return map[string]any{
			"type":     acpTypeResource,
			"resource": acpEmbeddedResourceLogValue(block.Resource.Resource),
		}
	default:
		return map[string]any{"type": unknownValue}
	}
}

func logACPImageBlockValue(img *acp.ContentBlockImage) map[string]any {
	obj := map[string]any{"type": acpTypeImage}
	if img.MimeType != "" {
		obj["mime_type"] = img.MimeType
	}
	if img.Uri != nil && *img.Uri != "" {
		obj["uri"] = *img.Uri
	}
	if img.Data != "" {
		obj["data_len"] = len(img.Data)
	}
	return obj
}

func logACPAudioBlockValue(audio *acp.ContentBlockAudio) map[string]any {
	obj := map[string]any{"type": acpTypeAudio}
	if audio.MimeType != "" {
		obj["mime_type"] = audio.MimeType
	}
	if audio.Data != "" {
		obj["data_len"] = len(audio.Data)
	}
	return obj
}

func logACPResourceLinkBlockValue(link *acp.ContentBlockResourceLink) map[string]any {
	obj := map[string]any{"type": "resource_link"}
	if link.Name != "" {
		obj["name"] = link.Name
	}
	if link.Uri != "" {
		obj["uri"] = link.Uri
	}
	if link.Description != nil && *link.Description != "" {
		obj["description"] = *link.Description
	}
	if link.MimeType != nil && *link.MimeType != "" {
		obj["mime_type"] = *link.MimeType
	}
	if link.Size != nil {
		obj["size"] = *link.Size
	}
	if link.Title != nil && *link.Title != "" {
		obj["title"] = *link.Title
	}
	return obj
}

func acpEmbeddedResourceLogValue(resource acp.EmbeddedResourceResource) map[string]any {
	switch {
	case resource.TextResourceContents != nil:
		return logACPTextResourceValue(resource.TextResourceContents)
	case resource.BlobResourceContents != nil:
		return logACPBlobResourceValue(resource.BlobResourceContents)
	default:
		return map[string]any{"kind": unknownValue}
	}
}

func logACPTextResourceValue(res *acp.TextResourceContents) map[string]any {
	obj := map[string]any{"kind": acpTypeText}
	if res.Uri != "" {
		obj["uri"] = res.Uri
	}
	if res.MimeType != nil && *res.MimeType != "" {
		obj["mime_type"] = *res.MimeType
	}
	if res.Text != "" {
		obj["text_len"] = len(res.Text)
	}
	return obj
}

func logACPBlobResourceValue(res *acp.BlobResourceContents) map[string]any {
	obj := map[string]any{"kind": "blob"}
	if res.Uri != "" {
		obj["uri"] = res.Uri
	}
	if res.MimeType != nil && *res.MimeType != "" {
		obj["mime_type"] = *res.MimeType
	}
	if res.Blob != "" {
		obj["blob_len"] = len(res.Blob)
	}
	return obj
}

func sessionUpdateType(update acp.SessionUpdate) string {
	switch {
	case update.UserMessageChunk != nil:
		return "user_message_chunk"
	case update.AgentMessageChunk != nil:
		return "agent_message_chunk"
	case update.AgentThoughtChunk != nil:
		return "agent_thought_chunk"
	case update.ToolCall != nil:
		return "tool_call"
	case update.ToolCallUpdate != nil:
		return "tool_call_update"
	case update.Plan != nil:
		return "plan"
	case update.CurrentModeUpdate != nil:
		return "current_mode_update"
	case update.AvailableCommandsUpdate != nil:
		return "available_commands_update"
	case update.ConfigOptionUpdate != nil:
		return "config_option_update"
	case update.SessionInfoUpdate != nil:
		return "session_info_update"
	case update.UsageUpdate != nil:
		return acpUsageUpdate
	default:
		return unknownValue
	}
}

func contentBlockType(block acp.ContentBlock) string {
	switch {
	case block.Text != nil:
		return acpTypeText
	case block.Image != nil:
		return acpTypeImage
	case block.Audio != nil:
		return acpTypeAudio
	case block.ResourceLink != nil:
		return "resource_link"
	case block.Resource != nil:
		return acpTypeResource
	default:
		return unknownValue
	}
}
