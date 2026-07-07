package acpagent

import (
	acp "github.com/coder/acp-go-sdk"
)

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
