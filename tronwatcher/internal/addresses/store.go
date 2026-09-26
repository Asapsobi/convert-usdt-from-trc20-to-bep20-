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
// once via Configure at process startup (cmd/tronwatcherd). Mutually
// exclusive with provisioner below -- a deployment picks exactly one
// backend for turning a derivation index into a real TRON address (see
// cmd/tronwatcherd's own fail-loud startup check).
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

// Provisioner is the one call this package needs to provision the real
// custody (a Privy Server Wallet, in production) backing a TRON deposit
// address at a given derivation index -- s1client's own
// ProvisionTronDepositKey, or a fake for testing.
type Provisioner interface {
	ProvisionTronDepositKey(ctx context.Context, index uint32) (string, error)
}

// provisioner is Assign's own real-custody backend, when configured --
// mirrors xpub's own package-level, set-once-at-startup shape. When set,
// Assign calls out to S1 for the real address instead of deriving one
// locally from xpub; DeriveAddress's own pure local math becomes
// unreachable for new assignments.
var provisioner Provisioner

// ConfigureS1Provisioning records the real-custody backend Assign
// provisions TRON deposit addresses through -- the Privy-backed
// alternative to Configure's own local xpub-derivation mode.
func ConfigureS1Provisioning(p Provisioner) {
	provisioner = p
}

const selectSQL = `
	SELECT id, address, derivation_index, order_id, external_id, customer_id,
		status, quoted_at, quote_expires_at, assigned_at, retired_at, retired_reason, last_scanned_at
	FROM watched_addresses`

// Assign leases a deposit wallet to orderID: an idle, cooled-down pool
// wallet, or a newly provisioned one while the pool is below its limit.
// Idempotent on orderID -- a repeated call returns the same address. When
// every wallet is busy and the pool is full it returns
// ErrNoWalletAvailable.
func Assign(ctx context.Context, q Queryer, orderID int64, externalID, customerID string, quotedAt, quoteExpiresAt time.Time) (Address, error) {
	if xpub == "" && provisioner == nil {
		return "", ErrNotConfigured
	}
	if existing, err := GetByOrderID(ctx, q, orderID); err == nil {
		return existing.Address, nil
	} else if !errors.Is(err, ErrOrderNotFound) {
		return "", err
	}

	for attempt := 0; attempt < 5; attempt++ {
		addr, ok, err := leaseFromPool(ctx, q, orderID, externalID, customerID, quotedAt, quoteExpiresAt)
		if err != nil {
			return "", err
		}
		if ok {
			return addr, nil
		}
		// Nothing leased: a concurrent call for this same order may have
		// won, another order may have taken the wallet first, or no wallet
		// is free.
		if existing, err := GetByOrderID(ctx, q, orderID); err == nil {
			return existing.Address, nil
		} else if !errors.Is(err, ErrOrderNotFound) {
			return "", err
		}
		free, err := eligibleWallets(ctx, q)
		if err != nil {
			return "", err
		}
		if free == 0 {
			// Create a wallet and lease it to this order in one step, or
			// fail with ErrNoWalletAvailable when the pool is full.
			if _, err := provision(ctx, q, &leaseRequest{orderID, externalID, customerID, quotedAt, quoteExpiresAt}); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("%w: kept losing wallets to concurrent orders", ErrNoWalletAvailable)
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

// Retire ends orderID's lease and starts its wallet's cooldown: short
// after a completed order, long after one that expired unpaid (its
// customer may still pay late). Idempotent: retiring an already-retired
// lease changes nothing, so a retry never extends the cooldown.
func Retire(ctx context.Context, q Queryer, orderID int64, reason string) error {
	if reason == "" {
		return fmt.Errorf("addresses: retire order %d: reason must not be empty", orderID)
	}
	var addr string
	err := q.QueryRow(ctx, `
		UPDATE watched_addresses
		SET status = 'RETIRED', retired_at = now(), retired_reason = $1
		WHERE order_id = $2 AND status <> 'RETIRED'
		RETURNING address
	`, reason, orderID).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := GetByOrderID(ctx, q, orderID); getErr != nil {
			return getErr
		}
		return nil // already retired
	}
	if err != nil {
		return mapTransitionError(err, orderID, StatusRetired)
	}
	if _, err := q.Exec(ctx, `
		UPDATE pool_wallets
		SET available_after = now() + (SELECT CASE WHEN $2 = 'expired' THEN cooldown_after_expiry ELSE cooldown_after_use END
			FROM pool_settings WHERE id = 1)
		WHERE address = $1
	`, addr, reason); err != nil {
		return fmt.Errorf("addresses: starting the cooldown of %s: %w", addr, err)
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
