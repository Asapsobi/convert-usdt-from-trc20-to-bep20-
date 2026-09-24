//go:build integration

package httpapi

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"gateway/internal/db"
)

// validTronRecipient is a real, checksum-valid TRON base58check address
// -- shared across this package's own integration tests wherever a
// syntactically-real (not necessarily meaningful) TRC20 destination is
// needed. This is dispatcher/internal/txbuild's own USDTContractAddress
// constant, reused here purely because it's already independently
// verified real (see that package's own doc comment), not because
// these tests care about USDT's own contract -- picking an
// already-proven-correct address removes any risk of a hand-typed
// checksum being wrong. internal/tronaddr now validates every
// recipient_address for real (base58check, not a prefix guess), so a
// placeholder string like the old "TRecipient"/"TAddr" no longer
// passes -- these tests intentionally do not use fake addresses to
// avoid weakening that check to make themselves pass.
const validTronRecipient = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

// testPool applies every migration against GATEWAY_TEST_DATABASE_URL
// and returns a real, connected *db.Pool -- same convention every
// sibling component's own integration suite uses.
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
