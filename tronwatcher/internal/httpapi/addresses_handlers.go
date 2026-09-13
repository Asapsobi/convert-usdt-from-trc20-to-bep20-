package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"tronwatcher/internal/addresses"
)

type addressResponse struct {
	Address        string     `json:"address"`
	OrderID        int64      `json:"order_id"`
	ExternalID     string     `json:"external_id"`
	CustomerID     string     `json:"customer_id"`
	Status         string     `json:"status"`
	QuotedAt       time.Time  `json:"quoted_at"`
	QuoteExpiresAt time.Time  `json:"quote_expires_at"`
	AssignedAt     time.Time  `json:"assigned_at"`
	RetiredAt      *time.Time `json:"retired_at,omitempty"`
	RetiredReason  *string    `json:"retired_reason,omitempty"`
}

func toAddressResponse(wa addresses.WatchedAddress) addressResponse {
	return addressResponse{
		Address: string(wa.Address), OrderID: wa.OrderID, ExternalID: wa.ExternalID, CustomerID: wa.CustomerID,
		Status: string(wa.Status), QuotedAt: wa.QuotedAt, QuoteExpiresAt: wa.QuoteExpiresAt,
		AssignedAt: wa.AssignedAt, RetiredAt: wa.RetiredAt, RetiredReason: wa.RetiredReason,
	}
}

// orderIDParam parses the {order_id} path parameter, common to all
// three address routes.
func orderIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw, ok := urlParam(w, r, "order_id")
	if !ok {
		return 0, false
	}
	orderID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "order_id must be an integer"))
		return 0, false
	}
	return orderID, true
}

type postAddressRequest struct {
	OrderID        int64     `json:"order_id"`
	ExternalID     string    `json:"external_id"`
	CustomerID     string    `json:"customer_id"`
	QuotedAt       time.Time `json:"quoted_at"`
	QuoteExpiresAt time.Time `json:"quote_expires_at"`
}

// postAddress is POST /v1/addresses. Always 200, idempotent on
// order_id.
func (s *Server) postAddress(w http.ResponseWriter, r *http.Request) {
	var req postAddressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OrderID == 0 || req.ExternalID == "" || req.CustomerID == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"order_id, external_id, and customer_id are required"))
		return
	}

	if _, err := addresses.Assign(r.Context(), s.Pool, req.OrderID, req.ExternalID, req.CustomerID, req.QuotedAt, req.QuoteExpiresAt); err != nil {
		writeErr(w, err)
		return
	}

	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, req.OrderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toAddressResponse(wa))
}

// getAddress is GET /v1/addresses/{order_id}.
func (s *Server) getAddress(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toAddressResponse(wa))
}

type postRetireAddressRequest struct {
	Reason string `json:"reason"`
}

// postRetireAddress is POST /v1/addresses/{order_id}/retire.
func (s *Server) postRetireAddress(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	var req postRetireAddressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "reason is required"))
		return
	}

	if err := addresses.Retire(r.Context(), s.Pool, orderID, req.Reason); err != nil {
		writeErr(w, err)
		return
	}
	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toAddressResponse(wa))
}
