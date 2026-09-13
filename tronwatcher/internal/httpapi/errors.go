package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"tronwatcher/internal/addresses"
	"tronwatcher/internal/orphaned"
)

// apiError is the one shape every error response takes: a stable
// status, a stable code string, and a human-readable message.
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
	errInternal       = newAPIError(http.StatusInternalServerError, "internal", "internal error")

	errOrderNotFound           = newAPIError(http.StatusNotFound, "order_not_found", "no watched address exists for that order")
	errIllegalStatusTransition = newAPIError(http.StatusConflict, "illegal_transition", "no such address status transition is legal from its current state")
	errOrphanedDepositNotFound = newAPIError(http.StatusNotFound, "orphaned_deposit_not_found", "no orphaned deposit exists with that id")
	errOrphanedDepositResolved = newAPIError(http.StatusConflict, "already_resolved", "that orphaned deposit has already been resolved")
)

// mapError translates a domain error from any lower layer into the
// stable apiError HTTP callers see.
func mapError(err error) *apiError {
	switch {
	case errors.Is(err, addresses.ErrOrderNotFound):
		return errOrderNotFound
	case errors.Is(err, addresses.ErrIllegalStatusTransition):
		return errIllegalStatusTransition
	case errors.Is(err, addresses.ErrNotConfigured):
		return errInternal

	case errors.Is(err, orphaned.ErrNotFound):
		return errOrphanedDepositNotFound
	case errors.Is(err, orphaned.ErrAlreadyResolved):
		return errOrphanedDepositResolved

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

// writeErr maps err through mapError and writes it.
func writeErr(w http.ResponseWriter, err error) {
	writeAPIError(w, mapError(err))
}
