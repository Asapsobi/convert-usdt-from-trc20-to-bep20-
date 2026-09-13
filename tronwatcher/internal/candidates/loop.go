package candidates

import (
	"context"
	"log/slog"
	"time"

	"tronwatcher/internal/addresses"
	"tronwatcher/internal/chain"
	"tronwatcher/internal/db"
	"tronwatcher/internal/finality"
)

// DefaultInterval mirrors depositwatcher/internal/candidates's own
// default -- there is no reason to scan more often than that at this
// system's real volume (per component-map.md's own ~100 payouts/day
// figure across the whole corridor).
const DefaultInterval = 3 * time.Second

// RunLoop is cmd/tronwatcherd's own engine: on each tick, it lists
// every non-retired watched address, scans each one individually (see
// ScanWatchedAddress), then checks finality once. Blocks until ctx is
// cancelled, returning ctx.Err().
//
// Unlike depositwatcher/internal/candidates.RunLoop, there is no shared
// block-range cursor to advance and no maxBlocksPerTick backlog-chunking
// concern -- each watched address carries its own independent
// last_scanned_at cursor (migration 0003), and TronGrid's own
// /v1/accounts/{address}/transactions/trc20 endpoint is scoped to one
// address at a time regardless, so there is no shared-resource backlog
// that could grow unboundedly the way a single global block cursor
// could.
func RunLoop(ctx context.Context, pool *chain.Pool, database *db.Pool, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, checker finality.FinalityChecker, cfg Config, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	runTick(ctx, pool, database, quotes, tracker, checker, cfg)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			runTick(ctx, pool, database, quotes, tracker, checker, cfg)
		}
	}
}

// runTick scans every active address independently, isolating one
// address's failure from the rest -- the same per-item error-isolation
// discipline dispatcher/internal/orchestrate's own RunTick uses, since a
// single misbehaving address (or a transient provider hiccup) must not
// stall every other order's own progress.
func runTick(ctx context.Context, pool *chain.Pool, database *db.Pool, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, checker finality.FinalityChecker, cfg Config) {
	active, err := addresses.ListActive(ctx, database)
	if err != nil {
		slog.Error("candidates: listing active addresses failed, will retry next tick", "error", err)
		return
	}

	for _, wa := range active {
		if err := ScanWatchedAddress(ctx, pool, database, quotes, tracker, cfg, wa); err != nil {
			slog.Error("candidates: scanning address failed, will retry next tick",
				"order_id", wa.OrderID, "address", wa.Address, "error", err)
		}
	}

	tracker.CheckFinality(ctx, checker)
}
