package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"s1/internal/requests"
	"s1/internal/slots"
)

// apiError is the one shape every error response takes: a stable
// status, a stable code string (never reused for a different
// condition), and a human-readable message that may change freely.
// Same discipline as every prior component's own apiError.
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
	errUnauthorized   = newAPIError(http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token for this operation")
	errInternal       = newAPIError(http.StatusInternalServerError, "internal", "internal error")

	errRequestNotFound             = newAPIError(http.StatusNotFound, "signing_request_not_found", "no signing request exists with that id")
	errAlreadyResolved             = newAPIError(http.StatusConflict, "already_resolved", "this signing request is already SIGNED or REJECTED")
	errSlotNotFound                = newAPIError(http.StatusNotFound, "slot_not_found", "no slot is registered with that id")
	errDepositSigningNotConfigured = newAPIError(http.StatusServiceUnavailable, "deposit_signing_not_configured", "this S1 deployment has no BSC deposit-sweep signing configured (S1_BSC_DEPOSIT_XPRV/XPUB unset)")
)

// mapError translates a domain error from any lower layer into the
// stable apiError HTTP callers see, same convention as every prior
// component's own mapError: more specific sentinels checked before more
// general ones.
func mapError(err error) *apiError {
	switch {
	case errors.Is(err, requests.ErrRequestNotFound):
		return errRequestNotFound
	case errors.Is(err, requests.ErrRequestAlreadyResolved):
		return errAlreadyResolved
	case errors.Is(err, slots.ErrSlotNotFound):
		return errSlotNotFound
	case errors.Is(err, requests.ErrDepositSigningNotConfigured):
		return errDepositSigningNotConfigured
	default:
		return errInternal
	}
}

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

// writeErr maps err through mapError and writes it -- the one place
// every handler's error path funnels through.
func writeErr(w http.ResponseWriter, err error) {
	writeAPIError(w, mapError(err))
}
