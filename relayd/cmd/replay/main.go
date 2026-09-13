// Command replay is R6: the deterministic simulator that drives relayd's
// whole surface (both relay directions' happy flow, R5's own automatic
// refund path, and the UNRECOVERABLE/alert path) through this module's
// own SCENARIO MIX and checks every FINAL ASSERTION afterward. It is
// relayd's own ship gate -- exit code 0 means every assertion held,
// non-zero means it didn't (see the printed report for which one, and
// the seed to reproduce it). See internal/replay/scenarios.go's own
// top-of-file doc comment for exactly how this run's scenario mix
// differs from docs/03-build/model-f-relay-build-prompts.md's original
// R6 wording, and why.
//
// This connects to TWO databases and ONE running service:
//   - RELAYD_DATABASE_URL: this run's own database. Must be empty of
//     everything but the schema (already migrated via `go run
//     ./cmd/migrate up`) -- this run's assertions are only meaningful
//     against a database this run had entirely to itself, the same
//     requirement every prior component's own ship gate has.
//   - LEDGER_DATABASE_URL: a raw connection to a REAL, ALREADY RUNNING
//     ledgerd's database, used only for fixture account creation and
//     journal-balance assertions (there is no public HTTP endpoint for
//     either).
//   - -ledger-url / -ledger-token (or LEDGER_BASE_URL / LEDGER_API_TOKEN):
//     where that running ledgerd is reachable over HTTP. Starting it is
//     this command's caller's job (an operator, or a CI script), not
//     this binary's -- same reasoning every prior component's own
//     cmd/replay gives for not managing its own dependencies'
//     lifecycles.
//
// Nothing here touches a real TRON/BSC node, a real S1, or a real
// upstream swap vendor: every boundary besides C1 is an in-process fake
// (internal/replay/harness.go's own doc comment).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"relayd/internal/db"
	"relayd/internal/replay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg := replay.DefaultConfig()

	seed := flag.Int64("seed", cfg.Seed, "PRNG seed; same seed replays byte-identically (default: time-based)")
	ledgerURL := flag.String("ledger-url", os.Getenv("LEDGER_BASE_URL"), "base URL of a real, already-running ledgerd (or LEDGER_BASE_URL)")
	ledgerToken := flag.String("ledger-token", os.Getenv("LEDGER_API_TOKEN"), "bearer token for that ledgerd (or LEDGER_API_TOKEN)")
	flag.Parse()

	cfg.Seed = *seed
	cfg.LedgerBaseURL = *ledgerURL
	cfg.LedgerToken = *ledgerToken
	if cfg.LedgerBaseURL == "" || cfg.LedgerToken == "" {
		return fmt.Errorf("replay: -ledger-url/-ledger-token (or LEDGER_BASE_URL/LEDGER_API_TOKEN) are required")
	}

	ctx := context.Background()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return fmt.Errorf("replay: connecting to relayd database: %w", err)
	}
	defer pool.Close()

	ledgerDBURL := os.Getenv("LEDGER_DATABASE_URL")
	if ledgerDBURL == "" {
		return fmt.Errorf("replay: LEDGER_DATABASE_URL is not set")
	}
	ledgerPool, err := pgxpool.New(ctx, ledgerDBURL)
	if err != nil {
		return fmt.Errorf("replay: connecting to ledger database: %w", err)
	}
	defer ledgerPool.Close()

	report, runErr := replay.Run(ctx, pool, ledgerPool, cfg)
	if report != nil {
		report.Print(os.Stdout)
	}
	if runErr != nil {
		if report == nil {
			return runErr
		}
		os.Exit(1)
	}
	return nil
}
