package candidates

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/db"
	"depositwatcher/internal/finality"
)

// DefaultInterval is how often RunLoop checks for newly-ingested blocks
// to scan for candidates and re-checks finality, when Interval is unset.
// Matched to chain.DefaultIngestionInterval: there is no point checking
// for candidates faster than new blocks can possibly have been ingested.
const DefaultInterval = 3 * time.Second

// maxBlocksPerTick bounds how many blocks runTick's own ScanRange call
// covers at once, regardless of how far behind lastCandidate has
// fallen -- see this constant's own call site for why. Real-traffic
// testing against a public provider's own 20000-result cap on the real
// USDT_BEP20 contract found even 200 blocks intermittently too wide (the
// contract's own Transfer volume varies enough by time of day that a
// fixed block count is an imperfect proxy for log count); 100 blocks
// (~5 minutes of BSC block production) left real margin against THAT
// cap. A second real run (10 Sep 2026) found free providers enforcing a
// much stricter, hard per-call BLOCK-RANGE ceiling regardless of result
// count, and that ceiling varies by provider: 1rpc.io's own free tier
// allows 50 blocks, Alchemy's free tier allows only 10. Lowered to 10 to
// clear the tightest of these with no margin to spare -- a deployment
// paying for a provider with a higher cap on every dimension could
// raise this.
const maxBlocksPerTick = 10

// RunLoop is cmd/watcherd's own engine: on each tick, it scans every
// block internal/chain's own ingestion loop has already vetted (never
// ahead of chain.LastScannedHeight -- ingestion's pre-final-reorg check
// for a height must have already happened before this package trusts a
// log found there) but that this package has not yet scanned for
// Transfer logs, then checks finality once. Blocks until ctx is
// cancelled, returning ctx.Err().
//
// Progress is persisted via chain.SetCandidateScannedHeight, so a
// restart resumes near where it left off rather than replaying the
// entire chain. A brand new cursor (first boot, column still NULL)
// starts from the CURRENT ingested height, not genesis -- there is no
// value in a fresh deployment trying to candidate-scan years of history
// it was never running to observe live.
func RunLoop(ctx context.Context, pool *chain.Pool, database *db.Pool, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := runTick(ctx, pool, database, quotes, tracker, cfg); err != nil {
		slog.Error("candidates: initial tick failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := runTick(ctx, pool, database, quotes, tracker, cfg); err != nil {
				slog.Error("candidates: tick failed", "error", err)
			}
		}
	}
}

func runTick(ctx context.Context, pool *chain.Pool, database *db.Pool, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config) error {
	ingestedHeight, err := chain.LastScannedHeight(ctx, database)
	if err != nil {
		return err
	}

	lastCandidate, found, err := chain.LastCandidateScannedHeight(ctx, database)
	if err != nil {
		return err
	}
	if !found {
		if err := chain.SetCandidateScannedHeight(ctx, database, ingestedHeight); err != nil {
			return err
		}
		lastCandidate = ingestedHeight
	}

	if ingestedHeight > lastCandidate {
		// Capped at maxBlocksPerTick, not the full [lastCandidate+1,
		// ingestedHeight] backlog in one call -- found live wiring the
		// MVP proof run: eth_getLogs against a high-traffic contract
		// (USDT_BEP20 itself, tens of thousands of Transfer events
		// across every holder, not just this watcher's own addresses)
		// exceeds a real RPC provider's own per-call result cap well
		// before this backlog was even large (a few hundred blocks was
		// enough), and with no chunking the range only ever grew every
		// tick, since a failed call never advances the cursor -- a
		// genuinely unrecoverable stall, not a transient error. Capping
		// consumes a large backlog (a fresh deployment, or a watcher
		// that was down for a while) incrementally across several ticks
		// instead.
		toHeight := ingestedHeight
		if toHeight-lastCandidate > maxBlocksPerTick {
			toHeight = lastCandidate + maxBlocksPerTick
		}
		if err := ScanRange(ctx, pool, database, quotes, tracker, cfg, lastCandidate+1, toHeight); err != nil {
			return fmt.Errorf("candidates: scanning [%d,%d]: %w", lastCandidate+1, toHeight, err)
		}
		if err := chain.SetCandidateScannedHeight(ctx, database, toHeight); err != nil {
			return err
		}
	}

	// Design B is only ever reached via cfg.AsyncFinality, itself only
	// ever set true through cmd/watcherd's own double-gated
	// WATCHER_FINALITY_MODE=async + WATCHER_ALLOW_ASYNC_FINALITY=true --
	// every other caller (including every existing test in this
	// package) gets exactly today's CheckFinality call, unchanged.
	if cfg.AsyncFinality {
		if err := tracker.CheckFinalityAsync(ctx, pool); err != nil {
			return fmt.Errorf("candidates: checking finality (async): %w", err)
		}
		return nil
	}
	if err := tracker.CheckFinality(ctx, pool); err != nil {
		return fmt.Errorf("candidates: checking finality: %w", err)
	}
	return nil
}
