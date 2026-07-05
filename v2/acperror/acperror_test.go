package acperror

import "testing"

func TestFromWireDataExtractsProviderError(t *testing.T) {
	t.Parallel()

	err, ok := FromWireData(map[string]any{
		"provider_error": map[string]any{
			"kind":       "quota_exceeded",
			"message":    "quota exceeded",
			"request_id": "req-1",
			"retryable":  false,
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
	if err.Retryable == nil || *err.Retryable {
		t.Fatalf("Retryable = %v, want false", err.Retryable)
	}
}

func TestFromWireDataRejectsPlainText(t *testing.T) {
	t.Parallel()

	if err, ok := FromWireData("Error: You have exceeded your monthly quota"); ok {
		t.Fatalf("FromWireData() = (%v, true), want false", err)
	}
}
