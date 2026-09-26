package httpapi

import (
	"errors"
	"net/http"
	"time"

	"dispatcher/internal/dispatch"
)

type postDispatchRequest struct {
	ExternalID string `json:"external_id"`
}

type dispatchResponse struct {
	OrderID              int64      `json:"order_id"`
	SlotID               int        `json:"slot_id"`
	ConversionEntryKey   string     `json:"conversion_entry_key"`
	Status               string     `json:"status"`
	EnteredDispatchingAt time.Time  `json:"entered_dispatching_at"`
	AttemptNumber        *int       `json:"latest_attempt_number,omitempty"`
	AttemptStatus        *string    `json:"latest_attempt_status,omitempty"`
	TronTxID             *string    `json:"tron_txid,omitempty"`
	BroadcastAt          *time.Time `json:"broadcast_at,omitempty"`
}

func toDispatchResponse(a dispatch.Attempt, latest *dispatch.BroadcastAttempt) dispatchResponse {
	resp := dispatchResponse{
		OrderID: a.OrderID, SlotID: a.SlotID, ConversionEntryKey: a.ConversionEntryKey,
		Status: string(a.Status), EnteredDispatchingAt: a.EnteredDispatchingAt,
	}
	if latest != nil {
		n := latest.AttemptNumber
		status := string(latest.Status)
		resp.AttemptNumber = &n
		resp.AttemptStatus = &status
		resp.TronTxID = latest.TronTxID
		resp.BroadcastAt = latest.BroadcastAt
	}
	return resp
}

// postDispatch is POST /v1/dispatch -- the explicit trigger C6 (or an
// ops tool) calls once an order is screened. Does only the synchronous
// half of a dispatch (slot selection under caps, EnterDispatching's own
// E2) -- see this package's own doc comment on why construction,
// signing, broadcast, and finality are not part of this request.
//
// occurredAt (the E2 entry's own timestamp) is resolved from any
// EXISTING local dispatch_state row for this order, not freshly computed
// every call: a retried POST /dispatch (the caller never saw the first
// response) must reuse the SAME value the first call used, or C1's own
// idempotency replay check rejects it as a payload mismatch -- see
// EnterDispatching's own doc comment for the underlying reason. Only a
// genuinely first-ever call for this order computes a fresh time.Now().
func (s *Server) postDispatch(w http.ResponseWriter, r *http.Request) {
	var req postDispatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ExternalID == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "external_id is required"))
		return
	}

	order, err := s.Dispatcher.Ledger.GetOrder(r.Context(), req.ExternalID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if order.State != "screened" {
		writeAPIError(w, newAPIError(http.StatusConflict, errOrderNotReady.Code, "order is "+order.State+", not screened"))
		return
	}
	if !dispatch.DispatchableTier(order.Tier) {
		writeAPIError(w, newAPIError(http.StatusConflict, errOrderNotReady.Code, "order is tier "+order.Tier+", which another service pays out"))
		return
	}

	occurredAt := time.Now().UTC()
	if existing, err := s.Dispatcher.Store.Get(r.Context(), order.ID); err == nil {
		occurredAt = existing.EnteredDispatchingAt
	} else if !errors.Is(err, dispatch.ErrAttemptNotFound) {
		writeErr(w, err)
		return
	}

	slot, err := s.Slots.SelectForDispatch(r.Context(), s.Ledger, s.SlotCaps)
	if err != nil {
		writeErr(w, err)
		return
	}

	attempt, err := s.Dispatcher.EnterDispatching(r.Context(), order, slot.ID, occurredAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.Metrics != nil {
		s.Metrics.DispatchesTotal.WithLabelValues("dispatching").Inc()
	}

	respondJSON(w, http.StatusAccepted, toDispatchResponse(attempt, nil))
}

// getDispatch is GET /v1/dispatch/{order_id} -- current attempt status,
// local dispatch_state plus (if any broadcast attempt has ever been
// recorded) the most recent dispatch_attempts row.
func (s *Server) getDispatch(w http.ResponseWriter, r *http.Request) {
	orderID, ok := urlParamInt64(w, r, "order_id")
	if !ok {
		return
	}

	attempt, err := s.Dispatcher.Store.Get(r.Context(), orderID)
	if err != nil {
		writeErr(w, err)
		return
	}

	var latest *dispatch.BroadcastAttempt
	if l, err := s.Dispatcher.Attempts.LatestForOrder(r.Context(), orderID); err == nil {
		latest = &l
	} else if !errors.Is(err, dispatch.ErrBroadcastAttemptNotFound) {
		writeErr(w, err)
		return
	}

	respondJSON(w, http.StatusOK, toDispatchResponse(attempt, latest))
}
