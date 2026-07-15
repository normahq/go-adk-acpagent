package acpagent

import (
	"context"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// PermissionOptionKind describes the lifetime and effect of an agent
// permission choice without exposing the backing provider protocol.
type PermissionOptionKind string

const (
	// PermissionOptionKindAllowOnce allows only the current action.
	PermissionOptionKindAllowOnce PermissionOptionKind = "allow_once"
	// PermissionOptionKindAllowAlways allows matching future actions too.
	PermissionOptionKindAllowAlways PermissionOptionKind = "allow_always"
	// PermissionOptionKindRejectOnce rejects only the current action.
	PermissionOptionKindRejectOnce PermissionOptionKind = "reject_once"
	// PermissionOptionKindRejectAlways rejects matching future actions too.
	PermissionOptionKindRejectAlways PermissionOptionKind = "reject_always"
)

// PermissionOption is one decision offered by the agent.
type PermissionOption struct {
	ID   string
	Name string
	Kind PermissionOptionKind
}

// PermissionLocation identifies a resource affected by a requested action.
type PermissionLocation struct {
	Path string
	Line *int
}

// PermissionDetailKind identifies a user-relevant detail of the requested
// action without exposing the backing provider's raw input schema.
type PermissionDetailKind string

const (
	// PermissionDetailKindReason explains why the action was requested.
	PermissionDetailKindReason PermissionDetailKind = "reason"
	// PermissionDetailKindCommand is a command the action intends to run.
	PermissionDetailKindCommand PermissionDetailKind = "command"
	// PermissionDetailKindWorkingDirectory is the action's working directory.
	PermissionDetailKindWorkingDirectory PermissionDetailKind = "working_directory"
)

// PermissionDetail is a normalized, display-safe detail of a requested
// action. Applications may render these fields without understanding RawInput.
type PermissionDetail struct {
	Kind  PermissionDetailKind
	Value string
}

// PermissionToolCall describes the action that requires authorization.
type PermissionToolCall struct {
	ID        string
	Title     string
	Kind      string
	RawInput  any
	Locations []PermissionLocation
	Details   []PermissionDetail
}

// PermissionRequest is the ADK-facing representation of an agent request to
// authorize a tool action.
type PermissionRequest struct {
	ToolCall PermissionToolCall
	Options  []PermissionOption
}

// PermissionDecision selects one offered option or cancels the request.
type PermissionDecision struct {
	OptionID string
	Canceled bool
}

// PermissionHandler decides an agent permission request at the ADK boundary.
// During an active run, ctx is the context passed to the ADK runner.
type PermissionHandler func(context.Context, PermissionRequest) (PermissionDecision, error)

func protocolPermissionHandler(handler PermissionHandler) ProtocolPermissionHandler {
	if handler == nil {
		return nil
	}
	return func(ctx context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		decision, err := handler(ctx, permissionRequestFromProtocol(request))
		if err != nil {
			return acp.RequestPermissionResponse{}, err
		}
		optionID := strings.TrimSpace(decision.OptionID)
		if !decision.Canceled && optionID != "" && protocolRequestHasOption(request, optionID) {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(optionID))}, nil
		}
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}
}

func permissionRequestFromProtocol(request acp.RequestPermissionRequest) PermissionRequest {
	options := make([]PermissionOption, 0, len(request.Options))
	for _, option := range request.Options {
		options = append(options, PermissionOption{
			ID:   string(option.OptionId),
			Name: strings.TrimSpace(option.Name),
			Kind: PermissionOptionKind(option.Kind),
		})
	}
	locations := make([]PermissionLocation, 0, len(request.ToolCall.Locations))
	for _, location := range request.ToolCall.Locations {
		locations = append(locations, PermissionLocation{Path: strings.TrimSpace(location.Path), Line: location.Line})
	}
	title := ""
	if request.ToolCall.Title != nil {
		title = strings.TrimSpace(*request.ToolCall.Title)
	}
	kind := ""
	if request.ToolCall.Kind != nil {
		kind = string(*request.ToolCall.Kind)
	}
	return PermissionRequest{
		ToolCall: PermissionToolCall{
			ID:        string(request.ToolCall.ToolCallId),
			Title:     title,
			Kind:      kind,
			RawInput:  request.ToolCall.RawInput,
			Locations: locations,
			Details:   permissionDetails(request.ToolCall.RawInput),
		},
		Options: options,
	}
}

func permissionDetails(rawInput any) []PermissionDetail {
	input, ok := rawInput.(map[string]any)
	if !ok {
		return nil
	}
	details := make([]PermissionDetail, 0, 3)
	appendStringDetail := func(kind PermissionDetailKind, key string) {
		value, ok := input[key].(string)
		value = strings.TrimSpace(value)
		if ok && value != "" {
			details = append(details, PermissionDetail{Kind: kind, Value: value})
		}
	}
	appendStringDetail(PermissionDetailKindReason, "reason")
	appendStringDetail(PermissionDetailKindCommand, "command")
	appendStringDetail(PermissionDetailKindWorkingDirectory, "cwd")
	return details
}

func protocolRequestHasOption(request acp.RequestPermissionRequest, optionID string) bool {
	for _, option := range request.Options {
		if string(option.OptionId) == optionID {
			return true
		}
	}
	return false
}
