package orchestrate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"relayd/internal/ledgerclient"
	"relayd/internal/relay"
	"relayd/internal/upstream"
)

// advanceForwardedLegs polls the upstream platform for every leg
// locally recorded FORWARDED and, once it reports completion, posts
// C1's own dispatching->settled "relay_settle" entry (this package's
// own doc comment) and marks the leg SETTLED. A `failed`/`expired`
// upstream status is left as a TODO for R5's own refund path
// (deliberately out of this pass's scope, per the approved plan) --
// logged loudly, not silently swallowed, but no automatic action taken
// yet.
func (o *Orchestrator) advanceForwardedLegs(ctx context.Context) error {
	legs, err := o.Store.ListByStatus(ctx, relay.StatusForwarded)
	if err != nil {
		return fmt.Errorf("listing forwarded legs: %w", err)
	}
	for _, leg := range legs {
		if err := o.advanceForwardedOne(ctx, leg); err != nil {
			slog.Error("orchestrate: advancing forwarded leg failed, will retry next tick",
				"external_id", leg.ExternalID, "error", err)
		}
	}
	return nil
}

func (o *Orchestrator) advanceForwardedOne(ctx context.Context, leg relay.Leg) error {
	if leg.UpstreamOrderID == nil {
		return fmt.Errorf("leg has no upstream_order_id recorded")
	}

	upstreamOrder, err := o.Upstream.GetOrder(ctx, *leg.UpstreamOrderID)
	if err != nil {
		return fmt.Errorf("checking upstream order status: %w", err)
	}

	switch upstreamOrder.Status {
	case upstream.StatusComplete:
		// fall through to settlement below
	case upstream.StatusFailed, upstream.StatusExpired:
		// R5's own refund/UNRECOVERABLE path, deliberately deferred --
		// see this function's own doc comment. Logged loudly so this
		// never silently vanishes even though nothing automatic happens
		// yet.
		slog.Error("orchestrate: upstream order did not complete -- R5's refund path is not yet built, this leg needs manual handling",
			"external_id", leg.ExternalID, "upstream_status", upstreamOrder.Status)
		return nil
	default:
		return nil // still in flight upstream -- check again next tick
	}

	if upstreamOrder.AmountOutActual == nil {
		return fmt.Errorf("upstream order reported complete with no amount_out_actual")
	}

	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.State == "settled" {
		// Already posted by a prior tick -- just finish the local mark.
		return o.Store.MarkSettled(ctx, leg.ExternalID, *upstreamOrder.AmountOutActual)
	}
	if order.State != "dispatching" {
		return fmt.Errorf("order is in state %q, not dispatching -- cannot settle", order.State)
	}

	inAsset := string(order.AmountIn.Asset)
	customerAccount := customerAccountCode(order.CustomerID, order.AmountIn.Asset)
	forwardingAccount := relayLegForwardingAccountCode(leg.OrderID)
	commissionWalletAccount := commissionWalletAccountCode(order.AmountIn.Asset)
	commissionAccount := commissionAccountCode(order.AmountIn.Asset)

	if err := o.Ledger.EnsureAccount(ctx, customerAccount, ledgerclient.AccountLiability, inAsset, "relayd:ensure-account:"+customerAccount); err != nil {
		return fmt.Errorf("ensuring %s exists: %w", customerAccount, err)
	}
	if err := o.Ledger.EnsureAccount(ctx, commissionWalletAccount, ledgerclient.AccountAsset, inAsset, "relayd:ensure-account:"+commissionWalletAccount); err != nil {
		return fmt.Errorf("ensuring %s exists: %w", commissionWalletAccount, err)
	}
	if err := o.Ledger.EnsureAccount(ctx, commissionAccount, ledgerclient.AccountRevenue, inAsset, "relayd:ensure-account:"+commissionAccount); err != nil {
		return fmt.Errorf("ensuring %s exists: %w", commissionAccount, err)
	}

	// Four lines, not three: the per-leg forwarding suspense account
	// must close to EXACTLY zero (architecture doc §5's own "still open
	// after an hour is an operational alarm" -- an account that never
	// reaches zero would trip that alarm forever). The commission
	// (fee_units) never actually left this system on-chain -- only
	// forward_amount did (relayd/internal/orchestrate's own
	// forwardAmount) -- so crediting the suspense account by the FULL
	// amount_in, with no offsetting debit, would make revenue:relay_commission's
	// own credit economically unbacked (recognized revenue with no
	// asset anywhere reflecting relayd actually holding that TRC20).
	// commissionWalletAccount is that backing: a real, ongoing (not
	// per-leg) asset account for TRC20/BEP20 commission relayd has
	// collected but not yet swept out -- found only by this exact
	// integration test asserting the forwarding account closes to zero
	// (it didn't, on the first version of this entry: 300000 units of
	// commission sat there forever, invisible to any test that doesn't
	// check the real post-settle balance against a real ledgerd).
	negAmountIn, err := order.AmountIn.Neg()
	if err != nil {
		return fmt.Errorf("negating amount_in: %w", err)
	}
	negFee, err := order.FeeUnits.Neg()
	if err != nil {
		return fmt.Errorf("negating fee_units: %w", err)
	}

	idemKey := "relayd:settle:" + leg.ExternalID
	lines := []ledgerclient.EntryLine{
		{AccountCode: customerAccount, Asset: inAsset, Amount: order.AmountIn},
		{AccountCode: forwardingAccount, Asset: inAsset, Amount: negAmountIn},
		{AccountCode: commissionWalletAccount, Asset: inAsset, Amount: order.FeeUnits},
		{AccountCode: commissionAccount, Asset: inAsset, Amount: negFee},
	}
	if _, err := o.Ledger.TransitionWithEntry(ctx, leg.ExternalID, "settled", order.Version,
		"relay_settle", "relay_settle", time.Now().UTC(), lines, idemKey); err != nil {
		return fmt.Errorf("posting relay_settle entry: %w", err)
	}

	if err := o.Store.MarkSettled(ctx, leg.ExternalID, *upstreamOrder.AmountOutActual); err != nil {
		return fmt.Errorf("marking settled: %w", err)
	}
	slog.Info("orchestrate: relay leg settled", "external_id", leg.ExternalID)
	return nil
}
