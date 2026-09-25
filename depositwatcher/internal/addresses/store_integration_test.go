//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. Everything in this file needs the real
// database -- the migration's trigger enforcement in particular can't be
// exercised any other way, by design (that's the whole point of pushing
// the check into Postgres rather than trusting Go alone).
package addresses_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/tyler-smith/go-bip32"

	"depositwatcher/internal/addresses"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

// testPool applies migrations, returns a fresh pgxpool, and configures
// package addresses with a real (fixture) extended public key so Assign
// is usable without every test calling Configure itself.
//
// Every test in this file runs sequentially (none call t.Parallel()) --
// deliberately, since addresses.Configure sets package-level state shared
// across the whole test binary. TestAssign_NotConfigured is the one test
// that manipulates that state directly instead of going through this
// helper; keeping everything else sequential is what makes reasoning
// about that safe.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := testDatabaseURL(t)

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir(t)); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	// The pool limit and cooldowns are exercised by their own tests; here
	// every order simply gets a free wallet, as with one wallet per order.
	if _, err := sqlDB.Exec(`UPDATE pool_settings SET max_wallets = 1000000, cooldown_after_use = '0', cooldown_after_expiry = '0'`); err != nil {
		t.Fatalf("relaxing pool settings for tests: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := addresses.Configure(testXpub(t)); err != nil {
		t.Fatalf("configuring: %v", err)
	}
	return pool
}

// testXpub returns a real, valid extended PUBLIC key from an obviously
// fake, hardcoded seed -- same fixture-generation approach as
// derive_test.go, for the same reason: this secures nothing, it exists
// purely to exercise this package's own code against a real key shape.
func testXpub(t *testing.T) string {
	t.Helper()
	master, err := bip32.NewMasterKey([]byte("depositwatcher store integration test fixture -- never use"))
	if err != nil {
		t.Fatalf("generating test master key: %v", err)
	}
	return master.PublicKey().B58Serialize()
}

var seq int64
var seqMu sync.Mutex

// uniqueOrderID hands out a fresh, never-repeated order id within one test
// run, so tests can run against a shared database without colliding on
// watched_addresses' unique order_id constraint.
func uniqueOrderID() int64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return time.Now().UnixNano() + seq
}

func fixedTimes() (quotedAt, quoteExpiresAt time.Time) {
	now := time.Now().UTC().Truncate(time.Second)
	return now, now.Add(90 * time.Second)
}

func TestAssign_IdempotentOnOrderID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	orderID := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()

	addr1, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt)
	if err != nil {
		t.Fatalf("first Assign: %v", err)
	}

	addr2, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt)
	if err != nil {
		t.Fatalf("second Assign: %v", err)
	}
	if addr1 != addr2 {
		t.Fatalf("replayed Assign returned a different address: %s vs %s", addr1, addr2)
	}

	// A brand-new order's index must be exactly one more than what the
	// FIRST Assign call above consumed -- proving the replay never burned
	// a second index. If it had, this new order would land two ahead
	// instead of one.
	nextOrderID := uniqueOrderID()
	quotedAt2, quoteExpiresAt2 := fixedTimes()
	if _, err := addresses.Assign(ctx, pool, nextOrderID, fmt.Sprintf("ext-%d", nextOrderID), "cust-2", quotedAt2, quoteExpiresAt2); err != nil {
		t.Fatalf("assigning the next order: %v", err)
	}
	wa1, err := addresses.GetByOrderID(ctx, pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	wa2, err := addresses.GetByOrderID(ctx, pool, nextOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if wa2.DerivationIndex != wa1.DerivationIndex+1 {
		t.Fatalf("expected the next order to consume exactly one index after the first "+
			"(got %d after %d) -- the replayed Assign call must have burned an extra one",
			wa2.DerivationIndex, wa1.DerivationIndex)
	}
}

func TestAssign_ConcurrentDistinctOrders(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const n = 100
	orderIDs := make([]int64, n)
	base := uniqueOrderID()
	for i := range orderIDs {
		orderIDs[i] = base + int64(i)*1000 // spaced out, still guaranteed unique
	}

	addrs := make([]addresses.Address, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, orderID := range orderIDs {
		wg.Add(1)
		go func(i int, orderID int64) {
			defer wg.Done()
			quotedAt, quoteExpiresAt := fixedTimes()
			addrs[i], errs[i] = addresses.Assign(ctx, pool, orderID,
				fmt.Sprintf("concurrent-ext-%d", orderID), "cust-concurrent", quotedAt, quoteExpiresAt)
		}(i, orderID)
	}
	wg.Wait()

	seenAddr := make(map[addresses.Address]bool, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("order %d: %v", orderIDs[i], err)
		}
		if seenAddr[addrs[i]] {
			t.Fatalf("duplicate address %s returned for order %d", addrs[i], orderIDs[i])
		}
		seenAddr[addrs[i]] = true
	}
	if len(seenAddr) != n {
		t.Fatalf("expected %d distinct addresses, got %d", n, len(seenAddr))
	}

	seenIndex := make(map[uint32]bool, n)
	for _, orderID := range orderIDs {
		wa, err := addresses.GetByOrderID(ctx, pool, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if seenIndex[wa.DerivationIndex] {
			t.Fatalf("duplicate derivation_index %d among the 100 concurrent orders", wa.DerivationIndex)
		}
		seenIndex[wa.DerivationIndex] = true
	}
	if len(seenIndex) != n {
		t.Fatalf("expected %d distinct derivation indices, got %d", n, len(seenIndex))
	}
}

func TestTransitions_LegalPaths(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	t.Run("watching to funded to retired", func(t *testing.T) {
		orderID := uniqueOrderID()
		quotedAt, quoteExpiresAt := fixedTimes()
		if _, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust", quotedAt, quoteExpiresAt); err != nil {
			t.Fatal(err)
		}
		if err := addresses.MarkFunded(ctx, pool, orderID); err != nil {
			t.Fatalf("WATCHING -> FUNDED: %v", err)
		}
		wa, err := addresses.GetByOrderID(ctx, pool, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if wa.Status != addresses.StatusFunded {
			t.Fatalf("expected FUNDED, got %s", wa.Status)
		}

		if err := addresses.Retire(ctx, pool, orderID, "settled"); err != nil {
			t.Fatalf("FUNDED -> RETIRED: %v", err)
		}
		wa, err = addresses.GetByOrderID(ctx, pool, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if wa.Status != addresses.StatusRetired {
			t.Fatalf("expected RETIRED, got %s", wa.Status)
		}
		if wa.RetiredAt == nil || wa.RetiredReason == nil || *wa.RetiredReason != "settled" {
			t.Fatalf("expected retired_at and retired_reason='settled' to be set, got %+v", wa)
		}
	})

	t.Run("watching directly to retired", func(t *testing.T) {
		orderID := uniqueOrderID()
		quotedAt, quoteExpiresAt := fixedTimes()
		if _, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust", quotedAt, quoteExpiresAt); err != nil {
			t.Fatal(err)
		}
		if err := addresses.Retire(ctx, pool, orderID, "expired"); err != nil {
			t.Fatalf("WATCHING -> RETIRED directly: %v", err)
		}
		wa, err := addresses.GetByOrderID(ctx, pool, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if wa.Status != addresses.StatusRetired {
			t.Fatalf("expected RETIRED, got %s", wa.Status)
		}
	})

	t.Run("funded back to watching, legal at the DB level though no Go function drives it yet", func(t *testing.T) {
		orderID := uniqueOrderID()
		quotedAt, quoteExpiresAt := fixedTimes()
		if _, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust", quotedAt, quoteExpiresAt); err != nil {
			t.Fatal(err)
		}
		if err := addresses.MarkFunded(ctx, pool, orderID); err != nil {
			t.Fatal(err)
		}
		// C2.1 doesn't add a Go function for this transition -- "once
		// C2.6 exists to drive it" -- so this proves the DB permits it
		// via the same raw SQL C2.6 will eventually issue.
		_, err := pool.Exec(ctx, `UPDATE watched_addresses SET status = 'WATCHING' WHERE order_id = $1`, orderID)
		if err != nil {
			t.Fatalf("FUNDED -> WATCHING should be legal at the DB level: %v", err)
		}
		wa, err := addresses.GetByOrderID(ctx, pool, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if wa.Status != addresses.StatusWatching {
			t.Fatalf("expected WATCHING, got %s", wa.Status)
		}
	})
}

func TestTransitions_IllegalRejectedAtDBLevel(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	orderID := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()

	if _, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust", quotedAt, quoteExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err := addresses.Retire(ctx, pool, orderID, "expired"); err != nil {
		t.Fatal(err)
	}

	// Through the Go API: MarkFunded on a RETIRED address.
	err := addresses.MarkFunded(ctx, pool, orderID)
	if !errors.Is(err, addresses.ErrIllegalStatusTransition) {
		t.Fatalf("expected ErrIllegalStatusTransition via MarkFunded, got %v", err)
	}

	// Bypassing Go entirely: raw SQL attempting the same illegal
	// transition. This is the "not just in Go" half of the acceptance
	// criterion -- the trigger must reject it even when nothing in this
	// package's own code is involved in the attempt.
	_, err = pool.Exec(ctx, `UPDATE watched_addresses SET status = 'WATCHING' WHERE order_id = $1`, orderID)
	if err == nil {
		t.Fatal("expected raw SQL RETIRED -> WATCHING to be rejected by the database trigger, but it succeeded")
	}
}

func TestListActive_ExcludesRetired(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	active := uniqueOrderID()
	retired := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()
	if _, err := addresses.Assign(ctx, pool, active, fmt.Sprintf("ext-active-%d", active), "cust", quotedAt, quoteExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := addresses.Assign(ctx, pool, retired, fmt.Sprintf("ext-retired-%d", retired), "cust", quotedAt, quoteExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err := addresses.Retire(ctx, pool, retired, "expired"); err != nil {
		t.Fatal(err)
	}

	list, err := addresses.ListActive(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var sawActive, sawRetired bool
	for _, wa := range list {
		if wa.OrderID == active {
			sawActive = true
		}
		if wa.OrderID == retired {
			sawRetired = true
		}
	}
	if !sawActive {
		t.Fatal("ListActive did not include the still-active order")
	}
	if sawRetired {
		t.Fatal("ListActive included a retired order")
	}
}

// TestAssign_NotConfigured lives in store_test.go, not here: xpub is
// unexported, so exercising "never configured" by actually clearing it
// requires being inside package addresses (this file is addresses_test,
// same restriction any other external importer has) -- and the check
// itself runs before Assign ever touches a Queryer, so it needs no
// database at all. See store_test.go.
