package acpagent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestProtocolPermissionHandlerMapsGenericRequestAndDecision(t *testing.T) {
	title := "run command"
	kind := acp.ToolKindExecute
	var got PermissionRequest
	handler := protocolPermissionHandler(func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
		got = request
		return PermissionDecision{OptionID: "reject"}, nil
	})
	response, err := handler(context.Background(), acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "call-1", Title: &title, Kind: &kind, RawInput: map[string]any{
			"reason":  "Check the current directory",
			"command": "pwd",
			"cwd":     "/workspace",
		}},
		Options: []acp.PermissionOption{
			{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	if got.ToolCall.ID != "call-1" || got.ToolCall.Title != title || got.ToolCall.Kind != "execute" {
		t.Fatalf("request = %+v", got)
	}
	if len(got.Options) != 2 || got.Options[0].Kind != PermissionOptionKindAllowOnce {
		t.Fatalf("options = %+v", got.Options)
	}
	wantDetails := []PermissionDetail{
		{Kind: PermissionDetailKindReason, Value: "Check the current directory"},
		{Kind: PermissionDetailKindCommand, Value: "pwd"},
		{Kind: PermissionDetailKindWorkingDirectory, Value: "/workspace"},
	}
	if !reflect.DeepEqual(got.ToolCall.Details, wantDetails) {
		t.Fatalf("details = %+v, want %+v", got.ToolCall.Details, wantDetails)
	}
	if response.Outcome.Selected == nil || response.Outcome.Selected.OptionId != "reject" {
		t.Fatalf("outcome = %+v", response.Outcome)
	}
}

func TestPermissionDetailsIgnoresUnknownAndNonStringInput(t *testing.T) {
	got := permissionDetails(map[string]any{
		"command":  []string{"go", "test"},
		"cwd":      "  /workspace  ",
		"threadId": "internal-id",
	})
	want := []PermissionDetail{{Kind: PermissionDetailKindWorkingDirectory, Value: "/workspace"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("permissionDetails() = %+v, want %+v", got, want)
	}
}

func TestProtocolPermissionHandlerCancelsUnknownOption(t *testing.T) {
	handler := protocolPermissionHandler(func(context.Context, PermissionRequest) (PermissionDecision, error) {
		return PermissionDecision{OptionID: "not-offered"}, nil
	})
	response, err := handler(context.Background(), acp.RequestPermissionRequest{Options: []acp.PermissionOption{{OptionId: "allow"}}})
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	if response.Outcome.Cancelled == nil {
		t.Fatalf("outcome = %+v, want cancelled", response.Outcome)
	}
}

func TestProtocolPermissionHandlerNilAndError(t *testing.T) {
	if protocolPermissionHandler(nil) != nil {
		t.Fatal("protocolPermissionHandler(nil) != nil")
	}
	wantErr := errors.New("review failed")
	handler := protocolPermissionHandler(func(context.Context, PermissionRequest) (PermissionDecision, error) {
		return PermissionDecision{}, wantErr
	})
	_, err := handler(context.Background(), acp.RequestPermissionRequest{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("handler() error = %v, want %v", err, wantErr)
	}
}
