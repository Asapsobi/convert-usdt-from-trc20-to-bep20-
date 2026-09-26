package orchestrate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"relayd/internal/alert"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/transfers"
	"relayd/internal/upstream"
)

// defaultEnergyDeadlineWindow bounds how long a single energy
// reservation attempt is allowed to take -- mirrors dispatcher's own
// EnergyReservationWait config, applied the same way.
const defaultEnergyDeadlineWindow = 30 * time.Second

// advanceForwardingLegs drives every leg locally recorded FORWARDING
// toward FORWARDED. The forward transfer goes from the leg's own deposit
// address -- where the customer's funds actually are -- to the vendor's
// deposit address, signed by that deposit address's own per-order key:
// over TRON for TRC20_TO_BEP20 (with C4 energy), over BSC for
// BEP20_TO_TRC20 (paying BNB gas). transfer.go does the chain work.
func (o *Orchestrator) advanceForwardingLegs(ctx context.Context) error {
	legs, err := o.Store.ListByStatus(ctx, relay.StatusForwarding)
	if err != nil {
		return fmt.Errorf("listing forwarding legs: %w", err)
	}
	for _, leg := range legs {
		if err := o.advanceForwardingOne(ctx, leg); err != nil {
			slog.Error("orchestrate: advancing forward leg failed, will retry next tick",
				"external_id", leg.ExternalID, "direction", leg.Direction, "error", err)
		}
	}
	return nil
}

// advanceForwardingOne marks leg FORWARDED only once its forward
// transfer has executed successfully on-chain and reached finality --
// never on a broadcast being accepted, which only means a node took the
// transaction (a real, accepted TRON broadcast still failed with
// OUT_OF_ENERGY, moving nothing).
func (o *Orchestrator) advanceForwardingOne(ctx context.Context, leg relay.Leg) error {
	if leg.UpstreamDepositAddress == nil {
		return fmt.Errorf("leg has no upstream_deposit_address recorded")
	}
	amount, err := o.forwardAmountFor(ctx, leg)
	if err != nil {
		return err
	}
	if renewed, err := o.renewExpiredVendorOrder(ctx, leg, amount); err != nil {
		return err
	} else if renewed != nil {
		leg = *renewed
	}

	done, err := o.driveTransfer(ctx, legTransfer(leg, transfers.Forward, *leg.UpstreamDepositAddress, amount))
	if err != nil || done == nil {
		return err
	}
	if err := o.Store.MarkForwarded(ctx, leg.ExternalID, *done.TxHash); err != nil {
		return fmt.Errorf("marking forwarded: %w", err)
	}
	slog.Info("orchestrate: relay leg forwarded", "external_id", leg.ExternalID, "tx_hash", *done.TxHash)
	return nil
}

// forwardAmountFor is what leg's forward transfer sends: the recorded
// deposit minus our profit (relay.Store.RecordDeposit). A leg that
// started forwarding before deposits were recorded falls back to the
// quoted amount minus the quoted fee, which is what its vendor order was
// created for.
func (o *Orchestrator) forwardAmountFor(ctx context.Context, leg relay.Leg) (money.Amount, error) {
	if leg.ForwardAmount != nil {
		return *leg.ForwardAmount, nil
	}
	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return money.Amount{}, fmt.Errorf("fetching order: %w", err)
	}
	amount, err := forwardAmount(leg.AmountIn, order.FeeUnits)
	if err != nil {
		return money.Amount{}, fmt.Errorf("computing forward amount: %w", err)
	}
	return amount, nil
}

// estimatedUSDFor is a straightforward minor-units-to-float conversion,
// the same simplification dispatcher/internal/orchestrate's own
// identical call site uses -- USDT is dollar-denominated in this
// system, and this value is used only for S1's own auto/human-approval
// threshold decision, never for accounting math (which stays in
// money.Amount end to end).
func estimatedUSDFor(amount money.Amount) float64 {
	return float64(amount.Units) / 1_000_000
}

// maxVendorOrderRenewals caps how often one leg's expired vendor order is
// replaced; past it, the leg waits for its forwarding timeout's refund.
const maxVendorOrderRenewals = 3

// renewExpiredVendorOrder replaces leg's vendor order when it expired or
// failed before any forward to it was signed: nothing was sent to it, so a
// fresh order -- same amount, the customer's own destination -- takes its
// place, rather than refunding a customer only because the vendor's quote
// window passed while their deposit was being processed. Returns the
// updated leg, or nil when the order stands.
func (o *Orchestrator) renewExpiredVendorOrder(ctx context.Context, leg relay.Leg, amount money.Amount) (*relay.Leg, error) {
	if leg.UpstreamOrderID == nil {
		return nil, nil
	}
	if a, ok, err := o.Transfers.Open(ctx, leg.ExternalID, transfers.Forward); err != nil {
		return nil, err
	} else if ok && a.Status.MayLand() {
		return nil, nil // a forward may already be on its way to this order
	}
	if _, ok, err := o.Transfers.Confirmed(ctx, leg.ExternalID, transfers.Forward); err != nil || ok {
		return nil, err
	}
	current, err := o.vendorOrder(ctx, leg)
	if err != nil {
		return nil, nil // can't tell right now; the send path re-checks before anything is broadcast
	}
	if current.Status != upstream.StatusExpired && current.Status != upstream.StatusFailed {
		return nil, nil
	}
	o.mu.Lock()
	if o.vendorRenewals == nil {
		o.vendorRenewals = make(map[string]int)
	}
	n := o.vendorRenewals[leg.ExternalID]
	o.mu.Unlock()
	if n >= maxVendorOrderRenewals {
		return nil, nil
	}
	order, err := o.Upstream.CreateOrder(ctx, upstreamPairFor(leg.Direction), amount, leg.DestinationAddress)
	if err != nil {
		return nil, fmt.Errorf("replacing %s vendor order %s: %w", current.Status, *leg.UpstreamOrderID, err)
	}
	if err := o.Store.ReplaceVendorOrder(ctx, leg.ExternalID, *leg.UpstreamOrderID, order.ProviderName, order.ProviderOrderID, order.DepositAddress); err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.vendorRenewals[leg.ExternalID] = n + 1
	o.mu.Unlock()
	o.fire(ctx, alert.Alert{Severity: alert.SeverityWarning, ExternalID: leg.ExternalID, Reason: "relay_leg_vendor_order_renewed",
		Detail: fmt.Sprintf("relay leg %s: vendor order %s was %s before anything was sent to it; replaced by %s order %s",
			leg.ExternalID, *leg.UpstreamOrderID, current.Status, order.ProviderName, order.ProviderOrderID)})
	slog.Info("orchestrate: expired vendor order replaced", "external_id", leg.ExternalID,
		"old", *leg.UpstreamOrderID, "new", order.ProviderOrderID, "vendor", order.ProviderName)
	leg.UpstreamProviderName, leg.UpstreamOrderID, leg.UpstreamDepositAddress = &order.ProviderName, &order.ProviderOrderID, &order.DepositAddress
	return &leg, nil
}
