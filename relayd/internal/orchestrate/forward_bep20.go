package orchestrate

import (
	"context"
	"fmt"
	"log/slog"

	"relayd/internal/evmtx"
	"relayd/internal/relay"
)

// advanceForwardingOneBEP20 is advanceForwardingOneTRC20's own direct
// mirror for the BEP20_TO_TRC20 direction: resolve a current nonce/gas
// price from a real BSC node, build+cache the unsigned ERC20 transfer,
// sign via S1, broadcast, mark forwarded. No C4 (energy) involvement --
// this leg spends BNB gas, not TRON energy (see Config.EnergyPerTransferUnits's
// own doc comment).
func (o *Orchestrator) advanceForwardingOneBEP20(ctx context.Context, leg relay.Leg) error {
	if leg.UpstreamDepositAddress == nil {
		return fmt.Errorf("leg has no upstream_deposit_address recorded")
	}

	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	amount, err := forwardAmount(leg.AmountIn, order.FeeUnits)
	if err != nil {
		return fmt.Errorf("computing forward amount: %w", err)
	}

	o.mu.Lock()
	pb, ok := o.pendingEVM[leg.ExternalID]
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
	sigReq, err := o.Signing.RequestSignature(ctx, o.Cfg.SlotID, pb.digest, estimatedUSD, "relayd:sign:"+leg.ExternalID)
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
	txHash, err := o.EVMChain.Broadcast(ctx, signed)
	if err != nil {
		return fmt.Errorf("broadcasting: %w", err)
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
