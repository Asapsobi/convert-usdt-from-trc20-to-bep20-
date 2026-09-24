package orchestrate

import (
	"context"
	"fmt"
	"log/slog"

	"relayd/internal/alert"
	"relayd/internal/relay"
)

// reverifyDepositAddress independently re-fetches leg's deposit address
// and derivation index from the watcher that assigned them, and refuses
// (with a critical alert) if they disagree with what relayd recorded.
// Every transfer out of a deposit address -- forward or refund -- runs
// this BEFORE asking S1 to sign: S1 derives-and-signs for whatever index
// it's given, so this is the one place the index is checked at all.
func (o *Orchestrator) reverifyDepositAddress(ctx context.Context, leg relay.Leg) error {
	var watcher DepositAddressLookup
	var name string
	switch leg.Direction {
	case relay.BEP20ToTRC20:
		watcher, name = o.BEP20DepositWatcher, "depositwatcher"
	case relay.TRC20ToBEP20:
		watcher, name = o.TronDepositWatcher, "tronwatcher"
	default:
		return fmt.Errorf("leg %s has unrecognized direction %q", leg.ExternalID, leg.Direction)
	}
	if leg.DepositDerivationIndex == nil {
		return fmt.Errorf("leg %s has no deposit_derivation_index recorded -- every relay leg gets one at Create "+
			"from the real AssignAddress response; this is a data-integrity bug, not a transient condition", leg.ExternalID)
	}
	if watcher == nil {
		return fmt.Errorf("leg %s is a %s leg, but this Orchestrator has no %s configured -- "+
			"refusing to sign without the defense-in-depth cross-check; this is a startup/wiring bug, not a transient condition",
			leg.ExternalID, leg.Direction, name)
	}

	got, err := watcher.GetAddress(ctx, leg.OrderID)
	if err != nil {
		return fmt.Errorf("re-verifying deposit address with %s: %w", name, err)
	}
	if got.Address != leg.DepositAddress || got.DerivationIndex == nil || *got.DerivationIndex != *leg.DepositDerivationIndex {
		detail := fmt.Sprintf("leg %s: locally recorded deposit_address=%s deposit_derivation_index=%d, "+
			"but %s's own live record says address=%s derivation_index=%v -- refusing to sign",
			leg.ExternalID, leg.DepositAddress, *leg.DepositDerivationIndex, name, got.Address, got.DerivationIndex)
		if alertErr := o.Alert.Fire(ctx, alert.Alert{
			Severity: alert.SeverityCritical, ExternalID: leg.ExternalID,
			Reason: "relay_leg_deposit_address_mismatch", Detail: detail,
		}); alertErr != nil {
			slog.Error("orchestrate: firing the deposit-address-mismatch alert itself failed", "external_id", leg.ExternalID, "error", alertErr)
		}
		return fmt.Errorf("%s", detail)
	}
	return nil
}
