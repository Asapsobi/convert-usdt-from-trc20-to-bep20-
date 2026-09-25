//go:build integration

// Requires a real, reachable Postgres 16 instance (WATCHER_TEST_DATABASE_URL
// and LEDGER_TEST_DATABASE_URL) AND a sibling checkout of the ledger module
// at ../../../ledger -- the same usdt-settlement-corridor layout this whole
// project already uses. Builds and runs a REAL ledgerd binary as a
// subprocess, exactly as C2.6/C2.7's own integration tests do. Run via
// `make test-integration` (build tag "integration").
package replay_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"depositwatcher/internal/replay"
)

const (
	replayLedgerListenAddr = ":18436"
	replayLedgerBaseURL    = "http://localhost:18436"
	replayLedgerToken      = "c2.10-replay-test-token"
	replayLedgerActor      = "watcher"
)

// startReplayLedgerd builds and starts a real ledgerd subprocess against
// ledgerDBURL, same pattern as ledgerclient_integration_test.go's
// startLiveLedger.
func startReplayLedgerd(t *testing.T, ledgerDBURL string) {
	t.Helper()
	ledgerRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "ledger"))
	if err != nil {
		t.Fatalf("resolving ledger module path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ledgerRoot, "go.mod")); err != nil {
		t.Skipf("no sibling ledger module found at %s; skipping", ledgerRoot)
	}

	tmpDir := t.TempDir()
	migrateBin := filepath.Join(tmpDir, "migrate_bin")
	ledgerdBin := filepath.Join(tmpDir, "ledgerd_bin")

	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = ledgerRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, output)
		}
	}
	build(migrateBin, "./cmd/migrate")
	build(ledgerdBin, "./cmd/ledgerd")

	migrate := exec.Command(migrateBin, "up")
	migrate.Dir = ledgerRoot
	migrate.Env = append(os.Environ(), "LEDGER_DATABASE_URL="+ledgerDBURL)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("running ledger migrations: %v\n%s", err, output)
	}

	ledgerd := exec.Command(ledgerdBin)
	ledgerd.Dir = ledgerRoot
	ledgerd.Env = append(os.Environ(),
		"LEDGER_DATABASE_URL="+ledgerDBURL,
		"LEDGER_API_TOKENS="+replayLedgerToken+":"+replayLedgerActor,
		"LEDGER_LISTEN_ADDR="+replayLedgerListenAddr,
	)
	var logs bytes.Buffer
	ledgerd.Stdout = &logs
	ledgerd.Stderr = &logs
	if err := ledgerd.Start(); err != nil {
		t.Fatalf("starting ledgerd: %v", err)
	}
	t.Cleanup(func() {
		_ = ledgerd.Process.Kill()
		_ = ledgerd.Wait()
		if t.Failed() {
			t.Logf("ledgerd output:\n%s", logs.String())
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(replayLedgerBaseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("ledgerd never became healthy within the deadline")
}

// freshIsolatedWatcherPool creates a throwaway database on the same
// cluster as WATCHER_TEST_DATABASE_URL, migrated fresh, and returns a
// pool for it. Same reasoning as C1.9's own freshIsolatedPool: this
// run's FINAL ASSERTIONS (no duplicate successful report anywhere, no
// orphaned_deposits row ever auto-resolved) are only meaningful against
// a database this run had entirely to itself -- sharing watcher_test with
// whatever else runs concurrently under `go test ./...` would let stale
// rows from other packages' own tests produce a false assertion failure.
func freshIsolatedWatcherPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseURL := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parsing WATCHER_TEST_DATABASE_URL: %v", err)
	}
	adminURL := *u
	adminURL.Path = "/postgres"
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connecting to admin database: %v", err)
	}
	dbName := fmt.Sprintf("watcher_replay_isolated_%d", time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("creating isolated database: %v", err)
	}
	adminPool.Close()

	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), adminURL.String())
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})

	freshURL := *u
	freshURL.Path = "/" + dbName

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", freshURL.String())
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("running watcher migrations: %v", err)
	}
	// The pool limit and cooldowns are exercised by their own tests; here
	// every order simply gets a free wallet, as with one wallet per order.
	if _, err := sqlDB.Exec(`UPDATE pool_settings SET max_wallets = 1000000, cooldown_after_use = '0', cooldown_after_expiry = '0'`); err != nil {
		t.Fatalf("relaxing pool settings for tests: %v", err)
	}

	pool, err := pgxpool.New(ctx, freshURL.String())
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRun_AllScenariosAndAssertionsPass(t *testing.T) {
	watcherPool := freshIsolatedWatcherPool(t)

	ledgerDBURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if ledgerDBURL == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	startReplayLedgerd(t, ledgerDBURL)

	ledgerPool, err := pgxpool.New(context.Background(), ledgerDBURL)
	if err != nil {
		t.Fatalf("connecting to ledger database: %v", err)
	}
	defer ledgerPool.Close()

	cfg := replay.Config{
		Seed:          1788253816246464000,
		LedgerBaseURL: replayLedgerBaseURL,
		LedgerToken:   replayLedgerToken,
	}

	report, err := replay.Run(context.Background(), watcherPool, ledgerPool, cfg)
	if report == nil {
		t.Fatalf("Run returned a nil report: %v", err)
	}

	var buf bytes.Buffer
	report.Print(&buf)
	if !report.Passed() {
		t.Fatalf("replay harness did not pass:\n%s", buf.String())
	}
	if err != nil {
		t.Fatalf("Run returned an error despite Passed()=true (should not happen): %v\n%s", err, buf.String())
	}
	t.Logf("replay harness report:\n%s", buf.String())
}
