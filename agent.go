package acpagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Config configures an ACP-backed ADK agent.
type Config struct {
	// Context is the base context for the agent's lifecycle.
	//
	// Deprecated: Use NewWithContext to pass the lifecycle context explicitly.
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
	// SessionConfig contains ACP session configuration values applied after
	// session/new or session/resume.
	SessionConfig []SessionConfigValue
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
	// PermissionHandler decides generic agent permission requests at the ADK boundary.
	PermissionHandler PermissionHandler
	// Logger is the slog logger to use for this agent.
	// Trace-level records can contain complete ACP payloads and other sensitive
	// content.
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
	sessionConfig             []SessionConfigValue
	reasoningEffort           string
	outputKey                 string
	instruction               string
	globalInstruction         string
	instructionProvider       InstructionProvider
	globalInstructionProvider InstructionProvider
	logger                    logger
	mcpServers                []acp.McpServer
}

type promptRunResult struct {
	promptResult       *PromptResult
	finalOutput        string
	latestPlanSnapshot map[string]any
	terminalError      *terminalPromptError
}

const (
	defaultAgentName        = "ACPAgent"
	defaultAgentDescription = "ACP coding agent exposed through ADK"
)

var _ adkagent.Agent = (*Agent)(nil)

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
	return newAgent(ctx, cfg)
}

// NewWithContext creates an ADK agent backed by an ACP client process using ctx
// for the agent and subprocess lifecycle.
//
// The caller must pass a non-nil context and call [Agent.Close] to shut down the
// subprocess. Config.Context is ignored.
func NewWithContext(ctx context.Context, cfg Config) (*Agent, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	return newAgent(ctx, cfg)
}

func newAgent(ctx context.Context, cfg Config) (*Agent, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = defaultAgentName
	}
	if strings.TrimSpace(cfg.Description) == "" {
		cfg.Description = defaultAgentDescription
	}

	l := newLogger(cfg.Logger, "acpagent.agent").withContext(ctx)

	client, err := NewClient(ctx, ClientConfig{
		Command:           cfg.Command,
		WorkingDir:        cfg.WorkingDir,
		ClientName:        cfg.ClientName,
		ClientVersion:     cfg.ClientVersion,
		Stderr:            cfg.Stderr,
		PermissionHandler: protocolPermissionHandler(cfg.PermissionHandler),
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
		sessionConfig:             normalizeSessionConfigValues(cfg.SessionConfig),
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
		stateEvent := session.NewEvent(ctx, ctx.InvocationID())
		a.persistSessionStateDelta(stateEvent, remote.id, remote.metaJSON, remote.configValues)
		if len(stateEvent.Actions.StateDelta) > 0 {
			a.logADKEvent(logger, stateEvent, "yielding acp session state event")
			if !yield(stateEvent, nil) {
				return
			}
		}

		result, err := a.runPromptOnce(ctx, logCtx, logger, remote.id, promptForRun, yield)
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
			stateEvent := session.NewEvent(ctx, ctx.InvocationID())
			a.persistSessionStateDelta(stateEvent, remote.id, remote.metaJSON, remote.configValues)
			if len(stateEvent.Actions.StateDelta) > 0 {
				a.logADKEvent(logger, stateEvent, "yielding recovered acp session state event")
				if !yield(stateEvent, nil) {
					return
				}
			}
			result, err = a.runPromptOnce(ctx, logCtx, logger, remote.id, promptForRun, yield)
		}
		if err != nil {
			yield(nil, err)
			return
		}
		ev := session.NewEvent(ctx, ctx.InvocationID())
		if result.promptResult != nil {
			ev.FinishReason = mapACPStopReasonToFinishReason(result.promptResult.Response.StopReason)
			ev.UsageMetadata = mapACPUsageToUsageMetadata(result.promptResult.Usage)
			copyACPProviderErrorMetadata(ev, result.promptResult.Response.Meta)
		}
		switch {
		case result.finalOutput != "":
			ev.Content = genai.NewContentFromText(result.finalOutput, genai.RoleModel)
		case result.terminalError != nil:
			ev.ErrorMessage = result.terminalError.Message
			ev.ErrorCode = result.terminalError.Code
		case result.promptResult != nil && ev.ErrorMessage == "":
			if promptMetaErr, ok := terminalPromptErrorFromPromptMeta(result.promptResult.Response.Meta); ok {
				ev.ErrorMessage = promptMetaErr.Message
				ev.ErrorCode = promptMetaErr.Code
			}
		}
		if result.latestPlanSnapshot != nil {
			ev.Actions.StateDelta[PlanStateKey] = result.latestPlanSnapshot
		}
		a.persistSessionStateDelta(ev, remote.id, remote.metaJSON, remote.configValues)
		a.maybeSaveOutputToState(ev, result.finalOutput)
		ev.TurnComplete = true
		a.logADKEvent(logger, ev, "yielding final turn complete event")
		if !yield(ev, nil) {
			return
		}
	}
}

func (a *Agent) runPromptOnce(ctx adkagent.InvocationContext, logCtx context.Context, logger logger, remoteSessionID, prompt string, yield func(*session.Event, error) bool) (promptRunResult, error) {
	var out promptRunResult

	logger.Debug().
		Str("acp_session_id", remoteSessionID).
		Int("prompt_len", len(prompt)).
		Msg("starting adk invocation")
	if logger.enabled(levelTrace) {
		logger.Trace().
			Str("acp_session_id", remoteSessionID).
			Str("prompt", prompt).
			Msg("starting adk invocation payload")
	}

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
			ev, ok := mapACPUpdateToEvent(ctx, logger, ctx.InvocationID(), ext)
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
			if !yield(ev, nil) {
				return out, nil
			}
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
	if ev == nil || !logger.enabled(levelTrace) {
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
