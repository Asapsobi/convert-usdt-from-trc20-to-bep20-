//go:build integration

package retailcustomers_test

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

// uniqueEmail keeps email (a real UNIQUE column in a real, persistent
// gateway_test database that is never reset between test runs) from
// colliding with a leftover row from a previous run -- the same
// "time.Now().UnixNano()" discipline orders_integration_test.go's own
// uniqueExternalID already uses for exactly this reason.
func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test+%s-%d@example.com", t.Name(), time.Now().UnixNano())
}

func TestRegister_ThenAuthenticate_Succeeds(t *testing.T) {
	pool := testPool(t)
	store := retailcustomers.NewStore(pool)
	ctx := context.Background()
	email := uniqueEmail(t)

	rc, err := store.Register(ctx, email, "correct-horse-battery")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rc.PasswordHash == "correct-horse-battery" {
		t.Fatal("PasswordHash equals the raw password -- not actually hashed")
	}

	authed, err := store.Authenticate(ctx, email, "correct-horse-battery")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authed.ID != rc.ID {
		t.Errorf("Authenticate returned id %d, want %d", authed.ID, rc.ID)
	}
}

func TestRegister_EmailIsCaseInsensitiveAndTrimmed(t *testing.T) {
	pool := testPool(t)
	store := retailcustomers.NewStore(pool)
	ctx := context.Background()
	base := uniqueEmail(t)

	if _, err := store.Register(ctx, base, "correct-horse-battery"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	upper := "  " + toUpperASCII(base) + "  "
	if _, err := store.Register(ctx, upper, "another-password"); err == nil {
		t.Fatal("expected ErrEmailTaken registering the same email with different case/whitespace")
	} else if err != retailcustomers.ErrEmailTaken {
		t.Fatalf("Register error = %v, want ErrEmailTaken", err)
	}

	if _, err := store.Authenticate(ctx, upper, "correct-horse-battery"); err != nil {
		t.Fatalf("Authenticate with differently-cased email: %v", err)
	}
}

func toUpperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func TestRegister_DuplicateEmailFails(t *testing.T) {
	pool := testPool(t)
	store := retailcustomers.NewStore(pool)
	ctx := context.Background()
	email := uniqueEmail(t)

	if _, err := store.Register(ctx, email, "correct-horse-battery"); err != nil {
		t.Fatalf("Register (1st): %v", err)
	}
	if _, err := store.Register(ctx, email, "different-password"); err != retailcustomers.ErrEmailTaken {
		t.Fatalf("Register (2nd) error = %v, want ErrEmailTaken", err)
	}
}

func TestAuthenticate_WrongPasswordFails(t *testing.T) {
	pool := testPool(t)
	store := retailcustomers.NewStore(pool)
	ctx := context.Background()
	email := uniqueEmail(t)

	if _, err := store.Register(ctx, email, "correct-horse-battery"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := store.Authenticate(ctx, email, "wrong-password"); err != retailcustomers.ErrInvalidCredentials {
		t.Fatalf("Authenticate with wrong password error = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthenticate_NonexistentEmailFailsWithTheSameError(t *testing.T) {
	pool := testPool(t)
	store := retailcustomers.NewStore(pool)
	ctx := context.Background()

	if _, err := store.Authenticate(ctx, "never-registered-"+uniqueEmail(t), "whatever"); err != retailcustomers.ErrInvalidCredentials {
		t.Fatalf("Authenticate for a nonexistent email error = %v, want ErrInvalidCredentials (same as wrong password, never distinguished)", err)
	}
}

func TestSuspend_BlocksAuthenticateEvenWithCorrectPassword(t *testing.T) {
	pool := testPool(t)
	store := retailcustomers.NewStore(pool)
	ctx := context.Background()
	email := uniqueEmail(t)

	rc, err := store.Register(ctx, email, "correct-horse-battery")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store.Suspend(ctx, rc.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if _, err := store.Authenticate(ctx, email, "correct-horse-battery"); err != retailcustomers.ErrSuspended {
		t.Fatalf("Authenticate on a suspended account error = %v, want ErrSuspended", err)
	}

	if err := store.Reactivate(ctx, rc.ID); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if _, err := store.Authenticate(ctx, email, "correct-horse-battery"); err != nil {
		t.Fatalf("Authenticate after Reactivate: %v", err)
	}
}
