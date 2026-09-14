// Model D's own B2C channel (docs/01-strategy/model-d-model-f-product-separation.md).
// postRetailOrder/getRetailOrderStatus mirror postOrder/getOrderStatus
// exactly -- the same C1+C2 choreography (c6-api-gateway-build-prompts.md's
// own "Read this second"), the same ownership-not-just-existence
// discipline -- scoped to RetailCustomerID instead of CustomerID.
package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"gateway/internal/c1client"
	"gateway/internal/orders"
	"gateway/internal/quotes"
)

// postRetailOrder is POST /v1/retail/orders.
func (s *Server) postRetailOrder(w http.ResponseWriter, r *http.Request) {
	var req postOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.QuoteID == 0 || req.ExternalID == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "quote_id and external_id are both required"))
		return
	}
	rc := retailCustomerFromContext(r.Context())

	if existing, err := s.Orders.Get(r.Context(), req.ExternalID); err == nil {
		if existing.RetailCustomerID == nil || *existing.RetailCustomerID != rc.ID {
			writeAPIError(w, errNotFound)
			return
		}
		q, err := s.Quotes.GetForRetail(r.Context(), existing.QuoteID, rc.ID)
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

	q, err := s.Quotes.GetForRetail(r.Context(), req.QuoteID, rc.ID)
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

	halt, err := s.Ledger.GetHaltState(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if halt.Halted {
		writeAPIError(w, errSystemHalted)
		return
	}

	ownerLabel := fmt.Sprintf("retail:%d", rc.ID)
	c1Order, err := s.Ledger.CreateOrder(r.Context(), c1client.PostOrderRequest{
		ExternalID: req.ExternalID, CustomerID: ownerLabel, Tier: string(q.Tier),
		AmountIn: q.AmountIn, AmountOut: q.AmountOut, FeeUnits: q.FeeUnits, NetworkFeeUnits: q.NetworkFeeUnits,
		RecipientAddress: q.RecipientAddress, QuotedAt: q.CreatedAt, QuoteExpiresAt: q.ExpiresAt,
	}, "gateway:create-order:"+req.ExternalID)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.OrdersCreatedTotal.Inc()

	if _, err := s.Quotes.MarkConsumed(r.Context(), q.ID, req.ExternalID); err != nil {
		if errors.Is(err, quotes.ErrAlreadyConsumed) {
			writeAPIError(w, errQuoteAlreadyConsumed)
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	gatewayOrder, err := s.Orders.CreateForRetail(r.Context(), req.ExternalID, rc.ID, q.ID, c1Order.ID)
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}

	addr, err := s.Watcher.AssignAddress(r.Context(), c1Order.ID, req.ExternalID, ownerLabel, q.CreatedAt, q.ExpiresAt,
		"gateway:assign-address:"+req.ExternalID)
	if err != nil {
		slog.Warn("postRetailOrder: C2 address assignment failed, returning address_pending -- C6.4's reconciliation loop will retry",
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

// getRetailOrderStatus is GET /v1/retail/orders/{external_id} -- mirrors
// getOrderStatus exactly, including reading through to C1's own live
// order state rather than caching/duplicating it (status_handlers.go's
// own doc comment: the pull-based status read and any future webhook
// must never be able to disagree).
func (s *Server) getRetailOrderStatus(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	rc := retailCustomerFromContext(r.Context())

	gatewayOrder, err := s.Orders.Get(r.Context(), externalID)
	if err != nil {
		if errors.Is(err, orders.ErrNotFound) {
			writeAPIError(w, errNotFound)
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	// Ownership, not just existence -- another retail customer's
	// external_id must 404, never leak that the id exists at all.
	if gatewayOrder.RetailCustomerID == nil || *gatewayOrder.RetailCustomerID != rc.ID {
		writeAPIError(w, errNotFound)
		return
	}

	q, err := s.Quotes.GetForRetail(r.Context(), gatewayOrder.QuoteID, rc.ID)
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
			slog.Error("getRetailOrderStatus: C1 reported a state this gateway has no customer-facing mapping for, refusing to pass it through",
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
