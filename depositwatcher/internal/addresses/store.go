package addresses

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"depositwatcher/internal/db"
)

// Status is the closed set of an address's lifecycle states. The legal
// transitions between them live entirely in migration 0002's database
// trigger -- Status itself carries no transition logic, same division of
// responsibility as C1's orders.State versus its transitionTable.
type Status string

const (
	StatusWatching Status = "WATCHING"
	StatusFunded   Status = "FUNDED"
	StatusRetired  Status = "RETIRED"
)

var (
	ErrOrderNotFound = errors.New("addresses: no watched address for that order")

	// ErrAddressNotFound is GetByAddress's own not-found -- distinct from
	// ErrOrderNotFound so a log line or error message naming "an address"
	// never gets misreported as naming "an order," which have different
	// callers and different meanings ("this order was never assigned an
	// address" vs. "this on-chain address is not in our book at all,"
	// e.g. a candidate pipeline's routine, expected chain noise).
	ErrAddressNotFound = errors.New("addresses: no watched address with that address")

	// ErrNotConfigured means Configure was never called. Assign fails
	// closed rather than deriving against an empty or zero-value key --
	// same reasoning as orders.ErrHaltCacheNotConfigured in the ledger: a
	// missing setup call must never be indistinguishable from "configured
	// with something that happens to work."
	ErrNotConfigured = errors.New("addresses: not configured; call Configure with an extended public key at startup")

	// ErrIllegalStatusTransition surfaces migration 0002's trigger
	// rejection as a typed Go error rather than a raw pgconn.PgError a
	// caller would have to know to inspect.
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
}

// Queryer is db.Queryer under this package's own name -- callers can run
// these against the pool directly or inside a transaction they already
// hold, without this package caring which. A type alias, not a fresh
// declaration, so addresses.Queryer and db.Queryer are the exact same
// type and never drift apart.
type Queryer = db.Queryer

// xpub is the extended PUBLIC key every Assign call derives under. Set
// once via Configure at process startup (cmd/watcherd), never per-call --
// C2.1's own spec signature for Assign has no xpub parameter, matching
// the operational reality that a deployment has exactly one HD public key
// for the whole lifetime of the process, supplied by S1.
var xpub string

// Configure validates and records the extended public key Assign will
// derive under. It runs the same guards DeriveAddress itself would --
// rejecting a private-key-shaped input and a structurally invalid xpub --
// so a misconfiguration fails loudly at startup, never lazily on whichever
// request happens to call Assign first.
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
// custody (a Privy Server Wallet, in production) backing a BSC deposit
// address at a given derivation index -- s1client's own
// ProvisionBSCDepositKey, or a fake for testing. Mirrors
// tronwatcher/internal/addresses's own identically-shaped Provisioner
// (separate modules, no shared internal package -- this repo's own
// established cross-module-boundary convention).
type Provisioner interface {
	ProvisionBSCDepositKey(ctx context.Context, index uint32) (string, error)
}

// provisioner is Assign's own real-custody backend, when configured --
// mirrors xpub's own package-level, set-once-at-startup shape. When set,
// Assign calls out to S1 for the real address instead of deriving one
// locally from xpub; DeriveAddress's own pure local math becomes
// unreachable for new assignments. Mutually exclusive with xpub -- a
// deployment picks exactly one backend for turning a derivation index
// into a real BSC address (see cmd/watcherd's own fail-loud startup
// check).
var provisioner Provisioner

// ConfigureS1Provisioning records the real-custody backend Assign
// provisions BSC deposit addresses through -- the Privy-backed
// alternative to Configure's own local xpub-derivation mode.
func ConfigureS1Provisioning(p Provisioner) {
	provisioner = p
}

const selectSQL = `
	SELECT id, address, derivation_index, order_id, external_id, customer_id,
		status, quoted_at, quote_expires_at, assigned_at, retired_at, retired_reason
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
// the candidate pipeline needs to resolve an observed Transfer log's `to`
// back to the order it belongs to (and whether that order's address is
// still WATCHING/FUNDED, or RETIRED -- a late deposit, C2.8's job) before
// it can do anything else with that log.
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

// MarkFunded transitions orderID's address to FUNDED. Illegal from
// RETIRED (rejected by migration 0002's trigger, surfaced here as
// ErrIllegalStatusTransition); a no-op status-wise from FUNDED itself is
// not reachable since the trigger only fires on an actual status change,
// and this UPDATE always sets 'FUNDED' -- calling MarkFunded twice simply
// re-affirms the same value rather than erroring, which is the right
// behavior for a caller retrying after an uncertain outcome.
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

// ListActive returns every address not yet RETIRED -- what C2.3's block-
// ingestion loop filters incoming Transfer logs against.
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

// mapTransitionError turns migration 0002's RAISE EXCEPTION (Postgres
// error code P0001, plpgsql's generic "raised exception" class) into
// ErrIllegalStatusTransition. Anything else is a genuine, unclassified
// database error and is wrapped as such rather than mislabeled.
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
		&status, &wa.QuotedAt, &wa.QuoteExpiresAt, &wa.AssignedAt, &wa.RetiredAt, &wa.RetiredReason)
	if err != nil {
		return WatchedAddress{}, err
	}
	wa.Address = Address(addr)
	wa.DerivationIndex = uint32(index)
	wa.Status = Status(status)
	return wa, nil
}
