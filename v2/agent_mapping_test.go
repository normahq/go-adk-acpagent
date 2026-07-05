package acpagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestMapACPUserAndThoughtChunks(t *testing.T) {
	t.Parallel()

	userEv, ok := mapACPUserMessageChunk(context.Background(), newLogger(nil, ""), "inv-user", &acp.SessionUpdateUserMessageChunk{
		Content: acp.TextBlock("user text"),
	})
	if !ok {
		t.Fatal("mapACPUserMessageChunk() ok = false, want true")
	}
	if userEv.Content.Role != genai.RoleUser || extractPromptText(userEv.Content) != "user text" || !userEv.Partial {
		t.Fatalf("user event = %#v", userEv)
	}

	thoughtEv, ok := mapACPAgentThoughtChunk(context.Background(), newLogger(nil, ""), "inv-thought", &acp.SessionUpdateAgentThoughtChunk{
		Content: acp.TextBlock("thinking"),
	})
	if !ok {
		t.Fatal("mapACPAgentThoughtChunk() ok = false, want true")
	}
	if thoughtEv.Content.Role != genai.RoleModel || extractPromptText(thoughtEv.Content) != "thinking" || !thoughtEv.Content.Parts[0].Thought {
		t.Fatalf("thought event = %#v", thoughtEv)
	}
}

func TestMapACPAgentMessageChunkMetadataVariants(t *testing.T) {
	t.Parallel()

	legacyID := "legacy-message"
	ev, ok := mapACPAgentMessageChunk(context.Background(), newLogger(nil, ""), "inv-message", &acp.SessionUpdateAgentMessageChunk{
		Content:   acp.TextBlock("agent text"),
		MessageId: &legacyID,
		Meta:      map[string]any{"messageId": "meta-message"},
	})
	if !ok {
		t.Fatal("mapACPAgentMessageChunk() ok = false, want true")
	}
	if got := ev.CustomMetadata["acp_message_id"]; got != legacyID {
		t.Fatalf("acp_message_id = %#v, want %q", got, legacyID)
	}

	if ev, ok := mapACPAgentMessageChunk(context.Background(), newLogger(nil, ""), "inv-empty", &acp.SessionUpdateAgentMessageChunk{}); ok || ev != nil {
		t.Fatalf("mapACPAgentMessageChunk(empty) = (%#v, %v), want nil false", ev, ok)
	}
}

func TestMapACPContentBlockToPartMedia(t *testing.T) {
	t.Parallel()

	imageData := base64.StdEncoding.EncodeToString([]byte("image"))
	imagePart, ok := mapACPContentBlockToPart(newLogger(nil, ""), acp.ImageBlock(imageData, "image/png"))
	if !ok {
		t.Fatal("image content block ok = false, want true")
	}
	if got := string(imagePart.InlineData.Data); got != "image" {
		t.Fatalf("image bytes = %q, want image", got)
	}
	if got := imagePart.InlineData.MIMEType; got != "image/png" {
		t.Fatalf("image MIMEType = %q, want image/png", got)
	}

	imageURI := "file:///tmp/image.jpg"
	uriPart, ok := mapACPContentBlockToPart(newLogger(nil, ""), acp.ContentBlock{
		Image: &acp.ContentBlockImage{Uri: &imageURI},
	})
	if !ok {
		t.Fatal("image URI content block ok = false, want true")
	}
	if uriPart.FileData.FileURI != imageURI || uriPart.FileData.MIMEType != "image/jpeg" {
		t.Fatalf("image URI part = %#v", uriPart.FileData)
	}

	audioData := base64.StdEncoding.EncodeToString([]byte("audio"))
	audioPart, ok := mapACPContentBlockToPart(newLogger(nil, ""), acp.AudioBlock(audioData, "audio/mpeg"))
	if !ok {
		t.Fatal("audio content block ok = false, want true")
	}
	if got := string(audioPart.InlineData.Data); got != "audio" {
		t.Fatalf("audio bytes = %q, want audio", got)
	}
	if got := audioPart.InlineData.MIMEType; got != "audio/mpeg" {
		t.Fatalf("audio MIMEType = %q, want audio/mpeg", got)
	}

	mimeType := "text/markdown"
	linkPart, ok := mapACPContentBlockToPart(newLogger(nil, ""), acp.ContentBlock{
		ResourceLink: &acp.ContentBlockResourceLink{Uri: "file:///tmp/readme.md", MimeType: &mimeType},
	})
	if !ok {
		t.Fatal("resource link content block ok = false, want true")
	}
	if linkPart.FileData.FileURI != "file:///tmp/readme.md" || linkPart.FileData.MIMEType != "text/markdown" {
		t.Fatalf("resource link part = %#v", linkPart.FileData)
	}
}

func TestMapACPContentBlockToPartRejectsUnsupportedMedia(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block acp.ContentBlock
	}{
		{name: "empty text", block: acp.TextBlock("")},
		{name: "bad image data", block: acp.ImageBlock("not-base64", "image/png")},
		{name: "empty image", block: acp.ContentBlock{Image: &acp.ContentBlockImage{}}},
		{name: "bad audio data", block: acp.AudioBlock("not-base64", "audio/wav")},
		{name: "empty audio", block: acp.ContentBlock{Audio: &acp.ContentBlockAudio{}}},
		{name: "empty resource link", block: acp.ResourceLinkBlock("empty", "")},
		{name: "embedded resource", block: acp.ResourceBlock(acp.EmbeddedResourceResource{})},
		{name: "unknown", block: acp.ContentBlock{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if part, ok := mapACPContentBlockToPart(newLogger(nil, ""), tc.block); ok {
				t.Fatalf("mapACPContentBlockToPart() = (%#v, true), want false", part)
			}
		})
	}
}

func TestMapACPUpdateToEventIgnoredAndLegacyUsage(t *testing.T) {
	t.Parallel()

	ignoredUpdates := []acp.SessionUpdate{
		{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{}},
		{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{}},
		{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{}},
		{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
		{UsageUpdate: &acp.SessionUsageUpdate{Size: 100, Used: 25}},
	}
	for i, update := range ignoredUpdates {
		t.Run(fmt.Sprintf("ignored_%d", i), func(t *testing.T) {
			t.Parallel()
			if ev, ok := mapACPUpdateToEvent(context.Background(), newLogger(nil, ""), "inv", ExtendedSessionNotification{SessionNotification: acp.SessionNotification{Update: update}}); ok {
				t.Fatalf("mapACPUpdateToEvent() = (%#v, true), want false", ev)
			}
		})
	}

	ev, ok := mapACPUpdateToEvent(context.Background(), newLogger(nil, ""), "inv-usage", ExtendedSessionNotification{
		SessionNotification: acp.SessionNotification{},
		Raw:                 []byte(`{"update":{"sessionUpdate":"usage_update","inputTokens":7,"outputTokens":11,"totalTokens":18}}`),
	})
	if !ok {
		t.Fatal("legacy usage update ok = false, want true")
	}
	if ev.UsageMetadata == nil || ev.UsageMetadata.PromptTokenCount != 7 || ev.UsageMetadata.CandidatesTokenCount != 11 || ev.UsageMetadata.TotalTokenCount != 18 {
		t.Fatalf("UsageMetadata = %#v", ev.UsageMetadata)
	}
	if !ev.Partial {
		t.Fatal("legacy usage event Partial = false, want true")
	}

	if ev, ok := mapACPLegacyUsageUpdate(context.Background(), newLogger(nil, ""), "inv-empty", map[string]any{"sessionUpdate": acpUsageUpdate}); ok {
		t.Fatalf("mapACPLegacyUsageUpdate(empty) = (%#v, true), want false", ev)
	}
}

func TestContentBlockLogHelpers(t *testing.T) {
	t.Parallel()

	description := "docs"
	mimeType := "text/plain"
	size := 42
	title := "Readme"
	link := acp.ContentBlock{
		ResourceLink: &acp.ContentBlockResourceLink{
			Name:        "README.md",
			Uri:         "file:///README.md",
			Description: &description,
			MimeType:    &mimeType,
			Size:        &size,
			Title:       &title,
		},
	}
	if got := acpContentBlockLogText(link); !strings.Contains(got, "README.md") || !strings.Contains(got, "file:///README.md") {
		t.Fatalf("acpContentBlockLogText(link) = %q", got)
	}
	wantLink := map[string]any{
		"type":        "resource_link",
		"name":        "README.md",
		"uri":         "file:///README.md",
		"description": "docs",
		"mime_type":   "text/plain",
		"size":        42,
		"title":       "Readme",
	}
	if got := acpContentBlockLogValue(link); !reflect.DeepEqual(got, wantLink) {
		t.Fatalf("acpContentBlockLogValue(link) = %#v, want %#v", got, wantLink)
	}

	textResource := acp.ResourceBlock(acp.EmbeddedResourceResource{
		TextResourceContents: &acp.TextResourceContents{Uri: "file:///a.txt", MimeType: &mimeType, Text: "hello"},
	})
	wantTextResource := map[string]any{
		"type": "resource",
		"resource": map[string]any{
			"kind":      "text",
			"uri":       "file:///a.txt",
			"mime_type": "text/plain",
			"text_len":  5,
		},
	}
	if got := acpContentBlockLogValue(textResource); !reflect.DeepEqual(got, wantTextResource) {
		t.Fatalf("acpContentBlockLogValue(text resource) = %#v, want %#v", got, wantTextResource)
	}

	blobResource := acp.ResourceBlock(acp.EmbeddedResourceResource{
		BlobResourceContents: &acp.BlobResourceContents{Uri: "file:///a.bin", MimeType: &mimeType, Blob: "abcd"},
	})
	wantBlobResource := map[string]any{
		"type": "resource",
		"resource": map[string]any{
			"kind":      "blob",
			"uri":       "file:///a.bin",
			"mime_type": "text/plain",
			"blob_len":  4,
		},
	}
	if got := acpContentBlockLogValue(blobResource); !reflect.DeepEqual(got, wantBlobResource) {
		t.Fatalf("acpContentBlockLogValue(blob resource) = %#v, want %#v", got, wantBlobResource)
	}

	if got := acpEmbeddedResourceLogValue(acp.EmbeddedResourceResource{}); !reflect.DeepEqual(got, map[string]any{"kind": unknownValue}) {
		t.Fatalf("acpEmbeddedResourceLogValue(empty) = %#v", got)
	}
}

func TestSessionUpdateAndContentBlockTypes(t *testing.T) {
	t.Parallel()

	status := acp.ToolCallStatusInProgress
	updateTypeTests := []struct {
		name   string
		update acp.SessionUpdate
		want   string
	}{
		{name: "user", update: acp.UpdateUserMessageText("hi"), want: "user_message_chunk"},
		{name: "agent", update: acp.UpdateAgentMessageText("hi"), want: "agent_message_chunk"},
		{name: "thought", update: acp.UpdateAgentThoughtText("thinking"), want: "agent_thought_chunk"},
		{name: "tool call", update: acp.StartToolCall("tool-1", "run"), want: "tool_call"},
		{name: "tool update", update: acp.UpdateToolCall("tool-1", acp.WithUpdateStatus(status)), want: "tool_call_update"},
		{name: "plan", update: acp.UpdatePlan(acp.PlanEntry{Content: "step"}), want: "plan"},
		{name: "mode", update: acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{}}, want: "current_mode_update"},
		{name: "commands", update: acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{}}, want: "available_commands_update"},
		{name: "config", update: acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{}}, want: "config_option_update"},
		{name: "session info", update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}}, want: "session_info_update"},
		{name: "usage", update: acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{}}, want: acpUsageUpdate},
		{name: "unknown", update: acp.SessionUpdate{}, want: unknownValue},
	}
	for _, tc := range updateTypeTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sessionUpdateType(tc.update); got != tc.want {
				t.Fatalf("sessionUpdateType() = %q, want %q", got, tc.want)
			}
		})
	}

	blockTypeTests := []struct {
		name  string
		block acp.ContentBlock
		want  string
	}{
		{name: "text", block: acp.TextBlock("hi"), want: acpTypeText},
		{name: "image", block: acp.ImageBlock("aW1hZ2U=", ""), want: acpTypeImage},
		{name: "audio", block: acp.AudioBlock("YXVkaW8=", ""), want: acpTypeAudio},
		{name: "resource link", block: acp.ResourceLinkBlock("readme", "file:///README.md"), want: "resource_link"},
		{name: "resource", block: acp.ResourceBlock(acp.EmbeddedResourceResource{}), want: acpTypeResource},
		{name: "unknown", block: acp.ContentBlock{}, want: unknownValue},
	}
	for _, tc := range blockTypeTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := contentBlockType(tc.block); got != tc.want {
				t.Fatalf("contentBlockType() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoggerContextHelpers(t *testing.T) {
	t.Parallel()

	var nilCtx context.Context
	ctx := contextWithLogger(nilCtx, newLogger(nil, "test"))
	if ctx == nil {
		t.Fatal("contextWithLogger(nil) returned nil")
	}
	got := loggerFromContext(ctx, newLogger(nil, "fallback"), "")
	if got.inner == nil {
		t.Fatal("loggerFromContext() returned nil inner logger")
	}

	fallback := newLogger(nil, "fallback")
	if got := loggerFromContext(context.Background(), fallback, "sub"); got.inner == nil {
		t.Fatal("loggerFromContext(fallback) returned nil inner logger")
	}
}

func TestAgentPureErrorHelpers(t *testing.T) {
	t.Parallel()

	errorCodeTests := []struct {
		name string
		code any
		want string
	}{
		{name: "nil", code: nil, want: ""},
		{name: "string", code: "rate_limit", want: "rate_limit"},
		{name: "single key map", code: map[string]any{"quota_exceeded": true}, want: "quota_exceeded"},
		{name: "multi key map", code: map[string]any{"a": true, "b": true}, want: ""},
		{name: "number", code: 429, want: "429"},
	}
	for _, tc := range errorCodeTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stringifyTerminalErrorCode(tc.code); got != tc.want {
				t.Fatalf("stringifyTerminalErrorCode() = %q, want %q", got, tc.want)
			}
		})
	}

	finishReasonTests := []struct {
		reason acp.StopReason
		want   genai.FinishReason
	}{
		{reason: acp.StopReasonEndTurn, want: genai.FinishReasonStop},
		{reason: acp.StopReasonMaxTokens, want: genai.FinishReasonMaxTokens},
		{reason: acp.StopReasonRefusal, want: genai.FinishReasonProhibitedContent},
		{reason: acp.StopReasonCancelled, want: genai.FinishReasonOther},
		{reason: acp.StopReasonMaxTurnRequests, want: genai.FinishReasonOther},
		{reason: acp.StopReason("unknown"), want: genai.FinishReasonUnspecified},
	}
	for _, tc := range finishReasonTests {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			if got := mapACPStopReasonToFinishReason(tc.reason); got != tc.want {
				t.Fatalf("mapACPStopReasonToFinishReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgentStateDeltaHelpers(t *testing.T) {
	t.Parallel()

	partial := session.NewEvent(context.Background(), "inv-partial")
	partial.Partial = true
	(&Agent{outputKey: "out"}).persistSessionStateDelta(partial, "session-1", `{"x":1}`)
	(&Agent{outputKey: "out"}).maybeSaveOutputToState(partial, "text")
	if len(partial.Actions.StateDelta) != 0 {
		t.Fatalf("partial StateDelta = %#v, want empty", partial.Actions.StateDelta)
	}

	ev := session.NewEvent(context.Background(), "inv")
	agent := &Agent{outputKey: "out"}
	agent.persistSessionStateDelta(ev, "session-1", `{"x":1}`)
	agent.maybeSaveOutputToState(ev, "visible")
	wantACPState := map[string]any{"session_id": "session-1", "meta": map[string]any{"x": float64(1)}}
	if !reflect.DeepEqual(ev.Actions.StateDelta[SessionStateKey], wantACPState) {
		t.Fatalf("acp StateDelta = %#v, want %#v", ev.Actions.StateDelta[SessionStateKey], wantACPState)
	}
	if got := ev.Actions.StateDelta["out"]; got != "visible" {
		t.Fatalf("output StateDelta = %#v, want visible", got)
	}
}

func TestAgentUpdateLoggingHelpers(t *testing.T) {
	t.Parallel()

	if payload, ok := marshalACPUpdatePayload(newLogger(nil, ""), "ok", map[string]any{"x": 1}); !ok || payload != `{"x":1}` {
		t.Fatalf("marshalACPUpdatePayload(success) = (%q, %v), want JSON true", payload, ok)
	}
	if payload, ok := marshalACPUpdatePayload(newLogger(nil, ""), "bad", func() {}); ok || payload != "" {
		t.Fatalf("marshalACPUpdatePayload(failure) = (%q, %v), want empty false", payload, ok)
	}

	known := ExtendedSessionNotification{SessionNotification: acp.SessionNotification{Update: acp.UpdateAgentMessageText("hi")}}
	if got := extendedSessionUpdateType(known); got != "agent_message_chunk" {
		t.Fatalf("extendedSessionUpdateType(known) = %q, want agent_message_chunk", got)
	}
	raw := ExtendedSessionNotification{Raw: []byte(`{"update":{"sessionUpdate":"custom_update"}}`)}
	if got := extendedSessionUpdateType(raw); got != "custom_update" {
		t.Fatalf("extendedSessionUpdateType(raw) = %q, want custom_update", got)
	}
	if got := extendedSessionUpdateType(ExtendedSessionNotification{Raw: []byte(`{`)}); got != unknownValue {
		t.Fatalf("extendedSessionUpdateType(invalid) = %q, want %q", got, unknownValue)
	}
}

func TestTerminalPromptErrorParsing(t *testing.T) {
	t.Parallel()

	if got, ok := terminalPromptErrorFromNotification(ExtendedSessionNotification{Method: "session/update"}); ok || got != nil {
		t.Fatalf("terminalPromptErrorFromNotification(non-error) = (%#v, %v), want nil false", got, ok)
	}
	if got, ok := parsePromptErrorNotification(json.RawMessage(`{`)); ok || got != nil {
		t.Fatalf("parsePromptErrorNotification(invalid) = (%#v, %v), want nil false", got, ok)
	}
	if got, ok := parsePromptErrorNotification(json.RawMessage(`{"willRetry":true,"error":{"message":"retrying"}}`)); ok || got != nil {
		t.Fatalf("parsePromptErrorNotification(retry) = (%#v, %v), want nil false", got, ok)
	}
	promptErr, ok := parsePromptErrorNotification(json.RawMessage(`{"error":{"additionalDetails":"quota details","codexErrorInfo":{"quota_exceeded":{}}}}`))
	if !ok {
		t.Fatal("parsePromptErrorNotification(additional details) ok = false, want true")
	}
	if promptErr.Message != "quota details" || promptErr.Code != "quota_exceeded" {
		t.Fatalf("prompt error = %#v, want quota details/quota_exceeded", promptErr)
	}

	if got, ok := parseTurnCompletedTerminalError(json.RawMessage(`{`)); ok || got != nil {
		t.Fatalf("parseTurnCompletedTerminalError(invalid) = (%#v, %v), want nil false", got, ok)
	}
	if got, ok := parseTurnCompletedTerminalError(json.RawMessage(`{"turn":{"status":"completed","error":{"message":"ignored"}}}`)); ok || got != nil {
		t.Fatalf("parseTurnCompletedTerminalError(completed) = (%#v, %v), want nil false", got, ok)
	}
	turnErr, ok := parseTurnCompletedTerminalError(json.RawMessage(`{"turn":{"status":"FAILED","error":{"message":"bad","codexErrorInfo":"rate_limit"}}}`))
	if !ok {
		t.Fatal("parseTurnCompletedTerminalError(failed) ok = false, want true")
	}
	if turnErr.Message != "bad" || turnErr.Code != "rate_limit" {
		t.Fatalf("turn error = %#v, want bad/rate_limit", turnErr)
	}
	if got, ok := newTerminalPromptError("", nil, ""); ok || got != nil {
		t.Fatalf("newTerminalPromptError(empty) = (%#v, %v), want nil false", got, ok)
	}
	defaultErr, ok := newTerminalPromptError("failed", nil, "")
	if !ok || defaultErr.Code != "provider_error" {
		t.Fatalf("newTerminalPromptError(default) = (%#v, %v), want provider_error true", defaultErr, ok)
	}
}

func TestAgentMetadataAndTextHelpers(t *testing.T) {
	t.Parallel()

	if got := normalizeACPStateMetaJSON(nil); got != "{}" {
		t.Fatalf("normalizeACPStateMetaJSON(nil) = %q, want {}", got)
	}
	if got := normalizeACPStateMetaJSON(map[string]any{"bad": func() {}}); got != "{}" {
		t.Fatalf("normalizeACPStateMetaJSON(unmarshalable) = %q, want {}", got)
	}
	if got := normalizeACPStateMetaJSONFromRaw(" "); got != "{}" {
		t.Fatalf("normalizeACPStateMetaJSONFromRaw(blank) = %q, want {}", got)
	}
	if got := normalizeACPStateMetaJSONFromRaw("{"); got != "{}" {
		t.Fatalf("normalizeACPStateMetaJSONFromRaw(invalid) = %q, want {}", got)
	}
	if !isNonEmptyMetaValue(1) || isNonEmptyMetaValue(nil) || isNonEmptyMetaValue(" \t ") || !isNonEmptyMetaValue("x") {
		t.Fatal("isNonEmptyMetaValue returned unexpected results")
	}
	if isIdentifier("1bad") || isIdentifier("bad-name") || !isIdentifier("_good1") {
		t.Fatal("isIdentifier returned unexpected results")
	}

	content := genai.NewContentFromParts([]*genai.Part{
		nil,
		genai.NewPartFromText("visible"),
		&genai.Part{Text: "thought", Thought: true},
	}, genai.RoleModel)
	if got := extractPromptText(content); got != "visiblethought" {
		t.Fatalf("extractPromptText() = %q, want visiblethought", got)
	}
	if got := contentVisibleText(content); got != "visible" {
		t.Fatalf("contentVisibleText() = %q, want visible", got)
	}
	if got := contentVisibleText(nil); got != "" {
		t.Fatalf("contentVisibleText(nil) = %q, want empty", got)
	}

	a := &Agent{sessionModel: "model", sessionMode: "mode"}
	a.logBoundRemoteSession(newLogger(nil, ""), "bound", "session-1", "/tmp", "{}")
	a.logADKEvent(newLogger(nil, ""), nil, "ignored")
}

func TestAgentConfigConversionHelpers(t *testing.T) {
	t.Parallel()

	env := envToEnvVars(map[string]string{"B": "2", "A": "1"})
	if len(env) != 2 || env[0].Name != "A" || env[0].Value != "1" || env[1].Name != "B" {
		t.Fatalf("envToEnvVars() = %#v, want sorted A/B", env)
	}
	headers := headersToHttpHeaders(map[string]string{"X-B": "2", "X-A": "1"})
	if len(headers) != 2 || headers[0].Name != "X-A" || headers[0].Value != "1" || headers[1].Name != "X-B" {
		t.Fatalf("headersToHttpHeaders() = %#v, want sorted X-A/X-B", headers)
	}
	if got := envToEnvVars(nil); len(got) != 0 {
		t.Fatalf("envToEnvVars(nil) length = %d, want 0", len(got))
	}
	if got := headersToHttpHeaders(nil); len(got) != 0 {
		t.Fatalf("headersToHttpHeaders(nil) length = %d, want 0", len(got))
	}

	if _, err := convertMCPServers(map[string]MCPServerConfig{"bad": {Type: MCPServerTypeStdio}}); err == nil {
		t.Fatal("convertMCPServers(empty stdio command) error = nil, want error")
	}
	if _, err := convertMCPServers(map[string]MCPServerConfig{"bad": {Type: "bad"}}); err == nil {
		t.Fatal("convertMCPServers(unsupported) error = nil, want error")
	}
}
