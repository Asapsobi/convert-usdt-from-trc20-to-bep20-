package orchestrate

import (
	"context"
	"fmt"

	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/pricing"
	"relayd/internal/relay"
)

// Every amount relayd moves or books for a leg comes from what actually
// arrived, recorded once (relay.Store.RecordDeposit) and never
// recomputed: the forward is received minus our profit, and a refund
// returns everything received. The quoted amount only ever describes
// what the customer said they would send.

// recordDeposit makes sure leg's deposit is recorded, reading what
// arrived from the ledger (the watcher booked it into the leg's own
// suspense account) and splitting it by the leg's pricing snapshot.
// tooSmall is true when the deposit can't be forwarded -- nothing would
// be left after our profit, or the customer sent less than both their
// quote and the current minimum -- and must be refunded instead.
func (o *Orchestrator) recordDeposit(ctx context.Context, leg relay.Leg, order ledgerclient.Order) (_ relay.Leg, tooSmall bool, err error) {
	if leg.ReceivedAmount != nil {
		return leg, leg.ForwardAmount == nil || leg.ProfitAmount == nil ||
			leg.ProfitAmount.Units+leg.ForwardAmount.Units != leg.ReceivedAmount.Units, nil
	}
	received, err := o.Ledger.AccountBalance(ctx, relayLegAccountCode(leg.OrderID))
	if err != nil {
		return leg, false, fmt.Errorf("reading the deposit booked for order %d: %w", leg.OrderID, err)
	}
	if received.Units <= 0 {
		return leg, false, fmt.Errorf("order %d is screened but no deposit is booked to %s", leg.OrderID, relayLegAccountCode(leg.OrderID))
	}
	if received.Asset != leg.AmountIn.Asset {
		return leg, false, fmt.Errorf("order %d's deposit was booked in %s, but the leg expects %s", leg.OrderID, received.Asset, leg.AmountIn.Asset)
	}
	sender := ""
	if order.SenderAddress != nil {
		sender = *order.SenderAddress
	}

	cfg, err := o.pricingFor(ctx, leg, order)
	if err != nil {
		return leg, false, err
	}
	profit, forward, ok := cfg.Split(received)
	if ok && o.Pricing != nil && received.Units < leg.AmountIn.Units {
		// Underpaid: still forward it, unless it's under the current minimum.
		if current, err := o.Pricing.Get(ctx); err == nil && received.Units < current.MinAmountIn {
			ok = false
		}
	}
	if !ok {
		// Nothing to forward: record it all as the amount to hand back.
		profit, forward = money.Amount{Asset: received.Asset}, received
	}
	recorded, err := o.Store.RecordDeposit(ctx, leg.ExternalID, received, sender, profit, forward)
	if err != nil {
		return leg, false, err
	}
	return recorded, !ok, nil
}

// pricingFor is the pricing a leg is settled under: the snapshot it was
// quoted with; the current admin pricing for a leg created before
// snapshots existed; or, with neither available, the order's own quoted
// fee as a flat amount.
func (o *Orchestrator) pricingFor(ctx context.Context, leg relay.Leg, order ledgerclient.Order) (pricing.Config, error) {
	if leg.ProfitBPS != nil {
		minProfit := int64(0)
		if leg.MinProfit != nil {
			minProfit = *leg.MinProfit
		}
		return pricing.Config{ProfitBPS: *leg.ProfitBPS, MinProfit: minProfit}, nil
	}
	if o.Pricing != nil {
		return o.Pricing.Get(ctx)
	}
	return pricing.Config{MinProfit: order.FeeUnits.Units}, nil
}

// receivedFor is what a leg's refund returns and its ledger entries
// book: the recorded deposit, or the quoted amount on a leg that predates
// deposit recording.
func receivedFor(leg relay.Leg, order ledgerclient.Order) money.Amount {
	if leg.ReceivedAmount != nil {
		return *leg.ReceivedAmount
	}
	return order.AmountIn
}

// profitFor is what we keep from leg once it settles.
func profitFor(leg relay.Leg, order ledgerclient.Order) money.Amount {
	if leg.ProfitAmount != nil {
		return *leg.ProfitAmount
	}
	return order.FeeUnits
}
