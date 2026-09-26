package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/deposits"
)

// getScanStatus is GET /v1/addresses/{order_id}/scan-status: how far this
// watcher has looked for deposits, and whether it holds one for the order
// that hasn't reached the ledger yet. relayd expires an unpaid order only
// once the watcher has scanned past the order's deadline and holds nothing
// for it -- never while the watcher is behind (a node outage) and a
// payment might still be unseen.
func (s *Server) getScanStatus(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	list, err := deposits.ForOrder(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	pending := 0
	for _, d := range list {
		if d.Status == "DETECTED" || d.Status == "REPORTED" {
			pending++
		}
	}
	height, found, err := chain.LastCandidateScannedHeight(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !found || s.ChainPool == nil {
		writeAPIError(w, newAPIError(http.StatusServiceUnavailable, "not_scanned", "this watcher has not scanned any block yet"))
		return
	}
	header, err := s.ChainPool.HeaderByNumber(r.Context(), height)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusServiceUnavailable, "chain_unavailable",
			fmt.Sprintf("can't read scanned block %d from the chain: %v", height, err)))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"scanned_through":  time.Unix(int64(header.Time), 0).UTC(),
		"scanned_height":   height,
		"pending_deposits": pending,
	})
}
