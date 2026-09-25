package opclient

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// relayd's administrator API (relayd/internal/httpapi/admin.go). The
// console's relayd token must be one of relayd's RELAYD_ADMIN_TOKENS.

// Pricing is what we keep from each deposit.
type Pricing struct {
	ProfitBPS   int64  `json:"profit_bps"`
	MinProfit   string `json:"min_profit"`
	MinAmountIn string `json:"min_amount_in"`
	MaxAmountIn string `json:"max_amount_in"`
}

// GetPricing calls GET /v1/admin/pricing.
func (c *RelaydClient) GetPricing(ctx context.Context) (Pricing, error) {
	var out Pricing
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.baseURL+"/v1/admin/pricing", nil, &out)
	return out, err
}

// PutPricing calls PUT /v1/admin/pricing.
func (c *RelaydClient) PutPricing(ctx context.Context, p Pricing) error {
	return do(ctx, c.http, "relayd", c.token, http.MethodPut, c.baseURL+"/v1/admin/pricing", p, nil)
}

// Vendor is one conversion or energy vendor and its health.
type Vendor struct {
	Service             string     `json:"service"`
	Name                string     `json:"name"`
	Enabled             bool       `json:"enabled"`
	Priority            int        `json:"priority"`
	Available           bool       `json:"available"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	UnavailableUntil    *time.Time `json:"unavailable_until,omitempty"`
	LastError           *string    `json:"last_error,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
}

// Vendors is every vendor plus each service's selection strategy.
type Vendors struct {
	Vendors    []Vendor          `json:"vendors"`
	Strategies map[string]string `json:"strategies"`
}

// GetVendors calls GET /v1/admin/vendors.
func (c *RelaydClient) GetVendors(ctx context.Context) (Vendors, error) {
	var out Vendors
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.baseURL+"/v1/admin/vendors", nil, &out)
	return out, err
}

// PatchVendor enables/disables a vendor or sets its priority.
func (c *RelaydClient) PatchVendor(ctx context.Context, service, name string, enabled *bool, priority *int) error {
	body := map[string]any{}
	if enabled != nil {
		body["enabled"] = *enabled
	}
	if priority != nil {
		body["priority"] = *priority
	}
	u := c.baseURL + "/v1/admin/vendors/" + url.PathEscape(service) + "/" + url.PathEscape(name)
	return do(ctx, c.http, "relayd", c.token, http.MethodPatch, u, body, nil)
}

// PutVendorStrategy sets how a service's vendor is chosen.
func (c *RelaydClient) PutVendorStrategy(ctx context.Context, service, strategy string) error {
	u := c.baseURL + "/v1/admin/vendors/" + url.PathEscape(service) + "/strategy"
	return do(ctx, c.http, "relayd", c.token, http.MethodPut, u, map[string]string{"strategy": strategy}, nil)
}

// SweepSettings is how profit is swept to the treasury.
type SweepSettings struct {
	Enabled         bool              `json:"enabled"`
	IntervalMinutes int64             `json:"interval_minutes"`
	MinAmount       map[string]string `json:"min_amount"`
}

// GetSweepSettings calls GET /v1/admin/sweeps/settings.
func (c *RelaydClient) GetSweepSettings(ctx context.Context) (SweepSettings, error) {
	var out SweepSettings
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.baseURL+"/v1/admin/sweeps/settings", nil, &out)
	return out, err
}

// PutSweepSettings calls PUT /v1/admin/sweeps/settings.
func (c *RelaydClient) PutSweepSettings(ctx context.Context, s SweepSettings) error {
	return do(ctx, c.http, "relayd", c.token, http.MethodPut, c.baseURL+"/v1/admin/sweeps/settings", s, nil)
}

// RunSweep asks relayd to look for wallets to sweep on its next tick.
func (c *RelaydClient) RunSweep(ctx context.Context) error {
	return do(ctx, c.http, "relayd", c.token, http.MethodPost, c.baseURL+"/v1/admin/sweeps/run", map[string]string{}, nil)
}

// ProfitWallet is the profit one deposit wallet holds, per the books.
type ProfitWallet struct {
	Chain        string     `json:"chain"`
	Address      string     `json:"address"`
	Legs         int        `json:"legs"`
	Asset        string     `json:"asset"`
	Earned       string     `json:"earned"`
	Swept        string     `json:"swept"`
	Unswept      string     `json:"unswept"`
	Busy         bool       `json:"busy"`
	SweepPending bool       `json:"sweep_pending"`
	LastFailedAt *time.Time `json:"last_failed_sweep_at,omitempty"`
}

// ProfitWallets is every deposit wallet's profit and the unswept totals.
type ProfitWallets struct {
	Wallets       []ProfitWallet    `json:"wallets"`
	UnsweptTotals map[string]string `json:"unswept_totals"`
}

// GetProfitWallets calls GET /v1/admin/wallets.
func (c *RelaydClient) GetProfitWallets(ctx context.Context) (ProfitWallets, error) {
	var out ProfitWallets
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.baseURL+"/v1/admin/wallets", nil, &out)
	return out, err
}

// JobCosts is what one order or sweep sent, topped up, and rented.
type JobCosts struct {
	Transfers []struct {
		Purpose   string    `json:"purpose"`
		Chain     string    `json:"chain"`
		From      string    `json:"from"`
		To        string    `json:"to"`
		Asset     string    `json:"asset"`
		Amount    string    `json:"amount"`
		Status    string    `json:"status"`
		TxHash    *string   `json:"tx_hash,omitempty"`
		Failure   *string   `json:"failure_reason,omitempty"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"transfers"`
	Rentals []struct {
		Resource string  `json:"resource"`
		Units    int64   `json:"units"`
		Provider *string `json:"provider,omitempty"`
		Status   string  `json:"status"`
		CostTRX  *string `json:"cost_trx,omitempty"`
		Error    *string `json:"error,omitempty"`
	} `json:"rentals"`
	GasBNB     string `json:"gas_topups_bnb"`
	TopUpTRX   string `json:"trx_topups"`
	RentalsTRX string `json:"energy_rentals_trx"`
}

// Sweep is one transfer of profit to the treasury.
type Sweep struct {
	ID          int64      `json:"id"`
	Chain       string     `json:"chain"`
	FromAddress string     `json:"from_address"`
	ToAddress   string     `json:"to_address"`
	Asset       string     `json:"asset"`
	Amount      string     `json:"amount"`
	Status      string     `json:"status"`
	TxHash      *string    `json:"tx_hash,omitempty"`
	Error       *string    `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Costs       *JobCosts  `json:"costs,omitempty"`
}

// ListSweeps calls GET /v1/admin/sweeps.
func (c *RelaydClient) ListSweeps(ctx context.Context) ([]Sweep, error) {
	var out struct {
		Sweeps []Sweep `json:"sweeps"`
	}
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.baseURL+"/v1/admin/sweeps?limit=50", nil, &out)
	return out.Sweeps, err
}

// LegDetail is one order's full record.
type LegDetail struct {
	Leg   RelayLeg `json:"leg"`
	Costs JobCosts `json:"costs"`
}

// GetLeg calls GET /v1/admin/legs/{external_id}.
func (c *RelaydClient) GetLeg(ctx context.Context, externalID string) (LegDetail, error) {
	var out LegDetail
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.baseURL+"/v1/admin/legs/"+url.PathEscape(externalID), nil, &out)
	return out, err
}

// PoolWallet is one deposit wallet in a watcher's pool.
type PoolWallet struct {
	Address         string     `json:"address"`
	DerivationIndex uint32     `json:"derivation_index"`
	Network         string     `json:"network"`
	Status          string     `json:"status"`
	Available       bool       `json:"available"`
	AvailableAfter  time.Time  `json:"available_after"`
	LastLeasedAt    *time.Time `json:"last_leased_at,omitempty"`
	Lease           *struct {
		ExternalID     string    `json:"external_id"`
		QuoteExpiresAt time.Time `json:"quote_expires_at"`
	} `json:"lease,omitempty"`
}

// PoolSettings bounds a pool and its wallets' cooldowns.
type PoolSettings struct {
	MaxWallets          int    `json:"max_wallets"`
	CooldownAfterUse    string `json:"cooldown_after_use"`
	CooldownAfterExpiry string `json:"cooldown_after_expiry"`
}

func (c *RelaydClient) poolURL(chain, path string) string {
	return c.baseURL + "/v1/admin/pool/" + url.PathEscape(chain) + path
}

// ListPool lists chain's ("bsc" or "tron") deposit wallets.
func (c *RelaydClient) ListPool(ctx context.Context, chain string) ([]PoolWallet, error) {
	var out struct {
		Wallets []PoolWallet `json:"wallets"`
	}
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.poolURL(chain, "/wallets"), nil, &out)
	return out.Wallets, err
}

// ProvisionWallet adds a wallet to chain's pool.
func (c *RelaydClient) ProvisionWallet(ctx context.Context, chain string) error {
	return do(ctx, c.http, "relayd", c.token, http.MethodPost, c.poolURL(chain, "/wallets"), map[string]string{}, nil)
}

// SetPoolWallet enables or disables one of chain's wallets.
func (c *RelaydClient) SetPoolWallet(ctx context.Context, chain, address string, enable bool) error {
	action := "/disable"
	if enable {
		action = "/enable"
	}
	return do(ctx, c.http, "relayd", c.token, http.MethodPost, c.poolURL(chain, "/wallets/"+url.PathEscape(address)+action), map[string]string{}, nil)
}

// GetPoolSettings reads chain's pool settings.
func (c *RelaydClient) GetPoolSettings(ctx context.Context, chain string) (PoolSettings, error) {
	var out PoolSettings
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.poolURL(chain, "/settings"), nil, &out)
	return out, err
}

// PutPoolSettings stores chain's pool settings.
func (c *RelaydClient) PutPoolSettings(ctx context.Context, chain string, s PoolSettings) error {
	return do(ctx, c.http, "relayd", c.token, http.MethodPut, c.poolURL(chain, "/settings"), s, nil)
}
