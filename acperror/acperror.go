// Package acperror defines ACP provider failure metadata carried through ACP
// and ADK runtime layers.
package acperror

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	acp "github.com/coder/acp-go-sdk"
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
	// KindSessionExhausted indicates the current provider session has exhausted
	// its per-session budget without affecting account health.
	KindSessionExhausted Kind = "session_exhausted"
	// KindInvalidRequest indicates the provider rejected the request shape or
	// parameters.
	KindInvalidRequest Kind = "invalid_request"
	// KindUnknown indicates the provider failure could not be classified.
	KindUnknown Kind = "unknown"
)

// Scope identifies which provider boundary a failure affects.
type Scope string

const (
	// ScopeRequest affects only the submitted request.
	ScopeRequest Scope = "request"
	// ScopeSession affects only the current provider session.
	ScopeSession Scope = "session"
	// ScopeProvider affects the selected provider service or member.
	ScopeProvider Scope = "provider"
	// ScopeAccount affects the configured provider account.
	ScopeAccount Scope = "account"
)

// ProviderError is an ACP provider-agnostic classification of a provider-side
// failure. It intentionally mirrors the ACP wire shape.
type ProviderError struct {
	// Kind classifies the provider failure.
	Kind Kind `json:"kind"`
	// Message contains bounded diagnostic text suitable for policy logs.
	Message string `json:"message,omitempty"`
	// Code preserves the stable provider error discriminator when available.
	Code string `json:"code,omitempty"`
	// Scope identifies the request, session, provider, or account affected.
	Scope Scope `json:"scope,omitempty"`
	// HTTPStatus is the upstream HTTP status when the provider reported one.
	HTTPStatus *int `json:"http_status,omitempty"`
	// RetryAt is an absolute Unix timestamp backed by trustworthy reset data.
	RetryAt *int64 `json:"retry_at,omitempty"`
	// RequestID identifies the failed provider request when available.
	RequestID string `json:"request_id,omitempty"`
	// Provider names the upstream provider when available.
	Provider string `json:"provider,omitempty"`
	// Retryable is retained for compatibility with older producers. Consumers
	// should derive policy from Kind, Scope, and RetryAt.
	//
	// Deprecated: retry policy belongs to the consuming runtime.
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
	if e.Code != "" {
		meta["code"] = e.Code
	}
	if e.Scope != "" {
		meta["scope"] = string(e.Scope)
	}
	if e.HTTPStatus != nil {
		meta["http_status"] = *e.HTTPStatus
	}
	if e.RetryAt != nil {
		meta["retry_at"] = *e.RetryAt
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

// FromRequestError extracts provider_error from an ACP JSON-RPC request error.
func FromRequestError(err error) (*ProviderError, *acp.RequestError, bool) {
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) || reqErr == nil {
		return nil, nil, false
	}
	providerErr, ok := FromWireData(reqErr.Data)
	if !ok {
		return nil, reqErr, false
	}
	return providerErr, reqErr, true
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
		Code:      strings.TrimSpace(stringValue(m["code"])),
		Scope:     Scope(strings.TrimSpace(stringValue(m["scope"]))),
		RequestID: strings.TrimSpace(stringValue(m["request_id"])),
		Provider:  strings.TrimSpace(stringValue(m["provider"])),
	}
	if status, ok := intValue(m["http_status"]); ok {
		err.HTTPStatus = &status
	}
	if retryAt, ok := int64Value(m["retry_at"]); ok {
		err.RetryAt = &retryAt
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
	err.Code = strings.TrimSpace(err.Code)
	err.Scope = Scope(strings.TrimSpace(string(err.Scope)))
	err.RequestID = strings.TrimSpace(err.RequestID)
	err.Provider = strings.TrimSpace(err.Provider)
	return err, true
}

func intValue(value any) (int, bool) {
	value64, ok := int64Value(value)
	if !ok || value64 > int64(math.MaxInt) || value64 < int64(math.MinInt) {
		return 0, false
	}
	return int(value64), true
}

func int64Value(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		if math.Trunc(value) == value && value <= math.MaxInt64 && value >= math.MinInt64 {
			return int64(value), true
		}
	case json.Number:
		parsed, err := value.Int64()
		return parsed, err == nil
	}
	return 0, false
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
