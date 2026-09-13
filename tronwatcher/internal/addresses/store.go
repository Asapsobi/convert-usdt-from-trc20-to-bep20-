package addresses

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"tronwatcher/internal/db"
)

// Status is the closed set of an address's lifecycle states. Mirrors
// depositwatcher/internal/addresses's own Status exactly -- the legal
// transitions live in migration 0002's database trigger, chain-agnostic.
type Status string

const (
	StatusWatching Status = "WATCHING"
	StatusFunded   Status = "FUNDED"
	StatusRetired  Status = "RETIRED"
)

var (
	ErrOrderNotFound = errors.New("addresses: no watched address for that order")

	ErrAddressNotFound = errors.New("addresses: no watched address with that address")

	// ErrNotConfigured means Configure was never called.
	ErrNotConfigured = errors.New("addresses: not configured; call Configure with an extended public key at startup")

	// ErrIllegalStatusTransition surfaces migration 0002's trigger
	// rejection as a typed Go error.
	ErrIllegalStatusTransition = errors.New("addresses: illegal status transition")
)

// WatchedAddress is a watched_addresses row.
type WatchedAddress struct {
	ID              int64
	Address         Address
	DerivationIndex uint32
	OrderID         int64
	ExternalID      string
	CustomerID      string
	Status          Status
	QuotedAt        time.Time
	QuoteExpiresAt  time.Time
	AssignedAt      time.Time
	RetiredAt       *time.Time
	RetiredReason   *string
	LastScannedAt   *time.Time
}

// Queryer is db.Queryer under this package's own name.
type Queryer = db.Queryer

// xpub is the extended PUBLIC key every Assign call derives under. Set
// once via Configure at process startup (cmd/tronwatcherd).
var xpub string

// Configure validates and records the extended public key Assign will
// derive under.
func Configure(newXpub string) error {
	if err := rejectPrivateKeyShaped(newXpub); err != nil {
		return err
	}
	if _, err := parseXpub(newXpub); err != nil {
		return err
	}
	xpub = newXpub
	return nil
}

const selectSQL = `
	SELECT id, address, derivation_index, order_id, external_id, customer_id,
		status, quoted_at, quote_expires_at, assigned_at, retired_at, retired_reason, last_scanned_at
	FROM watched_addresses`

// Assign derives a new watch-only address at the next never-reused
// derivation index and records it against orderID. Idempotent on
// orderID.
func Assign(ctx context.Context, q Queryer, orderID int64, externalID, customerID string, quotedAt, quoteExpiresAt time.Time) (Address, error) {
	if xpub == "" {
		return "", ErrNotConfigured
	}

	existing, err := GetByOrderID(ctx, q, orderID)
	if err == nil {
		return existing.Address, nil
	}
	if !errors.Is(err, ErrOrderNotFound) {
		return "", err
	}

	var index int64
	if err := q.QueryRow(ctx, `SELECT nextval('watched_addresses_derivation_index_seq')`).Scan(&index); err != nil {
		return "", fmt.Errorf("addresses: allocating derivation index: %w", err)
	}

	addr, err := DeriveAddress(xpub, uint32(index))
	if err != nil {
		return "", fmt.Errorf("addresses: deriving address at index %d: %w", index, err)
	}

	row := q.QueryRow(ctx, `
		INSERT INTO watched_addresses
			(address, derivation_index, order_id, external_id, customer_id,
			 status, quoted_at, quote_expires_at)
		VALUES ($1, $2, $3, $4, $5, 'WATCHING', $6, $7)
		ON CONFLICT DO NOTHING
		RETURNING address
	`, string(addr), index, orderID, externalID, customerID, quotedAt, quoteExpiresAt)

	var inserted string
	err = row.Scan(&inserted)
	if err == nil {
		return Address(inserted), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("addresses: assigning order %d: %w", orderID, err)
	}
	// ON CONFLICT DO NOTHING (no target) returned no row: we lost a race
	// against a concurrent Assign for the same order. Deliberately
	// untargeted, not `ON CONFLICT (order_id)`: a true concurrent race
	// (e.g. a caller retrying POST /v1/addresses while the original
	// request is still in flight) sends the same order_id AND the same
	// external_id together, and Postgres only suppresses a conflict on
	// the constraint(s) named in the ON CONFLICT target -- targeting
	// order_id alone left a simultaneous external_id conflict to raise a
	// real unique_violation instead of being absorbed here, discovered
	// by this package's own TestAssign_ConcurrentRaceProducesOneRowNoWastedIndex.
	// Untargeted DO NOTHING suppresses a conflict on ANY unique
	// constraint on this table, which is correct here since every
	// legitimate conflict source (order_id, external_id) belongs to the
	// same losing INSERT attempt.
	winner, err := GetByOrderID(ctx, q, orderID)
	if err != nil {
		return "", fmt.Errorf("addresses: assign order %d: lost the race but couldn't read the winner: %w", orderID, err)
	}
	return winner.Address, nil
}

// GetByOrderID looks up the address assigned to orderID.
func GetByOrderID(ctx context.Context, q Queryer, orderID int64) (WatchedAddress, error) {
	row := q.QueryRow(ctx, selectSQL+` WHERE order_id = $1`, orderID)
	wa, err := scanWatchedAddress(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return WatchedAddress{}, fmt.Errorf("%w: order %d", ErrOrderNotFound, orderID)
	}
	if err != nil {
		return WatchedAddress{}, fmt.Errorf("addresses: get order %d: %w", orderID, err)
	}
	return wa, nil
}

// GetByAddress looks up the watched address row for addr itself -- what
// the candidate pipeline needs to resolve an observed transfer's `to`
// back to the order it belongs to.
func GetByAddress(ctx context.Context, q Queryer, addr Address) (WatchedAddress, error) {
	row := q.QueryRow(ctx, selectSQL+` WHERE address = $1`, string(addr))
	wa, err := scanWatchedAddress(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return WatchedAddress{}, fmt.Errorf("%w: %s", ErrAddressNotFound, addr)
	}
	if err != nil {
		return WatchedAddress{}, fmt.Errorf("addresses: get address %s: %w", addr, err)
	}
	return wa, nil
}

// MarkFunded transitions orderID's address to FUNDED.
func MarkFunded(ctx context.Context, q Queryer, orderID int64) error {
	tag, err := q.Exec(ctx, `UPDATE watched_addresses SET status = 'FUNDED' WHERE order_id = $1`, orderID)
	if err != nil {
		return mapTransitionError(err, orderID, StatusFunded)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: order %d", ErrOrderNotFound, orderID)
	}
	return nil
}

// Retire transitions orderID's address to RETIRED with the given
// reason.
func Retire(ctx context.Context, q Queryer, orderID int64, reason string) error {
	if reason == "" {
		return fmt.Errorf("addresses: retire order %d: reason must not be empty", orderID)
	}
	tag, err := q.Exec(ctx, `
		UPDATE watched_addresses
		SET status = 'RETIRED', retired_at = now(), retired_reason = $1
		WHERE order_id = $2
	`, reason, orderID)
	if err != nil {
		return mapTransitionError(err, orderID, StatusRetired)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: order %d", ErrOrderNotFound, orderID)
	}
	return nil
}

// ListActive returns every address not yet RETIRED -- what the
// ingestion loop scans TRON for.
func ListActive(ctx context.Context, q Queryer) ([]WatchedAddress, error) {
	rows, err := q.Query(ctx, selectSQL+` WHERE status <> 'RETIRED' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("addresses: list active: %w", err)
	}
	defer rows.Close()

	var out []WatchedAddress
	for rows.Next() {
		wa, err := scanWatchedAddress(rows)
		if err != nil {
			return nil, fmt.Errorf("addresses: list active: %w", err)
		}
		out = append(out, wa)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("addresses: list active: %w", err)
	}
	return out, nil
}

// UpdateLastScannedAt records how far a single address has been scanned
// for inbound TRC20 transfers -- this package's own per-address cursor
// (migration 0003), the direct analogue of depositwatcher's global
// ingestion_cursor table, needed here because tronwatcher scans
// per-address rather than per-block-range (see internal/chain's own
// doc comment on why there is no TRON eth_getLogs equivalent). Does not
// use mapTransitionError/the status trigger -- this column carries no
// transition semantics, so an ordinary UPDATE with no WHERE-on-status
// guard is correct here, unlike MarkFunded/Retire.
func UpdateLastScannedAt(ctx context.Context, q Queryer, orderID int64, at time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE watched_addresses SET last_scanned_at = $1 WHERE order_id = $2`, at, orderID)
	if err != nil {
		return fmt.Errorf("addresses: updating last_scanned_at for order %d: %w", orderID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: order %d", ErrOrderNotFound, orderID)
	}
	return nil
}

func mapTransitionError(err error, orderID int64, to Status) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "P0001" {
		return fmt.Errorf("%w: order %d -> %s", ErrIllegalStatusTransition, orderID, to)
	}
	return fmt.Errorf("addresses: transitioning order %d to %s: %w", orderID, to, err)
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanWatchedAddress(row scanRow) (WatchedAddress, error) {
	var wa WatchedAddress
	var addr string
	var index int64
	var status string
	err := row.Scan(&wa.ID, &addr, &index, &wa.OrderID, &wa.ExternalID, &wa.CustomerID,
		&status, &wa.QuotedAt, &wa.QuoteExpiresAt, &wa.AssignedAt, &wa.RetiredAt, &wa.RetiredReason, &wa.LastScannedAt)
	if err != nil {
		return WatchedAddress{}, err
	}
	wa.Address = Address(addr)
	wa.DerivationIndex = uint32(index)
	wa.Status = Status(status)
	return wa, nil
}
