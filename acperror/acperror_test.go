package acperror

import (
	"errors"
	"fmt"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/go-cmp/cmp"
)

func TestFromWireDataExtractsProviderError(t *testing.T) {
	t.Parallel()

	err, ok := FromWireData(map[string]any{
		"provider_error": map[string]any{
			"kind":        "quota_exceeded",
			"message":     "quota exceeded",
			"code":        "usageLimitExceeded",
			"scope":       "account",
			"http_status": float64(429),
			"retry_at":    float64(1_792_231_200),
			"request_id":  "req-1",
			"retryable":   false,
		},
	})
	if !ok {
		t.Fatal("FromWireData() ok = false, want true")
	}
	if err.Kind != KindQuotaExceeded {
		t.Fatalf("Kind = %q, want %q", err.Kind, KindQuotaExceeded)
	}
	if err.Message != "quota exceeded" {
		t.Fatalf("Message = %q", err.Message)
	}
	if err.RequestID != "req-1" {
		t.Fatalf("RequestID = %q", err.RequestID)
	}
	if err.Code != "usageLimitExceeded" || err.Scope != ScopeAccount {
		t.Fatalf("Code/Scope = %q/%q", err.Code, err.Scope)
	}
	if err.HTTPStatus == nil || *err.HTTPStatus != 429 {
		t.Fatalf("HTTPStatus = %v, want 429", err.HTTPStatus)
	}
	if err.RetryAt == nil || *err.RetryAt != 1_792_231_200 {
		t.Fatalf("RetryAt = %v, want 1792231200", err.RetryAt)
	}
	if err.Retryable == nil || *err.Retryable {
		t.Fatalf("Retryable = %v, want false", err.Retryable)
	}
}

func TestFromWireDataRejectsPlainText(t *testing.T) {
	t.Parallel()

	if err, ok := FromWireData(nil); ok {
		t.Fatalf("FromWireData(nil) = (%v, true), want false", err)
	}
	if err, ok := FromWireData("Error: You have exceeded your monthly quota"); ok {
		t.Fatalf("FromWireData() = (%v, true), want false", err)
	}
}

func TestProviderErrorError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *ProviderError
		want string
	}{
		{name: "nil", want: "<nil>"},
		{name: "unknown", err: &ProviderError{}, want: "provider error unknown"},
		{name: "kind", err: &ProviderError{Kind: KindRateLimited}, want: "provider error rate_limited"},
		{name: "message", err: &ProviderError{Kind: KindPaymentRequired, Message: " billing required "}, want: "provider error payment_required: billing required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProviderErrorMetadata(t *testing.T) {
	t.Parallel()

	if got := (*ProviderError)(nil).Metadata(); got != nil {
		t.Fatalf("nil Metadata() = %#v, want nil", got)
	}

	retryable := true
	httpStatus := 503
	retryAt := int64(1_792_231_200)
	err := &ProviderError{
		Kind:       KindUnavailable,
		Message:    "temporarily unavailable",
		Code:       "serverOverloaded",
		Scope:      ScopeProvider,
		HTTPStatus: &httpStatus,
		RetryAt:    &retryAt,
		RequestID:  "req-1",
		Provider:   "test-provider",
		Retryable:  &retryable,
	}
	want := map[string]any{
		"kind":        string(KindUnavailable),
		"message":     "temporarily unavailable",
		"code":        "serverOverloaded",
		"scope":       "provider",
		"http_status": 503,
		"retry_at":    int64(1_792_231_200),
		"request_id":  "req-1",
		"provider":    "test-provider",
		"retryable":   true,
	}
	if diff := cmp.Diff(want, err.Metadata()); diff != "" {
		t.Errorf("Metadata() mismatch (-want +got):\n%s", diff)
	}
}

func TestFromRequestError(t *testing.T) {
	t.Parallel()

	reqErr := &acp.RequestError{
		Code:    -32603,
		Message: "Provider request failed",
		Data: map[string]any{
			"provider_error": map[string]any{"kind": "session_exhausted", "scope": "session"},
		},
	}
	providerErr, gotReqErr, ok := FromRequestError(reqErr)
	if !ok {
		t.Fatal("FromRequestError() ok = false, want true")
	}
	if gotReqErr != reqErr {
		t.Fatalf("request error = %p, want %p", gotReqErr, reqErr)
	}
	if providerErr.Kind != KindSessionExhausted || providerErr.Scope != ScopeSession {
		t.Fatalf("provider error = %#v, want session_exhausted/session", providerErr)
	}
	if _, _, ok := FromRequestError(errors.New("plain")); ok {
		t.Fatal("FromRequestError(plain) ok = true, want false")
	}
}

func TestFromMetadata(t *testing.T) {
	t.Parallel()

	if err, ok := FromMetadata(nil); ok {
		t.Fatalf("FromMetadata(nil) = (%v, true), want false", err)
	}
	err, ok := FromMetadata(map[string]any{
		ProviderErrorWireKey: map[string]any{"kind": string(KindAuthenticationRequired)},
	})
	if !ok {
		t.Fatal("FromMetadata() ok = false, want true")
	}
	if err.Kind != KindAuthenticationRequired {
		t.Fatalf("Kind = %q, want %q", err.Kind, KindAuthenticationRequired)
	}
}

func TestFromADKMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta map[string]any
		want Kind
		ok   bool
	}{
		{name: "empty", meta: nil},
		{name: "missing key", meta: map[string]any{ProviderErrorWireKey: map[string]any{"kind": string(KindQuotaExceeded)}}},
		{name: "present", meta: map[string]any{ProviderErrorMetadataKey: ProviderError{Kind: KindInvalidRequest}}, want: KindInvalidRequest, ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err, ok := FromADKMetadata(tc.meta)
			if ok != tc.ok {
				t.Fatalf("FromADKMetadata() ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if err.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", err.Kind, tc.want)
			}
		})
	}
}

func TestFromWireValueVariants(t *testing.T) {
	t.Parallel()

	retryable := false
	tests := []struct {
		name  string
		value any
		want  *ProviderError
		ok    bool
	}{
		{name: "nil", value: nil},
		{name: "typed nil", value: (*ProviderError)(nil)},
		{name: "empty pointer", value: &ProviderError{}},
		{name: "pointer", value: &ProviderError{Kind: " rate_limited ", Message: " wait ", RequestID: " req-2 ", Provider: " provider "}, want: &ProviderError{Kind: KindRateLimited, Message: "wait", RequestID: "req-2", Provider: "provider"}, ok: true},
		{name: "value", value: ProviderError{Kind: KindQuotaExceeded, Retryable: &retryable}, want: &ProviderError{Kind: KindQuotaExceeded, Retryable: &retryable}, ok: true},
		{name: "map", value: map[string]any{"kind": kindStringer{KindPaymentRequired}, "retryable": true}, want: &ProviderError{Kind: KindPaymentRequired, Retryable: ptrBool(true)}, ok: true},
		{name: "struct", value: struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}{Kind: string(KindUnavailable), Message: "down"}, want: &ProviderError{Kind: KindUnavailable, Message: "down"}, ok: true},
		{name: "missing kind", value: map[string]any{"message": "missing"}},
		{name: "marshal failure", value: func() {}},
		{name: "non-object json", value: []string{"not", "object"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := FromWireValue(tc.value)
			if ok != tc.ok {
				t.Fatalf("FromWireValue() ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("FromWireValue() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFromWireDataVariants(t *testing.T) {
	t.Parallel()

	err, ok := FromWireData(struct {
		ProviderError ProviderError `json:"provider_error"`
	}{ProviderError: ProviderError{Kind: KindRateLimited}})
	if !ok {
		t.Fatal("FromWireData(struct) ok = false, want true")
	}
	if err.Kind != KindRateLimited {
		t.Fatalf("Kind = %q, want %q", err.Kind, KindRateLimited)
	}

	if err, ok := FromWireData(func() {}); ok {
		t.Fatalf("FromWireData(func) = (%v, true), want false", err)
	}
}

func ptrBool(v bool) *bool {
	return &v
}

type kindStringer struct {
	kind Kind
}

func (k kindStringer) String() string {
	return fmt.Sprint(k.kind)
}
