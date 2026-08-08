package schema

import (
	"encoding/json"
	"net/http"
)

// Error type identifiers — the canonical set from the API contract (§8.5).
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeAuthentication = "authentication_error"
	TypePermission     = "permission_error"
	TypeRateLimit      = "rate_limit_error"
	TypeProvider       = "provider_error"
	TypeGuardrailBlock = "guardrail_blocked"
	TypeInternal       = "internal_error"
)

// Error codes.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeInvalidAPIKey     = "invalid_api_key"
	CodeModelNotAllowed   = "model_not_allowed"
	CodeRateLimitExceeded = "rate_limit_exceeded"
	CodeProviderError     = "provider_error"
	CodeGuardrailBlocked  = "guardrail_blocked"
	CodeInternal          = "internal_error"
)

// GatewaiError is the canonical error shape returned to clients (§8.5).
// All error paths in the gateway produce a GatewaiError, which guarantees a
// consistent {"error": {...}} envelope and HTTP status across every failure mode.
type GatewaiError struct {
	Type    string
	Code    string
	Message string
	Status  int
}

// Error implements the error interface.
func (e *GatewaiError) Error() string { return e.Message }

// WriteJSON writes the error to the client in the OpenAI-compatible envelope.
func (e *GatewaiError) WriteJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Error: errorBody{Message: e.Message, Type: e.Type, Code: e.Code},
	})
}

// errorBody is the inner object of the error envelope.
type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// errorEnvelope is the outer object: {"error": {...}}.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// Constructors — one per error type from §8.5.

func NewInvalidRequestError(message string) *GatewaiError {
	return &GatewaiError{Type: TypeInvalidRequest, Code: CodeInvalidRequest, Message: message, Status: http.StatusBadRequest}
}

func NewAuthenticationError(message string) *GatewaiError {
	return &GatewaiError{Type: TypeAuthentication, Code: CodeInvalidAPIKey, Message: message, Status: http.StatusUnauthorized}
}

func NewPermissionError(message string) *GatewaiError {
	return &GatewaiError{Type: TypePermission, Code: CodeModelNotAllowed, Message: message, Status: http.StatusForbidden}
}

func NewRateLimitError(message string) *GatewaiError {
	return &GatewaiError{Type: TypeRateLimit, Code: CodeRateLimitExceeded, Message: message, Status: http.StatusTooManyRequests}
}

func NewProviderError(message string) *GatewaiError {
	return &GatewaiError{Type: TypeProvider, Code: CodeProviderError, Message: message, Status: http.StatusBadGateway}
}

func NewGuardrailBlockedError(message string) *GatewaiError {
	return &GatewaiError{Type: TypeGuardrailBlock, Code: CodeGuardrailBlocked, Message: message, Status: http.StatusBadRequest}
}

func NewInternalError(message string) *GatewaiError {
	return &GatewaiError{Type: TypeInternal, Code: CodeInternal, Message: message, Status: http.StatusInternalServerError}
}
