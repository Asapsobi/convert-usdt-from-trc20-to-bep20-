package httpapi

import (
	"net/http"
	"strings"
	"time"

	"screening/internal/holds"
)

type holdResponse struct {
	ID                int64      `json:"id"`
	OrderID           int64      `json:"order_id"`
	ExternalID        string     `json:"external_id"`
	ReasonCode        string     `json:"reason_code"`
	ScreeningResultID *int64     `json:"screening_result_id"`
	OpenedAt          time.Time  `json:"opened_at"`
	Status            string     `json:"status"`
	ResolvedBy        *string    `json:"resolved_by"`
	ResolvedAt        *time.Time `json:"resolved_at"`
	ResolutionNote    *string    `json:"resolution_note"`
}

func toHoldResponse(h holds.Hold) holdResponse {
	return holdResponse{
		ID: h.ID, OrderID: h.OrderID, ExternalID: h.ExternalID, ReasonCode: h.ReasonCode,
		ScreeningResultID: h.ScreeningResultID, OpenedAt: h.OpenedAt, Status: string(h.Status),
		ResolvedBy: h.ResolvedBy, ResolvedAt: h.ResolvedAt, ResolutionNote: h.ResolutionNote,
	}
}

// getHolds is GET /v1/holds?status=<status> -- status omitted lists
// every hold (newest first); status=open (or any other holds.Status
// value) filters to exactly that status.
func (s *Server) getHolds(w http.ResponseWriter, r *http.Request) {
	var status *holds.Status
	if raw := r.URL.Query().Get("status"); raw != "" {
		st := holds.Status(strings.ToUpper(raw))
		status = &st
	}

	list, err := holds.List(r.Context(), s.Pool, status)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]holdResponse, len(list))
	for i, h := range list {
		out[i] = toHoldResponse(h)
	}
	respondJSON(w, http.StatusOK, map[string]any{"holds": out})
}

type resolveHoldRequest struct {
	Reviewer string `json:"reviewer"`
	Note     string `json:"note,omitempty"`
}

// postReleaseHold is POST /v1/holds/{id}/release. reviewer's identity
// comes from the request body, not this API's own bearer auth -- see
// AuthConfig's own doc comment for why: the ops tool calling this
// establishes who the human reviewer is, C3 itself never authenticates
// individual humans.
func (s *Server) postReleaseHold(w http.ResponseWriter, r *http.Request) {
	holdID, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	var req resolveHoldRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if err := holds.Release(r.Context(), s.Pool, s.LedgerClient, holdID, req.Reviewer, req.Note); err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.HoldReleased()

	h, err := holds.Get(r.Context(), s.Pool, holdID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toHoldResponse(h))
}

// postRejectHold is POST /v1/holds/{id}/reject. Wired with
// s.RefundEntryBuilder (holds.StubRefundEntryBuilder unless cmd/screend
// configured a real relayd, per Server's own doc comment) -- against the
// stub, this is EXPECTED to fail with 501 refund_entry_not_implemented
// for a non-RELAY order, same as always; against
// holds.RelayAwareRefundEntryBuilder, a RELAY-tier order now actually
// succeeds.
func (s *Server) postRejectHold(w http.ResponseWriter, r *http.Request) {
	holdID, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	var req resolveHoldRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if err := holds.Reject(r.Context(), s.Pool, s.LedgerClient, s.RefundEntryBuilder, holdID, req.Reviewer, req.Note); err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.HoldRejected()

	h, err := holds.Get(r.Context(), s.Pool, holdID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toHoldResponse(h))
}
