package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"relayd/internal/alert"
	"relayd/internal/evmbroadcast"
	"relayd/internal/evmtx"
	"relayd/internal/relay"
)

// advanceForwardingOneBEP20 is advanceForwardingOneTRC20's own direct
// mirror for the BEP20_TO_TRC20 direction: resolve a current nonce/gas
// price from a real BSC node, build+cache the unsigned ERC20 transfer,
// sign via S1, broadcast, mark forwarded. No C4 (energy) involvement --
// this leg spends BNB gas, not TRON energy (see Config.EnergyPerTransferUnits's
// own doc comment).
//
// The forward transfer must come FROM this leg's own real deposit
// address (where the customer's funds actually are), signed by that
// exact address's own per-order key -- never relayd's own shared slot
// key, which never held these funds. Both the nonce (an on-chain,
// address-scoped counter) and the signature are resolved against
// leg.DepositAddress/leg.DepositDerivationIndex accordingly. See this
// file's own git history / the plan that introduced this comment for
// the real, previously-uncaught bug this replaced (signing with the
// slot key while a leg's own deposit address held the actual funds).
func (o *Orchestrator) advanceForwardingOneBEP20(ctx context.Context, leg relay.Leg) error {
	if leg.UpstreamDepositAddress == nil {
		return fmt.Errorf("leg has no upstream_deposit_address recorded")
	}
	if leg.DepositDerivationIndex == nil {
		return fmt.Errorf("leg %s has no deposit_derivation_index recorded -- a BEP20_TO_TRC20 leg must always have one "+
			"(set at Create from the real AssignAddress response); this is a data-integrity bug, not a transient condition",
			leg.ExternalID)
	}

	// A broadcast already happened for this leg in an earlier tick --
	// check ITS OWN on-chain execution result before doing anything else.
	// See forward_trc20.go's own identical pattern (and pendingEVMForward's
	// own doc comment) for why this must happen before this leg is ever
	// marked forwarded: a mined BSC transaction can still revert.
	o.mu.Lock()
	existingPB, alreadyBroadcast := o.pendingEVM[leg.ExternalID]
	o.mu.Unlock()
	if alreadyBroadcast && existingPB.broadcastTxHash != "" {
		return o.checkForwardEVMExecutionAndFinish(ctx, leg, existingPB.broadcastTxHash)
	}

	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	amount, err := forwardAmount(leg.AmountIn, order.FeeUnits)
	if err != nil {
		return fmt.Errorf("computing forward amount: %w", err)
	}

	if o.BEP20DepositWatcher == nil {
		return fmt.Errorf("leg %s is a BEP20_TO_TRC20 forward leg, but this Orchestrator has no BEP20DepositWatcher configured -- "+
			"refusing to sign without the defense-in-depth cross-check; this is a startup/wiring bug, not a transient condition",
			leg.ExternalID)
	}

	// Defense in depth: independently re-fetch this order's own deposit
	// address/index from depositwatcher's own live address book and
	// verify it agrees with what's recorded locally, BEFORE ever asking
	// S1 to sign anything. S1's own RequestDepositSweepSignature performs
	// no such check itself -- it will derive-and-sign for whatever index
	// it's given, so this is the one place that verification happens at
	// all.
	got, err := o.BEP20DepositWatcher.GetAddress(ctx, leg.OrderID)
	if err != nil {
		return fmt.Errorf("re-verifying deposit address with depositwatcher: %w", err)
	}
	if got.Address != leg.DepositAddress || got.DerivationIndex == nil || *got.DerivationIndex != *leg.DepositDerivationIndex {
		detail := fmt.Sprintf("leg %s: locally recorded deposit_address=%s deposit_derivation_index=%d, "+
			"but depositwatcher's own live record says address=%s derivation_index=%v -- refusing to sign",
			leg.ExternalID, leg.DepositAddress, *leg.DepositDerivationIndex, got.Address, got.DerivationIndex)
		if alertErr := o.Alert.Fire(ctx, alert.Alert{
			Severity: alert.SeverityCritical, ExternalID: leg.ExternalID,
			Reason: "relay_leg_deposit_address_mismatch", Detail: detail,
		}); alertErr != nil {
			slog.Error("orchestrate: firing the deposit-address-mismatch alert itself failed", "external_id", leg.ExternalID, "error", alertErr)
		}
		return fmt.Errorf("%s", detail)
	}

	o.mu.Lock()
	pb, ok := o.pendingEVM[leg.ExternalID]
	o.mu.Unlock()
	if !ok {
		nonce, err := o.EVMChain.CurrentNonce(ctx, leg.DepositAddress)
		if err != nil {
			return fmt.Errorf("resolving a current BSC nonce: %w", err)
		}
		gasPrice, err := o.EVMChain.SuggestGasPrice(ctx)
		if err != nil {
			return fmt.Errorf("suggesting a BSC gas price: %w", err)
		}
		params := evmtx.TxParams{Nonce: nonce, GasPrice: gasPrice, GasLimit: o.Cfg.EVMGasLimit}
		tx, digest, err := evmtx.BuildTransfer(*leg.UpstreamDepositAddress, amount, params)
		if err != nil {
			return fmt.Errorf("building the unsigned transfer: %w", err)
		}
		pb = pendingEVMForward{unsignedTx: tx, digest: digest}
		o.mu.Lock()
		o.pendingEVM[leg.ExternalID] = pb
		o.mu.Unlock()
	}

	estimatedUSD := estimatedUSDFor(amount)
	// Digest-scoped idempotency key -- see forward_trc20.go's own
	// identical fix and doc comment for why leg.ExternalID alone is not
	// enough: the unsigned tx (nonce/gas price resolved fresh, per this
	// function's own top-of-file doc comment) changes across a relayd
	// restart, since pendingEVM is in-memory only, but a key scoped to
	// ExternalID alone would stay stable and cause S1 to replay a
	// stale, since-mismatched signature.
	idemKey := fmt.Sprintf("relayd:sign:%s:%x", leg.ExternalID, pb.digest)
	sigReq, err := o.Signing.RequestDepositSweepSignature(ctx, *leg.DepositDerivationIndex, pb.digest, estimatedUSD, idemKey)
	if err != nil {
		return fmt.Errorf("requesting signature: %w", err)
	}
	switch sigReq.Status {
	case "PENDING":
		slog.Info("orchestrate: signature still pending approval, resuming next tick", "external_id", leg.ExternalID)
		return nil
	case "REJECTED":
		o.mu.Lock()
		delete(o.pendingEVM, leg.ExternalID)
		o.mu.Unlock()
		return fmt.Errorf("signature request was rejected")
	case "SIGNED":
		// fall through to broadcast
	default:
		return fmt.Errorf("unexpected signing status %q", sigReq.Status)
	}

	signed, err := evmtx.WithSignature(pb.unsignedTx, sigReq.SignedTx)
	if err != nil {
		return fmt.Errorf("applying signature: %w", err)
	}
	intent := transferIntent{sender: leg.DepositAddress, recipient: *leg.UpstreamDepositAddress, amount: amount}
	if err := o.preflightForwardEVM(ctx, leg, signed, intent); err != nil {
		return err
	}
	txHash, err := o.EVMChain.Broadcast(ctx, signed)
	if err != nil {
		return fmt.Errorf("broadcasting: %w", err)
	}

	// Do NOT mark forwarded yet -- a broadcast BSC can accept into a
	// block while the transaction itself still reverts (out of gas, a
	// reverted ERC20 transfer, etc). Cache the hash and let the
	// early-return check at the top of this function (on the next
	// tick) verify the real execution receipt via checkForwardEVMExecutionAndFinish
	// before ever calling MarkForwarded. See forward_trc20.go's own
	// identical pattern.
	pb.broadcastTxHash = txHash
	o.mu.Lock()
	o.pendingEVM[leg.ExternalID] = pb
	o.mu.Unlock()
	slog.Info("orchestrate: forward transfer broadcast, awaiting on-chain execution result", "external_id", leg.ExternalID, "bsc_tx_hash", txHash)
	return nil
}

// checkForwardEVMExecutionAndFinish is advanceForwardingOneBEP20's own
// mirror of checkForwardExecutionAndFinish (forward_trc20.go): a
// broadcast already happened for this leg in an earlier tick, so
// verify its REAL on-chain execution result before ever marking the
// leg forwarded. EVMChain.IsFinal already correctly distinguishes
// not-yet-mined (false, nil) from a confirmed revert (false, non-nil
// err) from confirmed success (true, nil) -- see evmbroadcast.go's own
// IsFinal implementation -- so unlike the TRON path, no new interface
// method was needed here, only wiring this existing check into the
// orchestration flow at all.
func (o *Orchestrator) checkForwardEVMExecutionAndFinish(ctx context.Context, leg relay.Leg, txHash string) error {
	final, err := o.EVMFinality.IsFinal(ctx, txHash)
	if err != nil {
		if !errors.Is(err, evmbroadcast.ErrReverted) {
			// A transient error checking status (an RPC hiccup fetching
			// the receipt or the finalized height) -- NOT a confirmed
			// on-chain outcome either way. Leave the cached broadcast in
			// place and retry this same check next tick: clearing it
			// here would risk abandoning a transaction that may still go
			// on to succeed, and a freshly rebuilt retry (a new nonce)
			// landing too would be a real double-spend. See
			// evmbroadcast.ErrReverted's own doc comment.
			return fmt.Errorf("checking forward transfer execution result: %w", err)
		}
		detail := fmt.Sprintf("leg %s: forward transfer bsc_tx_hash=%s was broadcast and mined but its execution FAILED: %s -- "+
			"funds were NOT moved; clearing cached state to rebuild and retry with a fresh nonce/gas price",
			leg.ExternalID, txHash, err)
		if alertErr := o.Alert.Fire(ctx, alert.Alert{
			Severity: alert.SeverityCritical, ExternalID: leg.ExternalID,
			Reason: "relay_leg_forward_execution_failed", Detail: detail,
		}); alertErr != nil {
			slog.Error("orchestrate: firing the forward-execution-failed alert itself failed", "external_id", leg.ExternalID, "error", alertErr)
		}
		o.mu.Lock()
		delete(o.pendingEVM, leg.ExternalID)
		o.mu.Unlock()
		return fmt.Errorf("%s", detail)
	}
	if !final {
		slog.Info("orchestrate: forward transfer broadcast but not yet final, resuming next tick", "external_id", leg.ExternalID, "bsc_tx_hash", txHash)
		return nil
	}

	if err := o.Store.MarkForwarded(ctx, leg.ExternalID, txHash); err != nil {
		return fmt.Errorf("marking forwarded: %w", err)
	}
	o.mu.Lock()
	delete(o.pendingEVM, leg.ExternalID)
	o.mu.Unlock()
	slog.Info("orchestrate: relay leg forwarded", "external_id", leg.ExternalID, "bsc_tx_hash", txHash)
	return nil
}
