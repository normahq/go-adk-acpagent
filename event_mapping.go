package acpagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/normahq/go-adk-acpagent/v2/acperror"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const (
	acpTypeText       = "text"
	acpTypeImage      = "image"
	acpTypeAudio      = "audio"
	acpTypeResource   = "resource"
	acpUsageUpdate    = "usage_update"
	acpPlanEntriesKey = "entries"
)

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

func mapACPSessionUsageUpdateMetadata(update *acp.SessionUsageUpdate) map[string]any {
	if update == nil {
		return nil
	}
	meta := map[string]any{
		"size": update.Size,
		"used": update.Used,
	}
	found := update.Size > 0 || update.Used > 0
	if update.Cost != nil {
		meta["cost"] = map[string]any{
			"amount":   update.Cost.Amount,
			"currency": update.Cost.Currency,
		}
		found = true
	}
	if !found {
		return nil
	}
	return meta
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

func mapACPUpdateToEvent(ctx context.Context, logger logger, invocationID string, ext ExtendedSessionNotification) (*session.Event, bool) {
	update := ext.Update
	switch {
	case update.UserMessageChunk != nil:
		return mapACPUserMessageChunk(ctx, logger, invocationID, update.UserMessageChunk)
	case update.AgentMessageChunk != nil:
		return mapACPAgentMessageChunk(ctx, logger, invocationID, update.AgentMessageChunk)
	case update.AgentThoughtChunk != nil:
		return mapACPAgentThoughtChunk(ctx, logger, invocationID, update.AgentThoughtChunk)
	case update.ToolCall != nil:
		return mapACPToolCall(ctx, invocationID, update.ToolCall)
	case update.ToolCallUpdate != nil:
		return mapACPToolCallUpdate(ctx, invocationID, update.ToolCallUpdate)
	case update.Plan != nil:
		return mapACPPlanUpdate(ctx, invocationID, update.Plan)
	case update.AvailableCommandsUpdate != nil:
		logIgnoredACPUpdate(logger, "available_commands_update", map[string]any{
			"availableCommands": update.AvailableCommandsUpdate.AvailableCommands,
		})
		return nil, false
	case update.CurrentModeUpdate != nil:
		return mapACPCurrentModeUpdate(ctx, invocationID, string(ext.SessionId), update.CurrentModeUpdate)
	case update.ConfigOptionUpdate != nil:
		return mapACPConfigOptionUpdate(ctx, invocationID, string(ext.SessionId), update.ConfigOptionUpdate)
	case update.SessionInfoUpdate != nil:
		logIgnoredACPUpdate(logger, "session_info_update", map[string]any{
			"title":     update.SessionInfoUpdate.Title,
			"updatedAt": update.SessionInfoUpdate.UpdatedAt,
		})
		return nil, false
	case update.UsageUpdate != nil:
		return mapACPSessionUsageUpdate(ctx, logger, invocationID, update.UsageUpdate)
	default:
		// Check for recognized discriminators in raw JSON that are not in the SDK struct.
		var raw map[string]any
		if err := json.Unmarshal(ext.Raw, &raw); err == nil {
			if u, ok := raw["update"].(map[string]any); ok {
				if disc, ok := u["sessionUpdate"].(string); ok && disc == acpUsageUpdate {
					return mapACPLegacyUsageUpdate(ctx, logger, invocationID, u)
				}
			}
		}

		logUnsupportedACPUpdate(logger, ext)
		return nil, false
	}
}

func mapACPLegacyUsageUpdate(ctx context.Context, logger logger, invocationID string, update map[string]any) (*session.Event, bool) {
	usage := mapACPLegacyUsageToUsageMetadata(update)
	if usage == nil {
		logger.Debug().Interface("update", update).Msg("ignoring usage_update with no token counts")
		return nil, false
	}
	ev := session.NewEvent(ctx, invocationID)
	ev.UsageMetadata = usage
	ev.Partial = true
	return ev, true
}

func mapACPSessionUsageUpdate(ctx context.Context, logger logger, invocationID string, update *acp.SessionUsageUpdate) (*session.Event, bool) {
	metadata := mapACPSessionUsageUpdateMetadata(update)
	if metadata == nil {
		logIgnoredACPUpdate(logger, acpUsageUpdate, map[string]any{
			"size":   update.Size,
			"used":   update.Used,
			"cost":   update.Cost,
			"reason": "empty_session_usage",
		})
		return nil, false
	}
	ev := session.NewEvent(ctx, invocationID)
	ev.CustomMetadata = map[string]any{SessionUsageMetadataKey: metadata}
	ev.Partial = true
	return ev, true
}

func mapACPAgentMessageChunk(ctx context.Context, logger logger, invocationID string, chunk *acp.SessionUpdateAgentMessageChunk) (*session.Event, bool) {
	part, ok := mapACPContentBlockToPart(logger, chunk.Content)
	if !ok {
		return nil, false
	}
	ev := session.NewEvent(ctx, invocationID)
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

func mapACPUserMessageChunk(ctx context.Context, logger logger, invocationID string, chunk *acp.SessionUpdateUserMessageChunk) (*session.Event, bool) {
	part, ok := mapACPContentBlockToPart(logger, chunk.Content)
	if !ok {
		return nil, false
	}
	ev := session.NewEvent(ctx, invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleUser)
	ev.Partial = true
	return ev, true
}

func mapACPAgentThoughtChunk(ctx context.Context, logger logger, invocationID string, chunk *acp.SessionUpdateAgentThoughtChunk) (*session.Event, bool) {
	part, ok := mapACPContentBlockToPart(logger, chunk.Content)
	if !ok {
		return nil, false
	}
	part.Thought = true
	ev := session.NewEvent(ctx, invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleModel)
	ev.Partial = true
	return ev, true
}

func mapACPToolCall(ctx context.Context, invocationID string, tool *acp.SessionUpdateToolCall) (*session.Event, bool) {
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
	ev := session.NewEvent(ctx, invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleModel)
	if isACPToolStatusLongRunning(tool.Status) {
		ev.LongRunningToolIDs = []string{string(tool.ToolCallId)}
	}
	return ev, true
}

func mapACPToolCallUpdate(ctx context.Context, invocationID string, tool *acp.SessionToolCallUpdate) (*session.Event, bool) {
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
	ev := session.NewEvent(ctx, invocationID)
	ev.Content = genai.NewContentFromParts([]*genai.Part{part}, genai.RoleModel)
	if tool.Status != nil && isACPToolStatusLongRunning(*tool.Status) {
		ev.LongRunningToolIDs = []string{string(tool.ToolCallId)}
	}
	return ev, true
}

func mapACPPlanUpdate(ctx context.Context, invocationID string, plan *acp.SessionUpdatePlan) (*session.Event, bool) {
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
	ev := session.NewEvent(ctx, invocationID)
	ev.Actions.StateDelta[PlanStateKey] = map[string]any{
		acpPlanEntriesKey: entries,
	}
	ev.Partial = true
	return ev, true
}

func mapACPConfigOptionUpdate(ctx context.Context, invocationID, sessionID string, update *acp.SessionConfigOptionUpdate) (*session.Event, bool) {
	if update == nil || strings.TrimSpace(sessionID) == "" {
		return nil, false
	}
	values := collectSessionConfigValues(update.ConfigOptions, nil)
	if len(values) == 0 {
		return nil, false
	}
	ev := session.NewEvent(ctx, invocationID)
	ev.Actions.StateDelta[SessionStateKey] = buildACPStateWithConfigValues(sessionID, "", values)
	ev.Partial = true
	return ev, true
}

func mapACPCurrentModeUpdate(ctx context.Context, invocationID, sessionID string, update *acp.SessionCurrentModeUpdate) (*session.Event, bool) {
	if update == nil || strings.TrimSpace(sessionID) == "" {
		return nil, false
	}
	mode := strings.TrimSpace(string(update.CurrentModeId))
	if mode == "" {
		return nil, false
	}
	ev := session.NewEvent(ctx, invocationID)
	ev.Actions.StateDelta[SessionStateKey] = buildACPStateWithConfigValues(sessionID, "", []SessionConfigValue{{ID: "mode", Value: mode}})
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
	logger.Debug().
		Str("acp_update_type", updateType).
		Msg("ignoring unsupported acp session update")
	if !logger.enabled(levelTrace) {
		return
	}

	logEvent := logger.Trace().Str("acp_update_type", updateType)
	if updateType == unknownValue {
		logEvent = logEvent.RawJSON("acp_update_payload", ext.Raw)
	} else if payload, ok := marshalACPUpdatePayload(logger, "session_update_"+updateType, ext.Update); ok {
		logEvent = logEvent.Str("acp_update_payload", payload)
	}
	logEvent.Msg("ignored unsupported acp session update payload")
}

func logIgnoredACPUpdate(logger logger, updateType string, payload any) {
	logger.Debug().
		Str("acp_update_type", updateType).
		Msg("ignoring non-user-visible acp session update")
	if !logger.enabled(levelTrace) {
		return
	}

	logEvent := logger.Trace().Str("acp_update_type", updateType)
	if marshaled, ok := marshalACPUpdatePayload(logger, "session_update_"+updateType, payload); ok {
		logEvent = logEvent.Str("acp_update_payload", marshaled)
	}
	logEvent.Msg("ignored non-user-visible acp session update payload")
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
	logEvent := logger.Debug().Str("acp_content_block_type", blockType)
	if blockType == unknownValue {
		logEvent.Msg("ignoring unsupported acp content block")
	} else {
		logEvent.Msg("ignoring non-text acp content block")
	}
	if !logger.enabled(levelTrace) {
		return
	}
	logger.Trace().
		Str("acp_content_block_type", blockType).
		Str("acp_content_block_text", acpContentBlockLogText(block)).
		Interface("acp_content_block", acpContentBlockLogValue(block)).
		Msg("ignored acp content block payload")
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
