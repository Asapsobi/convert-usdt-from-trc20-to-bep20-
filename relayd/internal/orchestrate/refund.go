// R5's own first slice (docs/03-build/model-f-relay-build-prompts.md):
// automatically refunds a relay leg that has been stuck FORWARDING --
// its C1 order already moved to dispatching, but this system's own
// forward transfer has never once successfully broadcast/confirmed --
// for longer than Config.ForwardingTimeout, rather than retrying forever.
//
// A leg stuck AWAITING_DEPOSIT whose underlying C1 order is already
// screened (upstream.CreateOrder itself has never once succeeded) is now
// also covered (refundStuckAwaitingDepositLegs, below): distinguishing
// "no deposit has arrived yet" (nothing to refund, AWAITING_DEPOSIT's
// own CreatedAt legitimately can be old) from "a deposit arrived and
// CreateOrder keeps failing" needed a new relay_legs timestamp
// (forward_attempt_started_at, set by runloop.go's own startOne the
// first time it ever notices a leg needs forwarding, whether or not that
// attempt succeeds) and a new C1 transition ({Screened, Refunded},
// ledger/internal/orders/transitions.go) that did not exist before this
// -- refunding from `screened` is simpler than the FORWARDING case above
// in one respect: the deposit is still sitting untouched in
// asset:relay:leg:<id> (relay_forward_start never ran), so no reversal
// entry is needed first, just the refund entry directly.
//
// Deliberately NOT covered by this file (flagged, not silently skipped):
//   - The manual HELD->REFUNDED path via C3's own hold-review queue is
//     now wired (startExternallyRefundedLegs, below): screening's own
//     holds.Reject calls relayd's GET .../refund-entry
//     (internal/httpapi.getRelayLegRefundEntry -> driver.BuildRefundEntry)
//     to get the correctly-shaped entry, then submits held->refunded to
//     C1 itself, exactly as it already does for every other tier -- this
//     package only needs to notice the result and drive the physical
//     on-chain send.
//   - EXPIRED-specific vendor behavior (a real vendor might refund its
//     own receipt back to relayd's sending slot rather than delivering
//     it) -- gated on a real vendor existing (R2/R4), see settle.go's
//     own doc comment on why EXPIRED is treated identically to FAILED
//     post-FORWARDED for now.
//
// The mechanism this file DOES build is deliberately vendor-agnostic and
// needs none of the above. A leg still FORWARDING after
// Config.ForwardingTimeout is only refunded when internal/transfers shows
// no forward transaction that could still land -- none confirmed, and
// none signed but unresolved (see forwardMayStillLand). Transfer attempts
// are durable, so this holds across restarts: a forward sent just before
// a crash is found again and followed to its real outcome, never
// forgotten and refunded on top of.
package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"relayd/internal/ledgerclient"
	"relayd/internal/relay"
	"relayd/internal/transfers"
)

// startExternallyRefundedLegs scans C1 for orders in state=refunded and,
// for each ref that corresponds to a relay leg still AWAITING_DEPOSIT
// locally, marks it REFUND_PENDING directly -- no C1 call needed here,
// unlike startRefund's own timeout-triggered path: whoever moved the
// order to refunded (screening's own holds.Reject, per this file's own
// top-of-file doc comment) already posted the ledger entry itself, using
// the entry driver.BuildRefundEntry computed. advanceRefundPendingLegs,
// run later in the same tick, then drives the actual on-chain broadcast
// -- the identical machinery R5's own timeout-triggered refund already
// uses from that point on. A leg NOT still AWAITING_DEPOSIT is skipped:
// either this ref belongs to a different tier entirely (relay.ErrLegNotFound),
// or this leg already reached REFUND_PENDING/REFUNDED some other way
// (a replay of an already-handled ref), neither of which this phase
// should touch.
func (o *Orchestrator) startExternallyRefundedLegs(ctx context.Context) error {
	cursor := ""
	for {
		refs, next, err := o.Ledger.ListOrdersByState(ctx, "refunded", cursor)
		if err != nil {
			return fmt.Errorf("listing refunded orders: %w", err)
		}
		for _, ref := range refs {
			leg, err := o.Store.GetByExternalID(ctx, ref.ExternalID)
			if err != nil {
				if errors.Is(err, relay.ErrLegNotFound) {
					continue // not a relay leg -- Model D's own order, not ours
				}
				slog.Error("orchestrate: checking a refunded order for a local relay leg failed, will retry next tick",
					"external_id", ref.ExternalID, "error", err)
				continue
			}
			if leg.Status != relay.StatusAwaitingDeposit {
				continue
			}
			if err := o.Store.MarkRefundPending(ctx, leg.ExternalID); err != nil {
				slog.Error("orchestrate: marking externally-refunded leg refund pending failed, will retry next tick",
					"external_id", leg.ExternalID, "error", err)
				continue
			}
			slog.Info("orchestrate: relay leg's screening hold was manually rejected, refund pending", "external_id", leg.ExternalID)
		}
		if len(refs) < ledgerclient.DefaultPollLimit || next == "" || next == cursor {
			return nil
		}
		cursor = next
	}
}

// refundStuckForwardingLegs scans every FORWARDING leg and starts a
// refund for any whose UpdatedAt (set exactly once, by MarkForwarding,
// when CreateOrder first succeeded -- see relay.Store's own doc comment)
// is older than Config.ForwardingTimeout. A zero ForwardingTimeout
// disables this phase entirely -- an explicit opt-in, not a default,
// matching this Config's own "no hardcoded defaults for real-money
// thresholds" posture.
//
// A leg whose forward could still land is never refunded, however stale:
// UpdatedAt is not bumped by later attempts, so a forward finally sent
// after a long wait (a vendor order re-created, gas topped up) would
// otherwise be refunded while its transaction is on its way.
func (o *Orchestrator) refundStuckForwardingLegs(ctx context.Context) error {
	if o.Cfg.ForwardingTimeout <= 0 {
		return nil
	}
	legs, err := o.Store.ListByStatus(ctx, relay.StatusForwarding)
	if err != nil {
		return fmt.Errorf("listing forwarding legs for refund-timeout check: %w", err)
	}
	for _, leg := range legs {
		if time.Since(leg.UpdatedAt) < o.Cfg.ForwardingTimeout {
			continue
		}
		mayLand, err := o.forwardMayStillLand(ctx, leg.ExternalID)
		if err != nil {
			slog.Error("orchestrate: checking a stuck leg's forward attempts failed, will retry next tick",
				"external_id", leg.ExternalID, "error", err)
			continue
		}
		if mayLand {
			slog.Info("orchestrate: relay leg's forwarding attempt is stale but its forward transaction may still land, not refunding",
				"external_id", leg.ExternalID)
			continue
		}
		if err := o.startRefund(ctx, leg); err != nil {
			slog.Error("orchestrate: starting refund for stuck-forwarding leg failed, will retry next tick",
				"external_id", leg.ExternalID, "error", err)
		}
	}
	return nil
}

// forwardMayStillLand reports whether externalID's forward has confirmed
// or has a signed transaction not yet resolved by the chain -- either way
// the deposit may already be (or be about to be) with the vendor. An
// unsigned (BUILT) forward attempt can't land, so it is abandoned here to
// make way for the refund.
func (o *Orchestrator) forwardMayStillLand(ctx context.Context, externalID string) (bool, error) {
	if _, ok, err := o.Transfers.Confirmed(ctx, externalID, transfers.Forward); err != nil || ok {
		return ok, err
	}
	a, ok, err := o.Transfers.Open(ctx, externalID, transfers.Forward)
	if err != nil || !ok {
		return false, err
	}
	if a.Status.MayLand() {
		return true, nil
	}
	return false, o.Transfers.MarkAbandoned(ctx, a.ID, "leg is being refunded")
}

// startRefund commits the refund on C1 (reversing relay_forward_start,
// then closing the customer's own liability and the relay-leg suspense
// account with no commission taken) and marks the leg REFUND_PENDING
// locally. Written to be safely re-entered at any point: each C1 call
// checks the order's own current state before acting, and MarkRefundPending
// is itself an idempotent conditional update.
func (o *Orchestrator) startRefund(ctx context.Context, leg relay.Leg) error {
	if err := o.postForwardAbandonEntry(ctx, leg); err != nil {
		return err
	}
	if err := o.postRefundEntry(ctx, leg); err != nil {
		return err
	}
	if err := o.Store.MarkRefundPending(ctx, leg.ExternalID); err != nil {
		return fmt.Errorf("marking refund pending: %w", err)
	}
	slog.Info("orchestrate: relay leg's forward attempt timed out, refund committed on C1", "external_id", leg.ExternalID)
	return nil
}

// refundStuckAwaitingDepositLegs scans every AWAITING_DEPOSIT leg and
// starts a refund for any whose ForwardAttemptStartedAt (set once, by
// runloop.go's own startOne, the first time it ever notices this leg
// needs forwarding -- see relay.Store.MarkForwardAttemptStarted's own
// doc comment) is older than Config.ForwardingTimeout. A leg
// ForwardAttemptStartedAt is still nil for is skipped outright: relayd
// has never yet seen this leg's own C1 order reach screened, so there is
// nothing stuck here to refund -- it is either still genuinely
// AWAITING_DEPOSIT (no customer deposit has landed yet, legitimately
// open-ended) or the order has not been screened yet, C3's own job, not
// this phase's. Shares Config.ForwardingTimeout with
// refundStuckForwardingLegs rather than a separate config value -- the
// two are the same underlying question ("how long will relayd wait on
// itself before giving up and refunding"), just checked against a
// different clock for a leg that never made it past this phase's own
// FIRST step.
func (o *Orchestrator) refundStuckAwaitingDepositLegs(ctx context.Context) error {
	if o.Cfg.ForwardingTimeout <= 0 {
		return nil
	}
	legs, err := o.Store.ListByStatus(ctx, relay.StatusAwaitingDeposit)
	if err != nil {
		return fmt.Errorf("listing awaiting-deposit legs for refund-timeout check: %w", err)
	}
	for _, leg := range legs {
		if leg.ForwardAttemptStartedAt == nil {
			continue
		}
		if time.Since(*leg.ForwardAttemptStartedAt) < o.Cfg.ForwardingTimeout {
			continue
		}
		if err := o.startAwaitingDepositRefund(ctx, leg); err != nil {
			slog.Error("orchestrate: starting refund for stuck-awaiting-deposit leg failed, will retry next tick",
				"external_id", leg.ExternalID, "error", err)
		}
	}
	return nil
}

// startAwaitingDepositRefund commits the refund on C1 directly from
// screened (no reversal step needed first -- see this file's own
// top-of-file doc comment) and marks the leg REFUND_PENDING locally.
func (o *Orchestrator) startAwaitingDepositRefund(ctx context.Context, leg relay.Leg) error {
	if leg.ReceivedAmount == nil {
		// The refund returns what actually arrived, so that must be on
		// record first -- never fall back to the quoted amount here.
		order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
		if err != nil {
			return fmt.Errorf("fetching order: %w", err)
		}
		if leg, _, err = o.recordDeposit(ctx, leg, order); err != nil {
			return fmt.Errorf("recording the deposit before refunding it: %w", err)
		}
	}
	if err := o.postScreenedRefundEntry(ctx, leg); err != nil {
		return err
	}
	if err := o.Store.MarkRefundPending(ctx, leg.ExternalID); err != nil {
		return fmt.Errorf("marking refund pending: %w", err)
	}
	slog.Info("orchestrate: relay leg's forward attempt never got past screened, refund committed on C1", "external_id", leg.ExternalID)
	return nil
}

// postScreenedRefundEntry posts "relay_refund" directly from screened,
// closing the customer's own liability and the relay-leg suspense
// account by the full amount_in -- postRefundEntry's own sibling, one
// step earlier in the leg's lifecycle (screened, never dispatching, so
// no forwarding-suspense account was ever opened for it). Reuses C1's
// own {Screened, Refunded} transition (ledger/internal/orders/transitions.go).
// A no-op if already posted (idempotent replay); an error if the order
// is in neither screened nor refunded (a real bug, not a replay).
func (o *Orchestrator) postScreenedRefundEntry(ctx context.Context, leg relay.Leg) error {
	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.State == "refunded" {
		return nil // already posted -- safe replay
	}
	if order.State != "screened" {
		return fmt.Errorf("order is in state %q, not screened -- cannot post the refund entry", order.State)
	}

	customerAccount := customerAccountCode(order.CustomerID, order.AmountIn.Asset)
	legAccount := relayLegAccountCode(leg.OrderID)
	asset := string(order.AmountIn.Asset)

	received := receivedFor(leg, order)
	negAmountIn, err := received.Neg()
	if err != nil {
		return fmt.Errorf("negating the received amount: %w", err)
	}
	idemKey := "relayd:refund_awaiting:" + leg.ExternalID
	lines := []ledgerclient.EntryLine{
		{AccountCode: customerAccount, Asset: asset, Amount: received},
		{AccountCode: legAccount, Asset: asset, Amount: negAmountIn},
	}
	if _, err := o.Ledger.TransitionWithEntry(ctx, leg.ExternalID, "refunded", order.Version,
		"relay_refund", "relay_refund", time.Now().UTC(), lines, idemKey); err != nil {
		return fmt.Errorf("posting relay_refund entry: %w", err)
	}
	return nil
}

// postForwardAbandonEntry posts "relay_forward_abandon", the EXACT
// reverse of runloop.go's own postForwardStartEntry: moves the full
// amount_in back from the forwarding-specific suspense account
// (asset:relay:leg:forwarding:<id>) to the relay-leg suspense account
// (asset:relay:leg:<id>), and transitions C1's own order dispatching ->
// held via the SAME transition dispatcher's own non-retryable-payout-failure
// path already uses ({Dispatching, Held}, "non-retryable failure
// (reversal)" -- ledger/internal/orders/transitions.go). A no-op if the
// order has already moved past dispatching (idempotent replay).
func (o *Orchestrator) postForwardAbandonEntry(ctx context.Context, leg relay.Leg) error {
	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.State != "dispatching" {
		return nil // already reversed (or moved on) -- safe replay
	}

	legAccount := relayLegAccountCode(leg.OrderID)
	forwardingAccount := relayLegForwardingAccountCode(leg.OrderID)
	asset := string(order.AmountIn.Asset)

	received := receivedFor(leg, order)
	negAmountIn, err := received.Neg()
	if err != nil {
		return fmt.Errorf("negating the received amount: %w", err)
	}
	idemKey := "relayd:forward_abandon:" + leg.ExternalID
	lines := []ledgerclient.EntryLine{
		{AccountCode: legAccount, Asset: asset, Amount: received},
		{AccountCode: forwardingAccount, Asset: asset, Amount: negAmountIn},
	}
	if _, err := o.Ledger.TransitionWithEntry(ctx, leg.ExternalID, "held", order.Version,
		"relay_forward_abandon", "relay_forward_abandon", time.Now().UTC(), lines, idemKey); err != nil {
		return fmt.Errorf("posting relay_forward_abandon entry: %w", err)
	}
	return nil
}

// postRefundEntry posts "relay_refund": closes the customer's own
// liability and the relay-leg suspense account by the full amount_in,
// symmetric to settle.go's own relay_settle entry but with no commission
// line -- nothing was actually delivered, so nothing is earned. Reuses
// C1's own {Held, Refunded} transition -- the SAME one
// screening/internal/holds.go's Reject already calls for a manually
// rejected hold; this is simply a second, equally legitimate caller of
// it, not a new C1 transition. A no-op if already posted (idempotent
// replay); an error if the order is in neither held nor refunded (a real
// bug, not a replay).
func (o *Orchestrator) postRefundEntry(ctx context.Context, leg relay.Leg) error {
	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.State == "refunded" {
		return nil // already posted -- safe replay
	}
	if order.State != "held" {
		return fmt.Errorf("order is in state %q, not held -- cannot post the refund entry", order.State)
	}

	customerAccount := customerAccountCode(order.CustomerID, order.AmountIn.Asset)
	legAccount := relayLegAccountCode(leg.OrderID)
	asset := string(order.AmountIn.Asset)

	received := receivedFor(leg, order)
	negAmountIn, err := received.Neg()
	if err != nil {
		return fmt.Errorf("negating the received amount: %w", err)
	}
	idemKey := "relayd:refund:" + leg.ExternalID
	lines := []ledgerclient.EntryLine{
		{AccountCode: customerAccount, Asset: asset, Amount: received},
		{AccountCode: legAccount, Asset: asset, Amount: negAmountIn},
	}
	if _, err := o.Ledger.TransitionWithEntry(ctx, leg.ExternalID, "refunded", order.Version,
		"relay_refund", "relay_refund", time.Now().UTC(), lines, idemKey); err != nil {
		return fmt.Errorf("posting relay_refund entry: %w", err)
	}
	return nil
}

// advanceRefundPendingLegs drives every REFUND_PENDING leg's own actual
// on-chain refund transfer -- a TRC20_TO_BEP20 leg was funded in TRC20,
// so it refunds over TRON; BEP20_TO_TRC20 refunds over BSC. The mirror
// of advanceForwardingLegs, one phase later in the leg's own lifecycle.
func (o *Orchestrator) advanceRefundPendingLegs(ctx context.Context) error {
	legs, err := o.Store.ListByStatus(ctx, relay.StatusRefundPending)
	if err != nil {
		return fmt.Errorf("listing refund-pending legs: %w", err)
	}
	for _, leg := range legs {
		if err := o.advanceRefundPendingOne(ctx, leg); err != nil {
			slog.Error("orchestrate: advancing refund-pending leg failed, will retry next tick",
				"external_id", leg.ExternalID, "direction", leg.Direction, "error", err)
		}
	}
	return nil
}

// advanceRefundPendingOne sends the refund to the ORIGINAL depositor's own
// address (C1's order.SenderAddress -- never a customer-supplied one,
// closing the obvious social-engineering vector on a refund flow, per
// R5's own acceptance criterion) for the FULL amount_in -- nothing was
// delivered, so no commission is withheld. The amount is what actually
// arrived (receivedFor), which can differ from the quote.
//
// It sends FROM the leg's own deposit address, signed with that address's
// own per-order key, exactly like the forward: every path into
// REFUND_PENDING (screening rejected the order, CreateOrder never
// succeeded, or the forward never confirmed) means the customer's funds
// never left that address.
func (o *Orchestrator) advanceRefundPendingOne(ctx context.Context, leg relay.Leg) error {
	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.SenderAddress == nil || *order.SenderAddress == "" {
		return fmt.Errorf("order has no sender_address recorded -- cannot refund")
	}

	done, err := o.driveTransfer(ctx, legTransfer(leg, transfers.Refund, *order.SenderAddress, receivedFor(leg, order)))
	if err != nil || done == nil {
		return err
	}
	if err := o.Store.MarkRefunded(ctx, leg.ExternalID, *done.TxHash); err != nil {
		return fmt.Errorf("marking refunded: %w", err)
	}
	slog.Info("orchestrate: relay leg refunded", "external_id", leg.ExternalID, "tx_hash", *done.TxHash)
	return nil
}
