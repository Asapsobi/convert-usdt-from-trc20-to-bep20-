// Package driver is relayd's own customer-facing front door: get a
// quote, create the C1 order (tier=RELAY), assign a deposit address
// against whichever deposit watcher this leg's direction needs, and
// record the relay leg locally in AWAITING_DEPOSIT. Mirrors
// proofrun/internal/driver's own precedent exactly: a minimal
// order-origination entrypoint standing in for a real gateway
// integration, not a step toward one -- see this repo's own README on
// proofrun/ for why that precedent is a deliberate, accepted pattern in
// this codebase, not scope creep. Model F may eventually get a real C6
// (gateway) integration the same way Model D did; this is the
// "smallest real thing that proves the pipeline works" in the
// meantime.
package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

// Config is this driver's own required, no-hardcoded-default tuning.
type Config struct {
	// FeeBasisPoints is relayd's own commission, in basis points of
	// amount_in, withheld before forwarding -- a concrete stand-in for
	// R2's still-open pricing-mechanism decision (commission vs margin,
	// docs/01-strategy/model-f-relay-findings.md's own "two open pricing
	// mechanisms"), not itself that decision. Neither mechanism needs
	// different engineering, per that doc's own note, so this config
	// value is what changes once R2 resolves, not this driver's shape.
	FeeBasisPoints int64
	// QuoteValidity mirrors proofrun's own identical field: generous on
	// purpose, matching a real deposit's own real-world timing, not
	// C6's eventual tight 90s lock.
	QuoteValidity time.Duration
}

// Driver bundles every real dependency CreateRelayLeg needs.
type Driver struct {
	Ledger       *ledgerclient.Client
	Upstream     upstream.SwapProvider
	TronWatcher  *watcherclient.Client // C2' -- TRC20_TO_BEP20's own deposit side
	BEP20Watcher *watcherclient.Client // C2 (depositwatcher) -- BEP20_TO_TRC20's own deposit side
	Store        *relay.Store
	Cfg          Config
}

// CreateRelayLegRequest is this driver's own minimal quote-then-create
// input.
type CreateRelayLegRequest struct {
	ExternalID         string
	CustomerID         string
	Direction          relay.Direction
	DestinationAddress string // the customer's OWN wallet on the out-chain
	AmountIn           string // decimal string, in-asset for Direction
}

// CreateRelayLegResult is what a caller needs to actually fund the leg.
type CreateRelayLegResult struct {
	ExternalID      string
	OrderID         int64
	DepositAddress  string
	AmountIn        string
	AmountOutQuoted string
	FeeUnits        string
	QuoteExpiresAt  time.Time
}

func pairFor(direction relay.Direction) upstream.Pair {
	if direction == relay.TRC20ToBEP20 {
		return upstream.Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	}
	return upstream.Pair{From: money.USDT_BEP20, To: money.USDT_TRC20}
}

func inAssetFor(direction relay.Direction) money.Asset {
	if direction == relay.TRC20ToBEP20 {
		return money.USDT_TRC20
	}
	return money.USDT_BEP20
}

// CreateRelayLeg computes amount_in, gets a real quote from the
// upstream provider, withholds this driver's own commission, creates
// the order against C1, assigns a deposit address against the right
// watcher for this leg's direction, and records the leg locally.
//
// Order of operations mirrors proofrun's own CreatePayout: C1 first
// (idempotent-safe to retry: a duplicate external_id CreateOrder call
// is followed by one GetOrder attempt, on the theory a caller retried
// after an uncertain outcome), then the watcher (not idempotent the
// same way relay.Store.Create is, but the watcher's own POST
// /v1/addresses is idempotent on order_id, matching depositwatcher's/
// tronwatcher's own real, shared contract), then the local leg row.
func (d *Driver) CreateRelayLeg(ctx context.Context, req CreateRelayLegRequest) (CreateRelayLegResult, error) {
	inAsset := inAssetFor(req.Direction)
	amountIn, err := money.ParseDecimal(req.AmountIn, inAsset)
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: amount_in: %w", err)
	}
	if amountIn.Units <= 0 {
		return CreateRelayLegResult{}, fmt.Errorf("driver: amount_in must be positive")
	}

	feeUnits := money.Amount{Asset: inAsset, Units: amountIn.Units * d.Cfg.FeeBasisPoints / 10_000}
	forwardIn, err := amountIn.Sub(feeUnits)
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: computing forward amount: %w", err)
	}

	pair := pairFor(req.Direction)
	quote, err := d.Upstream.Quote(ctx, pair, forwardIn)
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: getting upstream quote: %w", err)
	}

	quotedAt := time.Now().UTC()
	quoteExpiresAt := quotedAt.Add(d.Cfg.QuoteValidity)
	if quote.ValidUntil.Before(quoteExpiresAt) {
		// Never quote the customer past the upstream's OWN lock window --
		// architecture doc §6's own "shorten the lock" guidance.
		quoteExpiresAt = quote.ValidUntil
	}

	order, err := d.Ledger.CreateOrder(ctx, req.ExternalID, req.CustomerID, amountIn, quote.AmountOut,
		feeUnits, money.Amount{Asset: inAsset, Units: 0}, req.DestinationAddress, quotedAt, quoteExpiresAt,
		"relayd:create-order:"+req.ExternalID)
	if err != nil {
		if existing, getErr := d.Ledger.GetOrder(ctx, req.ExternalID); getErr == nil {
			order = existing
		} else {
			return CreateRelayLegResult{}, fmt.Errorf("driver: creating order in C1: %w", err)
		}
	}

	watcher := d.BEP20Watcher
	if req.Direction == relay.TRC20ToBEP20 {
		watcher = d.TronWatcher
	}
	addr, err := watcher.AssignAddress(ctx, order.ID, order.ExternalID, req.CustomerID,
		quotedAt, quoteExpiresAt, "relayd:assign-address:"+req.ExternalID)
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: order %d created in C1 (external_id %s) but assigning a deposit address failed -- retry this call, C1's own side is idempotent-safe to repeat: %w",
			order.ID, order.ExternalID, err)
	}

	leg, err := d.Store.Create(ctx, relay.Leg{
		ExternalID: order.ExternalID, OrderID: order.ID, Direction: req.Direction,
		CustomerID: req.CustomerID, DestinationAddress: req.DestinationAddress,
		DepositAddress: addr.Address, AmountIn: amountIn, AmountOutExpected: quote.AmountOut,
	})
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: recording relay leg locally: %w", err)
	}

	feeStr, err := money.Format(feeUnits)
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: formatting fee_units: %w", err)
	}
	amountInStr, err := money.Format(amountIn)
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: formatting amount_in: %w", err)
	}
	amountOutStr, err := money.Format(quote.AmountOut)
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: formatting amount_out: %w", err)
	}

	return CreateRelayLegResult{
		ExternalID: leg.ExternalID, OrderID: leg.OrderID, DepositAddress: leg.DepositAddress,
		AmountIn: amountInStr, AmountOutQuoted: amountOutStr, FeeUnits: feeStr,
		QuoteExpiresAt: quoteExpiresAt,
	}, nil
}

// Status is this driver's own reconstructed lifecycle view -- a single
// external_id lets a caller see both C1's own order state and this
// leg's own finer-grained relay status.
type Status struct {
	ExternalID string
	Order      ledgerclient.Order
	Leg        relay.Leg
}

// GetStatus fetches both C1's order and this service's own leg row.
func (d *Driver) GetStatus(ctx context.Context, externalID string) (Status, error) {
	order, err := d.Ledger.GetOrder(ctx, externalID)
	if err != nil {
		return Status{}, fmt.Errorf("driver: fetching order: %w", err)
	}
	leg, err := d.Store.GetByExternalID(ctx, externalID)
	if err != nil {
		return Status{}, fmt.Errorf("driver: fetching relay leg: %w", err)
	}
	return Status{ExternalID: externalID, Order: order, Leg: leg}, nil
}

// ErrLegNotEligibleForRefundEntry means BuildRefundEntry was asked for a
// leg that is not currently in the one state a C3-driven manual refund
// can apply to: the local leg must still be AWAITING_DEPOSIT (relayd has
// never engaged it -- a held order's own leg always is, since
// startScreenedLegs only ever picks up state=screened orders) and C1's
// own order must currently be held (a manual reject is only legal from
// there). Returning this rather than silently building a wrong entry
// protects against the caller (screening's own Reject flow) racing a
// state change -- e.g. the order got released and moved on before this
// call landed.
var ErrLegNotEligibleForRefundEntry = errors.New("driver: this leg is not currently eligible for a refund entry")

// RefundEntry is the C1 "entry" object a caller embeds verbatim into its
// own held->refunded transition call. Built here, not by the caller,
// because relayd alone owns RELAY-tier's own account-code conventions
// (asset:relay:leg:<id>, liability:customer:<id>:<asset>) -- the exact
// same shape internal/orchestrate/refund.go's own postRefundEntry
// already posts for the timeout-triggered refund path, duplicated here
// rather than imported (internal/driver and internal/orchestrate are
// already separate packages within this module with no shared helper
// for this, and it is two one-line format strings, not worth a new
// shared package over).
type RefundEntry struct {
	EntryType  string
	OccurredAt time.Time
	Lines      []RefundEntryLine
}

// RefundEntryLine is one line of a RefundEntry. Amount is a decimal
// string (C1's own entry-line format), already carrying its own sign.
type RefundEntryLine struct {
	AccountCode string
	Asset       string
	Amount      string
}

// BuildRefundEntry computes the held->refunded ledger entry for
// externalID's own relay leg: the customer's liability closes by the
// full amount_in, the relay-leg suspense account (asset:relay:leg:<id>)
// closes by the same amount -- no commission line, mirroring
// postRefundEntry's own identical shape (nothing was ever delivered on a
// manually-rejected hold, so nothing is earned). This is the SAME entry
// shape whether the refund was triggered by R5's own automatic timeout
// path or by a human rejecting a screening hold via C3 -- only who
// calls it, and when, differs. The caller (relayd's own
// internal/httpapi.getRelayLegRefundEntry today) is responsible for
// actually submitting this to C1; this function only computes it.
func (d *Driver) BuildRefundEntry(ctx context.Context, externalID string) (RefundEntry, error) {
	leg, err := d.Store.GetByExternalID(ctx, externalID)
	if err != nil {
		return RefundEntry{}, err
	}
	if leg.Status != relay.StatusAwaitingDeposit {
		return RefundEntry{}, fmt.Errorf("%w: leg status is %s, want AWAITING_DEPOSIT", ErrLegNotEligibleForRefundEntry, leg.Status)
	}

	order, err := d.Ledger.GetOrder(ctx, externalID)
	if err != nil {
		return RefundEntry{}, fmt.Errorf("driver: fetching order: %w", err)
	}
	if order.State != "held" {
		return RefundEntry{}, fmt.Errorf("%w: order state is %s, want held", ErrLegNotEligibleForRefundEntry, order.State)
	}

	negAmountIn, err := order.AmountIn.Neg()
	if err != nil {
		return RefundEntry{}, fmt.Errorf("driver: negating amount_in: %w", err)
	}
	amountInStr, err := money.Format(order.AmountIn)
	if err != nil {
		return RefundEntry{}, fmt.Errorf("driver: formatting amount_in: %w", err)
	}
	negAmountInStr, err := money.Format(negAmountIn)
	if err != nil {
		return RefundEntry{}, fmt.Errorf("driver: formatting negated amount_in: %w", err)
	}

	asset := string(order.AmountIn.Asset)
	return RefundEntry{
		EntryType:  "relay_refund",
		OccurredAt: time.Now().UTC(),
		Lines: []RefundEntryLine{
			{AccountCode: refundCustomerAccountCode(order.CustomerID, asset), Asset: asset, Amount: amountInStr},
			{AccountCode: refundRelayLegAccountCode(leg.OrderID), Asset: asset, Amount: negAmountInStr},
		},
	}, nil
}

func refundCustomerAccountCode(customerID, asset string) string {
	return fmt.Sprintf("liability:customer:%s:%s", customerID, asset)
}
func refundRelayLegAccountCode(orderID int64) string {
	return fmt.Sprintf("asset:relay:leg:%d", orderID)
}

// ListLegs returns every relay leg matching status (nil means every
// status), this service's own local data only -- unlike GetStatus, it
// does NOT cross-reference C1's order state per row, so a listing of
// many legs costs one query here, not one HTTP round trip to C1 per row.
// Backs the ops console's own relay-leg visibility view (see
// docs/03-build/ops-console-build-prompts.md's own Model F addendum);
// httpapi.getRelayLegs is its only caller today.
func (d *Driver) ListLegs(ctx context.Context, status *relay.Status) ([]relay.Leg, error) {
	legs, err := d.Store.List(ctx, status)
	if err != nil {
		return nil, fmt.Errorf("driver: listing relay legs: %w", err)
	}
	return legs, nil
}
