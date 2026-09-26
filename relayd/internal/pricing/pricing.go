// Package pricing is how much relayd keeps from each deposit. An
// administrator sets it at runtime (settings table, key "pricing"); each
// leg snapshots the values it was quoted under.
//
// Every figure is in USDT minor units (6 decimals). USDT_BEP20 and
// USDT_TRC20 share that scale in money.Amount, so one figure serves both
// networks.
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"relayd/internal/db"
	"relayd/internal/money"
)

// Config is the admin-managed pricing.
type Config struct {
	// ProfitBPS is our profit in basis points of what the customer
	// actually deposits (25 = 0.25%).
	ProfitBPS int64
	// MinProfit is a floor on the profit per order, in minor units.
	MinProfit int64
	// MinAmountIn / MaxAmountIn bound the deposit size we accept. A
	// deposit below MinAmountIn is refunded rather than forwarded.
	MinAmountIn int64
	MaxAmountIn int64
	// Directions overrides the profit rule for one direction
	// ("TRC20_TO_BEP20" or "BEP20_TO_TRC20"). Sending from a TRON deposit
	// wallet rents energy (around 1 USD); a BSC one costs cents of gas --
	// one margin rarely suits both.
	Directions map[string]Margin
}

// Margin is one direction's profit rule.
type Margin struct {
	ProfitBPS int64
	MinProfit int64
}

// directions are the keys Directions accepts.
var directions = map[string]bool{"TRC20_TO_BEP20": true, "BEP20_TO_TRC20": true}

// For is c with direction's own profit rule, when it has one.
func (c Config) For(direction string) Config {
	if m, ok := c.Directions[direction]; ok {
		c.ProfitBPS, c.MinProfit = m.ProfitBPS, m.MinProfit
	}
	return c
}

// ErrInvalid means a Config fails validation.
var ErrInvalid = errors.New("pricing: invalid config")

// Validate rejects a config that could price an order at a loss to the
// customer's deposit or leave nothing to forward.
func (c Config) Validate() error {
	switch {
	case c.ProfitBPS < 0 || c.ProfitBPS >= 10_000:
		return fmt.Errorf("%w: profit_bps must be 0..9999, got %d", ErrInvalid, c.ProfitBPS)
	case c.MinProfit < 0:
		return fmt.Errorf("%w: min_profit must not be negative", ErrInvalid)
	case c.MinAmountIn <= 0:
		return fmt.Errorf("%w: min_amount_in must be positive", ErrInvalid)
	case c.MaxAmountIn < c.MinAmountIn:
		return fmt.Errorf("%w: max_amount_in must be at least min_amount_in", ErrInvalid)
	case c.MinProfit >= c.MinAmountIn:
		return fmt.Errorf("%w: min_profit must be below min_amount_in, or the smallest order forwards nothing", ErrInvalid)
	}
	for direction, m := range c.Directions {
		switch {
		case !directions[direction]:
			return fmt.Errorf("%w: unknown direction %q (use TRC20_TO_BEP20 or BEP20_TO_TRC20)", ErrInvalid, direction)
		case m.ProfitBPS < 0 || m.ProfitBPS >= 10_000:
			return fmt.Errorf("%w: %s profit_bps must be 0..9999, got %d", ErrInvalid, direction, m.ProfitBPS)
		case m.MinProfit < 0:
			return fmt.Errorf("%w: %s min_profit must not be negative", ErrInvalid, direction)
		case m.MinProfit >= c.MinAmountIn:
			return fmt.Errorf("%w: %s min_profit must be below min_amount_in, or the smallest order forwards nothing", ErrInvalid, direction)
		}
	}
	return nil
}

// Profit is what we keep from received minor units: ProfitBPS of it,
// rounded down, but never less than MinProfit.
func (c Config) Profit(received int64) int64 {
	p := received / 10_000 * c.ProfitBPS
	p += received % 10_000 * c.ProfitBPS / 10_000
	if p < c.MinProfit {
		p = c.MinProfit
	}
	return p
}

// Split divides a received amount into our profit and what is forwarded
// to the vendor. ok is false when nothing would be left to forward.
func (c Config) Split(received money.Amount) (profit, forward money.Amount, ok bool) {
	p := c.Profit(received.Units)
	if p >= received.Units {
		return money.Amount{}, money.Amount{}, false
	}
	return money.Amount{Asset: received.Asset, Units: p}, money.Amount{Asset: received.Asset, Units: received.Units - p}, true
}

// wire is the settings row's JSON shape. Amounts are decimal USDT
// strings so an admin reads "0.50", not 500000.
type wire struct {
	ProfitBPS   int64                 `json:"profit_bps"`
	MinProfit   string                `json:"min_profit"`
	MinAmountIn string                `json:"min_amount_in"`
	MaxAmountIn string                `json:"max_amount_in"`
	Directions  map[string]wireMargin `json:"directions,omitempty"`
}

type wireMargin struct {
	ProfitBPS int64  `json:"profit_bps"`
	MinProfit string `json:"min_profit"`
}

func parseUSDT(field, s string) (int64, error) {
	a, err := money.ParseDecimal(s, money.USDT_TRC20)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrInvalid, field, err)
	}
	return a.Units, nil
}

func formatUSDT(units int64) string {
	s, err := money.Format(money.Amount{Asset: money.USDT_TRC20, Units: units})
	if err != nil {
		return fmt.Sprint(units)
	}
	return s
}

// MarshalJSON renders c in the settings row's shape.
func (c Config) MarshalJSON() ([]byte, error) {
	w := wire{
		ProfitBPS: c.ProfitBPS, MinProfit: formatUSDT(c.MinProfit),
		MinAmountIn: formatUSDT(c.MinAmountIn), MaxAmountIn: formatUSDT(c.MaxAmountIn),
	}
	for direction, m := range c.Directions {
		if w.Directions == nil {
			w.Directions = map[string]wireMargin{}
		}
		w.Directions[direction] = wireMargin{ProfitBPS: m.ProfitBPS, MinProfit: formatUSDT(m.MinProfit)}
	}
	return json.Marshal(w)
}

// UnmarshalJSON parses the settings row's shape.
func (c *Config) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var err error
	out := Config{ProfitBPS: w.ProfitBPS}
	if out.MinProfit, err = parseUSDT("min_profit", w.MinProfit); err != nil {
		return err
	}
	if out.MinAmountIn, err = parseUSDT("min_amount_in", w.MinAmountIn); err != nil {
		return err
	}
	if out.MaxAmountIn, err = parseUSDT("max_amount_in", w.MaxAmountIn); err != nil {
		return err
	}
	for direction, m := range w.Directions {
		minProfit, err := parseUSDT(direction+" min_profit", m.MinProfit)
		if err != nil {
			return err
		}
		if out.Directions == nil {
			out.Directions = map[string]Margin{}
		}
		out.Directions[direction] = Margin{ProfitBPS: m.ProfitBPS, MinProfit: minProfit}
	}
	*c = out
	return nil
}

// Store reads and writes the pricing settings row.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// Get returns the current pricing.
func (s *Store) Get(ctx context.Context) (Config, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'pricing'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Config{}, errors.New("pricing: no pricing configured (settings row 'pricing' is missing)")
	}
	if err != nil {
		return Config{}, fmt.Errorf("pricing: reading settings: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("pricing: stored config is invalid: %w", err)
	}
	return c, nil
}

// Put validates and stores c, recording who changed it.
func (s *Store) Put(ctx context.Context, c Config, updatedBy string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at, updated_by) VALUES ('pricing', $1, now(), $2)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = now(), updated_by = excluded.updated_by
	`, raw, updatedBy)
	if err != nil {
		return fmt.Errorf("pricing: storing settings: %w", err)
	}
	return nil
}
