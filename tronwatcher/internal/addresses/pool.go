package addresses

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The deposit-wallet pool: a limited set of wallets, each leased to one
// order at a time. Keeping the set small keeps sweeping profit to
// treasury cheap. A wallet that finishes a lease cools down before its
// next one, so a late payment from the previous customer is recorded as
// orphaned instead of being credited to the next customer.
//
// Every wallet brought into use costs money, so a new wallet is created
// only when every existing one is leased or cooling down, and a wallet
// that has never received a deposit is leased only while every one that
// has is busy.

// ErrNoWalletAvailable means every pool wallet is leased or cooling down
// and the pool is already at its configured size.
var ErrNoWalletAvailable = errors.New("addresses: no deposit wallet is available")

// ErrWalletNotFound means no pool wallet has the given address.
var ErrWalletNotFound = errors.New("addresses: no pool wallet with that address")

// leaseTimeTolerance absorbs clock skew between this server (a lease's
// assigned_at) and the chain (a deposit's block time) when deciding which
// lease a deposit belongs to. Kept far below the pool cooldown, so a late
// payment can never slip into the next lease through it.
const leaseTimeTolerance = 30 * time.Second

// scanOverlap is how far behind its cursor each scan re-reads. TronGrid's
// confirmed-only index trails the chain by about a minute; re-reading a
// generous window means a deposit confirmed after an earlier scan moved
// past its block time is still found. Re-reading is harmless: a deposit
// already recorded is skipped.
const scanOverlap = 10 * time.Minute

// retiredWatchWindow keeps a retired lease's address watched this long
// after the lease ends, so a late payment to a wallet that has left the
// pool (a legacy one-address-per-order wallet) is still recorded.
const retiredWatchWindow = 7 * 24 * time.Hour

// PoolSettings is the admin-managed shape of the pool.
type PoolSettings struct {
	MaxWallets          int
	CooldownAfterUse    time.Duration // after an order completes
	CooldownAfterExpiry time.Duration // after an order expired unpaid -- its customer may still pay late
}

// PoolWallet is one managed deposit wallet and its open lease, if any.
type PoolWallet struct {
	Address         Address
	DerivationIndex uint32
	Status          string // ACTIVE or DISABLED
	AvailableAfter  time.Time
	LastLeasedAt    *time.Time
	CreatedAt       time.Time
	Lease           *WatchedAddress // the open lease, nil when idle
}

// canonical is the one spelling of addr this package stores and compares.
// TRON base58 addresses have exactly one spelling already.
func canonical(addr string) Address {
	return Address(addr)
}

// GetPoolSettings reads the pool settings.
func GetPoolSettings(ctx context.Context, q Queryer) (PoolSettings, error) {
	var s PoolSettings
	err := q.QueryRow(ctx, `SELECT max_wallets, cooldown_after_use, cooldown_after_expiry FROM pool_settings WHERE id = 1`).
		Scan(&s.MaxWallets, &s.CooldownAfterUse, &s.CooldownAfterExpiry)
	if err != nil {
		return PoolSettings{}, fmt.Errorf("addresses: reading pool settings: %w", err)
	}
	return s, nil
}

// PutPoolSettings stores s.
func PutPoolSettings(ctx context.Context, q Queryer, s PoolSettings) error {
	if s.MaxWallets < 0 || s.CooldownAfterUse < 0 || s.CooldownAfterExpiry < 0 {
		return fmt.Errorf("addresses: pool settings must not be negative")
	}
	_, err := q.Exec(ctx, `
		UPDATE pool_settings SET max_wallets = $1, cooldown_after_use = $2, cooldown_after_expiry = $3, updated_at = now()
		WHERE id = 1
	`, s.MaxWallets, s.CooldownAfterUse, s.CooldownAfterExpiry)
	if err != nil {
		return fmt.Errorf("addresses: storing pool settings: %w", err)
	}
	return nil
}

// leaseFromPool leases an idle, cooled-down, active wallet to orderID in
// one statement. ok is false when the order already has a lease, no
// wallet was eligible, or another order took the same wallet first -- the
// caller tells those apart.
//
// A wallet that has received a deposit before goes first (one later
// dropped by a reorg doesn't count). A never-used wallet joins the flow
// only while every used one is busy, since each wallet in use must first
// be activated (about 1.1 TRX) and needs its own profit sweep. Within
// each group, the wallet leased longest ago goes first.
func leaseFromPool(ctx context.Context, q Queryer, orderID int64, externalID, customerID string, quotedAt, quoteExpiresAt time.Time) (Address, bool, error) {
	var addr string
	err := q.QueryRow(ctx, `
		INSERT INTO watched_addresses
			(address, derivation_index, order_id, external_id, customer_id, status, quoted_at, quote_expires_at)
		SELECT pw.address, pw.derivation_index, $1, $2, $3, 'WATCHING', $4, $5
		FROM pool_wallets pw
		WHERE pw.status = 'ACTIVE' AND pw.available_after <= now()
			AND NOT EXISTS (SELECT 1 FROM watched_addresses wa WHERE wa.address = pw.address AND wa.status <> 'RETIRED')
		ORDER BY EXISTS (SELECT 1 FROM deposits d WHERE d.address = pw.address AND d.status <> 'DROPPED') DESC,
			pw.last_leased_at NULLS FIRST, pw.derivation_index
		LIMIT 1
		ON CONFLICT DO NOTHING
		RETURNING address
	`, orderID, externalID, customerID, quotedAt, quoteExpiresAt).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("addresses: leasing a pool wallet to order %d: %w", orderID, err)
	}
	if _, err := q.Exec(ctx, `UPDATE pool_wallets SET last_leased_at = now() WHERE address = $1`, addr); err != nil {
		return "", false, fmt.Errorf("addresses: recording the lease of %s: %w", addr, err)
	}
	return Address(addr), true, nil
}

// eligibleWallets counts wallets that could be leased right now.
func eligibleWallets(ctx context.Context, q Queryer) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM pool_wallets pw
		WHERE pw.status = 'ACTIVE' AND pw.available_after <= now()
			AND NOT EXISTS (SELECT 1 FROM watched_addresses wa WHERE wa.address = pw.address AND wa.status <> 'RETIRED')
	`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("addresses: counting eligible pool wallets: %w", err)
	}
	return n, nil
}

type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ProvisionWallet adds one new wallet to the pool, unless the pool is
// already at its configured size (ErrNoWalletAvailable) -- for an
// operator growing the pool ahead of demand.
func ProvisionWallet(ctx context.Context, q Queryer) (PoolWallet, error) {
	addr, err := provision(ctx, q, nil)
	if err != nil {
		return PoolWallet{}, err
	}
	return getWallet(ctx, q, addr)
}

// leaseRequest is an order waiting for a wallet.
type leaseRequest struct {
	orderID                  int64
	externalID, customerID   string
	quotedAt, quoteExpiresAt time.Time
}

// provision adds a wallet to the pool and, when lease is set, leases it
// to that order in the same transaction -- a wallet created for an order
// is never taken by another one first. Serialized with an advisory lock,
// so concurrent callers can never grow the pool past its limit; once the
// lock is held, a wallet that became free meanwhile is leased instead of
// creating a new one.
func provision(ctx context.Context, q Queryer, lease *leaseRequest) (Address, error) {
	if xpub == "" && provisioner == nil {
		return "", ErrNotConfigured
	}
	b, ok := q.(txBeginner)
	if !ok {
		return "", errors.New("addresses: provisioning a pool wallet needs a connection pool, not a transaction")
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("addresses: provisioning a pool wallet: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('tronwatcher_pool_provision'))`); err != nil {
		return "", fmt.Errorf("addresses: locking the pool: %w", err)
	}
	if lease != nil {
		addr, ok, err := leaseFromPool(ctx, tx, lease.orderID, lease.externalID, lease.customerID, lease.quotedAt, lease.quoteExpiresAt)
		if err != nil {
			return "", err
		}
		if ok {
			return addr, tx.Commit(ctx)
		}
	}
	var size, max int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM pool_wallets), (SELECT max_wallets FROM pool_settings WHERE id = 1)`).
		Scan(&size, &max); err != nil {
		return "", fmt.Errorf("addresses: reading the pool size: %w", err)
	}
	if size >= max {
		return "", fmt.Errorf("%w: the pool is at its limit of %d wallets", ErrNoWalletAvailable, max)
	}

	var index int64
	if err := tx.QueryRow(ctx, `SELECT nextval('watched_addresses_derivation_index_seq')`).Scan(&index); err != nil {
		return "", fmt.Errorf("addresses: allocating a derivation index: %w", err)
	}
	var raw string
	if provisioner != nil {
		if raw, err = provisioner.ProvisionTronDepositKey(ctx, uint32(index)); err != nil {
			return "", fmt.Errorf("addresses: provisioning the wallet at index %d: %w", index, err)
		}
	} else {
		derived, err := DeriveAddress(xpub, uint32(index))
		if err != nil {
			return "", fmt.Errorf("addresses: deriving the wallet at index %d: %w", index, err)
		}
		raw = string(derived)
	}
	if !Valid(Address(raw)) {
		return "", fmt.Errorf("addresses: index %d produced %q, not a TRON address", index, raw)
	}
	addr := canonical(raw)
	if _, err := tx.Exec(ctx, `INSERT INTO pool_wallets (address, derivation_index) VALUES ($1, $2)`, string(addr), index); err != nil {
		return "", fmt.Errorf("addresses: adding %s to the pool: %w", addr, err)
	}
	if lease != nil {
		leased, ok, err := leaseFromPool(ctx, tx, lease.orderID, lease.externalID, lease.customerID, lease.quotedAt, lease.quoteExpiresAt)
		if err != nil {
			return "", err
		}
		if ok {
			addr = leased
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("addresses: committing the new pool wallet: %w", err)
	}
	return addr, nil
}

// ListPool returns every pool wallet with its open lease, oldest first.
func ListPool(ctx context.Context, q Queryer) ([]PoolWallet, error) {
	rows, err := q.Query(ctx, `
		SELECT address, derivation_index, status, available_after, last_leased_at, created_at
		FROM pool_wallets ORDER BY derivation_index`)
	if err != nil {
		return nil, fmt.Errorf("addresses: listing the pool: %w", err)
	}
	var out []PoolWallet
	for rows.Next() {
		w, err := scanWallet(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("addresses: listing the pool: %w", err)
		}
		out = append(out, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("addresses: listing the pool: %w", err)
	}
	for i := range out {
		if out[i].Lease, err = openLease(ctx, q, out[i].Address); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func getWallet(ctx context.Context, q Queryer, addr Address) (PoolWallet, error) {
	w, err := scanWallet(q.QueryRow(ctx, `
		SELECT address, derivation_index, status, available_after, last_leased_at, created_at
		FROM pool_wallets WHERE address = $1`, string(addr)))
	if errors.Is(err, pgx.ErrNoRows) {
		return PoolWallet{}, fmt.Errorf("%w: %s", ErrWalletNotFound, addr)
	}
	if err != nil {
		return PoolWallet{}, fmt.Errorf("addresses: reading pool wallet %s: %w", addr, err)
	}
	if w.Lease, err = openLease(ctx, q, addr); err != nil {
		return PoolWallet{}, err
	}
	return w, nil
}

func openLease(ctx context.Context, q Queryer, addr Address) (*WatchedAddress, error) {
	wa, err := scanWatchedAddress(q.QueryRow(ctx, selectSQL+` WHERE address = $1 AND status <> 'RETIRED'`, string(addr)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("addresses: reading the open lease on %s: %w", addr, err)
	}
	return &wa, nil
}

func scanWallet(row scanRow) (PoolWallet, error) {
	var w PoolWallet
	var addr string
	var index int64
	if err := row.Scan(&addr, &index, &w.Status, &w.AvailableAfter, &w.LastLeasedAt, &w.CreatedAt); err != nil {
		return PoolWallet{}, err
	}
	w.Address, w.DerivationIndex = Address(addr), uint32(index)
	return w, nil
}

// SetWalletStatus activates or disables a pool wallet. A disabled wallet
// is never leased again, but stays watched (and sweepable).
func SetWalletStatus(ctx context.Context, q Queryer, addr string, active bool) error {
	status := "DISABLED"
	if active {
		status = "ACTIVE"
	}
	tag, err := q.Exec(ctx, `UPDATE pool_wallets SET status = $2 WHERE address = $1`, string(canonical(addr)), status)
	if err != nil {
		return fmt.Errorf("addresses: setting %s %s: %w", addr, status, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrWalletNotFound, addr)
	}
	return nil
}

// LeaseAt returns the lease a deposit to addr at blockTime belongs to:
// the most recent lease that had started by then. found is false when no
// lease on addr had started.
func LeaseAt(ctx context.Context, q Queryer, addr string, blockTime time.Time) (WatchedAddress, bool, error) {
	wa, err := scanWatchedAddress(q.QueryRow(ctx, selectSQL+`
		WHERE address = $1 AND assigned_at <= $2
		ORDER BY assigned_at DESC LIMIT 1`, string(canonical(addr)), blockTime.Add(leaseTimeTolerance)))
	if errors.Is(err, pgx.ErrNoRows) {
		return WatchedAddress{}, false, nil
	}
	if err != nil {
		return WatchedAddress{}, false, fmt.Errorf("addresses: finding the lease on %s at %s: %w", addr, blockTime, err)
	}
	return wa, true, nil
}

// IsPoolWallet reports whether addr is one of the pool's wallets.
func IsPoolWallet(ctx context.Context, q Queryer, addr string) (bool, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM pool_wallets WHERE address = $1`, string(canonical(addr))).Scan(&n); err != nil {
		return false, fmt.Errorf("addresses: checking pool membership of %s: %w", addr, err)
	}
	return n > 0, nil
}

// WatchSet is every address a deposit could arrive at that we care
// about: every pool wallet, every open lease (including legacy
// one-address-per-order leases), and recently retired leases.
func WatchSet(ctx context.Context, q Queryer) ([]Address, error) {
	rows, err := q.Query(ctx, `
		SELECT address FROM pool_wallets
		UNION
		SELECT address FROM watched_addresses WHERE status <> 'RETIRED' OR retired_at > now() - $1::interval
	`, retiredWatchWindow)
	if err != nil {
		return nil, fmt.Errorf("addresses: listing watched addresses: %w", err)
	}
	defer rows.Close()
	var out []Address
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("addresses: listing watched addresses: %w", err)
		}
		out = append(out, canonical(a))
	}
	return out, rows.Err()
}

// ScanFrom is where the next scan of addr starts: its cursor minus
// scanOverlap, or the given fallback when addr has never been scanned.
func ScanFrom(ctx context.Context, q Queryer, addr Address, fallback time.Time) (time.Time, error) {
	var last time.Time
	err := q.QueryRow(ctx, `SELECT last_scanned_at FROM scan_cursors WHERE address = $1`, string(addr)).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("addresses: reading the scan cursor of %s: %w", addr, err)
	}
	return last.Add(-scanOverlap), nil
}

// SetScanCursor records that addr has been scanned up to at. Only ever
// moves forward.
func SetScanCursor(ctx context.Context, q Queryer, addr Address, at time.Time) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO scan_cursors (address, last_scanned_at) VALUES ($1, $2)
		ON CONFLICT (address) DO UPDATE SET last_scanned_at = GREATEST(scan_cursors.last_scanned_at, excluded.last_scanned_at)
	`, string(addr), at); err != nil {
		return fmt.Errorf("addresses: advancing the scan cursor of %s: %w", addr, err)
	}
	return nil
}

// FirstSeen is when addr joined what we watch -- the earliest a deposit to
// it could matter, and where its very first scan starts.
func FirstSeen(ctx context.Context, q Queryer, addr Address) (time.Time, error) {
	var t *time.Time
	err := q.QueryRow(ctx, `
		SELECT LEAST((SELECT created_at FROM pool_wallets WHERE address = $1),
			(SELECT min(assigned_at) FROM watched_addresses WHERE address = $1))
	`, string(addr)).Scan(&t)
	if err != nil {
		return time.Time{}, fmt.Errorf("addresses: finding when %s was first watched: %w", addr, err)
	}
	if t == nil {
		return time.Now().UTC(), nil
	}
	return *t, nil
}
