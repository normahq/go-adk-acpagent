// Package acperror defines ACP provider failure metadata carried through ACP
// and ADK runtime layers.
package acperror

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// ProviderErrorWireKey is the ACP metadata/data key for provider errors.
	ProviderErrorWireKey = "provider_error"
	// ProviderErrorMetadataKey is the ADK event metadata key used after ACP
	// mapping.
	ProviderErrorMetadataKey = "acp_provider_error"
)

// Kind identifies a provider-side failure category.
type Kind string

const (
	// KindQuotaExceeded indicates the provider account or project has exhausted
	// its quota.
	KindQuotaExceeded Kind = "quota_exceeded"
	// KindAuthenticationRequired indicates the provider requires authentication.
	KindAuthenticationRequired Kind = "authentication_required"
	// KindPaymentRequired indicates the provider requires billing action.
	KindPaymentRequired Kind = "payment_required"
	// KindRateLimited indicates the provider rejected the request due to rate
	// limiting.
	KindRateLimited Kind = "rate_limited"
	// KindUnavailable indicates the provider is temporarily unavailable.
	KindUnavailable Kind = "unavailable"
	// KindInvalidRequest indicates the provider rejected the request shape or
	// parameters.
	KindInvalidRequest Kind = "invalid_request"
	// KindUnknown indicates the provider failure could not be classified.
	KindUnknown Kind = "unknown"
)

// ProviderError is an ACP provider-agnostic classification of a provider-side
// failure. It intentionally mirrors the ACP wire shape.
type ProviderError struct {
	// Kind classifies the provider failure.
	Kind Kind `json:"kind"`
	// Message contains provider-supplied diagnostic text.
	Message string `json:"message,omitempty"`
	// RequestID identifies the failed provider request when available.
	RequestID string `json:"request_id,omitempty"`
	// Provider names the upstream provider when available.
	Provider string `json:"provider,omitempty"`
	// Retryable reports whether retrying may succeed. Nil means unspecified.
	Retryable *bool `json:"retryable,omitempty"`
}

// Error returns a human-readable provider failure description.
func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil>"
	}
	kind := e.Kind
	if kind == "" {
		kind = KindUnknown
	}
	if msg := strings.TrimSpace(e.Message); msg != "" {
		return fmt.Sprintf("provider error %s: %s", kind, msg)
	}
	return fmt.Sprintf("provider error %s", kind)
}

// Metadata returns the wire-shaped metadata representation of e.
func (e *ProviderError) Metadata() map[string]any {
	if e == nil {
		return nil
	}
	meta := map[string]any{
		"kind": string(e.Kind),
	}
	if e.Message != "" {
		meta["message"] = e.Message
	}
	if e.RequestID != "" {
		meta["request_id"] = e.RequestID
	}
	if e.Provider != "" {
		meta["provider"] = e.Provider
	}
	if e.Retryable != nil {
		meta["retryable"] = *e.Retryable
	}
	return meta
}

// FromWireData extracts provider_error from JSON-RPC error data or ACP _meta.
func FromWireData(data any) (*ProviderError, bool) {
	if data == nil {
		return nil, false
	}
	if meta, ok := asMap(data); ok {
		if value, ok := meta[ProviderErrorWireKey]; ok {
			return FromWireValue(value)
		}
	}
	return FromWireValue(data)
}

// FromMetadata extracts provider_error from ACP metadata.
func FromMetadata(meta map[string]any) (*ProviderError, bool) {
	if len(meta) == 0 {
		return nil, false
	}
	return FromWireData(meta)
}

// FromADKMetadata extracts a provider error already mapped to ADK metadata.
func FromADKMetadata(meta map[string]any) (*ProviderError, bool) {
	if len(meta) == 0 {
		return nil, false
	}
	value, ok := meta[ProviderErrorMetadataKey]
	if !ok {
		return nil, false
	}
	return FromWireValue(value)
}

// FromWireValue parses the provider_error object itself.
func FromWireValue(value any) (*ProviderError, bool) {
	switch v := value.(type) {
	case nil:
		return nil, false
	case *ProviderError:
		return normalize(v)
	case ProviderError:
		return normalize(&v)
	case map[string]any:
		return fromMap(v)
	default:
		var decoded map[string]any
		b, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		if err := json.Unmarshal(b, &decoded); err != nil {
			return nil, false
		}
		return fromMap(decoded)
	}
}

func fromMap(m map[string]any) (*ProviderError, bool) {
	kind := Kind(strings.TrimSpace(stringValue(m["kind"])))
	if kind == "" {
		return nil, false
	}
	err := &ProviderError{
		Kind:      kind,
		Message:   strings.TrimSpace(stringValue(m["message"])),
		RequestID: strings.TrimSpace(stringValue(m["request_id"])),
		Provider:  strings.TrimSpace(stringValue(m["provider"])),
	}
	if retryable, ok := boolValue(m["retryable"]); ok {
		err.Retryable = &retryable
	}
	return normalize(err)
}

func normalize(err *ProviderError) (*ProviderError, bool) {
	if err == nil {
		return nil, false
	}
	err.Kind = Kind(strings.TrimSpace(string(err.Kind)))
	if err.Kind == "" {
		return nil, false
	}
	err.Message = strings.TrimSpace(err.Message)
	err.RequestID = strings.TrimSpace(err.RequestID)
	err.Provider = strings.TrimSpace(err.Provider)
	return err, true
}

func asMap(value any) (map[string]any, bool) {
	if m, ok := value.(map[string]any); ok {
		return m, true
	}
	var decoded map[string]any
	b, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

func boolValue(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	default:
		return false, false
	}
}
