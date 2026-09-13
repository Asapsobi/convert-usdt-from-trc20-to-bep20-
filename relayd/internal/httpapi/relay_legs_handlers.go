package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

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
