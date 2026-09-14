package httpapi

import (
	"encoding/json"
	"net/http"
)

// apiError is the one shape every error response takes: a stable
// status, a stable code string (never reused for a different
// condition), and a human-readable message that may change freely --
// same discipline as every prior component's own apiError, and C6.8's
// own acceptance criterion that every component in this corridor share
// one error shape.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{Status: status, Code: code, Message: message}
}

var (
	errInvalidRequest = newAPIError(http.StatusBadRequest, "invalid_request", "the request could not be validated")
	errUnauthorized   = newAPIError(http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
	errSuspended      = newAPIError(http.StatusForbidden, "customer_suspended", "this customer account is suspended")
	errInternal       = newAPIError(http.StatusInternalServerError, "internal", "internal error")
	errNotFound       = newAPIError(http.StatusNotFound, "not_found", "no such resource")
	errSystemHalted   = newAPIError(http.StatusLocked, "system_halted", "the ledger is currently halted; try again shortly")

	errQuoteExpired         = newAPIError(http.StatusConflict, "quote_expired", "this quote has expired; request a new one")
	errQuoteAlreadyConsumed = newAPIError(http.StatusConflict, "quote_already_consumed", "this quote was already used by a different order")
	errIdempotencyConflict  = newAPIError(http.StatusConflict, "idempotency_conflict", "the same Idempotency-Key was already used with a different request body")

	// errSandboxOnly/errProductionOnly enforce C6.7's own invariant 4 at
	// the API boundary: a sandbox key and a production key are never
	// interchangeable, on any route, ever.
	errSandboxOnly    = newAPIError(http.StatusForbidden, "sandbox_only", "this endpoint requires a sandbox API key")
	errProductionOnly = newAPIError(http.StatusForbidden, "production_only", "a sandbox API key cannot be used on this endpoint")

	// errUpstreamError covers a non-mapped failure reaching C1 or C2 --
	// surfaced distinctly from errInternal so an operator reading
	// logs/metrics can tell "an upstream service is unhappy" from "this
	// service broke on its own." Never leaks which upstream, or any
	// internal id -- C6.8's own acceptance criterion.
	errUpstreamError = newAPIError(http.StatusBadGateway, "upstream_error", "a downstream service returned an unexpected error")

	// Model D's own B2C channel (docs/01-strategy/model-d-model-f-product-separation.md).
	// errInvalidCredentials deliberately covers both "no such email" and
	// "wrong password" -- see retailcustomers.ErrInvalidCredentials's own
	// doc comment for why they're never distinguished at any layer.
	errEmailTaken         = newAPIError(http.StatusConflict, "email_taken", "an account with this email already exists")
	errInvalidCredentials = newAPIError(http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
	errRetailUnauthorized = newAPIError(http.StatusUnauthorized, "unauthorized", "missing or invalid session token")
	errRetailSuspended    = newAPIError(http.StatusForbidden, "account_suspended", "this account is suspended")
)

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: e.Code, Message: e.Message}})
}
