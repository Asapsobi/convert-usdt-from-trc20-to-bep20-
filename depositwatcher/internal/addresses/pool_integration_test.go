//go:build integration

package addresses_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"depositwatcher/internal/addresses"
)

// freshPool empties the shared test database's pool (retiring every open
// lease) and sets its limit and cooldowns, so a pool test sees only the
// wallets it creates.
func freshPool(t *testing.T, max int, afterUse, afterExpiry time.Duration) *pgxpool.Pool {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE watched_addresses SET status = 'RETIRED', retired_at = now(), retired_reason = 'test reset' WHERE status <> 'RETIRED'`,
		`DELETE FROM pool_wallets`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("resetting the pool: %v", err)
		}
	}
	if err := addresses.PutPoolSettings(ctx, pool, addresses.PoolSettings{
		MaxWallets: max, CooldownAfterUse: afterUse, CooldownAfterExpiry: afterExpiry,
	}); err != nil {
		t.Fatal(err)
	}
	return pool
}

func assign(t *testing.T, pool *pgxpool.Pool, orderID int64) (addresses.Address, error) {
	t.Helper()
	quotedAt, expiresAt := fixedTimes()
	return addresses.Assign(context.Background(), pool, orderID, fmt.Sprintf("ext-pool-%d", orderID), "cust-pool", quotedAt, expiresAt)
}

// The pool never grows past its limit, and a released wallet is handed to
// the next order once its cooldown has passed.
func TestPool_ReusesWalletsWithinItsLimit(t *testing.T) {
	pool := freshPool(t, 2, 0, time.Hour)
	ctx := context.Background()

	a, b, c := uniqueOrderID(), uniqueOrderID(), uniqueOrderID()
	addrA, err := assign(t, pool, a)
	if err != nil {
		t.Fatal(err)
	}
	addrB, err := assign(t, pool, b)
	if err != nil {
		t.Fatal(err)
	}
	if addrA == addrB {
		t.Fatal("two open leases share one wallet")
	}
	if _, err := assign(t, pool, c); !errors.Is(err, addresses.ErrNoWalletAvailable) {
		t.Fatalf("third order with a full pool: got %v, want ErrNoWalletAvailable", err)
	}

	if err := addresses.Retire(ctx, pool, a, "settled"); err != nil {
		t.Fatal(err)
	}
	addrC, err := assign(t, pool, c)
	if err != nil {
		t.Fatalf("after a release, the next order should get a wallet: %v", err)
	}
	if addrC != addrA {
		t.Fatalf("expected the released wallet %s to be reused, got %s", addrA, addrC)
	}
	wallets, err := addresses.ListPool(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(wallets) != 2 {
		t.Fatalf("expected the pool to stay at 2 wallets, got %d", len(wallets))
	}
}

// An order that expired unpaid starts the long cooldown: its customer may
// still pay late, and that payment must not land in the next order.
func TestPool_ExpiredLeaseCoolsDownBeforeReuse(t *testing.T) {
	pool := freshPool(t, 1, 0, time.Hour)
	ctx := context.Background()

	a, b := uniqueOrderID(), uniqueOrderID()
	if _, err := assign(t, pool, a); err != nil {
		t.Fatal(err)
	}
	if err := addresses.Retire(ctx, pool, a, "expired"); err != nil {
		t.Fatal(err)
	}
	if _, err := assign(t, pool, b); !errors.Is(err, addresses.ErrNoWalletAvailable) {
		t.Fatalf("wallet still cooling down: got %v, want ErrNoWalletAvailable", err)
	}
	// Releasing again must not extend (or reset) the cooldown.
	before, _ := addresses.ListPool(ctx, pool)
	if err := addresses.Retire(ctx, pool, a, "expired"); err != nil {
		t.Fatalf("repeated release: %v", err)
	}
	after, _ := addresses.ListPool(ctx, pool)
	if !before[0].AvailableAfter.Equal(after[0].AvailableAfter) {
		t.Fatal("a repeated release changed the cooldown")
	}
}

// A disabled wallet is never leased, but stays in the pool.
func TestPool_DisabledWalletIsNotLeased(t *testing.T) {
	pool := freshPool(t, 1, 0, 0)
	ctx := context.Background()
	w, err := addresses.ProvisionWallet(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := addresses.SetWalletStatus(ctx, pool, string(w.Address), false); err != nil {
		t.Fatal(err)
	}
	if _, err := assign(t, pool, uniqueOrderID()); !errors.Is(err, addresses.ErrNoWalletAvailable) {
		t.Fatalf("only a disabled wallet exists: got %v, want ErrNoWalletAvailable", err)
	}
	if err := addresses.SetWalletStatus(ctx, pool, string(w.Address), true); err != nil {
		t.Fatal(err)
	}
	if got, err := assign(t, pool, uniqueOrderID()); err != nil || got != w.Address {
		t.Fatalf("re-enabled wallet: got %s, %v", got, err)
	}
}

// A deposit belongs to the lease that was open at its block time.
func TestLeaseAt_AttributesByBlockTime(t *testing.T) {
	pool := freshPool(t, 1, 0, 0)
	ctx := context.Background()

	first, second := uniqueOrderID(), uniqueOrderID()
	addr, err := assign(t, pool, first)
	if err != nil {
		t.Fatal(err)
	}
	during := time.Now()
	if err := addresses.Retire(ctx, pool, first, "settled"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := assign(t, pool, second); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)

	lease, found, err := addresses.LeaseAt(ctx, pool, string(addr), during)
	if err != nil || !found {
		t.Fatalf("LeaseAt(during first): %v, found=%v", err, found)
	}
	// The tolerance for clock skew makes "during the first lease" and "a
	// moment before the second" indistinguishable; far enough apart, the
	// newest lease that had started wins.
	if lease.OrderID != first && lease.OrderID != second {
		t.Fatalf("LeaseAt(during first) = order %d", lease.OrderID)
	}
	lease, found, err = addresses.LeaseAt(ctx, pool, string(addr), later)
	if err != nil || !found || lease.OrderID != second {
		t.Fatalf("LeaseAt(later) = order %d found=%v err=%v, want %d", lease.OrderID, found, err, second)
	}
	if _, found, _ := addresses.LeaseAt(ctx, pool, string(addr), time.Now().Add(-24*time.Hour)); found {
		t.Fatal("a payment before any lease on this wallet started was attributed to one")
	}
}
