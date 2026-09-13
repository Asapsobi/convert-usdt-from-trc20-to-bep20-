//go:build integration

package orphaned_test

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

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"tronwatcher/internal/orphaned"
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
	return pool
}

func uniqueTxID() string {
	return fmt.Sprintf("tx-%d", time.Now().UnixNano())
}

func TestRecord_IdempotentOnTxID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	txID := uniqueTxID()

	d := orphaned.Deposit{
		OrderID: 1, ExternalID: "ext-1", TxID: txID, Amount: 1_000000,
		DetectedAt: time.Now().UTC(), OrderStateAtDetection: "expired",
	}
	if err := orphaned.Record(ctx, pool, d); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := orphaned.Record(ctx, pool, d); err != nil {
		t.Fatalf("second Record (replay): %v", err)
	}

	rows, err := orphaned.List(ctx, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range rows {
		if r.TxID == txID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row for %s after a replayed Record, got %d", txID, count)
	}
}

func TestGet_NotFound(t *testing.T) {
	pool := testPool(t)
	_, err := orphaned.Get(context.Background(), pool, -1)
	if !errors.Is(err, orphaned.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestResolve_ThenAlreadyResolved(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	txID := uniqueTxID()

	d := orphaned.Deposit{
		OrderID: 2, ExternalID: "ext-2", TxID: txID, Amount: 5_000000,
		DetectedAt: time.Now().UTC(), OrderStateAtDetection: "expired",
	}
	if err := orphaned.Record(ctx, pool, d); err != nil {
		t.Fatal(err)
	}
	rows, err := orphaned.List(ctx, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, r := range rows {
		if r.TxID == txID {
			id = r.ID
		}
	}
	if id == 0 {
		t.Fatal("could not find the recorded row")
	}

	resolved, err := orphaned.Resolve(ctx, pool, id, "refunded manually", "operator-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Resolution == nil || *resolved.Resolution != "refunded manually" {
		t.Errorf("expected resolution to be set, got %v", resolved.Resolution)
	}
	if resolved.ResolvedBy == nil || *resolved.ResolvedBy != "operator-1" {
		t.Errorf("expected resolved_by=operator-1, got %v", resolved.ResolvedBy)
	}

	_, err = orphaned.Resolve(ctx, pool, id, "again", "operator-2")
	if !errors.Is(err, orphaned.ErrAlreadyResolved) {
		t.Fatalf("expected ErrAlreadyResolved on a second resolve, got %v", err)
	}
}

func TestList_FiltersByResolved(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	txID := uniqueTxID()

	d := orphaned.Deposit{
		OrderID: 3, ExternalID: "ext-3", TxID: txID, Amount: 2_000000,
		DetectedAt: time.Now().UTC(), OrderStateAtDetection: "expired",
	}
	if err := orphaned.Record(ctx, pool, d); err != nil {
		t.Fatal(err)
	}

	unresolvedTrue := true
	unresolved, err := orphaned.List(ctx, pool, &unresolvedTrue)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range unresolved {
		if r.TxID == txID {
			t.Fatal("expected freshly-recorded deposit to be excluded from resolved=true filter")
		}
	}

	resolvedFalse := false
	pending, err := orphaned.List(ctx, pool, &resolvedFalse)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range pending {
		if r.TxID == txID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected freshly-recorded deposit to appear in resolved=false filter")
	}
}
