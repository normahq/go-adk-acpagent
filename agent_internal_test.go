package acpagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/go-cmp/cmp"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestMapACPUpdateToEventUsesContextProviders(t *testing.T) {
	t.Parallel()

	wantTime := time.Date(2026, time.July, 10, 12, 30, 0, 0, time.UTC)
	ctx := platform.WithTimeProvider(t.Context(), func() time.Time { return wantTime })
	ctx = platform.WithUUIDProvider(ctx, func() string { return "event-id" })

	ev, ok := mapACPUpdateToEvent(ctx, newLogger(nil, ""), "inv-1", ExtendedSessionNotification{
		SessionNotification: acp.SessionNotification{Update: acp.UpdateAgentMessageText("hello")},
	})
	if !ok || ev == nil {
		t.Fatalf("mapACPUpdateToEvent() = (%#v, %v), want event true", ev, ok)
	}
	if ev.ID != "event-id" {
		t.Errorf("mapACPUpdateToEvent() ID = %q, want %q", ev.ID, "event-id")
	}
	if !ev.Timestamp.Equal(wantTime) {
		t.Errorf("mapACPUpdateToEvent() Timestamp = %v, want %v", ev.Timestamp, wantTime)
	}
}

func TestMapACPUserAndThoughtChunks(t *testing.T) {
	t.Parallel()

	if ev, ok := mapACPUpdateToEvent(t.Context(), newLogger(nil, ""), "inv-dispatch-user", ExtendedSessionNotification{
		SessionNotification: acp.SessionNotification{Update: acp.UpdateUserMessageText("user text")},
	}); !ok || ev == nil || ev.Content.Role != genai.RoleUser {
		t.Fatalf("mapACPUpdateToEvent(user) = (%#v, %v), want user event true", ev, ok)
	}
	if ev, ok := mapACPUpdateToEvent(t.Context(), newLogger(nil, ""), "inv-dispatch-thought", ExtendedSessionNotification{
		SessionNotification: acp.SessionNotification{Update: acp.UpdateAgentThoughtText("thinking")},
	}); !ok || ev == nil || ev.Content.Role != genai.RoleModel || !ev.Content.Parts[0].Thought {
		t.Fatalf("mapACPUpdateToEvent(thought) = (%#v, %v), want thought event true", ev, ok)
	}

	userEv, ok := mapACPUserMessageChunk(t.Context(), newLogger(nil, ""), "inv-user", &acp.SessionUpdateUserMessageChunk{
		Content: acp.TextBlock("user text"),
	})
	if !ok {
		t.Fatal("mapACPUserMessageChunk() ok = false, want true")
	}
	if userEv.Content.Role != genai.RoleUser || extractPromptText(userEv.Content) != "user text" || !userEv.Partial {
		t.Fatalf("user event = %#v", userEv)
	}

	thoughtEv, ok := mapACPAgentThoughtChunk(t.Context(), newLogger(nil, ""), "inv-thought", &acp.SessionUpdateAgentThoughtChunk{
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
	ev, ok := mapACPAgentMessageChunk(t.Context(), newLogger(nil, ""), "inv-message", &acp.SessionUpdateAgentMessageChunk{
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

	if ev, ok := mapACPAgentMessageChunk(t.Context(), newLogger(nil, ""), "inv-empty", &acp.SessionUpdateAgentMessageChunk{}); ok || ev != nil {
		t.Fatalf("mapACPAgentMessageChunk(empty) = (%#v, %v), want nil false", ev, ok)
	}

	directID := "msg-direct"
	directEv, ok := mapACPAgentMessageChunk(t.Context(), newLogger(nil, ""), "inv-direct", &acp.SessionUpdateAgentMessageChunk{
		Content:   acp.TextBlock("direct id"),
		MessageId: &directID,
	})
	if !ok {
		t.Fatal("mapACPAgentMessageChunk(direct id) ok = false, want true")
	}
	if got := directEv.CustomMetadata["acp_message_id"]; got != directID {
		t.Fatalf("direct acp_message_id = %#v, want %q", got, directID)
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
			logger := testLogger(&testLogBuffer{}, levelTrace)
			if part, ok := mapACPContentBlockToPart(logger, tc.block); ok {
				t.Fatalf("mapACPContentBlockToPart() = (%#v, true), want false", part)
			}
		})
	}
}

func TestMapACPMappingEdgeCases(t *testing.T) {
	t.Parallel()

	if ev, ok := mapACPUserMessageChunk(t.Context(), newLogger(nil, ""), "inv-user", &acp.SessionUpdateUserMessageChunk{}); ok || ev != nil {
		t.Fatalf("mapACPUserMessageChunk(empty) = (%#v, %v), want nil false", ev, ok)
	}
	if ev, ok := mapACPAgentThoughtChunk(t.Context(), newLogger(nil, ""), "inv-thought", &acp.SessionUpdateAgentThoughtChunk{}); ok || ev != nil {
		t.Fatalf("mapACPAgentThoughtChunk(empty) = (%#v, %v), want nil false", ev, ok)
	}

	pending := acp.ToolCallStatusPending
	ev, ok := mapACPToolCallUpdate(t.Context(), "inv-tool", &acp.SessionToolCallUpdate{ToolCallId: "tool-1", Status: &pending})
	if !ok {
		t.Fatal("mapACPToolCallUpdate(pending) ok = false, want true")
	}
	if diff := cmp.Diff([]string{"tool-1"}, ev.LongRunningToolIDs); diff != "" {
		t.Errorf("LongRunningToolIDs mismatch (-want +got):\n%s", diff)
	}
	completed := acp.ToolCallStatusCompleted
	ev, ok = mapACPToolCallUpdate(t.Context(), "inv-tool", &acp.SessionToolCallUpdate{ToolCallId: "tool-1", Status: &completed})
	if !ok {
		t.Fatal("mapACPToolCallUpdate(completed) ok = false, want true")
	}
	if len(ev.LongRunningToolIDs) != 0 {
		t.Fatalf("completed LongRunningToolIDs = %#v, want empty", ev.LongRunningToolIDs)
	}

	if got := mapACPImageToPart(nil); got != nil {
		t.Fatalf("mapACPImageToPart(nil) = %#v, want nil", got)
	}
	if got := mapACPAudioToPart(nil); got != nil {
		t.Fatalf("mapACPAudioToPart(nil) = %#v, want nil", got)
	}
	if got := mapACPResourceLinkToPart(nil); got != nil {
		t.Fatalf("mapACPResourceLinkToPart(nil) = %#v, want nil", got)
	}
	logger := testLogger(&testLogBuffer{}, levelTrace)
	logUnsupportedACPUpdate(logger, ExtendedSessionNotification{Raw: []byte(`{"update":{"sessionUpdate":"custom"}}`)})
	logUnsupportedACPUpdate(logger, ExtendedSessionNotification{Raw: []byte(`{"not":"update"}`)})
}

func TestMapACPUpdateToEventIgnoredAndLegacyUsage(t *testing.T) {
	t.Parallel()

	ignoredUpdates := []acp.SessionUpdate{
		{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{}},
		{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{}},
		{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{}},
		{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
	}
	for i, update := range ignoredUpdates {
		t.Run(fmt.Sprintf("ignored_%d", i), func(t *testing.T) {
			t.Parallel()
			logger := testLogger(&testLogBuffer{}, levelTrace)
			if ev, ok := mapACPUpdateToEvent(t.Context(), logger, "inv", ExtendedSessionNotification{SessionNotification: acp.SessionNotification{Update: update}}); ok {
				t.Fatalf("mapACPUpdateToEvent() = (%#v, true), want false", ev)
			}
		})
	}

	cost := &acp.Cost{Amount: 1.25, Currency: "USD"}
	ev, ok := mapACPUpdateToEvent(t.Context(), newLogger(nil, ""), "inv-structured-usage", ExtendedSessionNotification{
		SessionNotification: acp.SessionNotification{Update: acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{
			Size: 100,
			Used: 25,
			Cost: cost,
		}}},
	})
	if !ok {
		t.Fatal("structured usage update ok = false, want true")
	}
	if ev.UsageMetadata == nil {
		t.Fatal("structured usage UsageMetadata = nil")
	}
	if ev.UsageMetadata.PromptTokenCount != 100 || ev.UsageMetadata.TotalTokenCount != 25 {
		t.Fatalf("structured UsageMetadata = %#v", ev.UsageMetadata)
	}
	if !ev.Partial {
		t.Fatal("structured usage event Partial = false, want true")
	}

	ev, ok = mapACPUpdateToEvent(t.Context(), newLogger(nil, ""), "inv-usage", ExtendedSessionNotification{
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

	if ev, ok := mapACPLegacyUsageUpdate(t.Context(), newLogger(nil, ""), "inv-empty", map[string]any{"sessionUpdate": acpUsageUpdate}); ok {
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
	if diff := cmp.Diff(wantLink, acpContentBlockLogValue(link)); diff != "" {
		t.Errorf("acpContentBlockLogValue(link) mismatch (-want +got):\n%s", diff)
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
	if diff := cmp.Diff(wantTextResource, acpContentBlockLogValue(textResource)); diff != "" {
		t.Errorf("acpContentBlockLogValue(text resource) mismatch (-want +got):\n%s", diff)
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
	if diff := cmp.Diff(wantBlobResource, acpContentBlockLogValue(blobResource)); diff != "" {
		t.Errorf("acpContentBlockLogValue(blob resource) mismatch (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(map[string]any{"kind": unknownValue}, acpEmbeddedResourceLogValue(acp.EmbeddedResourceResource{})); diff != "" {
		t.Errorf("acpEmbeddedResourceLogValue(empty) mismatch (-want +got):\n%s", diff)
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
	if got := loggerFromContext(nilCtx, newLogger(nil, "fallback"), "sub"); got.inner == nil {
		t.Fatal("loggerFromContext(nil) returned nil inner logger")
	}
	if got := loggerFromContext(context.WithValue(t.Context(), loggerContextKey{}, logger{}), newLogger(nil, "fallback"), "sub"); got.inner == nil {
		t.Fatal("loggerFromContext(nil stored logger) returned nil inner logger")
	}

	fallback := newLogger(nil, "fallback")
	if got := loggerFromContext(t.Context(), fallback, "sub"); got.inner == nil {
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

	wantSessionOnly := map[string]any{"session_id": "session-1"}
	if diff := cmp.Diff(wantSessionOnly, buildACPState("session-1", "")); diff != "" {
		t.Errorf("buildACPState(empty meta) mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantSessionOnly, buildACPState("session-1", "{")); diff != "" {
		t.Errorf("buildACPState(invalid meta) mismatch (-want +got):\n%s", diff)
	}
	if currentACPStateMatches(nil, "session-1", "{}") {
		t.Fatal("currentACPStateMatches(nil) = true, want false")
	}

	sessionService := session.InMemoryService()
	created, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			SessionStateKey: map[string]any{
				"session_id": "session-1",
				"meta":       map[string]any{"b": float64(2), "a": float64(1)},
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !currentACPStateMatches(created.Session, " session-1 ", `{"a":1,"b":2}`) {
		t.Fatal("currentACPStateMatches(equivalent meta) = false, want true")
	}
	if currentACPStateMatches(created.Session, "session-2", `{"a":1,"b":2}`) {
		t.Fatal("currentACPStateMatches(different session) = true, want false")
	}
	if currentACPStateMatches(created.Session, "session-1", `{"a":1}`) {
		t.Fatal("currentACPStateMatches(different meta) = true, want false")
	}
	agentForState := &Agent{}
	modelConfig := []SessionConfigValue{{ID: "model", Value: "openai/gpt-5.4"}}
	if err := agentForState.persistRemoteSessionBinding(nil, "session-1", "{}", modelConfig); err != nil {
		t.Fatalf("persistRemoteSessionBinding(nil ctx) error = %v, want nil", err)
	}
	if err := agentForState.persistRemoteSessionBinding(testInvocationContext{session: created.Session}, " ", "{}", modelConfig); err != nil {
		t.Fatalf("persistRemoteSessionBinding(empty session) error = %v, want nil", err)
	}
	if err := agentForState.persistRemoteSessionBinding(testInvocationContext{session: created.Session}, "session-1", `{"a":1,"b":2}`, modelConfig); err != nil {
		t.Fatalf("persistRemoteSessionBinding(matching state) error = %v", err)
	}
	wantLiveACPState := map[string]any{
		"session_id": "session-1",
		"config_values": []map[string]any{
			{"id": "model", "value": "openai/gpt-5.4"},
		},
		"meta": map[string]any{"a": float64(1), "b": float64(2)},
	}
	liveACPState, err := created.Session.State().Get(SessionStateKey)
	if err != nil {
		t.Fatalf("State().Get(%q) error = %v", SessionStateKey, err)
	}
	if diff := cmp.Diff(wantLiveACPState, liveACPState); diff != "" {
		t.Errorf("live acp state mismatch (-want +got):\n%s", diff)
	}
	setErr := errors.New("set failed")
	matchingState := map[string]any{"session_id": "session-1"}
	if err := agentForState.persistRemoteSessionBinding(testInvocationContext{session: testStateSession{
		state: testState{values: map[string]any{SessionStateKey: matchingState}, setErr: setErr},
	}}, "session-1", "{}", modelConfig); err == nil || !strings.Contains(err.Error(), "set live acp session state") {
		t.Fatalf("persistRemoteSessionBinding(matching set error) error = %v, want wrapped set error", err)
	}
	if err := agentForState.persistRemoteSessionBinding(testInvocationContext{session: testStateSession{
		state: testState{getErr: session.ErrStateKeyNotExist, setErr: setErr},
	}}, "session-1", "{}", modelConfig); err == nil || !strings.Contains(err.Error(), "set live acp session state") {
		t.Fatalf("persistRemoteSessionBinding(new set error) error = %v, want wrapped set error", err)
	}

	(&Agent{outputKey: "out"}).persistSessionStateDelta(nil, "session-1", "{}", nil)
	(&Agent{outputKey: "out"}).maybeSaveOutputToState(nil, "text")

	emptyRemote := session.NewEvent(t.Context(), "inv-empty-remote")
	(&Agent{outputKey: "out"}).persistSessionStateDelta(emptyRemote, " ", "{}", nil)
	if len(emptyRemote.Actions.StateDelta) != 0 {
		t.Fatalf("empty remote StateDelta = %#v, want empty", emptyRemote.Actions.StateDelta)
	}

	withoutOutputKey := session.NewEvent(t.Context(), "inv-without-output")
	(&Agent{}).maybeSaveOutputToState(withoutOutputKey, "text")
	if len(withoutOutputKey.Actions.StateDelta) != 0 {
		t.Fatalf("without output key StateDelta = %#v, want empty", withoutOutputKey.Actions.StateDelta)
	}

	partial := session.NewEvent(t.Context(), "inv-partial")
	partial.Partial = true
	(&Agent{outputKey: "out"}).persistSessionStateDelta(partial, "session-1", `{"x":1}`, nil)
	(&Agent{outputKey: "out"}).maybeSaveOutputToState(partial, "text")
	if len(partial.Actions.StateDelta) != 0 {
		t.Fatalf("partial StateDelta = %#v, want empty", partial.Actions.StateDelta)
	}

	ev := session.NewEvent(t.Context(), "inv")
	agent := &Agent{outputKey: "out"}
	agent.persistSessionStateDelta(ev, "session-1", `{"x":1}`, nil)
	agent.maybeSaveOutputToState(ev, "visible")
	wantACPState := map[string]any{"session_id": "session-1", "meta": map[string]any{"x": float64(1)}}
	if diff := cmp.Diff(wantACPState, ev.Actions.StateDelta[SessionStateKey]); diff != "" {
		t.Errorf("acp StateDelta mismatch (-want +got):\n%s", diff)
	}
	if got := ev.Actions.StateDelta["out"]; got != "visible" {
		t.Fatalf("output StateDelta = %#v, want visible", got)
	}

	evWithModel := session.NewEvent(t.Context(), "inv-model")
	agent.persistSessionStateDelta(evWithModel, "session-1", "{}", modelConfig)
	wantACPStateWithModel := map[string]any{
		"session_id": "session-1",
		"config_values": []map[string]any{
			{"id": "model", "value": "openai/gpt-5.4"},
		},
	}
	if diff := cmp.Diff(wantACPStateWithModel, evWithModel.Actions.StateDelta[SessionStateKey]); diff != "" {
		t.Errorf("model acp StateDelta mismatch (-want +got):\n%s", diff)
	}

	zeroEvent := &session.Event{}
	agent.persistSessionStateDelta(zeroEvent, "session-2", "{}", nil)
	agent.maybeSaveOutputToState(zeroEvent, "saved")
	if diff := cmp.Diff(map[string]any{"session_id": "session-2"}, zeroEvent.Actions.StateDelta[SessionStateKey]); diff != "" {
		t.Errorf("zero event acp StateDelta mismatch (-want +got):\n%s", diff)
	}
	if got := zeroEvent.Actions.StateDelta["out"]; got != "saved" {
		t.Fatalf("zero event output StateDelta = %#v, want saved", got)
	}
}

func TestSessionConfigUpdateMappers(t *testing.T) {
	t.Parallel()

	if ev, ok := mapACPConfigOptionUpdate(t.Context(), "inv-1", "session-1", &acp.SessionConfigOptionUpdate{
		ConfigOptions: []acp.SessionConfigOption{{
			Select: &acp.SessionConfigOptionSelect{
				Id:           "model",
				CurrentValue: "gpt-5-codex",
			},
		}},
	}); !ok {
		t.Fatal("mapACPConfigOptionUpdate() ok = false, want true")
	} else {
		want := map[string]any{
			"session_id": "session-1",
			"config_values": []map[string]any{
				{"id": "model", "value": "gpt-5-codex"},
			},
		}
		if !ev.Partial {
			t.Error("mapACPConfigOptionUpdate() Partial = false, want true")
		}
		if diff := cmp.Diff(want, ev.Actions.StateDelta[SessionStateKey]); diff != "" {
			t.Errorf("mapACPConfigOptionUpdate() state mismatch (-want +got):\n%s", diff)
		}
	}

	if ev, ok := mapACPConfigOptionUpdate(t.Context(), "inv-bool", "session-1", &acp.SessionConfigOptionUpdate{
		ConfigOptions: []acp.SessionConfigOption{{
			Boolean: &acp.SessionConfigOptionBoolean{
				Id:           "fast_mode",
				CurrentValue: true,
			},
		}},
	}); !ok {
		t.Fatal("mapACPConfigOptionUpdate(boolean) ok = false, want true")
	} else {
		want := map[string]any{
			"session_id": "session-1",
			"config_values": []map[string]any{
				{"id": "fast_mode", "type": "boolean", "value": true},
			},
		}
		if !ev.Partial {
			t.Error("mapACPConfigOptionUpdate(boolean) Partial = false, want true")
		}
		if diff := cmp.Diff(want, ev.Actions.StateDelta[SessionStateKey]); diff != "" {
			t.Errorf("mapACPConfigOptionUpdate(boolean) state mismatch (-want +got):\n%s", diff)
		}
	}

	if ev, ok := mapACPCurrentModeUpdate(t.Context(), "inv-2", "session-1", &acp.SessionCurrentModeUpdate{CurrentModeId: "coding"}); !ok {
		t.Fatal("mapACPCurrentModeUpdate() ok = false, want true")
	} else {
		want := map[string]any{
			"session_id": "session-1",
			"config_values": []map[string]any{
				{"id": "mode", "value": "coding"},
			},
		}
		if !ev.Partial {
			t.Error("mapACPCurrentModeUpdate() Partial = false, want true")
		}
		if diff := cmp.Diff(want, ev.Actions.StateDelta[SessionStateKey]); diff != "" {
			t.Errorf("mapACPCurrentModeUpdate() state mismatch (-want +got):\n%s", diff)
		}
	}

	if ev, ok := mapACPConfigOptionUpdate(t.Context(), "inv-3", "", &acp.SessionConfigOptionUpdate{}); ok || ev != nil {
		t.Fatalf("mapACPConfigOptionUpdate(empty session) = (%#v, %v), want nil false", ev, ok)
	}
	if ev, ok := mapACPConfigOptionUpdate(t.Context(), "inv-4", "session-1", &acp.SessionConfigOptionUpdate{}); ok || ev != nil {
		t.Fatalf("mapACPConfigOptionUpdate(empty values) = (%#v, %v), want nil false", ev, ok)
	}
	if ev, ok := mapACPCurrentModeUpdate(t.Context(), "inv-5", "session-1", &acp.SessionCurrentModeUpdate{}); ok || ev != nil {
		t.Fatalf("mapACPCurrentModeUpdate(empty mode) = (%#v, %v), want nil false", ev, ok)
	}
	if ev, ok := mapACPCurrentModeUpdate(t.Context(), "inv-6", "session-1", nil); ok || ev != nil {
		t.Fatalf("mapACPCurrentModeUpdate(nil) = (%#v, %v), want nil false", ev, ok)
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
	metaErr, ok := terminalPromptErrorFromPromptMeta(map[string]any{
		"error": map[string]any{
			"message": "quota exceeded",
			"code":    "quota_exceeded",
		},
	})
	if !ok {
		t.Fatal("terminalPromptErrorFromPromptMeta(message/code) ok = false, want true")
	}
	if metaErr.Message != "quota exceeded" || metaErr.Code != "quota_exceeded" {
		t.Fatalf("terminalPromptErrorFromPromptMeta(message/code) = %#v, want quota exceeded/quota_exceeded", metaErr)
	}
	fallbackErr, ok := terminalPromptErrorFromPromptMeta(map[string]any{
		"error": map[string]any{
			"additionalDetails": "billing required",
			"kind":              "payment_required",
		},
	})
	if !ok {
		t.Fatal("terminalPromptErrorFromPromptMeta(additionalDetails/kind) ok = false, want true")
	}
	if fallbackErr.Message != "billing required" || fallbackErr.Code != "payment_required" {
		t.Fatalf("terminalPromptErrorFromPromptMeta(additionalDetails/kind) = %#v, want billing required/payment_required", fallbackErr)
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

	if got := stringifyTerminalErrorCode(nil); got != "" {
		t.Fatalf("stringifyTerminalErrorCode(nil) = %q, want empty", got)
	}
	if got := stringifyTerminalErrorCode(map[string]any{"one": true, "two": true}); got != "" {
		t.Fatalf("stringifyTerminalErrorCode(multi map) = %q, want empty", got)
	}
	if got := stringifyTerminalErrorCode(map[string]any{"quota_exceeded": true}); got != "quota_exceeded" {
		t.Fatalf("stringifyTerminalErrorCode(single map) = %q, want quota_exceeded", got)
	}
	if got := stringifyTerminalErrorCode(42); got != "42" {
		t.Fatalf("stringifyTerminalErrorCode(int) = %q, want 42", got)
	}

	if got := mapACPLegacyUsageToUsageMetadata(nil); got != nil {
		t.Fatalf("mapACPLegacyUsageToUsageMetadata(nil) = %#v, want nil", got)
	}
	if got := mapACPLegacyUsageToUsageMetadata(map[string]any{"inputTokens": "bad"}); got != nil {
		t.Fatalf("mapACPLegacyUsageToUsageMetadata(non-numeric) = %#v, want nil", got)
	}
	cachedOnly := mapACPLegacyUsageToUsageMetadata(map[string]any{"cachedReadTokens": float64(7)})
	if cachedOnly == nil || cachedOnly.CachedContentTokenCount != 7 {
		t.Fatalf("cached legacy usage = %#v, want cached count 7", cachedOnly)
	}

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
	if isIdentifier("") {
		t.Fatal("isIdentifier(empty) = true, want false")
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

	sessionService := session.InMemoryService()
	templateSession, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State:   map[string]any{"nullable": nil},
	})
	if err != nil {
		t.Fatalf("Create(template session) error = %v", err)
	}
	templateCtx := testInvocationContext{session: templateSession.Session}
	if got, err := replaceTemplateMatch(templateCtx, "{{ nullable }}"); err != nil || got != "" {
		t.Fatalf("replaceTemplateMatch(nil state) = (%q, %v), want empty nil", got, err)
	}
	if got, err := replaceTemplateMatch(templateCtx, "{{ artifact.missing }}"); err == nil || got != "" ||
		!strings.Contains(err.Error(), "artifact service is not initialized") {
		t.Fatalf("replaceTemplateMatch(missing artifact service) = (%q, %v), want artifact service error", got, err)
	}

	a := &Agent{sessionConfig: []SessionConfigValue{{ID: "model", Value: "model"}, {ID: "mode", Value: "mode"}}}
	a.logBoundRemoteSession(newLogger(nil, ""), "bound", "session-1", "/tmp", "{}")
	a.logADKEvent(newLogger(nil, ""), nil, "ignored")

	copyACPProviderErrorMetadata(nil, map[string]any{"provider_error": map[string]any{"message": "ignored"}})
	emptyURI := ""
	imageLog := logACPImageBlockValue(&acp.ContentBlockImage{MimeType: "image/png", Uri: &emptyURI})
	if _, ok := imageLog["uri"]; ok {
		t.Fatalf("logACPImageBlockValue(empty uri) = %#v, want no uri", imageLog)
	}
	uri := "file:///tmp/image.png"
	imageLog = logACPImageBlockValue(&acp.ContentBlockImage{Data: "abc", Uri: &uri})
	if imageLog["uri"] != uri || imageLog["data_len"] != 3 {
		t.Fatalf("logACPImageBlockValue(uri data) = %#v, want uri and data_len", imageLog)
	}
}

func TestAgentSessionConfigErrorBranches(t *testing.T) {
	t.Parallel()

	if _, err := normalizeACPConfigCWD(acpSessionConfig{}); err == nil || !strings.Contains(err.Error(), "cwd is empty") {
		t.Fatalf("normalizeACPConfigCWD(empty cwd) error = %v, want cwd empty", err)
	}
	if _, err := normalizeACPConfigCWD(acpSessionConfig{cwd: t.TempDir(), meta: map[string]any{"bad": func() {}}}); err == nil || !strings.Contains(err.Error(), "marshal acp session meta") {
		t.Fatalf("normalizeACPConfigCWD(bad meta) error = %v, want marshal error", err)
	}
	if _, err := normalizeACPConfigCWD(acpSessionConfig{cwd: t.TempDir() + "/missing"}); err == nil || !strings.Contains(err.Error(), "stat acp session cwd") {
		t.Fatalf("normalizeACPConfigCWD(missing cwd) error = %v, want stat error", err)
	}
	file, err := os.CreateTemp(t.TempDir(), "cwd-file-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("file.Close() error = %v", err)
	}
	if _, err := normalizeACPConfigCWD(acpSessionConfig{cwd: file.Name()}); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("normalizeACPConfigCWD(file cwd) error = %v, want not directory", err)
	}

	if _, err := addInstructionMetaToSessionConfig(acpSessionConfig{
		meta: map[string]any{"codex": 1},
	}, resolvedInstructionParts{instruction: "dev"}); err == nil || !strings.Contains(err.Error(), "codex must be an object") {
		t.Fatalf("addInstructionMetaToSessionConfig(bad codex) error = %v, want codex object error", err)
	}
	if _, err := addInstructionMetaToSessionConfig(acpSessionConfig{
		meta: map[string]any{"bad": func() {}},
	}, resolvedInstructionParts{instruction: "dev"}); err == nil || !strings.Contains(err.Error(), "marshal acp session meta") {
		t.Fatalf("addInstructionMetaToSessionConfig(bad meta) error = %v, want marshal error", err)
	}
	if _, err := addReasoningEffortToSessionConfig(acpSessionConfig{
		meta: map[string]any{"codex": 1},
	}, "high"); err == nil || !strings.Contains(err.Error(), "codex must be an object") {
		t.Fatalf("addReasoningEffortToSessionConfig(bad codex) error = %v, want codex object error", err)
	}
	if _, err := addReasoningEffortToSessionConfig(acpSessionConfig{
		meta: map[string]any{"codex": map[string]any{"config": 1}},
	}, "high"); err == nil || !strings.Contains(err.Error(), "codex.config must be an object") {
		t.Fatalf("addReasoningEffortToSessionConfig(bad config) error = %v, want config object error", err)
	}
	if _, err := addReasoningEffortToSessionConfig(acpSessionConfig{
		meta: map[string]any{"bad": func() {}},
	}, "high"); err == nil || !strings.Contains(err.Error(), "marshal acp session meta") {
		t.Fatalf("addReasoningEffortToSessionConfig(bad meta) error = %v, want marshal error", err)
	}

	sessionService := session.InMemoryService()
	created, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			SessionStateKey: map[string]any{
				"session_id": "session-1",
				"config_values": []any{
					map[string]any{"id": "model", "value": "state-model"},
				},
				"meta": map[string]any{"x": float64(1)},
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	configuredValues, err := (&Agent{
		workingDir:    t.TempDir(),
		sessionConfig: []SessionConfigValue{{ID: "model", Value: "configured-model"}, {ID: "mode", Value: "code"}},
	}).resolveSessionConfig(testInvocationContext{session: created.Session})
	if err != nil {
		t.Fatalf("resolveSessionConfig(configured values) error = %v", err)
	}
	wantConfiguredValues := []SessionConfigValue{{ID: "model", Value: "state-model"}, {ID: "mode", Value: "code"}}
	if diff := cmp.Diff(wantConfiguredValues, configuredValues.configValues); diff != "" {
		t.Errorf("resolveSessionConfig(configured values) configValues mismatch (-want +got):\n%s", diff)
	}

	stateValues, err := (&Agent{workingDir: t.TempDir()}).resolveSessionConfig(testInvocationContext{session: created.Session})
	if err != nil {
		t.Fatalf("resolveSessionConfig(state values) error = %v", err)
	}
	wantStateValues := []SessionConfigValue{{ID: "model", Value: "state-model"}}
	if diff := cmp.Diff(wantStateValues, stateValues.configValues); diff != "" {
		t.Errorf("resolveSessionConfig(state values) configValues mismatch (-want +got):\n%s", diff)
	}

	badState, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
		State: map[string]any{
			SessionStateKey: map[string]any{"config_values": []any{map[string]any{"id": 123, "value": "model"}}},
		},
	})
	if err != nil {
		t.Fatalf("Create(bad state) error = %v", err)
	}
	if _, err := (&Agent{workingDir: t.TempDir()}).resolveSessionConfig(testInvocationContext{session: badState.Session}); err == nil ||
		!strings.Contains(err.Error(), `"id" must be a string`) {
		t.Fatalf("resolveSessionConfig(bad config_values) error = %v, want config value id string error", err)
	}
	if _, err := (&Agent{workingDir: t.TempDir()}).resolveSessionConfig(testInvocationContext{session: testStateSession{
		state: testState{getErr: errors.New("read failed")},
	}}); err == nil || !strings.Contains(err.Error(), "read \"cwd\"") {
		t.Fatalf("resolveSessionConfig(read cwd error) error = %v, want read cwd error", err)
	}

	resolveConfigErrorCases := []struct {
		name    string
		state   map[string]any
		wantErr string
	}{
		{
			name:    "bad cwd",
			state:   map[string]any{CWDStateKey: 123},
			wantErr: "must be a string",
		},
		{
			name:    "bad acp state",
			state:   map[string]any{SessionStateKey: "bad"},
			wantErr: "must be an object",
		},
		{
			name:    "bad meta",
			state:   map[string]any{SessionStateKey: map[string]any{"meta": 1}},
			wantErr: "meta must be an object",
		},
		{
			name:    "bad session id",
			state:   map[string]any{SessionStateKey: map[string]any{"session_id": 1}},
			wantErr: "session_id must be a string",
		},
		{
			name: "bad reasoning meta",
			state: map[string]any{
				SessionStateKey: map[string]any{"meta": map[string]any{"codex": 1}},
			},
			wantErr: "codex must be an object",
		},
	}
	for _, tc := range resolveConfigErrorCases {
		t.Run(tc.name, func(t *testing.T) {
			sessionWithState, err := sessionService.Create(t.Context(), &session.CreateRequest{
				AppName: "test-app",
				UserID:  "test-user",
				State:   tc.state,
			})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			_, err = (&Agent{workingDir: t.TempDir(), reasoningEffort: "high"}).resolveSessionConfig(testInvocationContext{session: sessionWithState.Session})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("resolveSessionConfig() error = %v, want %q", err, tc.wantErr)
			}
		})
	}

	noACPState, err := sessionService.Create(t.Context(), &session.CreateRequest{
		AppName: "test-app",
		UserID:  "test-user",
	})
	if err != nil {
		t.Fatalf("Create(no acp state) error = %v", err)
	}
	reasoningConfig, err := (&Agent{workingDir: t.TempDir(), reasoningEffort: "high"}).resolveSessionConfig(testInvocationContext{session: noACPState.Session})
	if err != nil {
		t.Fatalf("resolveSessionConfig(no acp state) error = %v", err)
	}
	if reasoningConfig.metaJSON == "{}" || !strings.Contains(reasoningConfig.metaJSON, "model_reasoning_effort") {
		t.Fatalf("resolveSessionConfig(no acp state) metaJSON = %q, want reasoning effort", reasoningConfig.metaJSON)
	}
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

	stdio, err := convertMCPServers(map[string]MCPServerConfig{
		"stdio": {
			Type: MCPServerTypeStdio,
			Cmd:  []string{"server"},
			Args: []string{"--flag"},
		},
	})
	if err != nil {
		t.Fatalf("convertMCPServers(stdio args) error = %v", err)
	}
	if len(stdio) != 1 || stdio[0].Stdio == nil {
		t.Fatalf("convertMCPServers(stdio args) = %#v, want args fallback", stdio)
	}
	if diff := cmp.Diff([]string{"--flag"}, stdio[0].Stdio.Args); diff != "" {
		t.Errorf("convertMCPServers(stdio args) mismatch (-want +got):\n%s", diff)
	}

	if _, err := convertMCPServers(map[string]MCPServerConfig{"bad": {Type: MCPServerTypeStdio}}); err == nil {
		t.Fatal("convertMCPServers(empty stdio command) error = nil, want error")
	}
	if _, err := convertMCPServers(map[string]MCPServerConfig{"bad": {Type: "bad"}}); err == nil {
		t.Fatal("convertMCPServers(unsupported) error = nil, want error")
	}
}

type testInvocationContext struct {
	context.Context
	session session.Session
}

func (c testInvocationContext) Agent() adkagent.Agent {
	return nil
}

func (c testInvocationContext) Artifacts() adkagent.Artifacts {
	return nil
}

func (c testInvocationContext) Memory() adkagent.Memory {
	return nil
}

func (c testInvocationContext) Session() session.Session {
	return c.session
}

func (c testInvocationContext) InvocationID() string {
	return "test-invocation"
}

func (c testInvocationContext) Branch() string {
	return ""
}

func (c testInvocationContext) UserContent() *genai.Content {
	return nil
}

func (c testInvocationContext) RunConfig() *adkagent.RunConfig {
	return nil
}

func (c testInvocationContext) IsolationScope() string {
	return ""
}

func (c testInvocationContext) EndInvocation() {}

func (c testInvocationContext) Ended() bool {
	return false
}

func (c testInvocationContext) ResumedInput(string) (any, bool) {
	return nil, false
}

func (c testInvocationContext) WithContext(ctx context.Context) adkagent.InvocationContext {
	c.Context = ctx
	return c
}

func (c testInvocationContext) WithICDelta(*adkagent.InvocationContextDelta) adkagent.InvocationContext {
	return c
}

type testStateSession struct {
	session.Session
	state session.State
}

func (s testStateSession) State() session.State {
	return s.state
}

type testState struct {
	values map[string]any
	getErr error
	setErr error
}

func (s testState) Get(key string) (any, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	value, ok := s.values[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return value, nil
}

func (s testState) Set(string, any) error {
	return s.setErr
}

func (s testState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for key, value := range s.values {
			if !yield(key, value) {
				return
			}
		}
	}
}
