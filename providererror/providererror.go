// Package providererror defines provider-agnostic provider failure metadata
// carried through ACP and ADK runtime layers.
package providererror

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// WireKey is the ACP metadata/data key for provider errors.
	WireKey = "provider_error"
	// ADKMetadataKey is the ADK event metadata key used after ACP mapping.
	ADKMetadataKey = "acp_provider_error"
)

type Kind string

const (
	KindQuotaExceeded          Kind = "quota_exceeded"
	KindAuthenticationRequired Kind = "authentication_required"
	KindPaymentRequired        Kind = "payment_required"
	KindRateLimited            Kind = "rate_limited"
	KindUnavailable            Kind = "unavailable"
	KindInvalidRequest         Kind = "invalid_request"
	KindUnknown                Kind = "unknown"
)

// ProviderError is a provider-agnostic classification of a provider-side
// failure. It intentionally mirrors the ACP wire shape without branding.
type ProviderError struct {
	Kind      Kind   `json:"kind"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Retryable *bool  `json:"retryable,omitempty"`
}

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

// FromWireData extracts provider_error from JSON-RPC error data or ACP _meta.
func FromWireData(data any) (*ProviderError, bool) {
	if data == nil {
		return nil, false
	}
	if meta, ok := asMap(data); ok {
		if value, ok := meta[WireKey]; ok {
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
	value, ok := meta[ADKMetadataKey]
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
