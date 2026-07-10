package acpagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/adk/v2/session"
)

func TestACPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_HELPER") != "1" {
		return
	}
	runACPHelper(os.Stdin, os.Stdout)
	os.Exit(0)
}

func runACPHelper(stdin *os.File, stdout *os.File) {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	sessionCount := 0
	promptCount := 0
	expectedClientName := os.Getenv("GO_EXPECT_CLIENT_NAME")
	expectedClientVersion := os.Getenv("GO_EXPECT_CLIENT_VERSION")
	expectedSessionModel := os.Getenv("GO_EXPECT_SESSION_MODEL")
	expectedBooleanConfigID := os.Getenv("GO_EXPECT_BOOLEAN_CONFIG_ID")
	expectedBooleanConfigValueRaw := os.Getenv("GO_EXPECT_BOOLEAN_CONFIG_VALUE")
	expectedConfigID := os.Getenv("GO_EXPECT_CONFIG_ID")
	if expectedConfigID == "" {
		expectedConfigID = "model"
	}
	expectedSessionMode := os.Getenv("GO_EXPECT_SESSION_MODE")
	advertiseModeConfigOption := os.Getenv("GO_ADVERTISE_MODE_CONFIG_OPTION") == "1"
	expectedMCPServers := os.Getenv("GO_EXPECT_MCP_SERVERS")
	expectedMCPServersRaw := os.Getenv("GO_EXPECT_MCP_SERVERS_RAW")
	expectedSessionCWD := os.Getenv("GO_EXPECT_SESSION_CWD")
	expectedNewSessionMetaRaw := os.Getenv("GO_EXPECT_NEW_SESSION_META_RAW")
	expectedResumeSessionID := os.Getenv("GO_EXPECT_RESUME_SESSION_ID")
	expectedResumeSessionCWD := os.Getenv("GO_EXPECT_RESUME_SESSION_CWD")
	expectedResumeMetaRaw := os.Getenv("GO_EXPECT_RESUME_META_RAW")
	expectedLoadSessionID := os.Getenv("GO_EXPECT_LOAD_SESSION_ID")
	expectedLoadSessionCWD := os.Getenv("GO_EXPECT_LOAD_SESSION_CWD")
	expectedLoadMetaRaw := os.Getenv("GO_EXPECT_LOAD_META_RAW")
	expectedAuthMethod := os.Getenv("GO_EXPECT_AUTH_METHOD")
	supportSessionResume := os.Getenv("GO_SUPPORT_SESSION_RESUME") == "1"
	supportLoadSession := os.Getenv("GO_SUPPORT_LOAD_SESSION") == "1"
	expectedPromptsRaw := os.Getenv("GO_EXPECT_PROMPTS")
	forceNewSessionID := os.Getenv("GO_FORCE_NEW_SESSION_ID")
	disableSetConfigOption := os.Getenv("GO_DISABLE_SET_CONFIG_OPTION") == "1"
	disableSetMode := os.Getenv("GO_DISABLE_SET_MODE") == "1"
	failResumeMethodNotFound := os.Getenv("GO_FAIL_RESUME_METHOD_NOT_FOUND") == "1"
	failResumeEntityNotFound := os.Getenv("GO_FAIL_RESUME_ENTITY_NOT_FOUND") == "1"
	failResumeInvalidParamsSessionNotFound := os.Getenv("GO_FAIL_RESUME_INVALID_PARAMS_SESSION_NOT_FOUND") == "1"
	failResumeInvalidThread := os.Getenv("GO_FAIL_RESUME_INVALID_THREAD") == "1"
	failResumeInvalidSessionID := os.Getenv("GO_FAIL_RESUME_INVALID_SESSION_ID") == "1"
	failResumeAlreadyExists := os.Getenv("GO_FAIL_RESUME_ALREADY_EXISTS") == "1"
	failFirstResumeInvalidParamsSessionNotFound := os.Getenv("GO_FAIL_FIRST_RESUME_INVALID_PARAMS_SESSION_NOT_FOUND") == "1"
	failLoadMethodNotFound := os.Getenv("GO_FAIL_LOAD_METHOD_NOT_FOUND") == "1"
	failLoadEntityNotFound := os.Getenv("GO_FAIL_LOAD_ENTITY_NOT_FOUND") == "1"
	failAuthenticate := os.Getenv("GO_FAIL_AUTHENTICATE") == "1"
	failFirstPromptEntityNotFound := os.Getenv("GO_FAIL_FIRST_PROMPT_ENTITY_NOT_FOUND") == "1"
	failIfResumeCalled := os.Getenv("GO_FAIL_IF_RESUME_CALLED") == "1"
	failIfLoadCalled := os.Getenv("GO_FAIL_IF_LOAD_CALLED") == "1"
	resumeCount := 0
	var expectedPrompts []string
	if strings.TrimSpace(expectedPromptsRaw) != "" {
		must(json.Unmarshal([]byte(expectedPromptsRaw), &expectedPrompts))
	}
	handleSessionRestore := func(
		msg helperEnvelope,
		method string,
		expectedSessionID string,
		expectedCWD string,
		expectedMetaRaw string,
		failMethodNotFound bool,
		failEntityNotFound bool,
	) {
		if method == acp.AgentMethodSessionResume && failIfResumeCalled {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32000, Message: "session/resume should not have been called"},
			})
			return
		}
		if method == acp.AgentMethodSessionLoad && failIfLoadCalled {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32000, Message: "session/load should not have been called"},
			})
			return
		}
		if method == acp.AgentMethodSessionResume {
			resumeCount++
		}
		if failMethodNotFound {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32601, Message: "unsupported"},
			})
			return
		}
		var req helperSessionRestoreRequest
		must(json.Unmarshal(msg.Params, &req))
		if expectedSessionID != "" && req.SessionID != expectedSessionID {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected %s sessionId: %q, want %q", method, req.SessionID, expectedSessionID)},
			})
			return
		}
		if expectedCWD != "" && req.Cwd != expectedCWD {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected %s cwd: %q, want %q", method, req.Cwd, expectedCWD)},
			})
			return
		}
		if expectedMetaRaw != "" {
			var reqRaw struct {
				Meta json.RawMessage `json:"_meta"`
			}
			must(json.Unmarshal(msg.Params, &reqRaw))
			gotRaw := compactJSONForCompare(reqRaw.Meta)
			wantRaw := compactJSONForCompare([]byte(expectedMetaRaw))
			if gotRaw != wantRaw {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected raw %s _meta payload: %q, want %q", method, gotRaw, wantRaw)},
				})
				return
			}
		}
		if failEntityNotFound {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    500,
					Message: "Requested entity was not found.",
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failResumeInvalidParamsSessionNotFound {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32602,
					Message: "Invalid params",
					Data:    "session not found",
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failResumeInvalidThread {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32603,
					Message: "Internal error",
					Data: map[string]any{
						"error": "thread/resume: bridge backend rpc error (-32600): invalid thread id: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `s` at 1",
					},
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failResumeInvalidSessionID {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32603,
					Message: "Internal error",
					Data: map[string]any{
						"error": "thread/resume: bridge backend rpc error (-32600): invalid session id: invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `s` at 1",
					},
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failResumeAlreadyExists {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32602,
					Message: "Invalid params",
					Data:    fmt.Sprintf("session %q already exists", req.SessionID),
				},
			})
			return
		}
		if method == acp.AgentMethodSessionResume && failFirstResumeInvalidParamsSessionNotFound && resumeCount == 1 {
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &helperError{
					Code:    -32602,
					Message: "Invalid params",
					Data:    "session not found",
				},
			})
			return
		}
		writeEnvelope(stdout, helperEnvelope{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Result: mustJSON(helperSessionRestoreResponse{
				ConfigOptions: helperSessionConfigOptions(expectedSessionModel, expectedConfigID, expectedSessionMode, advertiseModeConfigOption, expectedBooleanConfigID, expectedBooleanConfigValueRaw),
				Modes:         helperSessionModes(expectedSessionMode),
			}),
		})
	}

	for scanner.Scan() {
		var msg helperEnvelope
		must(json.Unmarshal(scanner.Bytes(), &msg))
		switch msg.Method {
		case acp.AgentMethodInitialize:
			var req helperInitializeRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedClientName != "" && req.ClientInfo.Name != expectedClientName {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected client name: %s", req.ClientInfo.Name)},
				})
				continue
			}
			if expectedClientVersion != "" && req.ClientInfo.Version != expectedClientVersion {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected client version: %s", req.ClientInfo.Version)},
				})
				continue
			}
			initResp := helperInitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}
			if supportSessionResume || supportLoadSession {
				initResp.AgentCapabilities = &helperAgentCapabilities{}
				if supportLoadSession {
					initResp.AgentCapabilities.LoadSession = true
				}
				if supportSessionResume {
					initResp.AgentCapabilities.SessionCapabilities = &helperSessionCapabilities{
						Resume: &helperSessionResumeCapabilities{},
					}
				}
			}
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(initResp)})
		case acp.AgentMethodAuthenticate:
			var req helperAuthenticateRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedAuthMethod != "" && req.MethodID != expectedAuthMethod {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected auth method: %q, want %q", req.MethodID, expectedAuthMethod)},
				})
				continue
			}
			if failAuthenticate {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32603, Message: "Authentication failed"},
				})
				continue
			}
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperAuthenticateResponse{})})
		case acp.AgentMethodSessionNew:
			var req helperNewSessionRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedSessionCWD != "" && req.Cwd != expectedSessionCWD {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session cwd: %q, want %q", req.Cwd, expectedSessionCWD)},
				})
				continue
			}
			if expectedMCPServersRaw != "" {
				var reqRaw struct {
					McpServers json.RawMessage `json:"mcpServers"`
				}
				must(json.Unmarshal(msg.Params, &reqRaw))
				gotRaw := compactJSONForCompare(reqRaw.McpServers)
				wantRaw := compactJSONForCompare([]byte(expectedMCPServersRaw))
				if gotRaw != wantRaw {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected raw mcp servers payload: %q, want %q", gotRaw, wantRaw)},
					})
					continue
				}
			}
			if expectedMCPServers != "" {
				gotJSON, _ := json.Marshal(req.McpServers)
				// Basic string comparison of JSON might be flaky if key order differs,
				// but for simple struct it might work if mostly empty.
				// Better to unmarshal expected and compare.
				var expected []acp.McpServer
				must(json.Unmarshal([]byte(expectedMCPServers), &expected))

				// Re-marshal both to ensure consistent ordering/formatting if possible,
				// or just check count and first element name.
				if len(req.McpServers) != len(expected) {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected mcp servers count: %d, want %d", len(req.McpServers), len(expected))},
					})
					continue
				}
				if len(expected) > 0 {
					if req.McpServers[0].Stdio == nil || expected[0].Stdio == nil || req.McpServers[0].Stdio.Name != expected[0].Stdio.Name {
						writeEnvelope(stdout, helperEnvelope{
							JSONRPC: "2.0",
							ID:      msg.ID,
							Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected mcp server: %s", string(gotJSON))},
						})
						continue
					}
				}
			}
			if expectedNewSessionMetaRaw != "" {
				var reqRaw struct {
					Meta json.RawMessage `json:"_meta"`
				}
				must(json.Unmarshal(msg.Params, &reqRaw))
				gotRaw := compactJSONForCompare(reqRaw.Meta)
				wantRaw := compactJSONForCompare([]byte(expectedNewSessionMetaRaw))
				if gotRaw != wantRaw {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected raw session/new _meta payload: %q, want %q", gotRaw, wantRaw)},
					})
					continue
				}
			}
			sessionCount++
			sessionID := fmt.Sprintf("session-%d", sessionCount)
			if forceNewSessionID != "" {
				sessionID = forceNewSessionID
			}
			writeEnvelope(stdout, helperEnvelope{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Result: mustJSON(helperNewSessionResponse{
					SessionID:     sessionID,
					ConfigOptions: helperSessionConfigOptions(expectedSessionModel, expectedConfigID, expectedSessionMode, advertiseModeConfigOption, expectedBooleanConfigID, expectedBooleanConfigValueRaw),
					Modes:         helperSessionModes(expectedSessionMode),
				}),
			})
		case acp.AgentMethodSessionResume:
			handleSessionRestore(
				msg,
				"session/resume",
				expectedResumeSessionID,
				expectedResumeSessionCWD,
				expectedResumeMetaRaw,
				failResumeMethodNotFound,
				failResumeEntityNotFound,
			)
		case acp.AgentMethodSessionLoad:
			handleSessionRestore(
				msg,
				"session/load",
				expectedLoadSessionID,
				expectedLoadSessionCWD,
				expectedLoadMetaRaw,
				failLoadMethodNotFound,
				failLoadEntityNotFound,
			)
		case acp.AgentMethodSessionSetConfigOption:
			if disableSetConfigOption {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32601, Message: "unsupported"},
				})
				continue
			}
			var req helperSetSessionConfigOptionRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedSessionModel != "" && expectedConfigID != "" && req.ConfigID != expectedConfigID {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session config id: %s", req.ConfigID)},
				})
				continue
			}
			reqValue := req.ValueString()
			if expectedSessionModel != "" && req.ConfigID == expectedConfigID && reqValue != expectedSessionModel {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session model: %s", reqValue)},
				})
				continue
			}
			if advertiseModeConfigOption && req.ConfigID == "mode" && expectedSessionMode != "" && reqValue != expectedSessionMode {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session mode config value: %s", reqValue)},
				})
				continue
			}
			if expectedBooleanConfigID != "" && req.ConfigID == expectedBooleanConfigID {
				expectedBooleanConfigValue := expectedBooleanConfigValueRaw == "true"
				if req.Type != "boolean" {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session config type: %s", req.Type)},
					})
					continue
				}
				reqBool := req.ValueBool()
				if reqBool == nil || *reqBool != expectedBooleanConfigValue {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected boolean session config value: %v", reqBool)},
					})
					continue
				}
			}
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperSetSessionConfigOptionResponse{ConfigOptions: helperSessionConfigOptions(expectedSessionModel, expectedConfigID, expectedSessionMode, advertiseModeConfigOption, expectedBooleanConfigID, expectedBooleanConfigValueRaw)})})
		case acp.AgentMethodSessionSetMode:
			if disableSetMode {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32601, Message: "unsupported"},
				})
				continue
			}
			var req helperSetSessionModeRequest
			must(json.Unmarshal(msg.Params, &req))
			if expectedSessionMode != "" && req.ModeID != expectedSessionMode {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected session mode: %s", req.ModeID)},
				})
				continue
			}
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperSetSessionModeResponse{})})
		case acp.AgentMethodSessionPrompt:
			var req helperPromptRequest
			must(json.Unmarshal(msg.Params, &req))
			promptCount++
			if failFirstPromptEntityNotFound && promptCount == 1 {
				writeEnvelope(stdout, helperEnvelope{
					JSONRPC: "2.0",
					ID:      msg.ID,
					Error: &helperError{
						Code:    500,
						Message: "Requested entity was not found.",
					},
				})
				continue
			}
			prompt := req.Prompt[0].Text
			if len(expectedPrompts) > 0 {
				if promptCount > len(expectedPrompts) {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected extra prompt %d: %q", promptCount, prompt)},
					})
					continue
				}
				wantPrompt := expectedPrompts[promptCount-1]
				if prompt != wantPrompt {
					writeEnvelope(stdout, helperEnvelope{
						JSONRPC: "2.0",
						ID:      msg.ID,
						Error:   &helperError{Code: -32000, Message: fmt.Sprintf("unexpected prompt[%d]: %q, want %q", promptCount, prompt, wantPrompt)},
					})
					continue
				}
			}
			responsePrompt := prompt
			if _, after, ok := strings.Cut(prompt, "\n\nUser message:\n"); ok {
				responsePrompt = after
			}
			if strings.HasPrefix(responsePrompt, "slow:") {
				time.Sleep(150 * time.Millisecond)
				responsePrompt = strings.TrimPrefix(responsePrompt, "slow:")
			}
			if responsePrompt == "permission" {
				title := "Edit file"
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: json.RawMessage(`"perm-1"`), Method: acp.ClientMethodSessionRequestPermission, Params: mustJSON(helperPermissionRequest{
					SessionID: req.SessionID,
					ToolCall:  helperPermissionToolCall{Title: &title},
					Options: []helperPermissionOption{
						{Kind: string(acp.PermissionOptionKindAllowOnce), Name: "Allow", OptionID: "allow"},
						{Kind: string(acp.PermissionOptionKindRejectOnce), Name: "Reject", OptionID: "reject"},
					},
				})})
				if !scanner.Scan() {
					return
				}
				var permitResp helperEnvelope
				must(json.Unmarshal(scanner.Bytes(), &permitResp))
				var outcome helperPermissionResponse
				must(json.Unmarshal(permitResp.Result, &outcome))
				text := "rejected"
				if outcome.Outcome.Outcome == "selected" && outcome.Outcome.OptionID == "allow" {
					text = "approved"
				}
				writeUpdate(stdout, req.SessionID, text)
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
				continue
			}
			if responsePrompt == "tooling" {
				writeToolCall(stdout, req.SessionID, testACPToolID, "run shell", acp.ToolCallStatusInProgress)
				writeToolCallUpdate(stdout, req.SessionID, testACPToolID, acp.ToolCallStatusCompleted, map[string]any{"ok": true})
				writeUpdate(stdout, req.SessionID, "tooling-done")
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
				continue
			}
			if responsePrompt == testACPPlanPrompt {
				writePlanUpdate(stdout, req.SessionID, []acp.PlanEntry{
					{
						Content:  "Run tests",
						Status:   acp.PlanEntryStatusInProgress,
						Priority: acp.PlanEntryPriorityMedium,
					},
				})
				writePlanUpdate(stdout, req.SessionID, []acp.PlanEntry{
					{
						Content:  "Run tests",
						Status:   acp.PlanEntryStatusCompleted,
						Priority: acp.PlanEntryPriorityMedium,
					},
					{
						Content:  "Run linters",
						Status:   acp.PlanEntryStatusPending,
						Priority: acp.PlanEntryPriorityHigh,
					},
				})
				writeUpdate(stdout, req.SessionID, "planning-done")
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
				continue
			}
			if responsePrompt == "terminal-error" {
				writePromptErrorNotification(stdout, req.SessionID, "Reconnecting... 1/5", true)
				writePromptErrorNotification(stdout, req.SessionID, "Reconnecting... 2/5", true)
				writeTurnCompletedFailure(stdout, req.SessionID, "unexpected status 401 Unauthorized: Missing bearer or basic authentication in header", "other")
				writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
				continue
			}
			if responsePrompt == "retry-then-success" {
				writePromptErrorNotification(stdout, req.SessionID, "Reconnecting... 1/5", true)
				writePromptErrorNotification(stdout, req.SessionID, "Reconnecting... 2/5", true)
			}
			prefix := req.SessionID + ":"
			writeUpdate(stdout, req.SessionID, prefix)
			writeUpdate(stdout, req.SessionID, responsePrompt)
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Result: mustJSON(helperPromptResponse{StopReason: string(acp.StopReasonEndTurn)})})
		case acp.AgentMethodSessionCancel:
			// Ignore in helper.
		default:
			writeEnvelope(stdout, helperEnvelope{JSONRPC: "2.0", ID: msg.ID, Error: &helperError{Code: -32601, Message: "unsupported"}})
		}
	}
}

func writeUpdate(stdout *os.File, sessionID, text string) {
	writeSessionUpdate(stdout, sessionID, map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content": map[string]any{
			"type": "text",
			"text": text,
		},
	})
}

func writeToolCall(stdout *os.File, sessionID, toolCallID, title string, status acp.ToolCallStatus) {
	writeSessionUpdate(stdout, sessionID, map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    toolCallID,
		"title":         title,
		"kind":          acp.ToolKindExecute,
		"status":        status,
		"rawInput": map[string]any{
			"cmd": "ls",
		},
	})
}

func writeToolCallUpdate(stdout *os.File, sessionID, toolCallID string, status acp.ToolCallStatus, rawOutput map[string]any) {
	writeSessionUpdate(stdout, sessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    toolCallID,
		"status":        status,
		"rawOutput":     rawOutput,
	})
}

func writePlanUpdate(stdout *os.File, sessionID string, entries []acp.PlanEntry) {
	writeSessionUpdate(stdout, sessionID, map[string]any{
		"sessionUpdate":   "plan",
		acpPlanEntriesKey: entries,
	})
}

func writePromptErrorNotification(stdout *os.File, sessionID, message string, willRetry bool) {
	writeEnvelope(stdout, helperEnvelope{
		JSONRPC: "2.0",
		Method:  "error",
		Params: mustJSON(map[string]any{
			"threadId":  sessionID,
			"willRetry": willRetry,
			"error": map[string]any{
				"message":        message,
				"codexErrorInfo": map[string]any{"responseStreamDisconnected": map[string]any{"httpStatusCode": 401}},
			},
		}),
	})
}

func writeTurnCompletedFailure(stdout *os.File, sessionID, message, code string) {
	writeEnvelope(stdout, helperEnvelope{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params: mustJSON(map[string]any{
			"threadId": sessionID,
			"turn": map[string]any{
				"id":     "turn-1",
				"status": "failed",
				"items":  []any{},
				"error": map[string]any{
					"message":        message,
					"codexErrorInfo": code,
				},
			},
		}),
	})
}

func writeSessionUpdate(stdout *os.File, sessionID string, update map[string]any) {
	writeEnvelope(stdout, helperEnvelope{
		JSONRPC: "2.0",
		Method:  acp.ClientMethodSessionUpdate,
		Params: mustJSON(map[string]any{
			"sessionId": sessionID,
			"update":    update,
		}),
	})
}

func writeEnvelope(stdout *os.File, msg helperEnvelope) {
	must(json.NewEncoder(stdout).Encode(msg))
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	must(err)
	return data
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func compactJSONForCompare(raw []byte) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return out.String()
}

type helperEnvelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *helperError    `json:"error,omitempty"`
}

type helperError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type helperInitializeResponse struct {
	AgentCapabilities *helperAgentCapabilities `json:"agentCapabilities,omitempty"`
	ProtocolVersion   int                      `json:"protocolVersion"`
}

type helperInitializeRequest struct {
	ClientInfo helperImplementation `json:"clientInfo"`
}

type helperAgentCapabilities struct {
	LoadSession         bool                       `json:"loadSession,omitempty"`
	SessionCapabilities *helperSessionCapabilities `json:"sessionCapabilities,omitempty"`
}

type helperSessionCapabilities struct {
	Resume *helperSessionResumeCapabilities `json:"resume,omitempty"`
}

type helperSessionResumeCapabilities struct{}

type helperImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type helperAuthenticateRequest struct {
	MethodID string `json:"methodId"`
}

type helperAuthenticateResponse struct{}

type helperNewSessionResponse struct {
	ConfigOptions []acp.SessionConfigOption `json:"configOptions,omitempty"`
	Modes         *acp.SessionModeState     `json:"modes,omitempty"`
	SessionID     string                    `json:"sessionId"`
}

type helperNewSessionRequest struct {
	Meta       map[string]any  `json:"_meta,omitempty"`
	Cwd        string          `json:"cwd"`
	McpServers []acp.McpServer `json:"mcpServers,omitempty"`
}

type helperSessionRestoreRequest struct {
	Meta       map[string]any  `json:"_meta,omitempty"`
	Cwd        string          `json:"cwd"`
	McpServers []acp.McpServer `json:"mcpServers,omitempty"`
	SessionID  string          `json:"sessionId"`
}

type helperSessionRestoreResponse struct {
	ConfigOptions []acp.SessionConfigOption `json:"configOptions,omitempty"`
	Modes         *acp.SessionModeState     `json:"modes,omitempty"`
}

type helperPromptResponse struct {
	StopReason string `json:"stopReason"`
}

type helperSetSessionConfigOptionRequest struct {
	SessionID string          `json:"sessionId"`
	ConfigID  string          `json:"configId"`
	Type      string          `json:"type,omitempty"`
	ValueRaw  json.RawMessage `json:"value"`
}

func (r helperSetSessionConfigOptionRequest) ValueString() string {
	var value string
	_ = json.Unmarshal(r.ValueRaw, &value)
	return value
}

func (r helperSetSessionConfigOptionRequest) ValueBool() *bool {
	var value bool
	if err := json.Unmarshal(r.ValueRaw, &value); err != nil {
		return nil
	}
	return &value
}

type helperSetSessionConfigOptionResponse struct {
	ConfigOptions []acp.SessionConfigOption `json:"configOptions"`
}

func helperSessionConfigOptions(model, configID, mode string, includeMode bool, booleanConfigID, booleanValueRaw string) []acp.SessionConfigOption {
	options := helperModelConfigOptions(model, configID)
	if strings.TrimSpace(mode) != "" && includeMode {
		modeCategory := acp.SessionConfigOptionCategoryMode
		option := acp.NewSessionConfigOptionSelect(
			acp.SessionConfigValueId(mode),
			acp.SessionConfigSelectOptions{
				Ungrouped: &acp.SessionConfigSelectOptionsUngrouped{
					{Name: mode, Value: acp.SessionConfigValueId(mode)},
				},
			},
		)
		option.Select.Id = "mode"
		option.Select.Name = "Mode"
		option.Select.Category = &modeCategory
		options = append(options, option)
	}
	if strings.TrimSpace(booleanConfigID) != "" {
		modelConfigCategory := acp.SessionConfigOptionCategory("model_config")
		options = append(options, acp.SessionConfigOption{
			Boolean: &acp.SessionConfigOptionBoolean{
				Id:           acp.SessionConfigId(booleanConfigID),
				Name:         "Fast mode",
				Category:     &modelConfigCategory,
				CurrentValue: booleanValueRaw == "true",
				Type:         "boolean",
			},
		})
	}
	return options
}

func helperModelConfigOptions(model, configID string) []acp.SessionConfigOption {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	modelCategory := acp.SessionConfigOptionCategoryModel
	option := acp.NewSessionConfigOptionSelect(
		acp.SessionConfigValueId(model),
		acp.SessionConfigSelectOptions{
			Ungrouped: &acp.SessionConfigSelectOptionsUngrouped{
				{Name: model, Value: acp.SessionConfigValueId(model)},
			},
		},
	)
	option.Select.Id = acp.SessionConfigId(configID)
	option.Select.Name = "Model"
	option.Select.Category = &modelCategory
	return []acp.SessionConfigOption{option}
}

func helperSessionModes(mode string) *acp.SessionModeState {
	if strings.TrimSpace(mode) == "" {
		return nil
	}
	return &acp.SessionModeState{
		CurrentModeId: acp.SessionModeId(mode),
		AvailableModes: []acp.SessionMode{{
			Id:   acp.SessionModeId(mode),
			Name: mode,
		}},
	}
}

type helperSetSessionModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type helperSetSessionModeResponse struct{}

type helperPromptRequest struct {
	SessionID string              `json:"sessionId"`
	Prompt    []helperContentPart `json:"prompt"`
}

type helperContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type helperPermissionRequest struct {
	SessionID string                   `json:"sessionId"`
	Options   []helperPermissionOption `json:"options"`
	ToolCall  helperPermissionToolCall `json:"toolCall"`
}

type helperPermissionOption struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	OptionID string `json:"optionId"`
}

type helperPermissionToolCall struct {
	Title *string `json:"title,omitempty"`
}

type helperPermissionResponse struct {
	Outcome helperPermissionOutcome `json:"outcome"`
}

type helperPermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

func readPromptOutput(t *testing.T, updates <-chan ExtendedSessionNotification, resultCh <-chan PromptResult) string {
	t.Helper()
	var chunks []string
	for note := range updates {
		ev, ok := mapACPUpdateToEvent(t.Context(), newLogger(nil, ""), "inv-1", ExtendedSessionNotification{SessionNotification: note.SessionNotification, Raw: note.Raw})
		if ok {
			if text := extractPromptText(ev.Content); text != "" {
				chunks = append(chunks, text)
			}
		}
	}
	result := <-resultCh
	if result.Err != nil {
		t.Fatalf("PromptResult.Err = %v", result.Err)
	}
	return strings.Join(chunks, "")
}

func TestMapACPPlanUpdate(t *testing.T) {
	tests := []struct {
		name        string
		plan        *acp.SessionUpdatePlan
		wantOK      bool
		wantEntries []map[string]any
	}{
		{
			name:   "nil plan",
			plan:   nil,
			wantOK: false,
		},
		{
			name:   "empty entries",
			plan:   &acp.SessionUpdatePlan{Entries: []acp.PlanEntry{}},
			wantOK: false,
		},
		{
			name: "single entry",
			plan: &acp.SessionUpdatePlan{
				Entries: []acp.PlanEntry{
					{
						Content:  "Run tests",
						Status:   acp.PlanEntryStatusInProgress,
						Priority: acp.PlanEntryPriorityMedium,
					},
				},
			},
			wantOK: true,
			wantEntries: []map[string]any{
				{
					"content":  "Run tests",
					"status":   acp.PlanEntryStatusInProgress,
					"priority": acp.PlanEntryPriorityMedium,
				},
			},
		},
		{
			name: "multiple entries",
			plan: &acp.SessionUpdatePlan{
				Entries: []acp.PlanEntry{
					{Content: "Step 1", Status: acp.PlanEntryStatusCompleted},
					{Content: "Step 2", Status: acp.PlanEntryStatusPending},
				},
			},
			wantOK: true,
			wantEntries: []map[string]any{
				{
					"content":  "Step 1",
					"status":   acp.PlanEntryStatusCompleted,
					"priority": acp.PlanEntryPriority(""),
				},
				{
					"content":  "Step 2",
					"status":   acp.PlanEntryStatusPending,
					"priority": acp.PlanEntryPriority(""),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok := mapACPPlanUpdate(t.Context(), "inv-1", tt.plan)
			if ok != tt.wantOK {
				t.Errorf("mapACPPlanUpdate() ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && ev == nil {
				t.Errorf("mapACPPlanUpdate() ev = nil, want event")
			}
			if !tt.wantOK {
				return
			}
			if ev.Content != nil {
				t.Fatalf("mapACPPlanUpdate() content = %#v, want nil", ev.Content)
			}
			if !ev.Partial {
				t.Fatal("mapACPPlanUpdate() Partial = false, want true")
			}
			gotSnapshot, ok := planStateSnapshotFromEvent(t, ev)
			if !ok {
				t.Fatalf("mapACPPlanUpdate() missing plan state delta")
			}
			if diff := cmp.Diff(tt.wantEntries, planSnapshotEntries(t, gotSnapshot)); diff != "" {
				t.Errorf("mapACPPlanUpdate() entries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func planStateSnapshotFromEvent(t *testing.T, ev *session.Event) (map[string]any, bool) {
	t.Helper()
	if ev == nil || ev.Actions.StateDelta == nil {
		return nil, false
	}
	rawSnapshot, ok := ev.Actions.StateDelta[PlanStateKey]
	if !ok {
		return nil, false
	}
	return planSnapshotFromValue(t, rawSnapshot), true
}

func planSnapshotFromValue(t *testing.T, rawSnapshot any) map[string]any {
	t.Helper()
	snapshot, ok := rawSnapshot.(map[string]any)
	if !ok {
		t.Fatalf("plan snapshot type = %T, want map[string]any", rawSnapshot)
	}
	return snapshot
}

func planSnapshotEntries(t *testing.T, snapshot map[string]any) []map[string]any {
	t.Helper()
	rawEntries, ok := snapshot[acpPlanEntriesKey]
	if !ok {
		t.Fatalf("plan snapshot missing %q", acpPlanEntriesKey)
	}
	switch entries := rawEntries.(type) {
	case []map[string]any:
		return entries
	case []any:
		normalized := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			entryMap, ok := entry.(map[string]any)
			if !ok {
				t.Fatalf("plan entry type = %T, want map[string]any", entry)
			}
			normalized = append(normalized, entryMap)
		}
		return normalized
	default:
		t.Fatalf("plan entries type = %T, want []map[string]any", rawEntries)
		return nil
	}
}
