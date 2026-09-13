package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"tronwatcher/internal/money"
	"tronwatcher/internal/orphaned"
)

type orphanedDepositResponse struct {
	ID                    int64      `json:"id"`
	OrderID               int64      `json:"order_id"`
	ExternalID            string     `json:"external_id"`
	TxID                  string     `json:"tx_id"`
	Amount                string     `json:"amount"` // decimal string, never a JSON number
	DetectedAt            time.Time  `json:"detected_at"`
	OrderStateAtDetection string     `json:"order_state_at_detection"`
	Resolution            *string    `json:"resolution,omitempty"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy            *string    `json:"resolved_by,omitempty"`
}

func toOrphanedDepositResponse(d orphaned.Deposit) orphanedDepositResponse {
	return orphanedDepositResponse{
		ID: d.ID, OrderID: d.OrderID, ExternalID: d.ExternalID, TxID: d.TxID,
		Amount: money.Amount(d.Amount).Format(), DetectedAt: d.DetectedAt, OrderStateAtDetection: d.OrderStateAtDetection,
		Resolution: d.Resolution, ResolvedAt: d.ResolvedAt, ResolvedBy: d.ResolvedBy,
	}
}

// getOrphanedDeposits is GET /v1/orphaned-deposits, optionally filtered
// by ?resolved=true|false.
func (s *Server) getOrphanedDeposits(w http.ResponseWriter, r *http.Request) {
	var resolved *bool
	if raw := r.URL.Query().Get("resolved"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "resolved must be true or false"))
			return
		}
		resolved = &parsed
	}

	deposits, err := orphaned.List(r.Context(), s.Pool, resolved)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]orphanedDepositResponse, len(deposits))
	for i, d := range deposits {
		out[i] = toOrphanedDepositResponse(d)
	}
	respondJSON(w, http.StatusOK, map[string]any{"deposits": out})
}

type postResolveOrphanedDepositRequest struct {
	Resolution string `json:"resolution"`
}

// postResolveOrphanedDeposit is POST /v1/orphaned-deposits/{id}/resolve.
// resolved_by comes from the authenticated actor, never a request body
// field, matching this whole system's actor convention.
func (s *Server) postResolveOrphanedDeposit(w http.ResponseWriter, r *http.Request) {
	raw, ok := urlParam(w, r, "id")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "id must be an integer"))
		return
	}

	var req postResolveOrphanedDepositRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Resolution == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "resolution is required"))
		return
	}

	resolved, err := orphaned.Resolve(r.Context(), s.Pool, id, req.Resolution, actorFromContext(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toOrphanedDepositResponse(resolved))
}
