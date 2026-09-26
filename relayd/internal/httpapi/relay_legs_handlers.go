package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"relayd/internal/driver"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/watcherclient"
)

func formatAmount(a money.Amount) (string, error) {
	return money.Format(a)
}

// breakdown is the money a customer sees: amount_in = our_fee +
// vendor_fee + amount_out (USDT, the same scale on both networks).
type breakdown struct {
	AmountIn  string `json:"amount_in"`
	OurFee    string `json:"our_fee"`
	VendorFee string `json:"vendor_fee"`
	AmountOut string `json:"amount_out"`
	Vendor    string `json:"vendor"`
}

func breakdownFrom(q driver.Quote) (breakdown, error) {
	var b breakdown
	var err error
	for _, f := range []struct {
		dst *string
		a   money.Amount
	}{{&b.AmountIn, q.AmountIn}, {&b.OurFee, q.OurFee}, {&b.VendorFee, q.VendorFee}, {&b.AmountOut, q.AmountOut}} {
		if *f.dst, err = formatAmount(f.a); err != nil {
			return breakdown{}, err
		}
	}
	b.Vendor = q.Vendor
	return b, nil
}

// writeDriverError maps a driver failure to a status: the customer's own
// mistake is a 400, anything else is ours or a vendor's.
func writeDriverError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, driver.ErrBadRequest):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, driver.ErrConflict):
		writeError(w, http.StatusConflict, errors.New("external_id is already used by a different order"))
	case errors.Is(err, watcherclient.ErrNoWalletAvailable):
		writeError(w, http.StatusServiceUnavailable, errors.New("all deposit wallets are busy right now -- please try again in a few minutes"))
	default:
		writeError(w, http.StatusBadGateway, err)
	}
}

type postQuoteRequest struct {
	Direction string `json:"direction"`
	AmountIn  string `json:"amount_in"`
}

type postQuoteResponse struct {
	breakdown
	Direction  string `json:"direction"`
	ValidUntil string `json:"valid_until"`
}

// postQuote prices a conversion without creating anything -- what the
// customer sees before committing.
func (s *Server) postQuote(w http.ResponseWriter, r *http.Request) {
	var req postQuoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	q, err := s.Driver.Quote(r.Context(), driver.QuoteRequest{Direction: relay.Direction(req.Direction), AmountIn: req.AmountIn})
	if err != nil {
		writeDriverError(w, err)
		return
	}
	b, err := breakdownFrom(q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, postQuoteResponse{breakdown: b, Direction: req.Direction, ValidUntil: q.ValidUntil.Format(time.RFC3339)})
}

type postRelayLegRequest struct {
	ExternalID string `json:"external_id"`
	// CustomerLabel is how the customer identifies themselves (a name or
	// email). customer_id is accepted as an older name for the same field.
	CustomerLabel      string `json:"customer_label"`
	CustomerID         string `json:"customer_id"`
	Direction          string `json:"direction"` // "TRC20_TO_BEP20" | "BEP20_TO_TRC20"
	DestinationAddress string `json:"destination_address"`
	AmountIn           string `json:"amount_in"`
}

type postRelayLegResponse struct {
	breakdown
	ExternalID      string `json:"external_id"`
	OrderID         int64  `json:"order_id"`
	DepositAddress  string `json:"deposit_address"`
	DepositDeadline string `json:"deposit_deadline"`
	// Older names, kept for existing clients.
	AmountOutQuoted string `json:"amount_out_quoted"`
	FeeUnits        string `json:"fee_units"`
	QuoteExpiresAt  string `json:"quote_expires_at"`
}

func (s *Server) postRelayLeg(w http.ResponseWriter, r *http.Request) {
	var req postRelayLegRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	label := req.CustomerLabel
	if label == "" {
		label = req.CustomerID
	}
	if req.ExternalID == "" || label == "" || req.DestinationAddress == "" || req.AmountIn == "" {
		writeError(w, http.StatusBadRequest, errors.New("external_id, customer_label, destination_address, and amount_in are all required"))
		return
	}
	result, err := s.Driver.CreateRelayLeg(r.Context(), driver.CreateRelayLegRequest{
		ExternalID: req.ExternalID, CustomerLabel: label, Direction: relay.Direction(req.Direction),
		DestinationAddress: req.DestinationAddress, AmountIn: req.AmountIn,
	})
	if err != nil {
		writeDriverError(w, err)
		return
	}
	b, err := breakdownFrom(result.Quote)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	deadline := result.DepositDeadline.Format(time.RFC3339)
	respondJSON(w, http.StatusCreated, postRelayLegResponse{
		breakdown: b, ExternalID: result.ExternalID, OrderID: result.OrderID, DepositAddress: result.DepositAddress,
		DepositDeadline: deadline, AmountOutQuoted: b.AmountOut, FeeUnits: b.OurFee, QuoteExpiresAt: deadline,
	})
}

type getRelayLegResponse struct {
	ExternalID         string  `json:"external_id"`
	OrderID            int64   `json:"order_id"`
	OrderState         string  `json:"order_state"`
	RelayStatus        string  `json:"relay_status"`
	Direction          string  `json:"direction"`
	DepositAddress     string  `json:"deposit_address"`
	DestinationAddress string  `json:"destination_address"`
	AmountIn           string  `json:"amount_in"`
	AmountOutExpected  string  `json:"amount_out_expected"`
	AmountOutActual    *string `json:"amount_out_actual,omitempty"`
	ReceivedAmount     *string `json:"received_amount,omitempty"`
	OurFee             *string `json:"our_fee,omitempty"`
	ForwardAmount      *string `json:"forward_amount,omitempty"`
	VendorFee          *string `json:"vendor_fee,omitempty"`
	Vendor             *string `json:"vendor,omitempty"`
	DepositDeadline    string  `json:"deposit_deadline"`
	ForwardTxID        *string `json:"forward_tx_id,omitempty"`
	RefundTxID         *string `json:"refund_tx_id,omitempty"`
	PayoutTxID         *string `json:"payout_tx_id,omitempty"`
}

func formatOptional(a *money.Amount) (*string, error) {
	if a == nil {
		return nil, nil
	}
	s, err := formatAmount(*a)
	return &s, err
}

func (s *Server) getRelayLeg(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	status, err := s.Driver.GetStatus(r.Context(), externalID)
	if err != nil {
		if errors.Is(err, relay.ErrLegNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	leg := status.Leg
	resp := getRelayLegResponse{
		ExternalID: externalID, OrderID: status.Order.ID, OrderState: status.Order.State,
		RelayStatus: string(leg.Status), Direction: string(leg.Direction),
		DepositAddress: leg.DepositAddress, DestinationAddress: leg.DestinationAddress,
		Vendor: leg.UpstreamProviderName, DepositDeadline: status.Order.QuoteExpiresAt.Format(time.RFC3339),
		ForwardTxID: leg.ForwardTxID, RefundTxID: leg.RefundTxID, PayoutTxID: leg.PayoutTxID,
	}
	var vendorFee *money.Amount
	if leg.VendorFeeAmount != nil {
		vendorFee = &money.Amount{Asset: leg.AmountIn.Asset, Units: *leg.VendorFeeAmount}
	}
	for _, f := range []struct {
		dst **string
		a   *money.Amount
	}{
		{&resp.AmountOutActual, leg.AmountOutActual}, {&resp.ReceivedAmount, leg.ReceivedAmount},
		{&resp.OurFee, leg.ProfitAmount}, {&resp.ForwardAmount, leg.ForwardAmount}, {&resp.VendorFee, vendorFee},
	} {
		if *f.dst, err = formatOptional(f.a); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if resp.AmountIn, err = formatAmount(leg.AmountIn); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if resp.AmountOutExpected, err = formatAmount(leg.AmountOutExpected); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, resp)
}

type refundEntryLineResponse struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

type refundEntryResponse struct {
	EntryType  string                    `json:"entry_type"`
	OccurredAt string                    `json:"occurred_at"`
	Lines      []refundEntryLineResponse `json:"lines"`
}

// getRelayLegRefundEntry is GET /v1/relay-legs/{external_id}/refund-entry
// -- the one thing screening's own RELAY-aware RefundEntryBuilder needs
// from relayd to manually reject a screening hold on a RELAY-tier order
// (screening/internal/holds.go's own Reject flow, which already exists
// for every tier but has never had a real RefundEntryBuilder
// implementation to call -- see that package's own StubRefundEntryBuilder).
// 404 means externalID has no relay leg at all (not a RELAY order --
// screening's own caller falls back to the stub for those, unchanged
// behavior); 409 means a leg exists but isn't currently eligible (not
// AWAITING_DEPOSIT locally, or C1's own order isn't held) -- see
// driver.ErrLegNotEligibleForRefundEntry's own doc comment for exactly
// why both conditions matter.
//
// This endpoint only COMPUTES the entry -- it does not submit it to C1
// (screening's own Reject flow does that, as part of its own existing
// held->refunded transition call) and does not itself trigger the
// physical on-chain refund (internal/orchestrate's own
// startExternallyRefundedLegs phase notices the order reached `refunded`
// on a later tick and drives that, the same advanceRefundPendingLegs
// machinery R5's own timeout-triggered refund already uses).
func (s *Server) getRelayLegRefundEntry(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	entry, err := s.Driver.BuildRefundEntry(r.Context(), externalID)
	if err != nil {
		if errors.Is(err, relay.ErrLegNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if errors.Is(err, driver.ErrLegNotEligibleForRefundEntry) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}

	lines := make([]refundEntryLineResponse, len(entry.Lines))
	for i, l := range entry.Lines {
		lines[i] = refundEntryLineResponse{AccountCode: l.AccountCode, Asset: l.Asset, Amount: l.Amount}
	}
	respondJSON(w, http.StatusOK, refundEntryResponse{
		EntryType: entry.EntryType, OccurredAt: entry.OccurredAt.Format(time.RFC3339), Lines: lines,
	})
}

type relayLegSummary struct {
	ExternalID           string  `json:"external_id"`
	OrderID              int64   `json:"order_id"`
	Direction            string  `json:"direction"`
	Status               string  `json:"status"`
	CustomerID           string  `json:"customer_id"`
	DestinationAddress   string  `json:"destination_address"`
	AmountIn             string  `json:"amount_in"`
	AmountOutExpected    string  `json:"amount_out_expected"`
	AmountOutActual      *string `json:"amount_out_actual,omitempty"`
	UpstreamProviderName *string `json:"upstream_provider_name,omitempty"`
	UpstreamOrderID      *string `json:"upstream_order_id,omitempty"`
	ForwardTxID          *string `json:"forward_tx_id,omitempty"`
	RefundTxID           *string `json:"refund_tx_id,omitempty"`
	CreatedAt            string  `json:"created_at"`
	UpdatedAt            string  `json:"updated_at"`

	// What actually happened, for full transaction tracking.
	CustomerLabel   *string `json:"customer_label,omitempty"`
	DepositAddress  string  `json:"deposit_address"`
	SenderAddress   *string `json:"sender_address,omitempty"`
	ProfitBPS       *int64  `json:"profit_bps,omitempty"`
	ReceivedAmount  *string `json:"received_amount,omitempty"`
	ProfitAmount    *string `json:"profit_amount,omitempty"`
	ForwardAmount   *string `json:"forward_amount,omitempty"`
	VendorFeeAmount *string `json:"vendor_fee_amount,omitempty"`
	LeaseReleasedAt *string `json:"lease_released_at,omitempty"`
	PayoutTxID      *string `json:"payout_tx_id,omitempty"`
}

// addTracking fills in what actually happened on leg.
func addTracking(summary *relayLegSummary, leg relay.Leg) {
	summary.CustomerLabel, summary.DepositAddress, summary.SenderAddress = leg.CustomerLabel, leg.DepositAddress, leg.SenderAddress
	summary.ProfitBPS, summary.PayoutTxID = leg.ProfitBPS, leg.PayoutTxID
	for _, f := range []struct {
		src *money.Amount
		dst **string
	}{{leg.ReceivedAmount, &summary.ReceivedAmount}, {leg.ProfitAmount, &summary.ProfitAmount}, {leg.ForwardAmount, &summary.ForwardAmount}} {
		if f.src != nil {
			if formatted, err := formatAmount(*f.src); err == nil {
				*f.dst = &formatted
			}
		}
	}
	if leg.VendorFeeAmount != nil {
		if formatted, err := formatAmount(money.Amount{Asset: leg.AmountIn.Asset, Units: *leg.VendorFeeAmount}); err == nil {
			summary.VendorFeeAmount = &formatted
		}
	}
	if leg.LeaseReleasedAt != nil {
		at := leg.LeaseReleasedAt.UTC().Format(time.RFC3339)
		summary.LeaseReleasedAt = &at
	}
}

type listRelayLegsResponse struct {
	Legs []relayLegSummary `json:"legs"`
}

// getRelayLegs is GET /v1/relay-legs?status= -- a general listing view
// (every leg if status is omitted), this service's own local data only,
// the same "local rows only, no per-row cross-service fetch" scoping
// dispatcher's own GET /v1/slots and screening's own GET /v1/holds
// already take. Backs the ops console's own relay-leg visibility page.
func (s *Server) getRelayLegs(w http.ResponseWriter, r *http.Request) {
	var status *relay.Status
	if raw := r.URL.Query().Get("status"); raw != "" {
		st := relay.Status(raw)
		status = &st
	}

	legs, err := s.Driver.ListLegs(r.Context(), status)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	resp := listRelayLegsResponse{Legs: make([]relayLegSummary, 0, len(legs))}
	for _, leg := range legs {
		amountIn, err := formatAmount(leg.AmountIn)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		amountOutExpected, err := formatAmount(leg.AmountOutExpected)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		summary := relayLegSummary{
			ExternalID: leg.ExternalID, OrderID: leg.OrderID, Direction: string(leg.Direction), Status: string(leg.Status),
			CustomerID: leg.CustomerID, DestinationAddress: leg.DestinationAddress,
			AmountIn: amountIn, AmountOutExpected: amountOutExpected,
			UpstreamProviderName: leg.UpstreamProviderName, UpstreamOrderID: leg.UpstreamOrderID,
			ForwardTxID: leg.ForwardTxID, RefundTxID: leg.RefundTxID,
			CreatedAt: leg.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), UpdatedAt: leg.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if leg.AmountOutActual != nil {
			actual, err := formatAmount(*leg.AmountOutActual)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			summary.AmountOutActual = &actual
		}
		addTracking(&summary, leg)
		resp.Legs = append(resp.Legs, summary)
	}

	respondJSON(w, http.StatusOK, resp)
}
