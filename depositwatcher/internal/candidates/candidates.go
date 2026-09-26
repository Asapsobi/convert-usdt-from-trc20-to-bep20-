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

// ScanRange finds deposits to our own wallets in [fromHeight, toHeight]
// and hands each to the finality tracker, which records it before this
// returns -- so the caller may advance its cursor past the range.
func ScanRange(ctx context.Context, pool *chain.Pool, database db.Queryer, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, fromHeight, toHeight uint64) error {
	if fromHeight > toHeight {
		return nil
	}
	watch, err := addresses.WatchSet(ctx, database)
	if err != nil {
		return fmt.Errorf("candidates: %w", err)
	}
	if len(watch) == 0 {
		return nil // no wallet of ours can receive anything in this range
	}
	// Only Transfer logs paying one of our wallets: topic 2 is the
	// recipient, left-padded to 32 bytes.
	to := make([]common.Hash, len(watch))
	for i, a := range watch {
		to[i] = common.BytesToHash(common.HexToAddress(string(a)).Bytes())
	}
	logs, err := pool.LogsAt(ctx, fromHeight, toHeight, cfg.ContractAddress, [][]common.Hash{{cfg.TransferTopic}, {}, to})
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

// processLog attributes one Transfer to the lease that was open on its
// wallet at its block time, then hands it to the tracker. A payment
// outside any lease, after its lease ended, or too small to be a real
// deposit (dust -- e.g. address poisoning) is recorded as orphaned and
// never funds an order.
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
	blockTime, err := blockTimeFor(ctx, pool, log.BlockNumber, blockTimes)
	if err != nil {
		return err
	}

	lease, found, err := addresses.LeaseAt(ctx, database, to.Hex(), blockTime)
	if err != nil {
		return err
	}
	if !found {
		return recordOrphan(ctx, database, addresses.WatchedAddress{Address: addresses.Address(to.Hex())}, log, amount, "no_lease")
	}
	// Block times are whole seconds: a payment in the same second the
	// lease ended counts as late.
	if lease.Status == addresses.StatusRetired && lease.RetiredAt != nil && !blockTime.Before(lease.RetiredAt.Truncate(time.Second)) {
		return recordOrphan(ctx, database, lease, log, amount, "address_retired")
	}

	quoted, err := quotes.QuotedAmount(ctx, lease.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching quoted amount for %s: %w", lease.ExternalID, err)
	}
	classification := chain.ClassifyAgainstOrder(amount, quoted, cfg.DustFloor)
	if classification == chain.Dust {
		return recordOrphan(ctx, database, lease, log, amount, "dust")
	}

	observed := finality.ObservedLog{
		TxHash: log.TxHash, LogIndex: uint(log.Index), Height: log.BlockNumber, BlockTime: blockTime,
		OrderID: lease.OrderID, ExternalID: lease.ExternalID, CustomerID: lease.CustomerID, Amount: amount,
		SenderAddress: from.Hex(), Address: string(lease.Address),
	}
	return tracker.OnLogObserved(ctx, observed, classification)
}

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

// recordOrphan records a payment that funds no order, for an operator to
// resolve (usually a refund). Idempotent on tx_hash:log_index.
func recordOrphan(ctx context.Context, database db.Queryer, lease addresses.WatchedAddress, log types.Log, amount money.Amount, reason string) error {
	slog.Error("candidates: ORPHANED DEPOSIT -- a payment to one of our wallets funds no order; recorded for manual reconciliation",
		"reason", reason, "tx_hash", log.TxHash, "log_index", log.Index, "address", lease.Address,
		"order_id", lease.OrderID, "external_id", lease.ExternalID, "amount", amount)
	return orphaned.Record(ctx, database, orphaned.Deposit{
		OrderID: lease.OrderID, ExternalID: lease.ExternalID, TxHash: log.TxHash.Hex(), LogIndex: int(log.Index),
		Amount: int64(amount), DetectedAt: time.Now().UTC(), OrderStateAtDetection: reason, Address: string(lease.Address),
	})
}
