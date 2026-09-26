package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"relayd/internal/money"
	"relayd/internal/pricing"
	"relayd/internal/relay"
	"relayd/internal/sweeps"
	"relayd/internal/transfers"
	"relayd/internal/vendors"
)

// The administrator API: pricing, vendors, the deposit-wallet pools,
// sweeps, and full tracking of every order. Every route needs a bearer
// token from RELAYD_ADMIN_TOKENS; each change is recorded under the
// token's operator name. Meant for the ops console, server to server --
// never exposed to customers.

// WatcherForwarder passes one request through to a watcher's own API
// (*watcherclient.Client).
type WatcherForwarder interface {
	Forward(ctx context.Context, method, path string, body []byte) (status int, respBody []byte, err error)
}

// Admin is what the administrator API manages. A nil store answers 503 on
// its routes.
type Admin struct {
	// Tokens maps each accepted bearer token to its operator's name.
	Tokens    map[string]string
	Pricing   *pricing.Store
	Vendors   *vendors.Store
	Sweeps    *sweeps.Store
	Legs      *relay.Store
	Transfers *transfers.Store
	// SweepNow makes the next tick look for wallets to sweep.
	SweepNow func()
	// Watchers are the wallet pools' owners, keyed "bsc" and "tron".
	Watchers map[string]WatcherForwarder
	// Treasury describes where top-ups come from and sweeps go, for display.
	Treasury map[string]string
	// Treasuries are every treasury wallet, the primary first.
	Treasuries []TreasuryWallet
	// Prices asks every vendor for its live price on amount USDT.
	// Optional.
	Prices func(ctx context.Context, amount string) (any, error)
	// Balance reads what an address holds on chain ("BSC" or "TRON").
	// Optional.
	Balance func(ctx context.Context, chain, address string) (WalletBalance, error)
}

// TreasuryWallet is one treasury wallet's S1 slot and addresses.
type TreasuryWallet struct {
	SlotID int    `json:"slot_id"`
	BSC    string `json:"bsc"`
	TRON   string `json:"tron"`
}

// WalletBalance is what one address holds on chain, formatted.
type WalletBalance struct {
	USDT      string `json:"usdt"`
	Native    string `json:"native"`
	NativeFor string `json:"native_asset"` // BNB or TRX
	Energy    *int64 `json:"energy,omitempty"`
	Bandwidth *int64 `json:"bandwidth,omitempty"`
	Error     string `json:"error,omitempty"`
}

// balanceOf reads one address's on-chain balance, never failing the
// whole response: an unreachable node shows as the entry's error.
func (s *Server) balanceOf(ctx context.Context, chain, address string) *WalletBalance {
	if s.Admin.Balance == nil || address == "" {
		return nil
	}
	b, err := s.Admin.Balance(ctx, chain, address)
	if err != nil {
		return &WalletBalance{Error: err.Error()}
	}
	return &b
}

type operatorKey struct{}

func operatorFrom(ctx context.Context) string {
	name, _ := ctx.Value(operatorKey{}).(string)
	return name
}

func (a *Admin) operator(header string) (string, bool) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	for t, name := range a.Tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			return name, true
		}
	}
	return "", false
}

// require lets a request through only with a valid admin token.
func (a *Admin) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a == nil || len(a.Tokens) == 0 {
			writeError(w, http.StatusNotFound, errors.New("the admin API is not enabled on this relayd (RELAYD_ADMIN_TOKENS is unset)"))
			return
		}
		name, ok := a.operator(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, http.StatusUnauthorized, errors.New("a valid admin bearer token is required"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), operatorKey{}, name)))
	})
}

func (s *Server) adminRoutes(r chi.Router) {
	r.Use(s.Admin.require)
	r.Get("/overview", s.getAdminOverview)
	r.Get("/pricing", s.getPricing)
	r.Put("/pricing", s.putPricing)
	r.Get("/vendors", s.getVendors)
	r.Get("/vendors/prices", s.getVendorPrices)
	r.Patch("/vendors/{service}/{name}", s.patchVendor)
	r.Put("/vendors/{service}/strategy", s.putVendorStrategy)
	r.Get("/sweeps", s.getSweeps)
	r.Get("/sweeps/settings", s.getSweepSettings)
	r.Put("/sweeps/settings", s.putSweepSettings)
	r.Post("/sweeps/run", s.postSweepRun)
	r.Get("/wallets", s.getProfitWallets)
	r.Get("/treasury", s.getTreasury)
	r.Get("/legs", s.getRelayLegs)
	r.Get("/legs/{external_id}", s.getAdminLeg)
	r.Get("/pool/{chain}/wallets", s.forwardToWatcher("/v1/wallets"))
	r.Post("/pool/{chain}/wallets", s.forwardToWatcher("/v1/wallets"))
	r.Post("/pool/{chain}/wallets/{address}/enable", s.forwardToWatcher("/v1/wallets/{address}/enable"))
	r.Post("/pool/{chain}/wallets/{address}/disable", s.forwardToWatcher("/v1/wallets/{address}/disable"))
	r.Get("/pool/{chain}/settings", s.forwardToWatcher("/v1/pool/settings"))
	r.Put("/pool/{chain}/settings", s.forwardToWatcher("/v1/pool/settings"))
}

var errNotConfigured = errors.New("not configured on this relayd")

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return false
	}
	return true
}

func (s *Server) getAdminOverview(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"treasury": s.Admin.Treasury}
	if s.Admin.Pricing != nil {
		if cfg, err := s.Admin.Pricing.Get(r.Context()); err == nil {
			out["pricing"] = cfg
		}
	}
	if s.Admin.Sweeps != nil {
		if settings, err := s.Admin.Sweeps.Settings(r.Context()); err == nil {
			out["sweep_settings"] = settings
		}
	}
	respondJSON(w, http.StatusOK, out)
}

func (s *Server) getPricing(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Pricing == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	cfg, err := s.Admin.Pricing.Get(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, cfg)
}

func (s *Server) putPricing(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Pricing == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	var cfg pricing.Config
	if !decodeBody(w, r, &cfg) {
		return
	}
	if err := s.Admin.Pricing.Put(r.Context(), cfg, operatorFrom(r.Context())); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, pricing.ErrInvalid) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}
	respondJSON(w, http.StatusOK, cfg)
}

type vendorResponse struct {
	Service             string     `json:"service"`
	Name                string     `json:"name"`
	Enabled             bool       `json:"enabled"`
	Priority            int        `json:"priority"`
	Available           bool       `json:"available"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	UnavailableUntil    *time.Time `json:"unavailable_until,omitempty"`
	LastError           *string    `json:"last_error,omitempty"`
	LastErrorAt         *time.Time `json:"last_error_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	RevenueBPS          int        `json:"revenue_bps"`
	Notes               string     `json:"notes"`
}

var vendorServices = []vendors.Service{vendors.Conversion, vendors.Energy}

func (s *Server) getVendors(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Vendors == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	now := time.Now()
	list := []vendorResponse{}
	strategies := map[string]string{}
	for _, service := range vendorServices {
		vs, err := s.Admin.Vendors.List(r.Context(), service)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for _, v := range vs {
			list = append(list, vendorResponse{
				Service: string(v.Service), Name: v.Name, Enabled: v.Enabled, Priority: v.Priority,
				Available: v.Available(now), ConsecutiveFailures: v.ConsecutiveFailures, UnavailableUntil: v.UnavailableUntil,
				LastError: v.LastError, LastErrorAt: v.LastErrorAt, LastSuccessAt: v.LastSuccessAt,
				RevenueBPS: v.RevenueBPS, Notes: v.Notes,
			})
		}
		strategy, err := s.Admin.Vendors.Strategy(r.Context(), service)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		strategies[string(service)] = strategy
	}
	respondJSON(w, http.StatusOK, map[string]any{"vendors": list, "strategies": strategies})
}

func vendorService(raw string) (vendors.Service, error) {
	for _, s := range vendorServices {
		if string(s) == raw {
			return s, nil
		}
	}
	return "", fmt.Errorf("unknown vendor service %q", raw)
}

func (s *Server) patchVendor(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Vendors == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	service, err := vendorService(chi.URLParam(r, "service"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Enabled    *bool   `json:"enabled"`
		Priority   *int    `json:"priority"`
		RevenueBPS *int    `json:"revenue_bps"`
		Notes      *string `json:"notes"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name := chi.URLParam(r, "name")
	current, err := s.Admin.Vendors.Get(r.Context(), service, name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if req.RevenueBPS != nil || req.Notes != nil {
		revenue, notes := current.RevenueBPS, current.Notes
		if req.RevenueBPS != nil {
			revenue = *req.RevenueBPS
		}
		if req.Notes != nil {
			notes = *req.Notes
		}
		if err := s.Admin.Vendors.SetTerms(r.Context(), service, name, revenue, notes); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if req.Enabled != nil {
		if err := s.Admin.Vendors.SetEnabled(r.Context(), service, name, *req.Enabled); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if req.Priority != nil {
		if err := s.Admin.Vendors.SetPriority(r.Context(), service, name, *req.Priority); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.getVendors(w, r)
}

func (s *Server) putVendorStrategy(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Vendors == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	service, err := vendorService(chi.URLParam(r, "service"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Strategy string `json:"strategy"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.Admin.Vendors.SetStrategy(r.Context(), service, req.Strategy); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.getVendors(w, r)
}

func (s *Server) getSweepSettings(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Sweeps == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	settings, err := s.Admin.Sweeps.Settings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, settings)
}

func (s *Server) putSweepSettings(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Sweeps == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	var settings sweeps.Settings
	if !decodeBody(w, r, &settings) {
		return
	}
	if err := s.Admin.Sweeps.PutSettings(r.Context(), settings, operatorFrom(r.Context())); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, sweeps.ErrInvalid) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}
	respondJSON(w, http.StatusOK, settings)
}

func (s *Server) postSweepRun(w http.ResponseWriter, r *http.Request) {
	if s.Admin.SweepNow == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	s.Admin.SweepNow()
	respondJSON(w, http.StatusAccepted, map[string]string{"status": "a sweep check runs on the next tick (if sweeping is enabled)"})
}

func fmtUnits(asset money.Asset, units int64) string {
	formatted, err := money.Format(money.Amount{Asset: asset, Units: units})
	if err != nil {
		return strconv.FormatInt(units, 10)
	}
	return formatted
}

type sweepResponse struct {
	ID            int64      `json:"id"`
	Chain         string     `json:"chain"`
	FromAddress   string     `json:"from_address"`
	ToAddress     string     `json:"to_address"`
	Asset         string     `json:"asset"`
	Amount        string     `json:"amount"`
	Status        string     `json:"status"`
	TxHash        *string    `json:"tx_hash,omitempty"`
	LedgerEntryID *int64     `json:"ledger_entry_id,omitempty"`
	Error         *string    `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	// Costs are the gas top-ups and energy rentals this sweep needed.
	Costs *jobCosts `json:"costs,omitempty"`
}

func (s *Server) getSweeps(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Sweeps == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	list, err := s.Admin.Sweeps.List(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]sweepResponse, 0, len(list))
	for _, sw := range list {
		resp := sweepResponse{
			ID: sw.ID, Chain: string(sw.Chain), FromAddress: sw.FromAddress, ToAddress: sw.ToAddress,
			Asset: string(sw.Amount.Asset), Amount: fmtUnits(sw.Amount.Asset, sw.Amount.Units), Status: string(sw.Status),
			TxHash: sw.TxHash, LedgerEntryID: sw.LedgerEntryID, Error: sw.Error, CreatedAt: sw.CreatedAt, FinishedAt: sw.FinishedAt,
		}
		if s.Admin.Transfers != nil {
			if costs, err := s.jobCosts(r.Context(), fmt.Sprintf("sweep:%d", sw.ID)); err == nil {
				resp.Costs = &costs
			}
		}
		out = append(out, resp)
	}
	respondJSON(w, http.StatusOK, map[string]any{"sweeps": out})
}

type profitWalletResponse struct {
	Chain         string     `json:"chain"`
	Address       string     `json:"address"`
	DepositIndex  *uint32    `json:"deposit_index,omitempty"`
	IndexConflict bool       `json:"index_conflict,omitempty"`
	Legs          int        `json:"legs"`
	Asset         string     `json:"asset"`
	Earned        string     `json:"earned"`
	Swept         string     `json:"swept"`
	Unswept       string     `json:"unswept"`
	Busy          bool       `json:"busy"`
	SweepPending  bool       `json:"sweep_pending"`
	LastFailedAt  *time.Time `json:"last_failed_sweep_at,omitempty"`
	// OnChain is what the wallet holds right now (?balances=1).
	OnChain *WalletBalance `json:"on_chain,omitempty"`
}

// getProfitWallets is the profit held in each deposit wallet, per the
// books: what the wallet's settled orders earned, what was swept, and
// what is still there.
func (s *Server) getProfitWallets(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Sweeps == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	wallets, err := s.Admin.Sweeps.Wallets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]profitWalletResponse, 0, len(wallets))
	totals := map[string]int64{}
	withBalances := r.URL.Query().Get("balances") == "1"
	balanceCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	for _, wl := range wallets {
		asset := wl.Earned.Asset
		unswept := wl.Unswept()
		totals[string(asset)] += unswept.Units
		out = append(out, profitWalletResponse{
			Chain: string(wl.Chain), Address: wl.Address, DepositIndex: wl.DepositIndex, IndexConflict: wl.IndexConflict,
			Legs: wl.Legs, Asset: string(asset), Earned: fmtUnits(asset, wl.Earned.Units), Swept: fmtUnits(asset, wl.Swept.Units),
			Unswept: fmtUnits(asset, unswept.Units), Busy: wl.Busy, SweepPending: wl.Pending, LastFailedAt: wl.LastFailedAt,
		})
	}
	if withBalances {
		// Read the wallets' balances in parallel, a few at a time per chain:
		// one by one is slower than a page should be, and TronGrid turns
		// away bursts.
		var wg sync.WaitGroup
		slots := map[string]chan struct{}{"BSC": make(chan struct{}, 8), "TRON": make(chan struct{}, 2)}
		for i := range out {
			sem, ok := slots[out[i].Chain]
			if !ok {
				continue
			}
			wg.Add(1)
			go func(i int, sem chan struct{}) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				out[i].OnChain = s.balanceOf(balanceCtx, out[i].Chain, out[i].Address)
			}(i, sem)
		}
		wg.Wait()
	}
	unsweptTotals := map[string]string{}
	for asset, units := range totals {
		unsweptTotals[asset] = fmtUnits(money.Asset(asset), units)
	}
	respondJSON(w, http.StatusOK, map[string]any{"wallets": out, "unswept_totals": unsweptTotals})
}

type attemptResponse struct {
	ID            int64     `json:"id"`
	Job           string    `json:"job"`
	Purpose       string    `json:"purpose"`
	Chain         string    `json:"chain"`
	From          string    `json:"from"`
	To            string    `json:"to"`
	Asset         string    `json:"asset"`
	Amount        string    `json:"amount"`
	Status        string    `json:"status"`
	TxHash        *string   `json:"tx_hash,omitempty"`
	FailureReason *string   `json:"failure_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type rentalResponse struct {
	Job       string    `json:"job"`
	Address   string    `json:"address"`
	Resource  string    `json:"resource"`
	Units     int64     `json:"units"`
	Provider  *string   `json:"provider,omitempty"`
	Status    string    `json:"status"`
	CostTRX   *string   `json:"cost_trx,omitempty"`
	Error     *string   `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// jobCosts is everything one job (a leg or a sweep) sent and rented: its
// own transfers, the treasury top-ups that paid for them, and energy.
type jobCosts struct {
	Transfers  []attemptResponse `json:"transfers"`
	Rentals    []rentalResponse  `json:"rentals"`
	GasBNB     string            `json:"gas_topups_bnb"`
	TopUpTRX   string            `json:"trx_topups"`
	RentalsTRX string            `json:"energy_rentals_trx"`
}

// decimalString renders units with decimals places.
func decimalString(units int64, decimals int) string {
	neg := units < 0
	if neg {
		units = -units
	}
	digits := strconv.FormatInt(units, 10)
	for len(digits) <= decimals {
		digits = "0" + digits
	}
	out := digits[:len(digits)-decimals] + "." + digits[len(digits)-decimals:]
	out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
	if neg {
		out = "-" + out
	}
	return out
}

func (s *Server) jobCosts(ctx context.Context, job string) (jobCosts, error) {
	costs := jobCosts{Transfers: []attemptResponse{}, Rentals: []rentalResponse{}}
	attempts, err := s.Admin.Transfers.ListRelated(ctx, job)
	if err != nil {
		return costs, err
	}
	var gasWei, trxSun, rentSun int64
	for _, a := range attempts {
		amount := decimalString(a.Amount.Units, 6)
		switch a.Amount.Asset {
		case "BNB":
			amount = decimalString(a.Amount.Units, 18)
		case "USDT_BEP20", "USDT_TRC20":
			amount = fmtUnits(a.Amount.Asset, a.Amount.Units)
		}
		if a.Status == transfers.StatusConfirmed {
			switch a.Purpose {
			case transfers.GasTopUp:
				gasWei += a.Amount.Units
			case transfers.TRXTopUp:
				trxSun += a.Amount.Units
			}
		}
		costs.Transfers = append(costs.Transfers, attemptResponse{
			ID: a.ID, Job: a.ExternalID, Purpose: string(a.Purpose), Chain: string(a.Chain), From: a.FromAddress, To: a.ToAddress,
			Asset: string(a.Amount.Asset), Amount: amount, Status: string(a.Status), TxHash: a.TxHash,
			FailureReason: a.FailureReason, CreatedAt: a.CreatedAt,
		})
	}
	rentals, err := s.Admin.Transfers.RentalsForJob(ctx, job)
	if err != nil {
		return costs, err
	}
	for _, rt := range rentals {
		var cost *string
		if rt.CostSun != nil {
			c := decimalString(*rt.CostSun, 6)
			cost = &c
			if rt.Status == "CONFIRMED" {
				rentSun += *rt.CostSun
			}
		}
		costs.Rentals = append(costs.Rentals, rentalResponse{
			Job: rt.Job, Address: rt.Address, Resource: rt.Resource, Units: rt.Units, Provider: rt.Provider,
			Status: rt.Status, CostTRX: cost, Error: rt.Error, CreatedAt: rt.CreatedAt,
		})
	}
	costs.GasBNB, costs.TopUpTRX, costs.RentalsTRX = decimalString(gasWei, 18), decimalString(trxSun, 6), decimalString(rentSun, 6)
	return costs, nil
}

// getAdminLeg is one order's full record: the leg, what arrived and was
// kept, forwarded, refunded, and every transfer, top-up, and rental it
// needed.
func (s *Server) getAdminLeg(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Legs == nil || s.Admin.Transfers == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	leg, err := s.Admin.Legs.GetByExternalID(r.Context(), chi.URLParam(r, "external_id"))
	if errors.Is(err, relay.ErrLegNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	summary := relayLegSummary{
		ExternalID: leg.ExternalID, OrderID: leg.OrderID, Direction: string(leg.Direction), Status: string(leg.Status),
		CustomerID: leg.CustomerID, DestinationAddress: leg.DestinationAddress,
		AmountIn: fmtUnits(leg.AmountIn.Asset, leg.AmountIn.Units), AmountOutExpected: fmtUnits(leg.AmountOutExpected.Asset, leg.AmountOutExpected.Units),
		UpstreamProviderName: leg.UpstreamProviderName, UpstreamOrderID: leg.UpstreamOrderID,
		ForwardTxID: leg.ForwardTxID, RefundTxID: leg.RefundTxID,
		CreatedAt: leg.CreatedAt.Format(time.RFC3339), UpdatedAt: leg.UpdatedAt.Format(time.RFC3339),
	}
	if leg.AmountOutActual != nil {
		actual := fmtUnits(leg.AmountOutActual.Asset, leg.AmountOutActual.Units)
		summary.AmountOutActual = &actual
	}
	addTracking(&summary, leg)
	costs, err := s.jobCosts(r.Context(), leg.ExternalID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"leg": summary, "costs": costs})
}

// forwardToWatcher passes a wallet-pool request through to the watcher
// that owns chain's pool ("bsc" or "tron"). pathTemplate may use
// {address}.
func (s *Server) forwardToWatcher(pathTemplate string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chain := chi.URLParam(r, "chain")
		watcher, ok := s.Admin.Watchers[chain]
		if !ok || watcher == nil {
			writeError(w, http.StatusNotFound, fmt.Errorf("no wallet pool for chain %q (use bsc or tron)", chain))
			return
		}
		path := strings.ReplaceAll(pathTemplate, "{address}", url.PathEscape(chi.URLParam(r, "address")))
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		var body []byte
		if r.Body != nil {
			var err error
			if body, err = io.ReadAll(io.LimitReader(r.Body, 1<<20)); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		status, respBody, err := watcher.Forward(r.Context(), r.Method, path, body)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}
}

// getTreasury is each chain's treasury -- where gas and TRX top-ups come
// from -- and sweep destination, with what they hold on chain now.
func (s *Server) getTreasury(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out := map[string]any{}
	for _, chain := range []string{"BSC", "TRON"} {
		key := strings.ToLower(chain)
		treasury, sweepTo := s.Admin.Treasury[key+"_treasury"], s.Admin.Treasury[key+"_sweep_to"]
		entry := map[string]any{"treasury": treasury, "sweep_to": sweepTo, "balance": s.balanceOf(ctx, chain, treasury)}
		if sweepTo != "" && sweepTo != treasury {
			entry["sweep_to_balance"] = s.balanceOf(ctx, chain, sweepTo)
		}
		out[key] = entry
	}
	var wallets []map[string]any
	for _, t := range s.Admin.Treasuries {
		wallets = append(wallets, map[string]any{
			"slot_id": t.SlotID, "bsc": t.BSC, "tron": t.TRON,
			"bsc_balance": s.balanceOf(ctx, "BSC", t.BSC), "tron_balance": s.balanceOf(ctx, "TRON", t.TRON),
		})
	}
	out["wallets"] = wallets
	respondJSON(w, http.StatusOK, out)
}

// getVendorPrices is every vendor's live price: conversion vendors on
// ?amount= USDT (default 100) in both directions, energy vendors on one
// transfer's energy.
func (s *Server) getVendorPrices(w http.ResponseWriter, r *http.Request) {
	if s.Admin.Prices == nil {
		writeError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	amount := r.URL.Query().Get("amount")
	if amount == "" {
		amount = "100"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	prices, err := s.Admin.Prices(ctx, amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	respondJSON(w, http.StatusOK, prices)
}
