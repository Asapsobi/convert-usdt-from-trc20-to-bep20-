package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/txbuild"
)

// DefaultInterval mirrors screening's own discovery-loop default --
// this system's real volume (component-map.md: ~4 payouts/hour peak at
// MVP) makes anything in the single-digit seconds "not a real cost yet."
const DefaultInterval = 5 * time.Second

// RunLoop ticks RunTick on an interval. Blocks until ctx is cancelled,
// returning ctx.Err() -- same shape as every other background loop in
// this project (screening/internal/discovery.RunLoop, pipeline.RunLoop).
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

// RunTick runs one pass: dispatch every currently-screened Direct/Standard
// order as far as it will go, then advance finality confirmation for
// everything already broadcast. The two are independent -- an order
// dispatchScreened fails to move past screened this tick is retried
// next tick for free, since state=screened is a live filter (C1's own
// GET /v1/orders?state=X lists whatever currently matches, not an
// append-only event log), not a cursor that could move past it.
func (o *Orchestrator) RunTick(ctx context.Context) error {
	if err := o.dispatchScreened(ctx); err != nil {
		return fmt.Errorf("orchestrate: dispatching screened orders: %w", err)
	}
	if err := o.confirmInFlight(ctx); err != nil {
		return fmt.Errorf("orchestrate: confirming in-flight attempts: %w", err)
	}
	return nil
}

// dispatchScreened walks every page of GET /v1/orders?state=screened,
// attempting each one. A single order's failure is logged and left
// screened for the next tick -- never allowed to stop the rest of the
// page, matching pipeline.RunTick's own per-row discipline.
func (o *Orchestrator) dispatchScreened(ctx context.Context) error {
	cursor := ""
	for {
		refs, next, err := o.Ledger.ListOrdersByState(ctx, "screened", cursor)
		if err != nil {
			return fmt.Errorf("listing screened orders: %w", err)
		}
		for _, ref := range refs {
			if err := o.dispatchOne(ctx, ref.ExternalID); err != nil {
				slog.Error("orchestrate: dispatching order failed, left screened for next tick",
					"order_id", ref.OrderID, "external_id", ref.ExternalID, "error", err)
			}
		}
		if len(refs) < ledgerclient.DefaultPollLimit || next == "" || next == cursor {
			return nil
		}
		cursor = next
	}
}

// dispatchOne drives one screened order through slot selection, energy
// reservation, EnterDispatching, and Broadcast -- everything up to (but
// not including) finality, which confirmInFlight handles separately on
// later ticks.
//
// unsignedTx is built at most once per order per process lifetime and
// cached in o.pending: Broadcast's own contract requires the identical
// bytes on every call for the same logical transfer (dispatch_attempts
// .unsigned_tx_hash is content-addressed), and unlike batches
// (batches.unsigned_tx), a single order's dispatch_attempts row never
// persists the unsigned bytes themselves -- only their hash. That means
// a genuine process restart while an attempt sits at BUILT or SIGNED
// (awaiting S1 approval) cannot resume it from here; a fresh process
// would rebuild different bytes (a fresh block reference and timestamp)
// and Broadcast would record it as a distinct attempt. This is a
// pre-existing limitation of the single-order path, not something this
// package introduces -- flagged plainly rather than silently worked
// around, matching C5.5's own signed_tx fix having been scoped to
// dispatch_attempts specifically, never single-order unsigned_tx.
func (o *Orchestrator) dispatchOne(ctx context.Context, externalID string) error {
	order, err := o.Ledger.GetOrder(ctx, externalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.State != "screened" {
		return nil // raced past us between the list call and here
	}
	if !dispatch.DispatchableTier(order.Tier) {
		// SWEEP is out of this package's scope; RELAY belongs to relayd,
		// which pays those customers itself.
		return nil
	}

	existing, err := o.Dispatcher.Store.Get(ctx, order.ID)
	var slotID int
	var occurredAt time.Time
	switch {
	case err == nil:
		// A previous tick already got as far as recording dispatch_state
		// (E2 landed) for this order -- reuse its own slot and timestamp
		// rather than picking fresh ones. EnterDispatching's own doc
		// comment: occurredAt must be stable across every retry of the
		// same logical attempt, or a resumed call collides with C1's own
		// idempotency-key replay check.
		slotID = existing.SlotID
		occurredAt = existing.EnteredDispatchingAt
	case errors.Is(err, dispatch.ErrAttemptNotFound):
		occurredAt = time.Now().UTC()
		slot, err := o.Slots.SelectForDispatch(ctx, o.Ledger, o.SlotCaps)
		if err != nil {
			return fmt.Errorf("selecting a slot: %w", err)
		}
		slotID = slot.ID

		// Reserve energy BEFORE the irreversible E2 entry -- this
		// package's own dispatch.go doc comment: "slot selection, energy
		// reservation... [are] reversible with zero ledger impact... This
		// is not: once the E2 entry lands." Idempotent on
		// "orchestrate:reserve:<external_id>", so a retry after a crash
		// between this call and EnterDispatching below re-reserves
		// nothing new -- C4's Reserve replays instead.
		deadline := time.Now().Add(o.Cfg.EnergyReservationWait)
		reservation, err := o.Energy.Reserve(ctx, order.ExternalID, slot.TronAddress,
			o.Cfg.EnergyPerTransferUnits, order.Tier, deadline, "orchestrate:reserve:"+order.ExternalID)
		if err != nil {
			return fmt.Errorf("reserving energy: %w", err)
		}
		if reservation.Status != "CONFIRMED" {
			return fmt.Errorf("energy reservation ended in status %s, not CONFIRMED", reservation.Status)
		}

		if _, err := o.Dispatcher.EnterDispatching(ctx, order, slot.ID, occurredAt); err != nil {
			return fmt.Errorf("entering dispatching: %w", err)
		}
	default:
		return fmt.Errorf("checking existing dispatch state: %w", err)
	}

	slot, err := o.Slots.Get(ctx, slotID)
	if err != nil {
		return fmt.Errorf("fetching slot %d: %w", slotID, err)
	}

	pb, ok := o.pending[order.ID]
	if !ok {
		ref, err := o.Blocks.CurrentBlockReference(ctx)
		if err != nil {
			return fmt.Errorf("resolving a current TRON block reference: %w", err)
		}
		unsignedTx, err := txbuild.BuildTransfer(slot.TronAddress, order.RecipientAddress, order.AmountOut, ref)
		if err != nil {
			return fmt.Errorf("building the unsigned transfer: %w", err)
		}

		attemptNumber := 1
		if latest, err := o.Dispatcher.Attempts.LatestForOrder(ctx, order.ID); err == nil {
			attemptNumber = latest.AttemptNumber + 1
		} else if !errors.Is(err, dispatch.ErrBroadcastAttemptNotFound) {
			return fmt.Errorf("checking for a prior broadcast attempt: %w", err)
		}

		pb = pendingBroadcast{attemptNumber: attemptNumber, unsignedTx: unsignedTx}
		o.pending[order.ID] = pb
	}

	// USDT is dollar-denominated in this system (component-map.md); a
	// straightforward minor-units-to-float conversion is enough for S1's
	// own auto/human-approval threshold decision, the only thing this
	// value is for (see SigningService.RequestSignature's own
	// estimatedUSD parameter) -- never used for accounting math, which
	// stays in money.Amount end to end.
	estimatedUSD := float64(order.AmountOut) / 1_000_000

	attempt, err := o.Dispatcher.Broadcast(ctx, order.ID, slot.ID, pb.attemptNumber, pb.unsignedTx, estimatedUSD, o.Chain)
	if err != nil {
		if errors.Is(err, dispatch.ErrSignaturePending) {
			slog.Info("orchestrate: signature still pending approval, resuming next tick",
				"order_id", order.ID, "external_id", order.ExternalID)
			return nil
		}
		if errors.Is(err, dispatch.ErrSignatureRejected) {
			// This specific attempt is dead; a future tick (order is
			// still `dispatching` in C1, still DISPATCHING locally) will
			// build a genuinely new one.
			delete(o.pending, order.ID)
		}
		return fmt.Errorf("broadcasting: %w", err)
	}

	delete(o.pending, order.ID)
	slog.Info("orchestrate: broadcast", "order_id", order.ID, "external_id", order.ExternalID,
		"status", attempt.Status, "tron_txid", attempt.TronTxID)
	return nil
}

// confirmInFlight advances finality for every order dispatch_state still
// carries as DISPATCHING -- including ones broadcast by a prior process
// lifetime (this phase needs no unsigned-bytes cache: ConfirmFinality
// only ever needs an attempt's own id and the order, both fully
// persisted and fetchable).
func (o *Orchestrator) confirmInFlight(ctx context.Context) error {
	inFlight, err := o.Dispatcher.Store.ListDispatching(ctx)
	if err != nil {
		return fmt.Errorf("listing in-flight dispatches: %w", err)
	}
	for _, a := range inFlight {
		if err := o.confirmOne(ctx, a); err != nil {
			slog.Error("orchestrate: confirming finality failed, will retry next tick",
				"order_id", a.OrderID, "external_id", a.ExternalID, "error", err)
		}
	}
	return nil
}

func (o *Orchestrator) confirmOne(ctx context.Context, a dispatch.Attempt) error {
	latest, err := o.Dispatcher.Attempts.LatestForOrder(ctx, a.OrderID)
	if err != nil {
		if errors.Is(err, dispatch.ErrBroadcastAttemptNotFound) {
			return nil // dispatchOne hasn't reached BUILT yet -- not this pass's job
		}
		return fmt.Errorf("fetching latest broadcast attempt: %w", err)
	}
	if latest.Status != dispatch.BroadcastBroadcast {
		return nil // BUILT/SIGNED (still dispatchOne's job) or already CONFIRMED/FAILED
	}

	order, err := o.Ledger.GetOrder(ctx, a.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	final, _, err := o.Dispatcher.ConfirmFinality(ctx, order, latest.ID, time.Now().UTC(), o.Finality)
	if err != nil {
		if errors.Is(err, dispatch.ErrFinalityStale) {
			slog.Warn("orchestrate: broadcast has not reached finality within the configured ceiling",
				"order_id", a.OrderID, "external_id", a.ExternalID, "tron_txid", latest.TronTxID)
			return nil
		}
		return fmt.Errorf("confirming finality: %w", err)
	}
	if final {
		delete(o.pending, a.OrderID)
		slog.Info("orchestrate: order settled", "order_id", a.OrderID, "external_id", a.ExternalID)
	}
	return nil
}
