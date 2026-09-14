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
)

func formatAmount(a money.Amount) (string, error) {
	return money.Format(a)
}

type postRelayLegRequest struct {
	ExternalID         string `json:"external_id"`
	CustomerID         string `json:"customer_id"`
	Direction          string `json:"direction"` // "TRC20_TO_BEP20" | "BEP20_TO_TRC20"
	DestinationAddress string `json:"destination_address"`
	AmountIn           string `json:"amount_in"`
}

type postRelayLegResponse struct {
	ExternalID      string `json:"external_id"`
	OrderID         int64  `json:"order_id"`
	DepositAddress  string `json:"deposit_address"`
	AmountIn        string `json:"amount_in"`
	AmountOutQuoted string `json:"amount_out_quoted"`
	FeeUnits        string `json:"fee_units"`
	QuoteExpiresAt  string `json:"quote_expires_at"`
}

// postRelayLeg is POST /v1/relay-legs -- this service's own quote-then-
// create entrypoint, mirroring proofrun's identical postPayout shape.
func (s *Server) postRelayLeg(w http.ResponseWriter, r *http.Request) {
	var req postRelayLegRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ExternalID == "" || req.CustomerID == "" || req.DestinationAddress == "" || req.AmountIn == "" {
		writeError(w, http.StatusBadRequest, errors.New("external_id, customer_id, destination_address, and amount_in are all required"))
		return
	}
	direction := relay.Direction(req.Direction)
	if direction != relay.TRC20ToBEP20 && direction != relay.BEP20ToTRC20 {
		writeError(w, http.StatusBadRequest, errors.New("direction must be TRC20_TO_BEP20 or BEP20_TO_TRC20"))
		return
	}

	result, err := s.Driver.CreateRelayLeg(r.Context(), driver.CreateRelayLegRequest{
		ExternalID: req.ExternalID, CustomerID: req.CustomerID, Direction: direction,
		DestinationAddress: req.DestinationAddress, AmountIn: req.AmountIn,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	respondJSON(w, http.StatusCreated, postRelayLegResponse{
		ExternalID: result.ExternalID, OrderID: result.OrderID, DepositAddress: result.DepositAddress,
		AmountIn: result.AmountIn, AmountOutQuoted: result.AmountOutQuoted, FeeUnits: result.FeeUnits,
		QuoteExpiresAt: result.QuoteExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
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
	ForwardTxID        *string `json:"forward_tx_id,omitempty"`
}

// getRelayLeg is GET /v1/relay-legs/{external_id} -- this service's own
// status entrypoint, reconstructing the lifecycle from both C1's order
// state and this service's own finer-grained relay status.
func (s *Server) getRelayLeg(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	status, err := s.Driver.GetStatus(r.Context(), externalID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	amountIn, err := formatAmount(status.Leg.AmountIn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	amountOutExpected, err := formatAmount(status.Leg.AmountOutExpected)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	resp := getRelayLegResponse{
		ExternalID: externalID, OrderID: status.Order.ID, OrderState: status.Order.State,
		RelayStatus: string(status.Leg.Status), Direction: string(status.Leg.Direction),
		DepositAddress: status.Leg.DepositAddress, DestinationAddress: status.Leg.DestinationAddress,
		AmountIn: amountIn, AmountOutExpected: amountOutExpected,
		ForwardTxID: status.Leg.ForwardTxID,
	}
	if status.Leg.AmountOutActual != nil {
		actual, err := formatAmount(*status.Leg.AmountOutActual)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		resp.AmountOutActual = &actual
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
		resp.Legs = append(resp.Legs, summary)
	}

	respondJSON(w, http.StatusOK, resp)
}
