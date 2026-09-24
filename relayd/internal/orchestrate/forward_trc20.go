package orchestrate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"relayd/internal/alert"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/txbuild"
)

// energyDeadlineWindow bounds how long a single energy reservation
// attempt is allowed to take -- mirrors dispatcher's own
// EnergyReservationWait config, applied the same way.
const defaultEnergyDeadlineWindow = 30 * time.Second

// advanceForwardingLegs scans every leg locally recorded FORWARDING and
// drives it toward FORWARDED, dispatching by Direction: TRC20_TO_BEP20
// legs forward over TRON (reserve energy via C4, sign, broadcast via
// internal/tronbroadcast); BEP20_TO_TRC20 legs forward over BSC (resolve
// nonce/gas price, sign, broadcast via internal/evmbroadcast -- no C4
// involvement, see Config.EnergyPerTransferUnits's own doc comment).
func (o *Orchestrator) advanceForwardingLegs(ctx context.Context) error {
	legs, err := o.Store.ListByStatus(ctx, relay.StatusForwarding)
	if err != nil {
		return fmt.Errorf("listing forwarding legs: %w", err)
	}
	for _, leg := range legs {
		switch leg.Direction {
		case relay.TRC20ToBEP20:
			if err := o.advanceForwardingOneTRC20(ctx, leg); err != nil {
				slog.Error("orchestrate: advancing TRC20 forward leg failed, will retry next tick",
					"external_id", leg.ExternalID, "error", err)
			}
		case relay.BEP20ToTRC20:
			if err := o.advanceForwardingOneBEP20(ctx, leg); err != nil {
				slog.Error("orchestrate: advancing BEP20 forward leg failed, will retry next tick",
					"external_id", leg.ExternalID, "error", err)
			}
		default:
			slog.Error("orchestrate: relay leg has unrecognized direction, skipping",
				"external_id", leg.ExternalID, "direction", leg.Direction)
		}
	}
	return nil
}

// advanceForwardingOneTRC20 reserves energy (idempotent on C4's own
// contract, safe to call every tick), then requests/awaits a signature
// and broadcasts, exactly mirroring dispatcher/internal/orchestrate's
// own dispatchOne shape: never blocks on a PENDING signature, and
// caches the unsigned tx bytes in memory across ticks since
// BlockReference is not deterministic and cannot simply be rebuilt.
//
// The forward transfer must come FROM this leg's own real deposit
// address (where the customer's funds actually are), signed by that
// exact address's own per-order key -- never relayd's own shared slot
// key, which never held these funds. Unlike the BEP20_TO_TRC20
// direction (forward_bep20.go), TRON bakes the sender address directly
// into the transaction's own signed bytes (txbuild.BuildTransfer's
// fromAddress becomes TriggerSmartContract.OwnerAddress, proto-marshaled
// into what gets SHA256'd into the digest) -- so fromAddress and the
// signing key below must change together, as a coupled pair; a real
// TRON node rejects a broadcast outright if they disagree. See this
// file's own git history / the plan that introduced this comment for
// the real, previously-uncaught bug this replaced (signing with the
// slot key while a leg's own deposit address held the actual funds).
func (o *Orchestrator) advanceForwardingOneTRC20(ctx context.Context, leg relay.Leg) error {
	if leg.UpstreamDepositAddress == nil {
		return fmt.Errorf("leg has no upstream_deposit_address recorded")
	}
	if leg.DepositDerivationIndex == nil {
		return fmt.Errorf("leg %s has no deposit_derivation_index recorded -- a TRC20_TO_BEP20 leg must always have one "+
			"(set at Create from the real AssignAddress response); this is a data-integrity bug, not a transient condition",
			leg.ExternalID)
	}

	// A broadcast already happened for this leg in an earlier tick --
	// check ITS OWN on-chain execution result before doing anything else
	// (not re-deriving the amount, not re-reserving energy, not
	// re-signing). See pendingForward's own doc comment and
	// checkForwardExecutionAndFinish for why this must happen before this
	// leg is ever marked forwarded.
	o.mu.Lock()
	existingPB, alreadyBroadcast := o.pending[leg.ExternalID]
	o.mu.Unlock()
	if alreadyBroadcast && existingPB.broadcastTxID != "" {
		return o.checkForwardExecutionAndFinish(ctx, leg, existingPB.broadcastTxID)
	}

	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	amount, err := forwardAmount(leg.AmountIn, order.FeeUnits)
	if err != nil {
		return fmt.Errorf("computing forward amount: %w", err)
	}

	if err := o.reverifyDepositAddress(ctx, leg); err != nil {
		return err
	}

	// Energy must be delegated to leg.DepositAddress -- the customer's own
	// per-order deposit address, which is the transaction's own SENDER
	// (txbuild.BuildTransfer below signs FROM this exact address) and
	// therefore the only address that needs energy to execute the TRC20
	// transfer contract call at all. leg.UpstreamDepositAddress (the
	// vendor's own deposit address) is the RECIPIENT -- it never needs
	// energy delegated to it for this transfer to succeed, and delegating
	// there instead leaves the real sender with no resources to broadcast
	// with (confirmed live: a real broadcast attempt failed with "account
	// does not exist" for leg.DepositAddress, the account TRON's own
	// model had never seen any resources delegated to).
	// The idempotency key includes the target address itself: C4's own
	// Reserve (energybroker/internal/reservations.Service.Create) replays
	// the ORIGINAL reservation verbatim -- original target address
	// included -- for any repeated idempotency key, regardless of what
	// target address THIS call passes. Confirmed live: leg.ExternalID
	// alone as the key meant every retry after the leg.UpstreamDepositAddress
	// -> leg.DepositAddress fix above just replayed the very first
	// (wrong-target) reservation, so no energy was ever actually
	// delegated to the real sender -- the same idempotency-staleness bug
	// class as this file's own digest-scoped signing key fix, applied
	// here to the target address instead of a digest.
	deadline := time.Now().Add(defaultEnergyDeadlineWindow)
	reserveIdemKey := fmt.Sprintf("relayd:reserve:%s:%s", leg.ExternalID, leg.DepositAddress)
	reservation, err := o.Energy.Reserve(ctx, leg.ExternalID, leg.DepositAddress,
		o.Cfg.EnergyPerTransferUnits, "STANDARD", deadline, reserveIdemKey)
	if err != nil {
		return fmt.Errorf("reserving energy: %w", err)
	}
	if reservation.Status != "CONFIRMED" {
		return fmt.Errorf("energy reservation ended in status %s, not CONFIRMED", reservation.Status)
	}

	o.mu.Lock()
	pb, ok := o.pending[leg.ExternalID]
	o.mu.Unlock()
	if !ok {
		ref, err := o.Chain.CurrentBlockReference(ctx)
		if err != nil {
			return fmt.Errorf("resolving a current TRON block reference: %w", err)
		}
		unsignedTx, err := txbuild.BuildTransfer(leg.DepositAddress, *leg.UpstreamDepositAddress, amount, ref)
		if err != nil {
			return fmt.Errorf("building the unsigned transfer: %w", err)
		}
		pb = pendingForward{unsignedTx: unsignedTx}
		o.mu.Lock()
		o.pending[leg.ExternalID] = pb
		o.mu.Unlock()
	}

	digest := txbuild.Digest(pb.unsignedTx)
	estimatedUSD := estimatedUSDFor(amount)
	// The idempotency key includes the digest itself, not just
	// leg.ExternalID: the unsigned tx (and therefore the digest) is
	// rebuilt from a FRESH TRON block reference whenever this leg's own
	// in-memory pending cache is empty -- normal on this process's first
	// attempt at this leg, but also true after ANY relayd restart, since
	// that cache is never persisted (this file's own pendingForward doc
	// comment already names that limitation). A key scoped to
	// leg.ExternalID alone stays identical across such a restart even
	// though the digest changed, so S1's own idempotent-replay contract
	// (correctly) returns the ORIGINAL signature -- for a digest that no
	// longer matches what's about to be broadcast. Confirmed live: TRON's
	// own network rejected the mismatched signature outright ("Validate
	// signature error ... not contained of permission"), so this failed
	// safely, but a digest-scoped key is what actually makes a retried
	// signing request idempotent on the thing it must be idempotent on.
	idemKey := fmt.Sprintf("relayd:sign:%s:%x", leg.ExternalID, digest)
	sigReq, err := o.Signing.RequestTronDepositSweepSignature(ctx, *leg.DepositDerivationIndex, digest, estimatedUSD, idemKey)
	if err != nil {
		return fmt.Errorf("requesting signature: %w", err)
	}
	switch sigReq.Status {
	case "PENDING":
		slog.Info("orchestrate: signature still pending approval, resuming next tick", "external_id", leg.ExternalID)
		return nil
	case "REJECTED":
		o.mu.Lock()
		delete(o.pending, leg.ExternalID)
		o.mu.Unlock()
		return fmt.Errorf("signature request was rejected")
	case "SIGNED":
		// fall through to broadcast
	default:
		return fmt.Errorf("unexpected signing status %q", sigReq.Status)
	}

	intent := transferIntent{sender: leg.DepositAddress, recipient: *leg.UpstreamDepositAddress, amount: amount}
	if err := o.preflightForwardTRON(ctx, leg, pb.unsignedTx, intent); err != nil {
		return err
	}
	txID, err := o.Chain.BroadcastSigned(ctx, pb.unsignedTx, sigReq.SignedTx)
	if err != nil {
		return fmt.Errorf("broadcasting: %w", err)
	}

	// Record the broadcast txid -- do NOT mark forwarded yet.
	// BroadcastSigned returning no error only means the network ACCEPTED
	// the transaction into a block; it does not mean the transaction's
	// own execution succeeded (confirmed live: a real, accepted broadcast
	// still failed on-chain with "OUT_OF_ENERGY", moving zero funds while
	// this exact code path used to log "relay leg forwarded" and mark it
	// done). The NEXT tick's own call to this function short-circuits to
	// checkForwardExecutionAndFinish via the broadcastTxID check at this
	// function's own top, which is the only place a leg is actually
	// marked forwarded.
	pb.broadcastTxID = txID
	o.mu.Lock()
	o.pending[leg.ExternalID] = pb
	o.mu.Unlock()
	slog.Info("orchestrate: forward transfer broadcast, awaiting on-chain execution result", "external_id", leg.ExternalID, "tron_txid", txID)
	return nil
}

// checkForwardExecutionAndFinish checks a previously-broadcast forward
// transfer's own on-chain execution result and marks the leg forwarded
// ONLY on confirmed success. Not yet final (not indexed yet) is not an
// error -- this returns nil and the same broadcastTxID is checked again
// next tick. A confirmed FAILURE clears the cached unsigned tx and
// broadcast id so the next tick rebuilds and retries entirely from
// scratch: a fresh block reference, a fresh energy reservation (critical
// if, as in the real incident this was built for, the previous attempt's
// own energy amount was simply too low for this transfer to complete).
func (o *Orchestrator) checkForwardExecutionAndFinish(ctx context.Context, leg relay.Leg, txID string) error {
	final, success, failureReason, err := o.Finality.CheckExecution(ctx, txID)
	if err != nil {
		return fmt.Errorf("checking forward transfer execution result: %w", err)
	}
	if !final {
		slog.Info("orchestrate: forward transfer broadcast but not yet finalized, resuming next tick", "external_id", leg.ExternalID, "tron_txid", txID)
		return nil
	}
	if !success {
		o.mu.Lock()
		delete(o.pending, leg.ExternalID)
		o.mu.Unlock()
		if alertErr := o.Alert.Fire(ctx, alert.Alert{
			Severity: alert.SeverityCritical, ExternalID: leg.ExternalID,
			Reason: "relay_leg_forward_execution_failed",
			Detail: fmt.Sprintf("leg %s: forward transfer %s was accepted by the network but its own execution failed: %s -- rebuilding and retrying next tick",
				leg.ExternalID, txID, failureReason),
		}); alertErr != nil {
			slog.Error("orchestrate: firing the forward-execution-failed alert itself failed", "external_id", leg.ExternalID, "error", alertErr)
		}
		return fmt.Errorf("forward transfer %s failed on-chain execution: %s", txID, failureReason)
	}

	if err := o.Store.MarkForwarded(ctx, leg.ExternalID, txID); err != nil {
		return fmt.Errorf("marking forwarded: %w", err)
	}
	o.mu.Lock()
	delete(o.pending, leg.ExternalID)
	o.mu.Unlock()
	slog.Info("orchestrate: relay leg forwarded", "external_id", leg.ExternalID, "tron_txid", txID)
	return nil
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
