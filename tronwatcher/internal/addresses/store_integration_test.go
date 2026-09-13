//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. Mirrors
// depositwatcher/internal/addresses/store_integration_test.go's own
// setup and coverage exactly -- the schema and Go logic are identical
// (this package's own DeriveAddress difference from depositwatcher's is
// already covered by derive_test.go, not this file).
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

	"tronwatcher/internal/addresses"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TRONWATCHER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TRONWATCHER_TEST_DATABASE_URL not set; skipping integration test")
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

func testXpub(t *testing.T) string {
	t.Helper()
	master, err := bip32.NewMasterKey([]byte("tronwatcher store integration test fixture -- never use"))
	if err != nil {
		t.Fatalf("generating test master key: %v", err)
	}
	return master.PublicKey().B58Serialize()
}

var seq int64
var seqMu sync.Mutex

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
	if !addresses.Valid(addr1) {
		t.Fatalf("Assign produced an invalid TRON address: %s", addr1)
	}

	addr2, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt)
	if err != nil {
		t.Fatalf("second Assign: %v", err)
	}
	if addr1 != addr2 {
		t.Fatalf("replayed Assign returned a different address: %s vs %s", addr1, addr2)
	}
}

func TestAssign_ConcurrentRaceProducesOneRowNoWastedIndex(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	orderID := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()

	const n = 10
	var wg sync.WaitGroup
	results := make([]addresses.Address, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-race", quotedAt, quoteExpiresAt)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Assign failed: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent Assign calls for the same order returned different addresses: %s vs %s", results[0], results[i])
		}
	}
}

func TestGetByOrderID_NotFound(t *testing.T) {
	pool := testPool(t)
	_, err := addresses.GetByOrderID(context.Background(), pool, uniqueOrderID())
	if !errors.Is(err, addresses.ErrOrderNotFound) {
		t.Fatalf("expected ErrOrderNotFound, got %v", err)
	}
}

func TestGetByAddress_ResolvesToOrder(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	orderID := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()

	addr, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	wa, err := addresses.GetByAddress(ctx, pool, addr)
	if err != nil {
		t.Fatalf("GetByAddress: %v", err)
	}
	if wa.OrderID != orderID {
		t.Errorf("expected order %d, got %d", orderID, wa.OrderID)
	}
}

func TestMarkFunded_ThenRetire(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	orderID := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()

	if _, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err := addresses.MarkFunded(ctx, pool, orderID); err != nil {
		t.Fatalf("MarkFunded: %v", err)
	}
	wa, err := addresses.GetByOrderID(ctx, pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if wa.Status != addresses.StatusFunded {
		t.Errorf("expected FUNDED, got %s", wa.Status)
	}

	if err := addresses.Retire(ctx, pool, orderID, "settled"); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	wa, err = addresses.GetByOrderID(ctx, pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if wa.Status != addresses.StatusRetired {
		t.Errorf("expected RETIRED, got %s", wa.Status)
	}
	if wa.RetiredReason == nil || *wa.RetiredReason != "settled" {
		t.Errorf("expected retired_reason=settled, got %v", wa.RetiredReason)
	}
}

func TestRetire_IllegalTransitionFromRetiredRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	orderID := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()

	if _, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err := addresses.Retire(ctx, pool, orderID, "expired"); err != nil {
		t.Fatal(err)
	}
	// RETIRED has no legal outgoing transition -- retiring again (the
	// trigger fires on any actual status change, but RETIRED -> RETIRED
	// is not a change, so this specific call is a no-op, not an error;
	// what IS illegal is RETIRED -> FUNDED, exercised below.
	err := addresses.MarkFunded(ctx, pool, orderID)
	if !errors.Is(err, addresses.ErrIllegalStatusTransition) {
		t.Fatalf("expected ErrIllegalStatusTransition for RETIRED -> FUNDED, got %v", err)
	}
}

func TestListActive_ExcludesRetired(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	orderID := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()

	if _, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt); err != nil {
		t.Fatal(err)
	}

	active, err := addresses.ListActive(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, wa := range active {
		if wa.OrderID == orderID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected newly-assigned address to appear in ListActive")
	}

	if err := addresses.Retire(ctx, pool, orderID, "expired"); err != nil {
		t.Fatal(err)
	}
	active, err = addresses.ListActive(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, wa := range active {
		if wa.OrderID == orderID {
			t.Fatal("expected retired address to be excluded from ListActive")
		}
	}
}
