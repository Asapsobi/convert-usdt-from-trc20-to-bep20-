// The stale-relay-leg reconciliation alarm
// (docs/02-architecture/model-f-relay-architecture.md §5's own "a
// reconciliation check that finds one of these accounts still open
// after, say, one hour is an operational alarm — nothing in a working
// relay should sit here that long"). Purely observational: it fires a
// SeverityWarning alert (internal/alert) once per leg and takes no
// automatic recovery action of its own -- recovery, where one exists, is
// already handled per-status by refund.go's own timeout-triggered
// refund and settle.go's own UNRECOVERABLE path. This is the catch-all
// visibility net for everything those two don't (or can't) act on
// automatically: a leg legitimately waiting on a slow upstream vendor
// (FORWARDED), or one waiting on a human S1 approval that never comes
// (REFUND_PENDING).
//
// Deliberately does NOT cover AWAITING_DEPOSIT. That status doesn't need
// this alarm's kind of passive visibility: refund.go's own
// refundStuckAwaitingDepositLegs already actively recovers it (a leg's
// own local UpdatedAt never advances past its original quote-time
// CreatedAt while still AWAITING_DEPOSIT, since no Mark<State> call
// fires until it leaves that status -- so refund.go tracks a dedicated
// relay_legs.forward_attempt_started_at timestamp instead, set once the
// first time a deposit is actually observed, rather than reusing
// UpdatedAt the way staleCheckedStatuses below can for every other
// status). An alarm here would be redundant with that recovery path,
// not a gap.
package orchestrate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"relayd/internal/alert"
	"relayd/internal/relay"
)

// staleCheckedStatuses is every non-terminal status whose own local
// UpdatedAt precisely reflects "time since this specific status was
// entered" (each is set by a real Mark<State> call, unlike
// AWAITING_DEPOSIT -- see this file's own top-of-file doc comment).
var staleCheckedStatuses = []relay.Status{
	relay.StatusForwarding,
	relay.StatusForwarded,
	relay.StatusRefundPending,
}

// checkStaleLegs scans every leg in one of staleCheckedStatuses and
// fires a one-time alert for any that has sat there longer than
// Config.StaleLegAlertAfter. A zero StaleLegAlertAfter disables this
// phase entirely, the same explicit-opt-in posture
// Config.ForwardingTimeout already takes.
func (o *Orchestrator) checkStaleLegs(ctx context.Context) error {
	if o.Cfg.StaleLegAlertAfter <= 0 {
		return nil
	}
	for _, status := range staleCheckedStatuses {
		legs, err := o.Store.ListByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("listing %s legs for the stale-leg reconciliation check: %w", status, err)
		}
		for _, leg := range legs {
			if leg.StaleAlertedAt != nil {
				continue // already alerted once for this leg -- see MarkStaleAlerted's own doc comment
			}
			age := time.Since(leg.UpdatedAt)
			if age < o.Cfg.StaleLegAlertAfter {
				continue
			}
			if err := o.fireStaleLegAlert(ctx, leg, age); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *Orchestrator) fireStaleLegAlert(ctx context.Context, leg relay.Leg, age time.Duration) error {
	fired, err := o.Store.MarkStaleAlerted(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("marking %s stale-alerted: %w", leg.ExternalID, err)
	}
	if !fired {
		// A concurrent/earlier tick already claimed this one -- safe,
		// expected replay, not an error.
		return nil
	}
	// The claim above already committed -- a retry on failure would just
	// see fired=false and silently skip, permanently losing this alert,
	// so a delivery failure here is logged loudly rather than returned
	// (which would only block OTHER legs in this same phase for no
	// benefit). Same posture settle.go's own handleUnrecoverable takes.
	if err := o.Alert.Fire(ctx, alert.Alert{
		Severity:   alert.SeverityWarning,
		ExternalID: leg.ExternalID,
		Reason:     "relay_leg_stale",
		Detail: fmt.Sprintf("relay leg %s has been %s for %s, past the configured %s reconciliation threshold -- "+
			"investigate (this is not necessarily UNRECOVERABLE, but nothing in a working relay should sit here this long).",
			leg.ExternalID, leg.Status, age.Round(time.Second), o.Cfg.StaleLegAlertAfter),
	}); err != nil {
		slog.Error("orchestrate: firing the stale-leg alert itself failed", "external_id", leg.ExternalID, "error", err)
	}
	return nil
}
