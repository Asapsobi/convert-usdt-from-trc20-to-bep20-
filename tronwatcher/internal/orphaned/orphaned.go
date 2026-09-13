// Package orphaned stores deposits that finalized on-chain for an order
// C1 no longer considers open. Mirrors depositwatcher/internal/orphaned
// exactly (separate Go modules, no shared internal package): nothing
// here decides what to DO about a row, it exists purely to capture one
// and make it visible until a human resolves it. The one shape change
// from depositwatcher's own version: idempotent on tx_id alone, not
// (tx_hash, log_index) -- a TRC20 transfer's natural key, matching
// internal/finality's own DepositFinalIdempotencyKey.
package orphaned

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"tronwatcher/internal/db"
)

// ErrNotFound means no orphaned_deposits row exists with the given id.
var ErrNotFound = errors.New("orphaned: no such deposit")

// ErrAlreadyResolved guards against silently overwriting a prior
// resolution.
var ErrAlreadyResolved = errors.New("orphaned: already resolved")

// Deposit is an orphaned_deposits row.
type Deposit struct {
	ID                    int64
	OrderID               int64
	ExternalID            string
	TxID                  string
	Amount                int64 // minor units, this service's own money.Decimals convention
	DetectedAt            time.Time
	OrderStateAtDetection string
	Resolution            *string
	ResolvedAt            *time.Time
	ResolvedBy            *string
}

// Queryer is db.Queryer under this package's own name.
type Queryer = db.Queryer

// Record inserts d, idempotent on tx_id -- the same candidate detected
// as orphaned more than once must never produce a second row for it.
func Record(ctx context.Context, q Queryer, d Deposit) error {
	_, err := q.Exec(ctx, `
		INSERT INTO orphaned_deposits
			(order_id, external_id, tx_id, amount, detected_at, order_state_at_detection)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tx_id) DO NOTHING
	`, d.OrderID, d.ExternalID, d.TxID, d.Amount, d.DetectedAt, d.OrderStateAtDetection)
	if err != nil {
		return fmt.Errorf("orphaned: recording %s: %w", d.TxID, err)
	}
	return nil
}

const selectSQL = `
	SELECT id, order_id, external_id, tx_id, amount, detected_at,
		order_state_at_detection, resolution, resolved_at, resolved_by
	FROM orphaned_deposits`

func scanDeposit(row scanRow) (Deposit, error) {
	var d Deposit
	err := row.Scan(&d.ID, &d.OrderID, &d.ExternalID, &d.TxID, &d.Amount,
		&d.DetectedAt, &d.OrderStateAtDetection, &d.Resolution, &d.ResolvedAt, &d.ResolvedBy)
	return d, err
}

type scanRow interface {
	Scan(dest ...any) error
}

// Get returns the orphaned deposit with this id, or ErrNotFound.
func Get(ctx context.Context, q Queryer, id int64) (Deposit, error) {
	row := q.QueryRow(ctx, selectSQL+` WHERE id = $1`, id)
	d, err := scanDeposit(row)
	if isNoRows(err) {
		return Deposit{}, fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	if err != nil {
		return Deposit{}, fmt.Errorf("orphaned: get %d: %w", id, err)
	}
	return d, nil
}

// List returns orphaned deposits, most recently detected first.
func List(ctx context.Context, q Queryer, resolved *bool) ([]Deposit, error) {
	query := selectSQL
	if resolved != nil {
		if *resolved {
			query += ` WHERE resolution IS NOT NULL`
		} else {
			query += ` WHERE resolution IS NULL`
		}
	}
	query += ` ORDER BY detected_at DESC`

	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("orphaned: list: %w", err)
	}
	defer rows.Close()

	var out []Deposit
	for rows.Next() {
		d, err := scanDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("orphaned: list: scanning row: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orphaned: list: %w", err)
	}
	return out, nil
}

// Resolve records resolution and resolvedBy against id, and returns the
// updated row. Fails with ErrAlreadyResolved rather than silently
// overwriting a prior resolution.
func Resolve(ctx context.Context, q Queryer, id int64, resolution, resolvedBy string) (Deposit, error) {
	row := q.QueryRow(ctx, `
		UPDATE orphaned_deposits
		SET resolution = $1, resolved_at = now(), resolved_by = $2
		WHERE id = $3 AND resolution IS NULL
		RETURNING id, order_id, external_id, tx_id, amount, detected_at,
			order_state_at_detection, resolution, resolved_at, resolved_by
	`, resolution, resolvedBy, id)
	d, err := scanDeposit(row)
	if isNoRows(err) {
		if _, getErr := Get(ctx, q, id); errors.Is(getErr, ErrNotFound) {
			return Deposit{}, fmt.Errorf("%w: id %d", ErrNotFound, id)
		}
		return Deposit{}, fmt.Errorf("%w: id %d", ErrAlreadyResolved, id)
	}
	if err != nil {
		return Deposit{}, fmt.Errorf("orphaned: resolve %d: %w", id, err)
	}
	return d, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
