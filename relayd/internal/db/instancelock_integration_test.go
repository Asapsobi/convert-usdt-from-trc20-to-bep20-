//go:build integration

// Requires a real, reachable Postgres; run via `make test-integration`.
package db_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"relayd/internal/db"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("RELAYD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RELAYD_TEST_DATABASE_URL not set")
	}
	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: url})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestInstanceLock_SecondHolderIsRefused(t *testing.T) {
	ctx := context.Background()
	first, second := openTestPool(t), openTestPool(t)

	lock, err := db.AcquireInstanceLock(ctx, first, "relayd-test-lock", 0)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := db.AcquireInstanceLock(ctx, second, "relayd-test-lock", 0); !errors.Is(err, db.ErrAnotherInstanceRunning) {
		t.Fatalf("second acquire: got %v, want ErrAnotherInstanceRunning", err)
	}

	lock.Release()
	again, err := db.AcquireInstanceLock(ctx, second, "relayd-test-lock", 0)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again.Release()
}

func TestInstanceLock_WaitsForPreviousHolderToExit(t *testing.T) {
	ctx := context.Background()
	first, second := openTestPool(t), openTestPool(t)

	lock, err := db.AcquireInstanceLock(ctx, first, "relayd-test-lock-wait", 0)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		lock.Release()
	}()

	again, err := db.AcquireInstanceLock(ctx, second, "relayd-test-lock-wait", 10*time.Second)
	if err != nil {
		t.Fatalf("waiting acquire: %v", err)
	}
	again.Release()
}

func TestInstanceLock_ReleasedWhenHolderSessionDies(t *testing.T) {
	ctx := context.Background()
	first, second := openTestPool(t), openTestPool(t)

	lock, err := db.AcquireInstanceLock(ctx, first, "relayd-test-lock-crash", 0)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lock.Release()

	// The holder's session dies without unlocking (a crash, a DB restart,
	// a dropped connection).
	if _, err := second.Exec(ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	`); err != nil {
		t.Fatalf("terminating the holder's session: %v", err)
	}

	select {
	case <-lock.Lost():
	case <-time.After(15 * time.Second):
		t.Fatal("holder was never told its lock was lost")
	}

	again, err := db.AcquireInstanceLock(ctx, second, "relayd-test-lock-crash", 10*time.Second)
	if err != nil {
		t.Fatalf("acquire after holder died: %v", err)
	}
	again.Release()
}
