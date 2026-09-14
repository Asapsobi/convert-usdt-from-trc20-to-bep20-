package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gateway/internal/orders"
)

// customerStates enumerates every customer-facing status this endpoint
// may ever return, one entry per state C1 itself can report (plus
// address_pending, this gateway's own pre-C1-visible state). An
// explicit, closed set -- not a passthrough of C1's raw state string --
// so a state C1 adds later that this gateway hasn't been taught about
// is refused loudly (errInternal) rather than leaked to a customer with
// no contract for it.
var customerStates = map[string]string{
	"quoted":      "quoted",
	"funded":      "funded",
	"screened":    "screened",
	"dispatching": "dispatching",
	"settled":     "settled",
	"held":        "held",
	"refunded":    "refunded",
	"expired":     "expired",
}

type orderStatusResponse struct {
	ExternalID       string  `json:"external_id"`
	Tier             string  `json:"tier"`
	AmountIn         string  `json:"amount_in"`
	AmountOut        string  `json:"amount_out"`
	FeeUnits         string  `json:"fee_units"`
	NetworkFeeUnits  string  `json:"network_fee_units"`
	RecipientAddress string  `json:"recipient_address"`
	DepositAddress   *string `json:"deposit_address"`
	Status           string  `json:"status"`
}

// getOrderStatus is GET /v1/orders/{external_id} -- C6.5, decision 5's
// own named pull-based backstop for webhook delivery (C6.6): the two
// must never be able to disagree, so this reads through to C1's own
// live state rather than caching or duplicating it anywhere.
func (s *Server) getOrderStatus(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	customer := customerFromContext(r.Context())

	gatewayOrder, err := s.Orders.Get(r.Context(), externalID)
	if err != nil {
		if errors.Is(err, orders.ErrNotFound) {
			writeAPIError(w, errNotFound)
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	// Ownership, not just existence -- another customer's external_id
	// must 404, never leak that the id exists at all.
	if gatewayOrder.CustomerID == nil || *gatewayOrder.CustomerID != customer.ID {
		writeAPIError(w, errNotFound)
		return
	}

	q, err := s.Quotes.Get(r.Context(), gatewayOrder.QuoteID, customer.ID)
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}

	status := "address_pending"
	if gatewayOrder.C2AddressAssigned {
		c1Order, err := s.Ledger.GetOrder(r.Context(), externalID)
		if err != nil {
			writeErr(w, err)
			return
		}
		translated, ok := customerStates[c1Order.State]
		if !ok {
			slog.Error("getOrderStatus: C1 reported a state this gateway has no customer-facing mapping for, refusing to pass it through",
				"external_id", externalID, "c1_state", c1Order.State)
			writeAPIError(w, errInternal)
			return
		}
		status = translated
	}

	respondJSON(w, http.StatusOK, orderStatusResponse{
		ExternalID: gatewayOrder.ExternalID, Tier: string(q.Tier),
		AmountIn: q.AmountIn.Format(), AmountOut: q.AmountOut.Format(),
		FeeUnits: q.FeeUnits.Format(), NetworkFeeUnits: q.NetworkFeeUnits.Format(),
		RecipientAddress: q.RecipientAddress, DepositAddress: gatewayOrder.DepositAddress, Status: status,
	})
}
