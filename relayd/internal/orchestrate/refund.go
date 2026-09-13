// R5's own first slice (docs/03-build/model-f-relay-build-prompts.md):
// automatically refunds a relay leg that has been stuck FORWARDING --
// its C1 order already moved to dispatching, but this system's own
// forward transfer has never once successfully broadcast/confirmed --
// for longer than Config.ForwardingTimeout, rather than retrying forever.
//
// Deliberately NOT covered by this file (flagged, not silently skipped):
//   - A leg stuck AWAITING_DEPOSIT whose underlying C1 order is already
//     screened (upstream.CreateOrder itself has never once succeeded).
//     Distinguishing "no deposit has arrived yet" (nothing to refund,
//     AWAITING_DEPOSIT's own CreatedAt legitimately can be old) from
//     "a deposit arrived and CreateOrder keeps failing" needs either a
//     new relay_legs timestamp this pass does not add, or a C1-exposed
//     per-state timestamp that does not exist today -- and refunding
//     from `screened` needs a new C1 transition ({Screened, Refunded})
//     that also does not exist today (only {Funded, Refunded} and
//     {Held, Refunded} do). Left as a known follow-up rather than
//     building a shortcut around either gap.
//   - The manual HELD->REFUNDED path C3's own holds.Reject already
//     models system-wide (screening/internal/holds.go's own
//     RefundEntryBuilder) -- that interface's only real implementation
//     today is StubRefundEntryBuilder (screening's own doc comment:
//     "nothing in this system owns constructing, signing, or
//     broadcasting the physical refund"). relayd is now positioned to be
//     that owner for RELAY-tier orders specifically (it already owns
//     real tx construction/signing/broadcast on both chains), but wiring
//     that requires a new relayd-facing HTTP endpoint C3 can call plus a
//     RELAY-aware RefundEntryBuilder in screening -- a cross-module
//     change out of scope for this pass.
//   - EXPIRED-specific vendor behavior (a real vendor might refund its
//     own receipt back to relayd's sending slot rather than delivering
//     it) -- gated on a real vendor existing (R2/R4), see settle.go's
//     own doc comment on why EXPIRED is treated identically to FAILED
//     post-FORWARDED for now.
//
// The mechanism this file DOES build is deliberately vendor-agnostic and
// needs none of the above: a leg still FORWARDING after Config.ForwardingTimeout
// has, by construction, never had its forward transfer leave this
// system's own custody on-chain (broadcasting is the very last step of
// advanceForwardingOneTRC20/BEP20, and MarkForwarded -- which would have
// moved the leg out of FORWARDING -- never ran). Refunding it is safe
// regardless of what any vendor does or says.
package orchestrate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"relayd/internal/evmtx"
	"relayd/internal/ledgerclient"
	"relayd/internal/relay"
	"relayd/internal/txbuild"
)

// refundStuckForwardingLegs scans every FORWARDING leg and starts a
// refund for any whose UpdatedAt (set exactly once, by MarkForwarding,
// when CreateOrder first succeeded -- see relay.Store's own doc comment)
// is older than Config.ForwardingTimeout. A zero ForwardingTimeout
// disables this phase entirely -- an explicit opt-in, not a default,
// matching this Config's own "no hardcoded defaults for real-money
// thresholds" posture.
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
		if err := o.startRefund(ctx, leg); err != nil {
			slog.Error("orchestrate: starting refund for stuck-forwarding leg failed, will retry next tick",
				"external_id", leg.ExternalID, "error", err)
		}
	}
	return nil
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

	negAmountIn, err := order.AmountIn.Neg()
	if err != nil {
		return fmt.Errorf("negating amount_in: %w", err)
	}
	idemKey := "relayd:forward_abandon:" + leg.ExternalID
	lines := []ledgerclient.EntryLine{
		{AccountCode: legAccount, Asset: asset, Amount: order.AmountIn},
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

	negAmountIn, err := order.AmountIn.Neg()
	if err != nil {
		return fmt.Errorf("negating amount_in: %w", err)
	}
	idemKey := "relayd:refund:" + leg.ExternalID
	lines := []ledgerclient.EntryLine{
		{AccountCode: customerAccount, Asset: asset, Amount: order.AmountIn},
		{AccountCode: legAccount, Asset: asset, Amount: negAmountIn},
	}
	if _, err := o.Ledger.TransitionWithEntry(ctx, leg.ExternalID, "refunded", order.Version,
		"relay_refund", "relay_refund", time.Now().UTC(), lines, idemKey); err != nil {
		return fmt.Errorf("posting relay_refund entry: %w", err)
	}
	return nil
}

// advanceRefundPendingLegs drives every REFUND_PENDING leg's own actual
// on-chain refund transfer, dispatching by Direction -- a
// TRC20_TO_BEP20 leg was funded in TRC20, so it refunds over TRON;
// BEP20_TO_TRC20 refunds over BSC. The mirror of advanceForwardingLegs
// (forward_trc20.go), one phase later in the leg's own lifecycle.
func (o *Orchestrator) advanceRefundPendingLegs(ctx context.Context) error {
	legs, err := o.Store.ListByStatus(ctx, relay.StatusRefundPending)
	if err != nil {
		return fmt.Errorf("listing refund-pending legs: %w", err)
	}
	for _, leg := range legs {
		var err error
		switch leg.Direction {
		case relay.TRC20ToBEP20:
			err = o.advanceRefundPendingOneTRC20(ctx, leg)
		case relay.BEP20ToTRC20:
			err = o.advanceRefundPendingOneBEP20(ctx, leg)
		default:
			err = fmt.Errorf("relay leg has unrecognized direction %q", leg.Direction)
		}
		if err != nil {
			slog.Error("orchestrate: advancing refund-pending leg failed, will retry next tick",
				"external_id", leg.ExternalID, "error", err)
		}
	}
	return nil
}

// advanceRefundPendingOneTRC20 is advanceForwardingOneTRC20's own
// refund-direction sibling: same energy reservation / build-cache-sign-
// broadcast shape, but the destination is the ORIGINAL depositor's own
// address (C1's order.SenderAddress -- never a customer-supplied one,
// closing the obvious social-engineering vector on a refund flow, per
// R5's own acceptance criterion) and the amount is the FULL amount_in,
// not forwardAmount's discounted figure -- nothing was delivered, so no
// commission is withheld.
func (o *Orchestrator) advanceRefundPendingOneTRC20(ctx context.Context, leg relay.Leg) error {
	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.SenderAddress == nil || *order.SenderAddress == "" {
		return fmt.Errorf("order has no sender_address recorded -- cannot refund")
	}

	o.mu.Lock()
	pb, ok := o.pendingRefund[leg.ExternalID]
	o.mu.Unlock()
	if !ok {
		ref, err := o.Chain.CurrentBlockReference(ctx)
		if err != nil {
			return fmt.Errorf("resolving a current TRON block reference: %w", err)
		}
		unsignedTx, err := txbuild.BuildTransfer(o.Cfg.SlotAddress, *order.SenderAddress, leg.AmountIn, ref)
		if err != nil {
			return fmt.Errorf("building the unsigned refund transfer: %w", err)
		}
		pb = pendingForward{unsignedTx: unsignedTx}
		o.mu.Lock()
		o.pendingRefund[leg.ExternalID] = pb
		o.mu.Unlock()
	}

	deadline := time.Now().Add(defaultEnergyDeadlineWindow)
	reservation, err := o.Energy.Reserve(ctx, leg.ExternalID, *order.SenderAddress,
		o.Cfg.EnergyPerTransferUnits, "STANDARD", deadline, "relayd:refund-reserve:"+leg.ExternalID)
	if err != nil {
		return fmt.Errorf("reserving energy for refund: %w", err)
	}
	if reservation.Status != "CONFIRMED" {
		return fmt.Errorf("energy reservation ended in status %s, not CONFIRMED", reservation.Status)
	}

	digest := txbuild.Digest(pb.unsignedTx)
	estimatedUSD := estimatedUSDFor(leg.AmountIn)
	sigReq, err := o.Signing.RequestSignature(ctx, o.Cfg.SlotID, digest, estimatedUSD, "relayd:refund-sign:"+leg.ExternalID)
	if err != nil {
		return fmt.Errorf("requesting refund signature: %w", err)
	}
	switch sigReq.Status {
	case "PENDING":
		slog.Info("orchestrate: refund signature still pending approval, resuming next tick", "external_id", leg.ExternalID)
		return nil
	case "REJECTED":
		o.mu.Lock()
		delete(o.pendingRefund, leg.ExternalID)
		o.mu.Unlock()
		return fmt.Errorf("refund signature request was rejected")
	case "SIGNED":
		// fall through to broadcast
	default:
		return fmt.Errorf("unexpected signing status %q", sigReq.Status)
	}

	txID, err := o.Chain.BroadcastSigned(ctx, pb.unsignedTx, sigReq.SignedTx)
	if err != nil {
		return fmt.Errorf("broadcasting refund: %w", err)
	}

	if err := o.Store.MarkRefunded(ctx, leg.ExternalID, txID); err != nil {
		return fmt.Errorf("marking refunded: %w", err)
	}
	o.mu.Lock()
	delete(o.pendingRefund, leg.ExternalID)
	o.mu.Unlock()
	slog.Info("orchestrate: relay leg refunded", "external_id", leg.ExternalID, "tron_txid", txID)
	return nil
}

// advanceRefundPendingOneBEP20 is advanceForwardingOneBEP20's own
// refund-direction sibling -- see advanceRefundPendingOneTRC20's own doc
// comment for the shared reasoning (SenderAddress-only destination, full
// amount_in, no commission). No C4 involvement, same as the BEP20
// forward leg.
func (o *Orchestrator) advanceRefundPendingOneBEP20(ctx context.Context, leg relay.Leg) error {
	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	if order.SenderAddress == nil || *order.SenderAddress == "" {
		return fmt.Errorf("order has no sender_address recorded -- cannot refund")
	}

	o.mu.Lock()
	pb, ok := o.pendingRefundEVM[leg.ExternalID]
	o.mu.Unlock()
	if !ok {
		nonce, err := o.EVMChain.CurrentNonce(ctx, o.Cfg.SlotEVMAddress)
		if err != nil {
			return fmt.Errorf("resolving a current BSC nonce: %w", err)
		}
		gasPrice, err := o.EVMChain.SuggestGasPrice(ctx)
		if err != nil {
			return fmt.Errorf("suggesting a BSC gas price: %w", err)
		}
		params := evmtx.TxParams{Nonce: nonce, GasPrice: gasPrice, GasLimit: o.Cfg.EVMGasLimit}
		tx, digest, err := evmtx.BuildTransfer(*order.SenderAddress, leg.AmountIn, params)
		if err != nil {
			return fmt.Errorf("building the unsigned refund transfer: %w", err)
		}
		pb = pendingEVMForward{unsignedTx: tx, digest: digest}
		o.mu.Lock()
		o.pendingRefundEVM[leg.ExternalID] = pb
		o.mu.Unlock()
	}

	estimatedUSD := estimatedUSDFor(leg.AmountIn)
	sigReq, err := o.Signing.RequestSignature(ctx, o.Cfg.SlotID, pb.digest, estimatedUSD, "relayd:refund-sign:"+leg.ExternalID)
	if err != nil {
		return fmt.Errorf("requesting refund signature: %w", err)
	}
	switch sigReq.Status {
	case "PENDING":
		slog.Info("orchestrate: refund signature still pending approval, resuming next tick", "external_id", leg.ExternalID)
		return nil
	case "REJECTED":
		o.mu.Lock()
		delete(o.pendingRefundEVM, leg.ExternalID)
		o.mu.Unlock()
		return fmt.Errorf("refund signature request was rejected")
	case "SIGNED":
		// fall through to broadcast
	default:
		return fmt.Errorf("unexpected signing status %q", sigReq.Status)
	}

	signed, err := evmtx.WithSignature(pb.unsignedTx, sigReq.SignedTx)
	if err != nil {
		return fmt.Errorf("applying signature: %w", err)
	}
	txHash, err := o.EVMChain.Broadcast(ctx, signed)
	if err != nil {
		return fmt.Errorf("broadcasting refund: %w", err)
	}

	if err := o.Store.MarkRefunded(ctx, leg.ExternalID, txHash); err != nil {
		return fmt.Errorf("marking refunded: %w", err)
	}
	o.mu.Lock()
	delete(o.pendingRefundEVM, leg.ExternalID)
	o.mu.Unlock()
	slog.Info("orchestrate: relay leg refunded", "external_id", leg.ExternalID, "bsc_tx_hash", txHash)
	return nil
}
