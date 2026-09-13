package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"tronwatcher/internal/candidates"
	"tronwatcher/internal/chain"
	"tronwatcher/internal/db"
	"tronwatcher/internal/finality"
	"tronwatcher/internal/ledgerclient"
	"tronwatcher/internal/money"
	"tronwatcher/internal/orphaned"
)

// defaultDustFloor mirrors depositwatcher's own example ("< $1 equiv"),
// overridable via TRONWATCHER_DUST_FLOOR.
const defaultDustFloor = money.Amount(1_000000)

// engine bundles the chain-watching pieces: a real multi-provider
// TronGrid pool, a finality.Tracker wired with the REAL
// ledgerclient/orphaned implementations, a finality checker against a
// real TRON node's solidity endpoint, and the one background loop
// (candidates.RunLoop) that makes this a live service rather than just
// an HTTP boundary over a database. Unlike
// depositwatcher/cmd/watcherd's own engine, there is no separate
// ingestion loop -- see internal/candidates.RunLoop's own doc comment
// for why per-address scanning needs no equivalent.
type engine struct {
	chainPool *chain.Pool
	checker   finality.FinalityChecker
	tracker   *finality.Tracker
	ledger    *ledgerclient.Client
	cfg       candidates.Config

	quotes       candidates.QuotedAmountFetcher
	pollInterval time.Duration
}

// newEngineFromEnv builds an engine from TRONWATCHER_PROVIDERS,
// TRONWATCHER_CONTRACT_ADDRESS (optional), TRONWATCHER_FINALITY_BASE_URL
// (optional), TRONWATCHER_LEDGER_BASE_URL, and TRONWATCHER_LEDGER_TOKEN.
// Returns (nil, nil) -- not an error -- if TRONWATCHER_PROVIDERS is
// unset, mirroring depositwatcher's own identical posture: a deployment
// that only wants the HTTP boundary is legitimate.
func newEngineFromEnv(pool *db.Pool) (*engine, error) {
	raw := os.Getenv("TRONWATCHER_PROVIDERS")
	if raw == "" {
		return nil, nil
	}

	apiKey := os.Getenv("TRONWATCHER_API_KEY")

	var providers []*chain.Provider
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, url, ok := strings.Cut(pair, "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("tronwatcherd: malformed TRONWATCHER_PROVIDERS entry %q, want name=url", pair)
		}
		providers = append(providers, chain.NewProvider(chain.Config{Name: name, BaseURL: url, APIKey: apiKey}))
	}
	chainPool, err := chain.NewPool(providers)
	if err != nil {
		return nil, fmt.Errorf("tronwatcherd: building provider pool: %w", err)
	}

	finalityBaseURL := os.Getenv("TRONWATCHER_FINALITY_BASE_URL")
	if finalityBaseURL == "" {
		finalityBaseURL = chain.DefaultBaseURL
	}
	checker := chain.NewFinalityClient(finalityBaseURL)

	ledgerBaseURL := os.Getenv("TRONWATCHER_LEDGER_BASE_URL")
	ledgerToken := os.Getenv("TRONWATCHER_LEDGER_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return nil, errors.New("tronwatcherd: TRONWATCHER_PROVIDERS is set, so a live engine was requested, " +
			"but TRONWATCHER_LEDGER_BASE_URL and TRONWATCHER_LEDGER_TOKEN are also required (the engine reports to a real C1)")
	}
	ledger := ledgerclient.New(ledgerBaseURL, ledgerToken)

	contractAddress := os.Getenv("TRONWATCHER_CONTRACT_ADDRESS")
	if contractAddress == "" {
		contractAddress = chain.USDTTRC20ContractAddress
	}

	dustFloor := defaultDustFloor
	if raw := os.Getenv("TRONWATCHER_DUST_FLOOR"); raw != "" {
		parsed, err := money.ParseDecimal(raw)
		if err != nil {
			return nil, fmt.Errorf("tronwatcherd: TRONWATCHER_DUST_FLOOR: %w", err)
		}
		dustFloor = parsed
	}

	pollInterval := candidates.DefaultInterval
	if raw := os.Getenv("TRONWATCHER_POLL_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("tronwatcherd: TRONWATCHER_POLL_INTERVAL: %w", err)
		}
		pollInterval = parsed
	}

	e := &engine{
		chainPool:    chainPool,
		checker:      checker,
		ledger:       ledger,
		quotes:       ledger,
		pollInterval: pollInterval,
		cfg: candidates.Config{
			ContractAddress: contractAddress,
			DustFloor:       dustFloor,
		},
	}

	tracker, err := finality.New(finality.Config{
		OnFinal:                 ledger.ReportDepositFinal,
		OrphanedDepositRecorder: orphanedRecorder{pool: pool, ledger: ledger},
	})
	if err != nil {
		return nil, fmt.Errorf("tronwatcherd: building finality tracker: %w", err)
	}
	e.tracker = tracker
	return e, nil
}

// run starts the background loop and blocks until ctx is cancelled.
func (e *engine) run(ctx context.Context, pool *db.Pool) {
	go func() {
		err := candidates.RunLoop(ctx, e.chainPool, pool, e.quotes, e.tracker, e.checker, e.cfg, e.pollInterval)
		if err != nil && ctx.Err() == nil {
			// RunLoop only returns non-nil for a cancelled context in
			// normal operation (per-tick errors are logged and retried,
			// never fatal here) -- surfacing anything else loudly rather
			// than letting the process silently stop watching.
			panic(fmt.Sprintf("tronwatcherd: candidate loop exited unexpectedly: %v", err))
		}
	}()
}

// orphanedRecorder implements finality.OrphanedDepositRecorder: fetches
// the order's current state from C1 (best-effort -- "unknown" if even
// that fails) and persists via orphaned.Record.
type orphanedRecorder struct {
	pool   *db.Pool
	ledger *ledgerclient.Client
}

func (o orphanedRecorder) RecordOrphanedDeposit(ctx context.Context, c finality.Candidate, c1Error error) error {
	state := "unknown"
	if order, err := o.ledger.GetOrder(ctx, c.ExternalID); err == nil {
		state = order.State
	}
	return orphaned.Record(ctx, o.pool, orphaned.Deposit{
		OrderID: c.OrderID, ExternalID: c.ExternalID, TxID: c.TxID,
		Amount: int64(c.Amount), DetectedAt: time.Now().UTC(), OrderStateAtDetection: state,
	})
}
