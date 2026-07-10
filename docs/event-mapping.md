# Event Mapping

`go-adk-acpagent` converts ACP `session/update` callbacks and final prompt
results into ADK `session.Event` values. Streamed ACP updates become partial
ADK events. The final prompt result becomes one `TurnComplete` event.

## Message Content

| ACP update | ADK event |
| --- | --- |
| `user_message_chunk` | `Content.Role` is `user`; supported content blocks become one ADK part; `Partial` is true. |
| `agent_message_chunk` | `Content.Role` is `model`; supported content blocks become one ADK part; `Partial` is true. |
| `agent_thought_chunk` | `Content.Role` is `model`; the mapped part has `Thought` set; `Partial` is true. |

Agent message chunks preserve ACP message IDs in
`event.CustomMetadata["acp_message_id"]` when the provider sends one.

Provider errors carried in ACP message metadata are copied into:

- `event.ErrorCode`, prefixed with `acp_provider_error:`
- `event.ErrorMessage`
- `event.CustomMetadata[acperror.ProviderErrorMetadataKey]`

Use `acperror.FromADKMetadata` to read provider error metadata from an ADK
event.

## Content Blocks

Supported ACP content blocks map to ADK parts:

| ACP content block | ADK part |
| --- | --- |
| text | text part |
| image data | bytes part; default MIME type is `image/jpeg` |
| image URI | URI part; default MIME type is `image/jpeg` |
| audio data | bytes part; default MIME type is `audio/wav` |
| resource link | URI part; default MIME type is `application/octet-stream` |

Empty text, invalid base64 data, empty resource URIs, and unsupported content
blocks are ignored and logged as adapter diagnostics.

## Tool Calls

ACP `tool_call` updates become ADK function-call parts:

- `FunctionCall.ID` is the ACP tool call ID.
- `FunctionCall.Name` is `acp_tool_call`.
- `FunctionCall.Args` includes `kind`, `status`, `title`, `locations`,
  `rawInput`, and `rawOutput`.

ACP `tool_call_update` updates become ADK function-response parts:

- `FunctionResponse.ID` is the ACP tool call ID.
- `FunctionResponse.Name` is `acp_tool_call_update`.
- `FunctionResponse.Response` includes `kind`, `status`, `title`,
  `locations`, `rawInput`, and `rawOutput`.

Pending and in-progress tool statuses are also reported through
`event.LongRunningToolIDs`.

## State Updates

ACP `plan` updates do not produce content parts. They produce partial ADK
events whose `Actions.StateDelta[acpagent.PlanStateKey]` contains the full
replacement plan snapshot:

```go
map[string]any{
	"entries": []map[string]any{
		{"content": "...", "status": "...", "priority": "..."},
	},
}
```

The final `TurnComplete` event repeats the latest plan snapshot so ADK session
state stores the final value.

ACP `config_option_update` updates persist the provider's current ACP session
config values under `SessionStateKey.config_values`. ACP
`current_mode_update` persists the current mode as
`SessionConfigValue{ID: "mode"}`. These updates are partial ADK events with
state deltas and no content.

## Usage And Completion

ACP prompt-result usage maps to ADK usage metadata on the final `TurnComplete`
event:

- input tokens -> `PromptTokenCount`
- output tokens -> `CandidatesTokenCount`
- total tokens -> `TotalTokenCount`

Legacy raw `usage_update` notifications with token counts are mapped to partial
ADK events with `UsageMetadata`.

Structured ACP `session/update.usage_update` notifications are also mapped to
partial ADK events with `UsageMetadata`. The adapter projects `size` into
`PromptTokenCount` and `used` into `TotalTokenCount` so the signal stays on the
standard ADK usage lane.

On every successful prompt, the adapter emits one final ADK event:

- `TurnComplete` is true.
- `FinishReason` is copied from the ACP stop reason when available.
- final visible output becomes model text content when present.
- terminal provider failures become `ErrorCode` and `ErrorMessage`.
- terminal prompt-result `_meta.error` payloads are also projected into
  `ErrorCode` and `ErrorMessage` when the prompt ends without visible output.
- `SessionStateKey` is updated with the current ACP session binding and config
  values.
- `OutputKey`, when configured, stores the final visible output in event state.

ACP `available_commands_update`, `session_info_update`, unsupported updates,
and non-user-visible updates are logged and not emitted as ADK events.
