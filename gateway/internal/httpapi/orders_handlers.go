package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gateway/internal/c1client"
	"gateway/internal/orders"
	"gateway/internal/quotes"
)

type postOrderRequest struct {
	QuoteID    int64  `json:"quote_id"`
	ExternalID string `json:"external_id"`
}

type postOrderResponse struct {
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

func orderResponseFrom(o orders.GatewayOrder, q quotes.Quote) postOrderResponse {
	status := "address_pending"
	if o.C2AddressAssigned {
		status = "created"
	}
	return postOrderResponse{
		ExternalID: o.ExternalID, Tier: string(q.Tier),
		AmountIn: q.AmountIn.Format(), AmountOut: q.AmountOut.Format(),
		FeeUnits: q.FeeUnits.Format(), NetworkFeeUnits: q.NetworkFeeUnits.Format(),
		RecipientAddress: q.RecipientAddress, DepositAddress: o.DepositAddress, Status: status,
	}
}

// postOrder is POST /v1/orders -- C6.3, the C1+C2 choreography
// c6-api-gateway-build-prompts.md's own "Read this second" names in
// full. See this file's own package doc comment for the step-by-step
// account; this function follows it exactly, including the
// deliberately-not-retried step 5 failure mode (address_pending, closed
// asynchronously by C6.4).
func (s *Server) postOrder(w http.ResponseWriter, r *http.Request) {
	var req postOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.QuoteID == 0 || req.ExternalID == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "quote_id and external_id are both required"))
		return
	}
	customer := customerFromContext(r.Context())

	// A retry (or a second call reusing external_id): served entirely
	// from local state, no C1/C2 call at all -- C6.3's own acceptance
	// criterion ("C1 and C2 each called exactly once").
	if existing, err := s.Orders.Get(r.Context(), req.ExternalID); err == nil {
		if existing.CustomerID == nil || *existing.CustomerID != customer.ID {
			writeAPIError(w, errNotFound) // not this customer's order -- ownership, not just existence
			return
		}
		q, err := s.Quotes.Get(r.Context(), existing.QuoteID, customer.ID)
		if err != nil {
			writeAPIError(w, errInternal)
			return
		}
		respondJSON(w, http.StatusCreated, orderResponseFrom(existing, q))
		return
	} else if !errors.Is(err, orders.ErrNotFound) {
		writeAPIError(w, errInternal)
		return
	}

	// Step 1: load the quote, scoped to this customer.
	q, err := s.Quotes.Get(r.Context(), req.QuoteID, customer.ID)
	if err != nil {
		if errors.Is(err, quotes.ErrNotFound) {
			writeAPIError(w, errNotFound)
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	if q.Expired(time.Now().UTC()) {
		writeAPIError(w, errQuoteExpired)
		return
	}

	// Step 2: halt check (invariant 6) -- before either downstream call.
	halt, err := s.Ledger.GetHaltState(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if halt.Halted {
		writeAPIError(w, errSystemHalted)
		return
	}

	// Step 3: create the order in C1, verbatim from the quote row --
	// never recomputed here.
	c1Order, err := s.Ledger.CreateOrder(r.Context(), c1client.PostOrderRequest{
		ExternalID: req.ExternalID, CustomerID: fmt.Sprintf("%d", customer.ID), Tier: string(q.Tier),
		AmountIn: q.AmountIn, AmountOut: q.AmountOut, FeeUnits: q.FeeUnits, NetworkFeeUnits: q.NetworkFeeUnits,
		RecipientAddress: q.RecipientAddress, QuotedAt: q.CreatedAt, QuoteExpiresAt: q.ExpiresAt,
	}, "gateway:create-order:"+req.ExternalID)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.OrdersCreatedTotal.Inc()

	// Step 4: mark the quote consumed and record the gateway_orders row
	// -- both committed once C1's own order is real. A quote already
	// consumed by a DIFFERENT external_id is a genuine race lost, not
	// this request's own replay.
	if _, err := s.Quotes.MarkConsumed(r.Context(), q.ID, req.ExternalID); err != nil {
		if errors.Is(err, quotes.ErrAlreadyConsumed) {
			writeAPIError(w, errQuoteAlreadyConsumed)
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	gatewayOrder, err := s.Orders.Create(r.Context(), req.ExternalID, customer.ID, q.ID, c1Order.ID)
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}

	// Step 5: assign the deposit address via C2. A failure here is NOT
	// fatal to this request -- see "Read this second": return
	// address_pending and let C6.4 close the gap asynchronously, never
	// retry step 3.
	addr, err := s.Watcher.AssignAddress(r.Context(), c1Order.ID, req.ExternalID, fmt.Sprintf("%d", customer.ID), q.CreatedAt, q.ExpiresAt,
		"gateway:assign-address:"+req.ExternalID)
	if err != nil {
		slog.Warn("postOrder: C2 address assignment failed, returning address_pending -- C6.4's reconciliation loop will retry",
			"external_id", req.ExternalID, "error", err)
		respondJSON(w, http.StatusCreated, orderResponseFrom(gatewayOrder, q))
		return
	}

	gatewayOrder, err = s.Orders.MarkAddressAssigned(r.Context(), req.ExternalID, addr.Address)
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}

	respondJSON(w, http.StatusCreated, orderResponseFrom(gatewayOrder, q))
}
