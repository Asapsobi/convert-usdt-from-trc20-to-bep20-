// Model D's own B2C channel (docs/01-strategy/model-d-model-f-product-separation.md).
// postRetailQuote mirrors postQuote exactly (same halt-check-then-price-
// once discipline, same response shape) -- the only difference is who's
// asking and which owner column the resulting row is written under.
package httpapi

import (
	"net/http"
	"time"

	"gateway/internal/money"
	"gateway/internal/pricing"
)

// postRetailQuote is POST /v1/retail/quotes.
func (s *Server) postRetailQuote(w http.ResponseWriter, r *http.Request) {
	var req postQuoteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Tier == "" || req.AmountIn == "" || req.RecipientAddress == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "tier, amount_in, and recipient_address are all required"))
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

	amountIn, err := money.ParseDecimal(req.AmountIn)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "amount_in: "+err.Error()))
		return
	}

	priced, err := pricing.ComputeQuote(pricing.Tier(req.Tier), amountIn)
	if err != nil {
		writeErr(w, err)
		return
	}

	rc := retailCustomerFromContext(r.Context())
	now := time.Now().UTC()
	q, err := s.Quotes.CreateForRetail(r.Context(), rc.ID, priced, req.RecipientAddress, now, s.quoteValidity())
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}

	s.Metrics.QuotesIssuedTotal.Inc()
	respondJSON(w, http.StatusCreated, quoteResponse{
		QuoteID: q.ID, Tier: string(q.Tier),
		AmountIn: q.AmountIn.Format(), AmountOut: q.AmountOut.Format(),
		FeeUnits: q.FeeUnits.Format(), NetworkFeeUnits: q.NetworkFeeUnits.Format(),
		RecipientAddress: q.RecipientAddress,
		CreatedAt:        q.CreatedAt.Format(timeLayout),
		ExpiresAt:        q.ExpiresAt.Format(timeLayout),
	})
}
