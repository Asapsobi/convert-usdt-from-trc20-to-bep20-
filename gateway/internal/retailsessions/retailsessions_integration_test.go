//go:build integration

package retailsessions_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"gateway/internal/db"
	"gateway/internal/retailcustomers"
	"gateway/internal/retailsessions"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newRetailCustomer registers a fresh retail customer with a
// nanosecond-unique email -- the same "never collide with a leftover
// row in this persistent test database" discipline every other
// integration suite in this repo uses.
func newRetailCustomer(t *testing.T, pool *db.Pool) int64 {
	t.Helper()
	email := fmt.Sprintf("test+%s-%d@example.com", t.Name(), time.Now().UnixNano())
	rc, err := retailcustomers.NewStore(pool).Register(context.Background(), email, "correct-horse-battery")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return rc.ID
}

func TestCreate_ThenValidate_Succeeds(t *testing.T) {
	pool := testPool(t)
	store := retailsessions.NewStore(pool)
	ctx := context.Background()
	rcID := newRetailCustomer(t, pool)
	now := time.Now().UTC()

	sess, rawToken, err := store.Create(ctx, rcID, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rawToken == "" {
		t.Fatal("Create returned an empty raw token")
	}
	if sess.RetailCustomerID != rcID {
		t.Errorf("session RetailCustomerID = %d, want %d", sess.RetailCustomerID, rcID)
	}

	validated, err := store.Validate(ctx, rawToken, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if validated.ID != sess.ID {
		t.Errorf("Validate returned session id %d, want %d", validated.ID, sess.ID)
	}
}

func TestValidate_UnknownTokenFails(t *testing.T) {
	pool := testPool(t)
	store := retailsessions.NewStore(pool)
	if _, err := store.Validate(context.Background(), "rs_never_issued", time.Now().UTC()); err != retailsessions.ErrInvalidOrExpired {
		t.Fatalf("Validate for an unknown token error = %v, want ErrInvalidOrExpired", err)
	}
}

func TestValidate_ExpiredSessionFails(t *testing.T) {
	pool := testPool(t)
	store := retailsessions.NewStore(pool)
	ctx := context.Background()
	rcID := newRetailCustomer(t, pool)
	now := time.Now().UTC()

	_, rawToken, err := store.Create(ctx, rcID, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Validate as of AFTER the session's own DefaultSessionDuration has
	// elapsed -- proves expiry is enforced against the real expires_at
	// column, not just a happy-path check.
	future := now.Add(retailsessions.DefaultSessionDuration + time.Hour)
	if _, err := store.Validate(ctx, rawToken, future); err != retailsessions.ErrInvalidOrExpired {
		t.Fatalf("Validate for an expired session error = %v, want ErrInvalidOrExpired", err)
	}
}

func TestRevoke_ThenValidateFails(t *testing.T) {
	pool := testPool(t)
	store := retailsessions.NewStore(pool)
	ctx := context.Background()
	rcID := newRetailCustomer(t, pool)
	now := time.Now().UTC()

	_, rawToken, err := store.Create(ctx, rcID, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Revoke(ctx, rawToken); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := store.Validate(ctx, rawToken, now); err != retailsessions.ErrInvalidOrExpired {
		t.Fatalf("Validate after Revoke error = %v, want ErrInvalidOrExpired", err)
	}

	// Idempotent: revoking again is a no-op success, not an error.
	if err := store.Revoke(ctx, rawToken); err != nil {
		t.Fatalf("Revoke (2nd, already revoked): %v", err)
	}
}

func TestCreate_TwoSessionsForSameCustomerAreIndependent(t *testing.T) {
	pool := testPool(t)
	store := retailsessions.NewStore(pool)
	ctx := context.Background()
	rcID := newRetailCustomer(t, pool)
	now := time.Now().UTC()

	_, tokenA, err := store.Create(ctx, rcID, now)
	if err != nil {
		t.Fatalf("Create (A): %v", err)
	}
	_, tokenB, err := store.Create(ctx, rcID, now)
	if err != nil {
		t.Fatalf("Create (B): %v", err)
	}
	if tokenA == tokenB {
		t.Fatal("two Create calls for the same customer produced the same token")
	}

	if err := store.Revoke(ctx, tokenA); err != nil {
		t.Fatalf("Revoke (A): %v", err)
	}
	if _, err := store.Validate(ctx, tokenB, now); err != nil {
		t.Fatalf("Validate (B) after revoking A: %v -- revoking one session must not affect another", err)
	}
}
