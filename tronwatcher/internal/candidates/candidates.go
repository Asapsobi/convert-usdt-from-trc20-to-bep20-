// Package candidates turns "a watched address has been scanned" into
// "this transfer is a trackable deposit candidate, tracked toward
// finality; this other one is not, and here is why." The direct sibling
// of depositwatcher/internal/candidates, but per-ADDRESS rather than
// per-block-range: this service's own chain.Pool.ScanAddress already
// returns every agreed-upon transfer for one address since a given
// timestamp, so there is no separate block-range/log-filtering step to
// do here the way BSC's eth_getLogs-based pipeline needs.
package candidates

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"tronwatcher/internal/addresses"
	"tronwatcher/internal/chain"
	"tronwatcher/internal/db"
	"tronwatcher/internal/finality"
	"tronwatcher/internal/money"
	"tronwatcher/internal/orphaned"
)

// QuotedAmountFetcher is the one fact this pipeline needs from C1 that
// it has no local copy of: an order's quoted amount_in. Mirrors
// depositwatcher/internal/candidates.QuotedAmountFetcher exactly --
// ledgerclient.Client.QuotedAmount implements this.
type QuotedAmountFetcher interface {
	QuotedAmount(ctx context.Context, externalID string) (money.Amount, error)
}

// Config scopes what ScanWatchedAddress looks for and how it classifies
// what it finds.
type Config struct {
	ContractAddress string // USDT-TRC20, see chain.USDTTRC20ContractAddress
	DustFloor       money.Amount
}

// ScanWatchedAddress scans wa's own address for new inbound TRC20
// transfers (since wa.LastScannedAt, or wa.AssignedAt on a never-
// scanned address -- never from the epoch) via pool.ScanAddress
// (2-provider agreement), classifies each against the order's quoted
// amount, and hands trackable ones to tracker.OnTransferObserved. A
// transfer landing on an already-RETIRED address is routed to
// orphaned.Record instead -- real money, nowhere to put it, never
// silently dropped.
//
// On success, advances the address's own last_scanned_at cursor to the
// scan's start time (not "now" -- see this function's own call site for
// why: a transfer that lands between when the scan started and when
// this call returns must not be skipped by an overly-optimistic cursor
// advance).
func ScanWatchedAddress(ctx context.Context, pool *chain.Pool, database db.Queryer, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, wa addresses.WatchedAddress) error {
	scanStart := time.Now().UTC()

	since := wa.AssignedAt
	if wa.LastScannedAt != nil {
		since = *wa.LastScannedAt
	}

	transfers, err := pool.ScanAddress(ctx, string(wa.Address), cfg.ContractAddress, since.UnixMilli())
	if err != nil {
		return fmt.Errorf("candidates: scanning %s (order %d): %w", wa.Address, wa.OrderID, err)
	}

	for _, t := range transfers {
		if err := processTransfer(ctx, database, quotes, tracker, cfg, wa, t); err != nil {
			return fmt.Errorf("candidates: processing transfer %s: %w", t.TxID, err)
		}
	}

	if err := addresses.UpdateLastScannedAt(ctx, database, wa.OrderID, scanStart); err != nil {
		return fmt.Errorf("candidates: advancing scan cursor for order %d: %w", wa.OrderID, err)
	}
	return nil
}

func processTransfer(ctx context.Context, database db.Queryer, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, wa addresses.WatchedAddress, t chain.Transfer) error {
	amount, err := chain.ParseTransferValue(t)
	if err != nil {
		return err
	}

	if wa.Status == addresses.StatusRetired {
		return recordLateDeposit(ctx, database, wa, t, amount)
	}

	quoted, err := quotes.QuotedAmount(ctx, wa.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching quoted amount for %s: %w", wa.ExternalID, err)
	}
	classification := chain.ClassifyAgainstOrder(amount, quoted, cfg.DustFloor)

	observed := finality.ObservedTransfer{
		TxID: t.TxID, BlockTimestamp: t.BlockTimestamp,
		OrderID: wa.OrderID, ExternalID: wa.ExternalID, CustomerID: wa.CustomerID, Amount: amount,
		SenderAddress: t.From,
	}
	return tracker.OnTransferObserved(ctx, observed, classification)
}

// recordLateDeposit handles a transfer landing on an address already
// RETIRED -- real money, nowhere to put it, never silently dropped and
// never silently credited. order_state_at_detection is a fixed marker
// rather than an actual C1 state, since this path never asks C1
// anything, mirroring depositwatcher's own identical reasoning.
func recordLateDeposit(ctx context.Context, database db.Queryer, wa addresses.WatchedAddress, t chain.Transfer, amount money.Amount) error {
	slog.Error("candidates: LATE DEPOSIT -- a transfer landed on a retired address; recorded for manual reconciliation",
		"tx_id", t.TxID, "order_id", wa.OrderID, "external_id", wa.ExternalID, "amount", amount)
	return orphaned.Record(ctx, database, orphaned.Deposit{
		OrderID: wa.OrderID, ExternalID: wa.ExternalID, TxID: t.TxID,
		Amount: int64(amount), DetectedAt: time.Now().UTC(), OrderStateAtDetection: "address_retired",
	})
}
