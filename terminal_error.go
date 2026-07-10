package acpagent

import (
	"encoding/json"
	"fmt"
	"strings"
)

type terminalPromptError struct {
	Message string
	Code    string
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

func terminalPromptErrorFromPromptMeta(meta map[string]any) (*terminalPromptError, bool) {
	if len(meta) == 0 {
		return nil, false
	}
	errMeta, ok := meta["error"].(map[string]any)
	if !ok || len(errMeta) == 0 {
		return nil, false
	}
	return terminalPromptErrorFromMap(errMeta)
}

func terminalPromptErrorFromMap(errMeta map[string]any) (*terminalPromptError, bool) {
	if len(errMeta) == 0 {
		return nil, false
	}
	return newTerminalPromptError(
		terminalPromptErrorStringValue(errMeta["message"]),
		terminalPromptErrorCodeValue(errMeta),
		terminalPromptErrorStringValue(errMeta["additionalDetails"]),
	)
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

func terminalPromptErrorStringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func terminalPromptErrorCodeValue(errMeta map[string]any) any {
	if len(errMeta) == 0 {
		return nil
	}
	for _, key := range []string{"code", "kind", "type", "codexErrorInfo"} {
		if value, ok := errMeta[key]; ok {
			if text := strings.TrimSpace(stringifyTerminalErrorCode(value)); text != "" {
				return value
			}
		}
	}
	return nil
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
