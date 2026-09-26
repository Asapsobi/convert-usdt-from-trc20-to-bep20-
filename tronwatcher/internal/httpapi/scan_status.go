package httpapi

import (
	"net/http"
	"time"

	"tronwatcher/internal/addresses"
	"tronwatcher/internal/deposits"
)

// getScanStatus is GET /v1/addresses/{order_id}/scan-status: how far this
// watcher has looked for deposits to the order's address, and whether it
// holds one that hasn't reached the ledger yet. relayd expires an unpaid
// order only once the watcher has scanned past the order's deadline and
// holds nothing for it -- never while the watcher is behind.
func (s *Server) getScanStatus(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
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
	last, err := addresses.ScanFrom(r.Context(), s.Pool, wa.Address, time.Time{})
	if err != nil {
		writeErr(w, err)
		return
	}
	if last.IsZero() {
		writeAPIError(w, newAPIError(http.StatusServiceUnavailable, "not_scanned", "this address has not been scanned yet"))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"scanned_through": last.UTC(), "pending_deposits": pending})
}
