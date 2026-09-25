// Package sweeps records the profit held in each deposit wallet and every
// sweep of it to the treasury. The orchestrator decides when to sweep and
// sends the transfers (internal/orchestrate/sweep.go); this package is
// the durable record and the administrator's settings.
//
// What a wallet may give up is computed from the books, never from its
// on-chain balance: the profit recorded on the wallet's settled legs,
// less every sweep already taken from it. Anything else a wallet holds --
// a payment nobody ordered, a deposit still being processed -- is not
// ours to sweep.
package sweeps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"relayd/internal/db"
	"relayd/internal/money"
	"relayd/internal/transfers"
)

// Settings is the administrator's sweep configuration (settings table,
// key "sweep").
type Settings struct {
	// Enabled turns automatic sweeping on.
	Enabled bool
	// Interval is how often wallets are checked for profit to sweep.
	Interval time.Duration
	// MinAmount is the least unswept profit, per asset in minor units,
	// worth one sweep: every sweep pays gas (BSC) or energy (TRON), so
	// small amounts wait until more has accumulated.
	MinAmount map[money.Asset]int64
}

// ErrInvalid means Settings fail validation.
var ErrInvalid = errors.New("sweeps: invalid settings")

// sweptAssets are the assets a sweep can move.
var sweptAssets = []money.Asset{money.USDT_BEP20, money.USDT_TRC20}

// Validate rejects settings that would sweep dust or check constantly.
func (s Settings) Validate() error {
	if s.Interval < 5*time.Minute || s.Interval > 7*24*time.Hour {
		return fmt.Errorf("%w: interval_minutes must be between 5 and 10080, got %v", ErrInvalid, s.Interval.Minutes())
	}
	for _, asset := range sweptAssets {
		if s.MinAmount[asset] <= 0 {
			return fmt.Errorf("%w: min_amount for %s must be positive", ErrInvalid, asset)
		}
	}
	return nil
}

// Defaults is what applies when no settings row exists: disabled.
func Defaults() Settings {
	return Settings{Interval: time.Hour, MinAmount: map[money.Asset]int64{
		money.USDT_BEP20: 10_000000, money.USDT_TRC20: 50_000000,
	}}
}

// wire is the settings row's JSON shape; amounts are decimal USDT strings.
type wire struct {
	Enabled         bool              `json:"enabled"`
	IntervalMinutes int64             `json:"interval_minutes"`
	MinAmount       map[string]string `json:"min_amount"`
}

// MarshalJSON renders s in the settings row's shape.
func (s Settings) MarshalJSON() ([]byte, error) {
	w := wire{Enabled: s.Enabled, IntervalMinutes: int64(s.Interval / time.Minute), MinAmount: map[string]string{}}
	for asset, units := range s.MinAmount {
		formatted, err := money.Format(money.Amount{Asset: asset, Units: units})
		if err != nil {
			return nil, err
		}
		w.MinAmount[string(asset)] = formatted
	}
	return json.Marshal(w)
}

// UnmarshalJSON parses the settings row's shape.
func (s *Settings) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	out := Settings{Enabled: w.Enabled, Interval: time.Duration(w.IntervalMinutes) * time.Minute, MinAmount: map[money.Asset]int64{}}
	for asset, decimal := range w.MinAmount {
		a := money.Asset(asset)
		if a != money.USDT_BEP20 && a != money.USDT_TRC20 {
			return fmt.Errorf("%w: min_amount: unknown asset %q", ErrInvalid, asset)
		}
		amount, err := money.ParseDecimal(decimal, a)
		if err != nil {
			return fmt.Errorf("%w: min_amount %s: %v", ErrInvalid, asset, err)
		}
		out.MinAmount[a] = amount.Units
	}
	*s = out
	return nil
}

// Status is a sweep's lifecycle stage.
type Status string

const (
	// StatusPending: started; its transfer may be in flight.
	StatusPending Status = "PENDING"
	// StatusConfirmed: the transfer confirmed and the ledger booked it.
	StatusConfirmed Status = "CONFIRMED"
	// StatusFailed: given up with nothing moved; the amount is sweepable
	// again later.
	StatusFailed Status = "FAILED"
)

// Sweep is one transfer of profit from a deposit wallet to the treasury.
type Sweep struct {
	ID            int64
	Chain         transfers.Chain
	FromAddress   string
	DepositIndex  uint32
	ToAddress     string
	Amount        money.Amount
	Status        Status
	TxHash        *string
	LedgerEntryID *int64
	Error         *string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	FinishedAt    *time.Time
}

// Wallet is one deposit wallet's profit position, from the books.
type Wallet struct {
	Chain   transfers.Chain
	Address string
	// DepositIndex is the key that controls the wallet; nil when no leg
	// recorded it or when legs disagree (IndexConflict).
	DepositIndex  *uint32
	IndexConflict bool
	// Legs is how many orders used the wallet.
	Legs int
	// Earned is the profit recorded on the wallet's settled legs.
	Earned money.Amount
	// Swept is what sweeps took or are taking from it.
	Swept money.Amount
	// Busy means an order may still send from the wallet.
	Busy bool
	// Pending means a sweep of it is in flight.
	Pending bool
	// LastFailedAt is when its latest failed sweep gave up.
	LastFailedAt *time.Time
}

// Unswept is the profit still in the wallet.
func (w Wallet) Unswept() money.Amount {
	return money.Amount{Asset: w.Earned.Asset, Units: w.Earned.Units - w.Swept.Units}
}

// ErrPending means a sweep of that wallet is already in flight.
var ErrPending = errors.New("sweeps: a sweep of this wallet is already in flight")

// Store is the sweeps table's and the sweep settings' entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// Settings returns the administrator's sweep settings, or Defaults
// (disabled) when none are stored.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'sweep'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Defaults(), nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("sweeps: reading settings: %w", err)
	}
	var out Settings
	if err := json.Unmarshal(raw, &out); err != nil {
		return Settings{}, err
	}
	if err := out.Validate(); err != nil {
		return Settings{}, fmt.Errorf("sweeps: stored settings are invalid: %w", err)
	}
	return out, nil
}

// PutSettings validates and stores settings, recording who changed them.
func (s *Store) PutSettings(ctx context.Context, settings Settings, updatedBy string) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at, updated_by) VALUES ('sweep', $1, now(), $2)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = now(), updated_by = excluded.updated_by
	`, raw, updatedBy); err != nil {
		return fmt.Errorf("sweeps: storing settings: %w", err)
	}
	return nil
}

// Wallets returns every deposit wallet any leg used, with its profit
// position.
func (s *Store) Wallets(ctx context.Context) ([]Wallet, error) {
	rows, err := s.pool.Query(ctx, `
		WITH legs AS (
			SELECT deposit_address,
				CASE direction WHEN 'BEP20_TO_TRC20' THEN 'BSC' ELSE 'TRON' END AS chain,
				min(amount_in_asset) AS asset,
				count(*) AS legs,
				count(DISTINCT deposit_derivation_index) AS indexes,
				min(deposit_derivation_index) AS deposit_index,
				coalesce(sum(profit_amount) FILTER (WHERE status = 'SETTLED'), 0) AS earned,
				bool_or(status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'REFUND_PENDING')) AS busy
			FROM relay_legs
			WHERE deposit_address <> ''
			GROUP BY deposit_address, direction
		), swept AS (
			SELECT from_address,
				coalesce(sum(amount) FILTER (WHERE status IN ('PENDING', 'CONFIRMED')), 0) AS swept,
				bool_or(status = 'PENDING') AS pending,
				max(finished_at) FILTER (WHERE status = 'FAILED') AS last_failed_at
			FROM sweeps GROUP BY from_address
		)
		SELECT l.chain, l.deposit_address, l.asset, l.legs, l.indexes, l.deposit_index, l.earned, l.busy,
			coalesce(sw.swept, 0), coalesce(sw.pending, false), sw.last_failed_at
		FROM legs l LEFT JOIN swept sw ON sw.from_address = l.deposit_address
		ORDER BY l.chain, l.deposit_address`)
	if err != nil {
		return nil, fmt.Errorf("sweeps: listing wallets: %w", err)
	}
	defer rows.Close()
	var out []Wallet
	for rows.Next() {
		var w Wallet
		var chain, asset string
		var indexes int
		var index *int64
		var earned, swept int64
		if err := rows.Scan(&chain, &w.Address, &asset, &w.Legs, &indexes, &index, &earned, &w.Busy,
			&swept, &w.Pending, &w.LastFailedAt); err != nil {
			return nil, fmt.Errorf("sweeps: listing wallets: %w", err)
		}
		w.Chain = transfers.Chain(chain)
		w.Earned = money.Amount{Asset: money.Asset(asset), Units: earned}
		w.Swept = money.Amount{Asset: money.Asset(asset), Units: swept}
		switch {
		case indexes > 1:
			w.IndexConflict = true
		case index != nil:
			v := uint32(*index)
			w.DepositIndex = &v
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

const selectSQL = `
	SELECT id, chain, from_address, deposit_index, to_address, amount, asset, status,
		tx_hash, ledger_entry_id, error, created_at, updated_at, finished_at
	FROM sweeps`

// Create records a new PENDING sweep. It returns ErrPending when the
// wallet already has one in flight.
func (s *Store) Create(ctx context.Context, sw Sweep) (Sweep, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sweeps (chain, from_address, deposit_index, to_address, amount, asset)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		string(sw.Chain), sw.FromAddress, int64(sw.DepositIndex), sw.ToAddress, sw.Amount.Units, string(sw.Amount.Asset),
	).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "sweeps_one_pending_per_wallet" {
		return Sweep{}, fmt.Errorf("%w: %s", ErrPending, sw.FromAddress)
	}
	if err != nil {
		return Sweep{}, fmt.Errorf("sweeps: recording a sweep of %s: %w", sw.FromAddress, err)
	}
	return s.Get(ctx, id)
}

// Get fetches one sweep.
func (s *Store) Get(ctx context.Context, id int64) (Sweep, error) {
	sw, err := scanSweep(s.pool.QueryRow(ctx, selectSQL+` WHERE id = $1`, id))
	if err != nil {
		return Sweep{}, fmt.Errorf("sweeps: fetching sweep %d: %w", id, err)
	}
	return sw, nil
}

// ListPending returns every sweep in flight, oldest first.
func (s *Store) ListPending(ctx context.Context) ([]Sweep, error) {
	return s.list(ctx, selectSQL+` WHERE status = 'PENDING' ORDER BY id`)
}

// List returns the most recent sweeps, newest first.
func (s *Store) List(ctx context.Context, limit int) ([]Sweep, error) {
	return s.list(ctx, selectSQL+` ORDER BY id DESC LIMIT $1`, limit)
}

func (s *Store) list(ctx context.Context, query string, args ...any) ([]Sweep, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sweeps: listing sweeps: %w", err)
	}
	defer rows.Close()
	var out []Sweep
	for rows.Next() {
		sw, err := scanSweep(rows)
		if err != nil {
			return nil, fmt.Errorf("sweeps: listing sweeps: %w", err)
		}
		out = append(out, sw)
	}
	return out, rows.Err()
}

// MarkConfirmed records that sweep id's transfer confirmed as txHash and
// the ledger booked it as entryID.
func (s *Store) MarkConfirmed(ctx context.Context, id int64, txHash string, entryID int64) error {
	return s.finish(ctx, id, StatusConfirmed, `, tx_hash = $3, ledger_entry_id = $4`, txHash, entryID)
}

// MarkFailed records that sweep id was given up with nothing moved.
func (s *Store) MarkFailed(ctx context.Context, id int64, reason string) error {
	return s.finish(ctx, id, StatusFailed, `, error = $3`, reason)
}

func (s *Store) finish(ctx context.Context, id int64, to Status, extra string, args ...any) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sweeps SET status = $2, finished_at = now(), updated_at = now()`+extra+`
		WHERE id = $1 AND status = 'PENDING'`, append([]any{id, string(to)}, args...)...)
	if err != nil {
		return fmt.Errorf("sweeps: marking sweep %d %s: %w", id, to, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("sweeps: sweep %d is not PENDING", id)
	}
	return nil
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanSweep(row scanRow) (Sweep, error) {
	var sw Sweep
	var chain, asset, status string
	var index, units int64
	if err := row.Scan(&sw.ID, &chain, &sw.FromAddress, &index, &sw.ToAddress, &units, &asset, &status,
		&sw.TxHash, &sw.LedgerEntryID, &sw.Error, &sw.CreatedAt, &sw.UpdatedAt, &sw.FinishedAt); err != nil {
		return Sweep{}, err
	}
	sw.Chain = transfers.Chain(chain)
	sw.DepositIndex = uint32(index)
	sw.Amount = money.Amount{Asset: money.Asset(asset), Units: units}
	sw.Status = Status(status)
	return sw, nil
}
