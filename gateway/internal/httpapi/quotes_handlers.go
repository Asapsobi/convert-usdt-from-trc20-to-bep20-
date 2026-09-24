package httpapi

import (
	"errors"
	"net/http"
	"time"

	"gateway/internal/c1client"
	"gateway/internal/money"
	"gateway/internal/pricing"
	"gateway/internal/quotes"
)

type postQuoteRequest struct {
	Tier             string `json:"tier"`
	AmountIn         string `json:"amount_in"`
	RecipientAddress string `json:"recipient_address"`
}

type quoteResponse struct {
	QuoteID          int64  `json:"quote_id"`
	Tier             string `json:"tier"`
	AmountIn         string `json:"amount_in"`
	AmountOut        string `json:"amount_out"`
	FeeUnits         string `json:"fee_units"`
	NetworkFeeUnits  string `json:"network_fee_units"`
	RecipientAddress string `json:"recipient_address"`
	CreatedAt        string `json:"created_at"`
	ExpiresAt        string `json:"expires_at"`
}

const timeLayout = time.RFC3339

// postQuote is POST /v1/quotes -- C6.2. Checks C1's own halt state
// first (invariant 6: halted -> 423, no row written), then prices via
// internal/pricing exactly once and persists the result. This is the
// row C6.3's order creation reads back verbatim -- "quote-then-order,
// never quote-inside-order."
func (s *Server) postQuote(w http.ResponseWriter, r *http.Request) {
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

	customer := customerFromContext(r.Context())
	now := time.Now().UTC()
	q, err := s.Quotes.Create(r.Context(), customer.ID, priced, req.RecipientAddress, now, s.quoteValidity())
	if err != nil {
		writeQuoteCreateErr(w, err)
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

// writeErr maps a domain/upstream error to the stable customer-facing
// apiError and writes it -- the one place every handler's error path
// funnels through, mirroring every prior component's own writeErr.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pricing.ErrUnknownTier), errors.Is(err, pricing.ErrNonPositiveAmount), errors.Is(err, pricing.ErrAmountTooSmall):
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, err.Error()))
	case errors.Is(err, c1client.ErrSystemHalted):
		writeAPIError(w, errSystemHalted)
	case errors.Is(err, c1client.ErrOrderNotFound):
		writeAPIError(w, errNotFound)
	default:
		writeAPIError(w, errUpstreamError)
	}
}

// writeQuoteCreateErr maps a Quotes.Create/CreateForRetail error --
// internal/tronaddr's own validation failure (via
// quotes.ErrInvalidRecipientAddress) gets its own clear, distinct 400,
// never lumped in with a generic internal-error or the c1client/pricing
// cases writeErr above handles (this call never reaches either of
// those). Anything else is a real, unexpected failure in this
// service's own DB layer -- errInternal (500), not errUpstreamError
// (502): no upstream service was involved in a Quotes.Create call.
// Shared by both postQuote and postRetailQuote so the two never drift
// on how this is mapped.
func writeQuoteCreateErr(w http.ResponseWriter, err error) {
	if errors.Is(err, quotes.ErrInvalidRecipientAddress) {
		writeAPIError(w, errInvalidDestinationAddress)
		return
	}
	writeAPIError(w, errInternal)
}
