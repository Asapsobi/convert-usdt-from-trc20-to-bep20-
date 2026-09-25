package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"relayd/internal/relay"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

// Deposit wallets are leased from a limited pool (see the watchers' own
// addresses/pool.go). These two phases keep that pool moving: a leg
// nobody paid for is expired, and a finished leg's wallet is handed back.

// LeaseReleaser is the slice of a watcher client that hands a leg's
// deposit wallet back to the pool. *watcherclient.Client implements it; a
// DepositAddressLookup that doesn't is simply never released through.
type LeaseReleaser interface {
	RetireAddress(ctx context.Context, orderID int64, reason, idempotencyKey string) error
}

// namedOrderGetter is a conversion router that can ask the exact vendor an
// order was created with (vendors.ConversionRouter).
type namedOrderGetter interface {
	GetOrderFrom(ctx context.Context, provider, providerOrderID string) (upstream.SwapOrder, error)
}

// vendorOrder asks the vendor leg's upstream order was created with for
// its current state.
func (o *Orchestrator) vendorOrder(ctx context.Context, leg relay.Leg) (upstream.SwapOrder, error) {
	if leg.UpstreamOrderID == nil {
		return upstream.SwapOrder{}, fmt.Errorf("leg %s has no upstream order recorded", leg.ExternalID)
	}
	if g, ok := o.Upstream.(namedOrderGetter); ok && leg.UpstreamProviderName != nil {
		return g.GetOrderFrom(ctx, *leg.UpstreamProviderName, *leg.UpstreamOrderID)
	}
	return o.Upstream.GetOrder(ctx, *leg.UpstreamOrderID)
}

func (o *Orchestrator) watcherFor(leg relay.Leg) DepositAddressLookup {
	if leg.Direction == relay.BEP20ToTRC20 {
		return o.BEP20DepositWatcher
	}
	return o.TronDepositWatcher
}

// expireUnpaidLegs expires every leg whose deposit window, plus
// Config.DepositGrace, passed with nothing paid: its C1 order goes
// quoted -> expired (so a payment arriving later is recorded as orphaned
// instead of funding it) and the leg becomes EXPIRED, which frees its
// wallet. A leg whose deposit landed in the meantime is left alone -- C1's
// version check makes the race safe either way. A zero DepositGrace
// disables this phase.
func (o *Orchestrator) expireUnpaidLegs(ctx context.Context) error {
	if o.Cfg.DepositGrace <= 0 {
		return nil
	}
	legs, err := o.Store.ListByStatus(ctx, relay.StatusAwaitingDeposit)
	if err != nil {
		return fmt.Errorf("listing awaiting-deposit legs for expiry: %w", err)
	}
	now := time.Now()
	for _, leg := range legs {
		if leg.ForwardAttemptStartedAt != nil || now.Sub(leg.CreatedAt) < o.Cfg.DepositGrace {
			continue // a deposit arrived, or it can't be past its window yet
		}
		if err := o.expireOne(ctx, leg, now); err != nil {
			slog.Error("orchestrate: expiring an unpaid leg failed, will retry next tick", "external_id", leg.ExternalID, "error", err)
		}
	}
	return nil
}

func (o *Orchestrator) expireOne(ctx context.Context, leg relay.Leg, now time.Time) error {
	order, err := o.Ledger.GetOrder(ctx, leg.ExternalID)
	if err != nil {
		return fmt.Errorf("fetching order: %w", err)
	}
	switch order.State {
	case "expired":
		// C1 already expired it (an earlier tick crashed before the local
		// mark): just finish the local side.
	case "quoted":
		if now.Before(order.QuoteExpiresAt.Add(o.Cfg.DepositGrace)) {
			return nil
		}
		if _, err := o.Ledger.Transition(ctx, leg.ExternalID, "expired", order.Version, "deposit_window_passed",
			now.UTC(), "relayd:expire:"+leg.ExternalID); err != nil {
			return fmt.Errorf("expiring the C1 order (a deposit may have just landed): %w", err)
		}
	default:
		return nil // funded or further along: a deposit arrived after all
	}
	if err := o.Store.MarkExpired(ctx, leg.ExternalID); err != nil {
		return err
	}
	slog.Info("orchestrate: relay leg expired unpaid", "external_id", leg.ExternalID,
		"deposit_deadline", order.QuoteExpiresAt.Format(time.RFC3339))
	return nil
}

// releaseRetryAfter spaces out retries of a release whose watcher failed.
const releaseRetryAfter = 5 * time.Minute

// releaseFinishedLeases hands the deposit wallet of every finished leg
// back to its watcher's pool. Nothing is still being sent from a
// finished leg's wallet: its forward or refund has confirmed, or nothing
// was ever sent.
func (o *Orchestrator) releaseFinishedLeases(ctx context.Context) error {
	legs, err := o.Store.ListLeasesToRelease(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, leg := range legs {
		releaser, ok := o.watcherFor(leg).(LeaseReleaser)
		if !ok {
			continue
		}
		o.mu.Lock()
		retryAt := o.releaseRetryAt[leg.ExternalID]
		o.mu.Unlock()
		if now.Before(retryAt) {
			continue // its watcher failed recently; don't hammer it (or the log) every tick
		}
		reason := map[relay.Status]string{
			relay.StatusSettled: "settled", relay.StatusRefunded: "refunded",
			relay.StatusUnrecoverable: "unrecoverable", relay.StatusExpired: "expired",
		}[leg.Status]
		err := releaser.RetireAddress(ctx, leg.OrderID, reason, "relayd:release:"+leg.ExternalID)
		var apiErr *watcherclient.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			// The watcher has no lease for this order -- nothing to release.
			slog.Warn("orchestrate: watcher has no lease for a finished leg, treating it as released", "external_id", leg.ExternalID)
			err = nil
		}
		if err != nil {
			o.mu.Lock()
			if o.releaseRetryAt == nil {
				o.releaseRetryAt = make(map[string]time.Time)
			}
			o.releaseRetryAt[leg.ExternalID] = now.Add(releaseRetryAfter)
			o.mu.Unlock()
			slog.Error("orchestrate: releasing a finished leg's wallet failed, will retry", "external_id", leg.ExternalID,
				"retry_in", releaseRetryAfter, "error", err)
			continue
		}
		if err := o.Store.MarkLeaseReleased(ctx, leg.ExternalID); err != nil {
			slog.Error("orchestrate: recording a released wallet failed", "external_id", leg.ExternalID, "error", err)
			continue
		}
		slog.Info("orchestrate: deposit wallet released to the pool", "external_id", leg.ExternalID, "reason", reason)
	}
	return nil
}
