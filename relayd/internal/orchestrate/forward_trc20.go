package orchestrate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
func (o *Orchestrator) advanceForwardingOneTRC20(ctx context.Context, leg relay.Leg) error {
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

	deadline := time.Now().Add(defaultEnergyDeadlineWindow)
	reservation, err := o.Energy.Reserve(ctx, leg.ExternalID, *leg.UpstreamDepositAddress,
		o.Cfg.EnergyPerTransferUnits, "STANDARD", deadline, "relayd:reserve:"+leg.ExternalID)
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
		unsignedTx, err := txbuild.BuildTransfer(o.Cfg.SlotAddress, *leg.UpstreamDepositAddress, amount, ref)
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
	sigReq, err := o.Signing.RequestSignature(ctx, o.Cfg.SlotID, digest, estimatedUSD, "relayd:sign:"+leg.ExternalID)
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

	txID, err := o.Chain.BroadcastSigned(ctx, pb.unsignedTx, sigReq.SignedTx)
	if err != nil {
		return fmt.Errorf("broadcasting: %w", err)
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
