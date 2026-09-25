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

// ScanAddress finds deposits to one of our wallets. Each scan re-reads an
// overlap window behind the wallet's cursor (addresses.ScanFrom), because
// TronGrid's confirmed-only index trails the chain: a deposit confirmed
// after an earlier scan passed its block time is still found. Every
// deposit is recorded (by the tracker) before the cursor moves, and one
// already recorded is skipped, so re-reading is harmless.
func ScanAddress(ctx context.Context, pool *chain.Pool, database db.Queryer, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, addr addresses.Address) error {
	scanStart := time.Now().UTC()
	firstSeen, err := addresses.FirstSeen(ctx, database, addr)
	if err != nil {
		return err
	}
	since, err := addresses.ScanFrom(ctx, database, addr, firstSeen)
	if err != nil {
		return err
	}
	transfers, err := pool.ScanAddress(ctx, string(addr), cfg.ContractAddress, since.UnixMilli())
	if err != nil {
		return fmt.Errorf("candidates: scanning %s: %w", addr, err)
	}
	for _, t := range transfers {
		if err := processTransfer(ctx, database, quotes, tracker, cfg, addr, t); err != nil {
			return fmt.Errorf("candidates: processing transfer %s: %w", t.TxID, err)
		}
	}
	return addresses.SetScanCursor(ctx, database, addr, scanStart)
}

// processTransfer attributes one transfer to the lease that was open on
// its wallet at its block time, then hands it to the tracker. A payment
// outside any lease, after its lease ended, or too small to be a real
// deposit (dust) is recorded as orphaned and never funds an order.
func processTransfer(ctx context.Context, database db.Queryer, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, addr addresses.Address, t chain.Transfer) error {
	amount, err := chain.ParseTransferValue(t)
	if err != nil {
		return err
	}
	lease, found, err := addresses.LeaseAt(ctx, database, string(addr), t.BlockTimestamp)
	if err != nil {
		return err
	}
	if !found {
		return recordOrphan(ctx, database, addresses.WatchedAddress{Address: addr}, t, amount, "no_lease")
	}
	// Block times are whole seconds: a payment in the same second the
	// lease ended counts as late.
	if lease.Status == addresses.StatusRetired && lease.RetiredAt != nil && !t.BlockTimestamp.Before(lease.RetiredAt.Truncate(time.Second)) {
		return recordOrphan(ctx, database, lease, t, amount, "address_retired")
	}

	quoted, err := quotes.QuotedAmount(ctx, lease.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching quoted amount for %s: %w", lease.ExternalID, err)
	}
	classification := chain.ClassifyAgainstOrder(amount, quoted, cfg.DustFloor)
	if classification == chain.Dust {
		return recordOrphan(ctx, database, lease, t, amount, "dust")
	}
	observed := finality.ObservedTransfer{
		TxID: t.TxID, BlockTimestamp: t.BlockTimestamp,
		OrderID: lease.OrderID, ExternalID: lease.ExternalID, CustomerID: lease.CustomerID, Amount: amount,
		SenderAddress: t.From, Address: string(addr),
	}
	return tracker.OnTransferObserved(ctx, observed, classification)
}

// recordOrphan records a payment that funds no order, for an operator to
// resolve (usually a refund). Idempotent on tx_id.
func recordOrphan(ctx context.Context, database db.Queryer, lease addresses.WatchedAddress, t chain.Transfer, amount money.Amount, reason string) error {
	slog.Error("candidates: ORPHANED DEPOSIT -- a payment to one of our wallets funds no order; recorded for manual reconciliation",
		"reason", reason, "tx_id", t.TxID, "address", lease.Address, "order_id", lease.OrderID, "external_id", lease.ExternalID, "amount", amount)
	return orphaned.Record(ctx, database, orphaned.Deposit{
		OrderID: lease.OrderID, ExternalID: lease.ExternalID, TxID: t.TxID,
		Amount: int64(amount), DetectedAt: time.Now().UTC(), OrderStateAtDetection: reason, Address: string(lease.Address),
	})
}
