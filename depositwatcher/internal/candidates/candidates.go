// Package candidates is the wiring none of C2.1/C2.2/C2.4/C2.5 explicitly
// owned: turning "a range of blocks has been ingested" into "these
// Transfer logs are trackable deposit candidates, tracked toward
// finality; these others are not, and here is why." Built while
// implementing C2.10, whose replay harness cannot exercise "the whole
// C2 pipeline" without this existing somewhere -- it is real production
// code, not harness-only glue, even though no earlier chunk named it.
package candidates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/chain"
	"depositwatcher/internal/db"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/money"
	"depositwatcher/internal/orphaned"
)

// QuotedAmountFetcher is the one fact this pipeline needs from C1 that it
// has no local copy of: an order's quoted amount_in. C2.1's
// watched_addresses deliberately stores no amount at all ("no ledger of
// money," per the build spec's WHAT C2 IS NOT), so classification has to
// reach out for it -- ledgerclient.Client.QuotedAmount implements this;
// behind an interface so scanning is testable without a live C1.
type QuotedAmountFetcher interface {
	QuotedAmount(ctx context.Context, externalID string) (money.Amount, error)
}

// Config scopes what ScanRange looks for and how it classifies what it
// finds. All three fields are required -- a value this load-bearing must
// never be left as an accidental zero value.
type Config struct {
	ContractAddress common.Address
	TransferTopic   common.Hash
	DustFloor       money.Amount

	// AsyncFinality selects Design B (finality.Tracker.CheckFinalityAsync)
	// over the default Design A (finality.Tracker.CheckFinality) in
	// runTick. False (the zero value) means Design A -- byte-identical
	// to this package's pre-Phase-2 behavior -- so every existing
	// deployment that never sets this explicitly is completely
	// unaffected. Set via cmd/watcherd's own WATCHER_FINALITY_MODE /
	// WATCHER_ALLOW_ASYNC_FINALITY double gate, never a bare default.
	AsyncFinality bool
}

// ScanRange fetches Transfer logs in [fromHeight, toHeight] via
// pool.LogsAt (2-provider agreement, invariant 5) and resolves each one
// into exactly one of four outcomes:
//
//   - Not in the address book at all: expected noise on a shared chain,
//     ignored entirely -- not even logged as an anomaly, per the C2.4
//     build spec's own acceptance criterion.
//   - Address RETIRED: a late deposit, a distinct case handed to C2.8's
//     own orphaned_deposits mechanism (reused here, since "money arrived
//     with nowhere to put it" is the same category whether C1 rejected
//     an already-final report or the address was simply retired first).
//   - ParseTransferLog fails (ErrWrongToken): logged as an anomaly,
//     never reaches classification -- "should be near-impossible given
//     the filter," since LogsAt's own query already scopes by contract
//     address and this exact topic.
//   - Address WATCHING or FUNDED: classified against the order's quoted
//     amount and handed to tracker.OnLogObserved -- deposit.detected
//     fires there, immediately, before finality is ever considered.
func ScanRange(ctx context.Context, pool *chain.Pool, database db.Queryer, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, fromHeight, toHeight uint64) error {
	if fromHeight > toHeight {
		return nil
	}

	logs, err := pool.LogsAt(ctx, fromHeight, toHeight, cfg.ContractAddress, [][]common.Hash{{cfg.TransferTopic}})
	if err != nil {
		return fmt.Errorf("candidates: fetching logs [%d,%d]: %w", fromHeight, toHeight, err)
	}

	blockTimes := make(map[uint64]time.Time)
	for _, log := range logs {
		if err := processLog(ctx, pool, database, quotes, tracker, cfg, log, blockTimes); err != nil {
			return fmt.Errorf("candidates: processing log %s:%d: %w", log.TxHash, log.Index, err)
		}
	}
	return nil
}

func processLog(ctx context.Context, pool *chain.Pool, database db.Queryer, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, log types.Log, blockTimes map[uint64]time.Time) error {
	from, to, amount, err := chain.ParseTransferLog(log)
	if err != nil {
		if errors.Is(err, chain.ErrWrongToken) {
			slog.Warn("candidates: log does not match the Transfer shape, filtered before classification",
				"tx_hash", log.TxHash, "log_index", log.Index, "error", err)
			return nil
		}
		return err
	}

	wa, err := addresses.GetByAddress(ctx, database, addresses.Address(to.Hex()))
	if err != nil {
		if errors.Is(err, addresses.ErrAddressNotFound) {
			return nil // expected chain noise, not a signal
		}
		return err
	}

	if wa.Status == addresses.StatusRetired {
		return recordLateDeposit(ctx, database, wa, log, amount)
	}

	quoted, err := quotes.QuotedAmount(ctx, wa.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching quoted amount for %s: %w", wa.ExternalID, err)
	}
	classification := chain.ClassifyAgainstOrder(amount, quoted, cfg.DustFloor)

	blockTime, err := blockTimeFor(ctx, pool, log.BlockNumber, blockTimes)
	if err != nil {
		return err
	}

	observed := finality.ObservedLog{
		TxHash: log.TxHash, LogIndex: uint(log.Index), Height: log.BlockNumber, BlockTime: blockTime,
		OrderID: wa.OrderID, ExternalID: wa.ExternalID, CustomerID: wa.CustomerID, Amount: amount,
		SenderAddress: from.Hex(),
	}
	return tracker.OnLogObserved(ctx, observed, classification)
}

// blockTimeFor fetches the block timestamp for height via the primary
// provider only (chain.HeaderByNumber), the same primary-only convention
// C2.3's own advisory reads use -- this is invariant 7's chain-time
// requirement, not a finality decision, so it does not need 2-provider
// agreement. Cached per ScanRange call: several logs in the same block
// share one header fetch.
func blockTimeFor(ctx context.Context, pool *chain.Pool, height uint64, cache map[uint64]time.Time) (time.Time, error) {
	if t, ok := cache[height]; ok {
		return t, nil
	}
	header, err := pool.HeaderByNumber(ctx, height)
	if err != nil {
		return time.Time{}, fmt.Errorf("fetching header for block time at height %d: %w", height, err)
	}
	t := time.Unix(int64(header.Time), 0).UTC()
	cache[height] = t
	return t, nil
}

// recordLateDeposit handles a Transfer landing on an address already
// RETIRED -- distinct from C2.7's illegal_transition case (no C1 call
// was ever attempted here), but the same underlying category: real
// money, nowhere to put it, never silently dropped and never silently
// credited. order_state_at_detection is a fixed marker rather than an
// actual C1 state, since this path never asks C1 anything.
func recordLateDeposit(ctx context.Context, database db.Queryer, wa addresses.WatchedAddress, log types.Log, amount money.Amount) error {
	slog.Error("candidates: LATE DEPOSIT -- a Transfer landed on a retired address; recorded for manual reconciliation",
		"tx_hash", log.TxHash, "log_index", log.Index, "order_id", wa.OrderID, "external_id", wa.ExternalID, "amount", amount)
	return orphaned.Record(ctx, database, orphaned.Deposit{
		OrderID: wa.OrderID, ExternalID: wa.ExternalID, TxHash: log.TxHash.Hex(), LogIndex: int(log.Index),
		Amount: int64(amount), DetectedAt: time.Now().UTC(), OrderStateAtDetection: "address_retired",
	})
}
