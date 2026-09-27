package opclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Stats is relayd's overview numbers.
type Stats struct {
	GeneratedAt    time.Time      `json:"generated_at"`
	OrdersByStatus map[string]int `json:"orders_by_status"`
	Periods        []StatsPeriod  `json:"periods"`
	Attention      []Attention    `json:"attention"`
}

// StatsPeriod sums one time window.
type StatsPeriod struct {
	Name      string `json:"name"`
	Orders    int    `json:"orders"`
	Settled   int    `json:"settled"`
	Volume    string `json:"volume_usdt"`
	Profit    string `json:"profit_usdt"`
	GasBNB    string `json:"gas_bnb"`
	TRX       string `json:"trx"`
	EnergyTRX string `json:"energy_trx"`
}

// Attention is an order an operator should look at.
type Attention struct {
	ExternalID string    `json:"external_id"`
	Direction  string    `json:"direction"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	Since      time.Time `json:"since"`
}

// GetStats calls GET /v1/admin/stats.
func (c *RelaydClient) GetStats(ctx context.Context) (Stats, error) {
	var out Stats
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.baseURL+"/v1/admin/stats", nil, &out)
	return out, err
}

// GetEnergy calls GET /v1/admin/energy: each energy vendor's prepaid
// account (balance_trx, order_cost_trx, orders_covered, top_up_address).
func (c *RelaydClient) GetEnergy(ctx context.Context) ([]map[string]string, error) {
	var out struct {
		Energy []map[string]string `json:"energy"`
	}
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, c.baseURL+"/v1/admin/energy", nil, &out)
	return out.Energy, err
}

// Quote is what a customer would be offered.
type Quote struct {
	AmountIn  string `json:"amount_in"`
	OurFee    string `json:"our_fee"`
	VendorFee string `json:"vendor_fee"`
	AmountOut string `json:"amount_out"`
	Vendor    string `json:"vendor"`
	Direction string `json:"direction"`
}

// Quote calls POST /v1/quotes, the storefront's own pricing call.
func (c *RelaydClient) Quote(ctx context.Context, direction, amountIn string) (Quote, error) {
	var out Quote
	err := do(ctx, c.http, "relayd", c.token, http.MethodPost, c.baseURL+"/v1/quotes",
		map[string]string{"direction": direction, "amount_in": amountIn}, &out)
	return out, err
}

// Deposit is one deposit a watcher recorded for an order.
type Deposit struct {
	TxHash        string `json:"tx_hash"`
	Amount        string `json:"amount"`
	SenderAddress string `json:"sender_address"`
	Status        string `json:"status"`
}

// OrphanedDeposit is a payment that arrived at a deposit wallet no order
// was waiting on -- typically a customer paying after their order expired.
type OrphanedDeposit struct {
	ID                    int64      `json:"id"`
	OrderID               int64      `json:"order_id"`
	ExternalID            string     `json:"external_id"`
	TxHash                string     `json:"tx_hash"` // BSC
	TxID                  string     `json:"tx_id"`   // TRON
	Amount                string     `json:"amount"`
	DetectedAt            time.Time  `json:"detected_at"`
	OrderStateAtDetection string     `json:"order_state_at_detection"`
	Resolution            *string    `json:"resolution,omitempty"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy            *string    `json:"resolved_by,omitempty"`
}

// Tx is the deposit's transaction, whichever chain reported it.
func (d OrphanedDeposit) Tx() string {
	if d.TxHash != "" {
		return d.TxHash
	}
	return d.TxID
}

func listOrphaned(ctx context.Context, h *http.Client, service, token, base string, resolved bool) ([]OrphanedDeposit, error) {
	var out struct {
		Deposits []OrphanedDeposit `json:"deposits"`
	}
	err := do(ctx, h, service, token, http.MethodGet, base+"/v1/orphaned-deposits?resolved="+strconv.FormatBool(resolved), nil, &out)
	return out.Deposits, err
}

func resolveOrphaned(ctx context.Context, h *http.Client, service, token, base string, id int64, resolution string) error {
	return do(ctx, h, service, token, http.MethodPost, base+"/v1/orphaned-deposits/"+strconv.FormatInt(id, 10)+"/resolve",
		map[string]string{"resolution": resolution}, nil)
}

func orderDeposits(ctx context.Context, h *http.Client, service, token, base string, orderID int64) ([]Deposit, error) {
	var out struct {
		Deposits []Deposit `json:"deposits"`
	}
	err := do(ctx, h, service, token, http.MethodGet, base+"/v1/addresses/"+url.PathEscape(strconv.FormatInt(orderID, 10))+"/deposits", nil, &out)
	return out.Deposits, err
}

// ListOrphanedDeposits lists late or unmatched BSC payments.
func (c *WatcherClient) ListOrphanedDeposits(ctx context.Context, resolved bool) ([]OrphanedDeposit, error) {
	return listOrphaned(ctx, c.http, "watcher", c.token, c.baseURL, resolved)
}

// ResolveOrphanedDeposit records how a BSC orphaned deposit was handled.
func (c *WatcherClient) ResolveOrphanedDeposit(ctx context.Context, id int64, resolution string) error {
	return resolveOrphaned(ctx, c.http, "watcher", c.token, c.baseURL, id, resolution)
}

// OrderDeposits lists the BSC deposits recorded for an order.
func (c *WatcherClient) OrderDeposits(ctx context.Context, orderID int64) ([]Deposit, error) {
	return orderDeposits(ctx, c.http, "watcher", c.token, c.baseURL, orderID)
}

// ListOrphanedDeposits lists late or unmatched TRON payments.
func (c *TronwatcherClient) ListOrphanedDeposits(ctx context.Context, resolved bool) ([]OrphanedDeposit, error) {
	return listOrphaned(ctx, c.http, "tronwatcher", c.token, c.baseURL, resolved)
}

// ResolveOrphanedDeposit records how a TRON orphaned deposit was handled.
func (c *TronwatcherClient) ResolveOrphanedDeposit(ctx context.Context, id int64, resolution string) error {
	return resolveOrphaned(ctx, c.http, "tronwatcher", c.token, c.baseURL, id, resolution)
}

// OrderDeposits lists the TRON deposits recorded for an order.
func (c *TronwatcherClient) OrderDeposits(ctx context.Context, orderID int64) ([]Deposit, error) {
	return orderDeposits(ctx, c.http, "tronwatcher", c.token, c.baseURL, orderID)
}
