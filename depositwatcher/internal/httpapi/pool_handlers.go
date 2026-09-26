package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/deposits"
)

// The deposit-wallet pool, for operators (and relayd's sweeper): which
// wallets exist, which are active, which are leased right now, and the
// pool's own limits.

type leaseResponse struct {
	OrderID        int64     `json:"order_id"`
	ExternalID     string    `json:"external_id"`
	AssignedAt     time.Time `json:"assigned_at"`
	QuoteExpiresAt time.Time `json:"quote_expires_at"`
}

type walletResponse struct {
	Address         string         `json:"address"`
	DerivationIndex uint32         `json:"derivation_index"`
	Network         string         `json:"network"`
	Status          string         `json:"status"`
	Available       bool           `json:"available"` // can be leased right now
	AvailableAfter  time.Time      `json:"available_after"`
	LastLeasedAt    *time.Time     `json:"last_leased_at,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	Lease           *leaseResponse `json:"lease,omitempty"`
}

func toWalletResponse(w addresses.PoolWallet, now time.Time) walletResponse {
	out := walletResponse{
		Address: string(w.Address), DerivationIndex: w.DerivationIndex, Network: "BSC", Status: w.Status,
		AvailableAfter: w.AvailableAfter, LastLeasedAt: w.LastLeasedAt, CreatedAt: w.CreatedAt,
	}
	out.Available = w.Status == "ACTIVE" && w.Lease == nil && !w.AvailableAfter.After(now)
	if w.Lease != nil {
		out.Lease = &leaseResponse{OrderID: w.Lease.OrderID, ExternalID: w.Lease.ExternalID,
			AssignedAt: w.Lease.AssignedAt, QuoteExpiresAt: w.Lease.QuoteExpiresAt}
	}
	return out
}

func (s *Server) getWallets(w http.ResponseWriter, r *http.Request) {
	pool, err := addresses.ListPool(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	now := time.Now()
	out := make([]walletResponse, 0, len(pool))
	for _, pw := range pool {
		out = append(out, toWalletResponse(pw, now))
	}
	respondJSON(w, http.StatusOK, map[string]any{"wallets": out})
}

// postWallet adds one wallet to the pool now, instead of waiting for an
// order to need it -- within the pool's limit.
func (s *Server) postWallet(w http.ResponseWriter, r *http.Request) {
	pw, err := addresses.ProvisionWallet(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, toWalletResponse(pw, time.Now()))
}

func (s *Server) postWalletStatus(active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := addresses.SetWalletStatus(r.Context(), s.Pool, chi.URLParam(r, "address"), active); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type poolSettingsBody struct {
	MaxWallets          int    `json:"max_wallets"`
	CooldownAfterUse    string `json:"cooldown_after_use"`    // Go duration, e.g. "30m"
	CooldownAfterExpiry string `json:"cooldown_after_expiry"` // e.g. "6h"
}

func (s *Server) getPoolSettings(w http.ResponseWriter, r *http.Request) {
	ps, err := addresses.GetPoolSettings(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, poolSettingsBody{MaxWallets: ps.MaxWallets,
		CooldownAfterUse: ps.CooldownAfterUse.String(), CooldownAfterExpiry: ps.CooldownAfterExpiry.String()})
}

func (s *Server) putPoolSettings(w http.ResponseWriter, r *http.Request) {
	var body poolSettingsBody
	if !decodeJSON(w, r, &body) {
		return
	}
	use, err1 := time.ParseDuration(body.CooldownAfterUse)
	expiry, err2 := time.ParseDuration(body.CooldownAfterExpiry)
	if err1 != nil || err2 != nil || body.MaxWallets < 0 || use < 0 || expiry < 0 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"max_wallets must be >= 0 and cooldowns must be durations like \"30m\" or \"6h\""))
		return
	}
	if err := addresses.PutPoolSettings(r.Context(), s.Pool, addresses.PoolSettings{
		MaxWallets: body.MaxWallets, CooldownAfterUse: use, CooldownAfterExpiry: expiry,
	}); err != nil {
		writeErr(w, err)
		return
	}
	s.getPoolSettings(w, r)
}

type depositResponse struct {
	TxHash        string  `json:"tx_hash"`
	LogIndex      int     `json:"log_index"`
	Address       string  `json:"address"`
	Amount        string  `json:"amount"`
	SenderAddress string  `json:"sender_address"`
	Height        int64   `json:"height"`
	Status        string  `json:"status"`
	Note          *string `json:"note,omitempty"`
}

func (s *Server) getOrderDeposits(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	list, err := deposits.ForOrder(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]depositResponse, 0, len(list))
	for _, d := range list {
		out = append(out, depositResponse{TxHash: d.TxHash, LogIndex: d.LogIndex, Address: d.Address,
			Amount: d.Amount.Format(), SenderAddress: d.SenderAddress, Height: d.Height, Status: d.Status, Note: d.Note})
	}
	respondJSON(w, http.StatusOK, map[string]any{"deposits": out})
}
