package orchestrate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"relayd/internal/alert"
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
		// Once a leg reaches FORWARDED, this system's own forward
		// transfer has already confirmed on-chain -- there is no on-chain
		// lever left to pull, regardless of which of the two upstream
		// statuses comes back (see this package's own doc comment and
		// docs/02-architecture/model-f-relay-architecture.md §4's own
		// note on UNRECOVERABLE). A real vendor might, for EXPIRED
		// specifically, refund its OWN receipt back to relayd's sending
		// slot rather than delivering it -- but that is vendor-specific
		// behavior this pass has no real vendor to build or verify
		// against (R2/R4 are still open), so both statuses are treated
		// identically here: UNRECOVERABLE, loudly alerted, a human
		// decision from here per the build-prompts doc's own "Open
		// items". Revisit once a real vendor's actual EXPIRED behavior is
		// known.
		return o.handleUnrecoverable(ctx, leg, upstreamOrder.Status)
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

// handleUnrecoverable marks leg UNRECOVERABLE and fires a real alert --
// idempotent: MarkUnrecoverable's own conditional UPDATE (WHERE
// status=FORWARDED) only succeeds once, so a leg already UNRECOVERABLE
// no longer appears in ListByStatus(FORWARDED) on a later tick and this
// function is never re-entered for it, and the alert fires exactly once
// (at the moment of transition), not every tick.
func (o *Orchestrator) handleUnrecoverable(ctx context.Context, leg relay.Leg, upstreamStatus upstream.SwapStatus) error {
	if err := o.Store.MarkUnrecoverable(ctx, leg.ExternalID); err != nil {
		return fmt.Errorf("marking unrecoverable: %w", err)
	}
	firedErr := o.Alert.Fire(ctx, alert.Alert{
		Severity:   alert.SeverityCritical,
		ExternalID: leg.ExternalID,
		Reason:     "relay_leg_unrecoverable",
		Detail: fmt.Sprintf("relay leg %s's own forward transfer already confirmed on-chain; "+
			"upstream order %s reported status %s afterward. No automatic recovery exists -- "+
			"see docs/03-build/model-f-relay-build-prompts.md's own Open items for the operational runbook.",
			leg.ExternalID, valueOrEmpty(leg.UpstreamOrderID), upstreamStatus),
	})
	if firedErr != nil {
		// The alert channel being down must never re-block this leg's own
		// terminal transition, which already committed above -- but it
		// must not be silent either.
		slog.Error("orchestrate: firing the UNRECOVERABLE alert itself failed", "external_id", leg.ExternalID, "error", firedErr)
	}
	return nil
}

func valueOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
