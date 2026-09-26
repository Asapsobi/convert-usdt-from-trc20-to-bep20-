// Package orphaned stores deposits that finalized on-chain for an order
// C1 no longer considers open -- gap #3's mechanism (C2.8), not its
// policy. Nothing in this package decides what to DO about a row here;
// it exists purely to capture one and make it visible (C2.9's HTTP
// surface) until a human, or an eventual product decision, resolves it.
package orphaned

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"depositwatcher/internal/db"
)

// ErrNotFound means no orphaned_deposits row exists with the given id.
var ErrNotFound = errors.New("orphaned: no such deposit")

// ErrAlreadyResolved guards against silently overwriting a prior
// resolution -- if a second resolve is genuinely needed (correcting a
// mistake), that is itself an action worth a human being explicit about,
// not something this method does by default.
var ErrAlreadyResolved = errors.New("orphaned: already resolved")

// Deposit is an orphaned_deposits row.
type Deposit struct {
	ID                    int64
	OrderID               int64
	ExternalID            string
	TxHash                string
	LogIndex              int
	Amount                int64 // minor units, this service's own money.Decimals convention
	DetectedAt            time.Time
	OrderStateAtDetection string
	Resolution            *string
	ResolvedAt            *time.Time
	ResolvedBy            *string
	// Address is the deposit wallet the payment landed in -- sweeping
	// leaves an unresolved orphan's funds where they are, so it can still
	// be refunded from there.
	Address string
}

// Queryer is db.Queryer under this package's own name, matching the
// convention internal/addresses and internal/chain already established.
type Queryer = db.Queryer

// Record inserts d, idempotent on (tx_hash, log_index) -- the same
// candidate detected as orphaned more than once (a retried recording
// attempt after a transient failure, or the surrounding process
// restarting) must never produce a second row for it.
func Record(ctx context.Context, q Queryer, d Deposit) error {
	_, err := q.Exec(ctx, `
		INSERT INTO orphaned_deposits
			(order_id, external_id, tx_hash, log_index, amount, detected_at, order_state_at_detection, address)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
		ON CONFLICT (tx_hash, log_index) DO NOTHING
	`, d.OrderID, d.ExternalID, d.TxHash, d.LogIndex, d.Amount, d.DetectedAt, d.OrderStateAtDetection, d.Address)
	if err != nil {
		return fmt.Errorf("orphaned: recording %s:%d: %w", d.TxHash, d.LogIndex, err)
	}
	return nil
}

const selectSQL = `
	SELECT id, order_id, external_id, tx_hash, log_index, amount, detected_at,
		order_state_at_detection, resolution, resolved_at, resolved_by
	FROM orphaned_deposits`

func scanDeposit(row scanRow) (Deposit, error) {
	var d Deposit
	err := row.Scan(&d.ID, &d.OrderID, &d.ExternalID, &d.TxHash, &d.LogIndex, &d.Amount,
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

// List returns orphaned deposits, most recently detected first. resolved
// nil returns every row; non-nil filters to only resolved (true) or only
// unresolved (false) rows -- what C2.9's GET /orphaned-deposits?resolved=
// filter maps onto directly.
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

// Resolve records resolution and resolvedBy (the authenticated actor --
// never a caller-supplied body field, same convention as every actor
// elsewhere in this system) against id, and returns the updated row.
// Fails with ErrAlreadyResolved rather than silently overwriting a prior
// resolution -- a genuine correction is a deliberate action, not this
// method's default.
func Resolve(ctx context.Context, q Queryer, id int64, resolution, resolvedBy string) (Deposit, error) {
	row := q.QueryRow(ctx, `
		UPDATE orphaned_deposits
		SET resolution = $1, resolved_at = now(), resolved_by = $2
		WHERE id = $3 AND resolution IS NULL
		RETURNING id, order_id, external_id, tx_hash, log_index, amount, detected_at,
			order_state_at_detection, resolution, resolved_at, resolved_by
	`, resolution, resolvedBy, id)
	d, err := scanDeposit(row)
	if isNoRows(err) {
		// Distinguish "no such row" from "row exists but already
		// resolved" -- the caller-visible errors mean different things
		// and map to different HTTP statuses.
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
