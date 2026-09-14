// Package reconcile is C6.4: the background loop that closes the real
// partial-failure window C6.3's own "Read this second" describes --
// C1's order created, C2's address assignment lost (network partition,
// C6 crashed mid-choreography, C2 down). Without this loop that gap is
// permanent; C6.3's own build-prompts note calls this "not optional
// polish."
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"gateway/internal/c2client"
	"gateway/internal/orders"
	"gateway/internal/quotes"
)

// DefaultInterval is how often RunLoop ticks, when Config's own value
// is unset.
const DefaultInterval = 30 * time.Second

// DefaultGracePeriod is how old a gateway_orders row must be before
// this loop will touch it -- long enough that C6.3's own step 5 (the
// synchronous C2 call a fresh POST /v1/orders request just made) is
// never still in flight when this loop first looks at that same row.
const DefaultGracePeriod = 10 * time.Second

// DefaultAlertThreshold is how long a row may sit address_pending
// before this loop raises an alert, when Config's own value is unset.
const DefaultAlertThreshold = 10 * time.Minute

// Alerter is how a row stuck past the alert threshold gets surfaced --
// optional (nil means no alert is sent, never a panic), same
// convention C4's own buffer.Alerter established.
type Alerter interface {
	AlertAddressPending(ctx context.Context, o orders.GatewayOrder, age time.Duration)
}

// Config tunes the loop. Every field defaults sensibly when zero.
type Config struct {
	Interval       time.Duration
	GracePeriod    time.Duration
	AlertThreshold time.Duration
}

func (c Config) interval() time.Duration {
	if c.Interval <= 0 {
		return DefaultInterval
	}
	return c.Interval
}

func (c Config) gracePeriod() time.Duration {
	if c.GracePeriod <= 0 {
		return DefaultGracePeriod
	}
	return c.GracePeriod
}

func (c Config) alertThreshold() time.Duration {
	if c.AlertThreshold <= 0 {
		return DefaultAlertThreshold
	}
	return c.AlertThreshold
}

// Reconciler retries C2's own address assignment for every
// gateway_orders row C6.3 left in address_pending.
type Reconciler struct {
	orders  *orders.Store
	quotes  *quotes.Store
	watcher *c2client.Client
	alerter Alerter
	cfg     Config

	lastPendingCount atomic.Int64 // C6.8's own orders_address_pending gauge reads this
}

// NewReconciler wires a Reconciler. alerter may be nil.
func NewReconciler(ordersStore *orders.Store, quotesStore *quotes.Store, watcher *c2client.Client, alerter Alerter, cfg Config) *Reconciler {
	return &Reconciler{orders: ordersStore, quotes: quotesStore, watcher: watcher, alerter: alerter, cfg: cfg}
}

// PendingCount is the number of address_pending rows observed on the
// most recent tick -- the value C6.8's /metrics endpoint exposes as
// orders_address_pending.
func (r *Reconciler) PendingCount() int64 {
	return r.lastPendingCount.Load()
}

// RunLoop ticks on Config's own Interval until ctx is cancelled,
// returning ctx.Err(). Runs one Tick immediately, not just on the
// first tick after Interval, so a fresh process closes any existing
// gap right away rather than waiting a full interval.
func (r *Reconciler) RunLoop(ctx context.Context) error {
	if err := r.Tick(ctx); err != nil {
		slog.Error("reconcile: initial tick failed", "error", err)
	}

	ticker := time.NewTicker(r.cfg.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.Tick(ctx); err != nil {
				slog.Error("reconcile: tick failed", "error", err)
			}
		}
	}
}

// Tick resolves every row past the grace period: retries C2's address
// assignment (idempotent on order_id -- safe to call as many times as
// it takes), and alerts exactly once for any row past the alert
// threshold. One row's failure never blocks the rest of the tick.
func (r *Reconciler) Tick(ctx context.Context) error {
	pending, err := r.orders.ListPendingAddress(ctx, r.cfg.gracePeriod())
	if err != nil {
		return fmt.Errorf("reconcile: listing pending-address rows: %w", err)
	}
	r.lastPendingCount.Store(int64(len(pending)))

	now := time.Now().UTC()
	for _, o := range pending {
		age := now.Sub(o.CreatedAt)
		if age >= r.cfg.alertThreshold() && o.AddressPendingAlertedAt == nil {
			r.alertOnce(ctx, o, age)
		}

		if err := r.resolve(ctx, o); err != nil {
			slog.Warn("reconcile: resolving address_pending row failed, will retry next tick",
				"external_id", o.ExternalID, "error", err)
		}
	}
	return nil
}

// alertOnce claims this row's one-time alert via the row itself
// (MarkAddressPendingAlerted only succeeds for the first caller), so a
// concurrent tick or a second reconciler instance never double-fires
// it -- matches C4's own "alert once, not per retry" discipline.
func (r *Reconciler) alertOnce(ctx context.Context, o orders.GatewayOrder, age time.Duration) {
	claimed, err := r.orders.MarkAddressPendingAlerted(ctx, o.ExternalID)
	if err != nil {
		slog.Error("reconcile: claiming address-pending alert failed", "external_id", o.ExternalID, "error", err)
		return
	}
	if !claimed {
		return
	}
	if r.alerter != nil {
		r.alerter.AlertAddressPending(ctx, o, age)
	}
}

// resolve retries C2's address assignment for one row. Checks first
// whether C2 already has an address on file (a prior retry's response
// may have been lost after C2 itself succeeded) before issuing another
// write; either path ends the same way, marking the gateway_orders row
// resolved.
func (r *Reconciler) resolve(ctx context.Context, o orders.GatewayOrder) error {
	addr, err := r.watcher.GetAddress(ctx, o.C1OrderID)
	if err != nil {
		if !errors.Is(err, c2client.ErrAddressNotFound) {
			return fmt.Errorf("checking existing C2 address: %w", err)
		}

		// Re-derive the original quote window from the quote row itself
		// -- never fabricated from gateway_orders' own created_at, which
		// carries no relation to the quote's own quoted_at/expires_at.
		q, err := r.quotes.GetByID(ctx, o.QuoteID)
		if err != nil {
			return fmt.Errorf("loading quote %d for %q: %w", o.QuoteID, o.ExternalID, err)
		}

		addr, err = r.watcher.AssignAddress(ctx, o.C1OrderID, o.ExternalID, o.OwnerLabel(),
			q.CreatedAt, q.ExpiresAt, "gateway:reconcile:"+o.ExternalID)
		if err != nil {
			return fmt.Errorf("assigning C2 address: %w", err)
		}
	}

	if _, err := r.orders.MarkAddressAssigned(ctx, o.ExternalID, addr.Address); err != nil {
		return fmt.Errorf("marking %q address assigned: %w", o.ExternalID, err)
	}
	return nil
}
