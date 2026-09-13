package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"relayd/internal/ledgerclient"
	"relayd/internal/relay"
)

// RunLoop ticks RunTick on an interval. Blocks until ctx is cancelled,
// returning ctx.Err() -- same shape as every other background loop in
// this project.
func (o *Orchestrator) RunLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := o.RunTick(ctx); err != nil {
		slog.Error("orchestrate: initial tick failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := o.RunTick(ctx); err != nil {
				slog.Error("orchestrate: tick failed", "error", err)
			}
		}
	}
}

// RunTick runs one pass: start forwarding every relay leg whose C1
// order has just reached screened, advance every leg already
// forwarding toward FORWARDED, check every FORWARDED leg for upstream
// settlement, then R5's own two refund phases -- refund any leg stuck
// FORWARDING past Config.ForwardingTimeout, and drive every already-
// committed refund's own on-chain transfer. refundStuckForwardingLegs
// runs AFTER advanceForwardingLegs within the same tick so a leg that
// successfully broadcasts its forward transfer THIS tick is naturally
// excluded (it has already left FORWARDING by the time the refund phase
// re-lists it) -- ordering is a wasted-attempt optimization here, not a
// correctness requirement: relay.Store's own conditional updates make
// the race safe either way. Each phase is independent and each leg
// within a phase is isolated from its neighbors' failures -- the same
// per-row-error-isolation discipline every sibling orchestrator uses.
func (o *Orchestrator) RunTick(ctx context.Context) error {
	if err := o.startScreenedLegs(ctx); err != nil {
		return fmt.Errorf("orchestrate: starting screened legs: %w", err)
	}
	if err := o.advanceForwardingLegs(ctx); err != nil {
		return fmt.Errorf("orchestrate: advancing forwarding legs: %w", err)
	}
	if err := o.advanceForwardedLegs(ctx); err != nil {
		return fmt.Errorf("orchestrate: advancing forwarded legs: %w", err)
	}
	if err := o.refundStuckForwardingLegs(ctx); err != nil {
		return fmt.Errorf("orchestrate: refunding stuck forwarding legs: %w", err)
	}
	if err := o.advanceRefundPendingLegs(ctx); err != nil {
		return fmt.Errorf("orchestrate: advancing refund-pending legs: %w", err)
	}
	return nil
}

// startScreenedLegs walks every page of GET /v1/orders?state=screened
// and, for each ref that corresponds to a relay leg still in
// AWAITING_DEPOSIT, creates the upstream swap order and marks it
// FORWARDING. C1's own state=screened list mixes every tier
// (DIRECT/STANDARD/SWEEP/RELAY all share the same order state machine,
// per docs/02-architecture/model-f-relay-architecture.md's own §2) --
// relay.ErrLegNotFound for a ref this service has no leg for is the
// normal, silent case (it belongs to Model D, not this service), not
// logged as an error.
func (o *Orchestrator) startScreenedLegs(ctx context.Context) error {
	cursor := ""
	for {
		refs, next, err := o.Ledger.ListOrdersByState(ctx, "screened", cursor)
		if err != nil {
			return fmt.Errorf("listing screened orders: %w", err)
		}
		for _, ref := range refs {
			if err := o.startOne(ctx, ref.ExternalID); err != nil {
				slog.Error("orchestrate: starting relay leg failed, left screened for next tick",
					"external_id", ref.ExternalID, "error", err)
			}
		}
		if len(refs) < ledgerclient.DefaultPollLimit || next == "" || next == cursor {
			return nil
		}
		cursor = next
	}
}

// startOne is written to be safely re-entered at either of its two
// real steps (create the upstream order, post C1's own screened->
// dispatching transition) independently -- a crash between them must
// not leave a leg stuck FORWARDING locally while C1 still thinks it is
// screened, since phase 1 only re-lists C1's own state=screened orders,
// never local relay_legs rows directly.
func (o *Orchestrator) startOne(ctx context.Context, externalID string) error {
	leg, err := o.Store.GetByExternalID(ctx, externalID)
	if err != nil {
		if errors.Is(err, relay.ErrLegNotFound) {
			return nil // not a relay leg -- Model D's own order, not ours
		}
		return err
	}
	if leg.Status != relay.StatusAwaitingDeposit && leg.Status != relay.StatusForwarding {
		return nil // already past this phase, or a terminal status
	}

	if leg.Status == relay.StatusAwaitingDeposit {
		pair := upstreamPairFor(leg.Direction)
		order, err := o.Upstream.CreateOrder(ctx, pair, leg.AmountIn, leg.DestinationAddress)
		if err != nil {
			return fmt.Errorf("creating upstream order: %w", err)
		}
		if err := o.Store.MarkForwarding(ctx, leg.ExternalID, order.ProviderName, order.ProviderOrderID, order.DepositAddress); err != nil {
			return fmt.Errorf("marking forwarding: %w", err)
		}
		slog.Info("orchestrate: relay leg started forwarding", "external_id", leg.ExternalID,
			"upstream_provider", order.ProviderName, "upstream_order_id", order.ProviderOrderID)
	}

	return o.postForwardStartEntry(ctx, leg.OrderID, externalID)
}

// postForwardStartEntry posts the screened->dispatching "relay_forward_start"
// entry (this package's own doc comment) -- a no-op if the order has
// already moved past screened (the idempotent-replay case: a prior
// tick's own call already succeeded, or this is the resume path after a
// crash between MarkForwarding and this call).
func (o *Orchestrator) postForwardStartEntry(ctx context.Context, orderID int64, externalID string) error {
	order, err := o.Ledger.GetOrder(ctx, externalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.State != "screened" {
		return nil // already transitioned -- safe replay
	}

	fromAccount := relayLegAccountCode(orderID)
	toAccount := relayLegForwardingAccountCode(orderID)
	asset := string(order.AmountIn.Asset)
	if err := o.Ledger.EnsureAccount(ctx, toAccount, ledgerclient.AccountAsset, asset, "relayd:ensure-account:"+toAccount); err != nil {
		return fmt.Errorf("ensuring %s exists: %w", toAccount, err)
	}

	negAmountIn, err := order.AmountIn.Neg()
	if err != nil {
		return fmt.Errorf("negating amount_in: %w", err)
	}
	idemKey := "relayd:forward_start:" + externalID
	lines := []ledgerclient.EntryLine{
		{AccountCode: toAccount, Asset: asset, Amount: order.AmountIn},
		{AccountCode: fromAccount, Asset: asset, Amount: negAmountIn},
	}
	_, err = o.Ledger.TransitionWithEntry(ctx, externalID, "dispatching", order.Version,
		"relay_forward_start", "relay_forward_start", time.Now().UTC(), lines, idemKey)
	if err != nil {
		return fmt.Errorf("posting relay_forward_start entry: %w", err)
	}
	return nil
}
