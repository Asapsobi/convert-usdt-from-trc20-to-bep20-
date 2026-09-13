//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package relay_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"relayd/internal/db"
	"relayd/internal/money"
	"relayd/internal/relay"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("RELAYD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RELAYD_TEST_DATABASE_URL not set; skipping integration test")
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

func testStore(t *testing.T) *relay.Store {
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

	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return relay.NewStore(pool)
}

var seq int64

func uniqueExternalID(t *testing.T) string {
	seq++
	return fmt.Sprintf("ext:%s:%d:%d", t.Name(), time.Now().UnixNano(), seq)
}

func testLeg(t *testing.T) relay.Leg {
	return relay.Leg{
		ExternalID:         uniqueExternalID(t),
		OrderID:            1,
		Direction:          relay.TRC20ToBEP20,
		CustomerID:         "cust-1",
		DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:     "Trelayd-deposit-address",
		AmountIn:           money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:  money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}
}

func TestCreate_IdempotentOnExternalID(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	l := testLeg(t)

	first, err := store.Create(ctx, l)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if first.Status != relay.StatusAwaitingDeposit {
		t.Errorf("expected AWAITING_DEPOSIT, got %s", first.Status)
	}

	second, err := store.Create(ctx, l)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("replayed Create returned a different row: %d vs %d", first.ID, second.ID)
	}
}

func TestFullHappyPathTransitions(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	l := testLeg(t)

	created, err := store.Create(ctx, l)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.MarkForwarding(ctx, created.ExternalID, "mock", "mock-order-1", "Tupstream-deposit-addr"); err != nil {
		t.Fatalf("MarkForwarding: %v", err)
	}
	got, err := store.GetByExternalID(ctx, created.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != relay.StatusForwarding {
		t.Errorf("expected FORWARDING, got %s", got.Status)
	}
	if got.UpstreamOrderID == nil || *got.UpstreamOrderID != "mock-order-1" {
		t.Errorf("expected upstream_order_id to be recorded, got %v", got.UpstreamOrderID)
	}

	if err := store.MarkForwarded(ctx, created.ExternalID, "forward-tx-abc"); err != nil {
		t.Fatalf("MarkForwarded: %v", err)
	}
	got, err = store.GetByExternalID(ctx, created.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != relay.StatusForwarded {
		t.Errorf("expected FORWARDED, got %s", got.Status)
	}

	actual := money.Amount{Asset: money.USDT_BEP20, Units: 99_650000}
	if err := store.MarkSettled(ctx, created.ExternalID, actual); err != nil {
		t.Fatalf("MarkSettled: %v", err)
	}
	got, err = store.GetByExternalID(ctx, created.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != relay.StatusSettled {
		t.Errorf("expected SETTLED, got %s", got.Status)
	}
	if got.AmountOutActual == nil || *got.AmountOutActual != actual {
		t.Errorf("expected amount_out_actual to be recorded, got %v", got.AmountOutActual)
	}
}

func TestMarkTransition_IdempotentReplay(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	l := testLeg(t)
	created, err := store.Create(ctx, l)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.MarkForwarding(ctx, created.ExternalID, "mock", "order-1", "Taddr"); err != nil {
		t.Fatal(err)
	}
	// A second call is a safe replay, not an error.
	if err := store.MarkForwarding(ctx, created.ExternalID, "mock", "order-1", "Taddr"); err != nil {
		t.Fatalf("expected replayed MarkForwarding to succeed, got %v", err)
	}
}

func TestMarkTransition_WrongStatusRejected(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	l := testLeg(t)
	created, err := store.Create(ctx, l)
	if err != nil {
		t.Fatal(err)
	}

	// Still AWAITING_DEPOSIT -- MarkForwarded (which expects FORWARDING)
	// must be rejected, not silently applied.
	err = store.MarkForwarded(ctx, created.ExternalID, "some-tx")
	if !errors.Is(err, relay.ErrNotInExpectedStatus) {
		t.Fatalf("expected ErrNotInExpectedStatus, got %v", err)
	}
}

func TestMarkFailed_FromEitherForwardingOrForwarded(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	l1 := testLeg(t)
	created1, err := store.Create(ctx, l1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForwarding(ctx, created1.ExternalID, "mock", "order-a", "Taddr"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFailed(ctx, created1.ExternalID); err != nil {
		t.Fatalf("MarkFailed from FORWARDING: %v", err)
	}

	l2 := testLeg(t)
	created2, err := store.Create(ctx, l2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForwarding(ctx, created2.ExternalID, "mock", "order-b", "Taddr"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForwarded(ctx, created2.ExternalID, "tx-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFailed(ctx, created2.ExternalID); err != nil {
		t.Fatalf("MarkFailed from FORWARDED: %v", err)
	}
}

func TestListByStatus(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	l := testLeg(t)
	created, err := store.Create(ctx, l)
	if err != nil {
		t.Fatal(err)
	}

	awaiting, err := store.ListByStatus(ctx, relay.StatusAwaitingDeposit)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, leg := range awaiting {
		if leg.ExternalID == created.ExternalID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected newly-created leg to appear in ListByStatus(AWAITING_DEPOSIT)")
	}
}

func TestUpstreamOrderUniqueConstraint(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	l1 := testLeg(t)
	created1, err := store.Create(ctx, l1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForwarding(ctx, created1.ExternalID, "mock", "dup-order-id", "Taddr"); err != nil {
		t.Fatal(err)
	}

	l2 := testLeg(t)
	created2, err := store.Create(ctx, l2)
	if err != nil {
		t.Fatal(err)
	}
	// A second leg claiming the SAME (provider_name, upstream_order_id)
	// must be rejected by the DB's own unique index -- R3's own
	// acceptance criterion: a duplicate CreateOrder response for the
	// same logical order must never silently overwrite another leg's
	// own row.
	err = store.MarkForwarding(ctx, created2.ExternalID, "mock", "dup-order-id", "Taddr")
	if err == nil {
		t.Fatal("expected a unique-constraint violation for a duplicate (provider_name, upstream_order_id)")
	}
}
