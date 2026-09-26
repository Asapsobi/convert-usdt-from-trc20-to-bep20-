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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"relayd/internal/addrcheck"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/pricing"
	"relayd/internal/relay"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

type Config struct {
	// QuoteValidity is how long a displayed quote stays valid, capped by
	// the vendor's own rate lock. It only bounds the quote screen: the
	// vendor order itself is created when the deposit arrives.
	QuoteValidity time.Duration
	// DepositWindow is how long a customer has, after creating an order,
	// to send their deposit to the address they were given.
	DepositWindow time.Duration
}

// Driver is relayd's front door: quotes, and creating relay legs.
type Driver struct {
	Ledger       *ledgerclient.Client
	Upstream     upstream.SwapProvider
	TronWatcher  *watcherclient.Client // C2' -- TRC20_TO_BEP20's own deposit side
	BEP20Watcher *watcherclient.Client // C2 (depositwatcher) -- BEP20_TO_TRC20's own deposit side
	Store        *relay.Store
	Pricing      *pricing.Store
	Cfg          Config
}

// ErrBadRequest marks a request the customer has to fix (an invalid
// address, an amount outside the accepted range) -- as opposed to a
// failure on our side or a vendor's.
var ErrBadRequest = errors.New("bad request")

// ErrConflict means the request's external_id already belongs to a
// different order -- another customer's, or a Model D order sharing the
// ledger's namespace. Only an exact retry of the original request may
// reuse an external_id.
var ErrConflict = errors.New("driver: external_id is already used by a different order")

// validExternalID keeps external ids long enough not to be guessed and
// free of characters that would need escaping.
var validExternalID = regexp.MustCompile(`^[A-Za-z0-9._:-]{16,128}$`)

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

// QuoteRequest asks what a deposit of AmountIn would pay out.
type QuoteRequest struct {
	Direction relay.Direction
	AmountIn  string // decimal string, in-asset for Direction
}

// Quote is the full breakdown a customer sees before committing:
// AmountIn = OurFee + VendorFee + AmountOut (all USDT, same scale on both
// networks). The vendor fee includes the vendor's network costs.
type Quote struct {
	Direction  relay.Direction
	AmountIn   money.Amount
	OurFee     money.Amount // in-asset
	Forward    money.Amount // in-asset: what the vendor receives
	VendorFee  money.Amount // in-asset units: Forward minus AmountOut
	AmountOut  money.Amount // out-asset: what the customer receives
	Vendor     string
	ValidUntil time.Time
	Pricing    pricing.Config
}

// Quote prices a deposit without creating anything.
func (d *Driver) Quote(ctx context.Context, req QuoteRequest) (Quote, error) {
	if req.Direction != relay.TRC20ToBEP20 && req.Direction != relay.BEP20ToTRC20 {
		return Quote{}, fmt.Errorf("%w: direction must be TRC20_TO_BEP20 or BEP20_TO_TRC20", ErrBadRequest)
	}
	inAsset := inAssetFor(req.Direction)
	amountIn, err := money.ParseDecimal(req.AmountIn, inAsset)
	if err != nil {
		return Quote{}, fmt.Errorf("%w: amount_in: %v", ErrBadRequest, err)
	}
	cfg, err := d.Pricing.Get(ctx)
	if err != nil {
		return Quote{}, err
	}
	cfg = cfg.For(string(req.Direction))
	if amountIn.Units < cfg.MinAmountIn || amountIn.Units > cfg.MaxAmountIn {
		return Quote{}, fmt.Errorf("%w: amount must be between %s and %s USDT", ErrBadRequest,
			formatUnits(cfg.MinAmountIn, inAsset), formatUnits(cfg.MaxAmountIn, inAsset))
	}
	ourFee, forward, ok := cfg.Split(amountIn)
	if !ok {
		return Quote{}, fmt.Errorf("%w: amount is too small to cover our fee", ErrBadRequest)
	}

	vendorQuote, err := d.Upstream.Quote(ctx, pairFor(req.Direction), forward)
	if err != nil {
		return Quote{}, fmt.Errorf("driver: getting a vendor quote: %w", err)
	}
	vendorFee := money.Amount{Asset: inAsset, Units: forward.Units - vendorQuote.AmountOut.Units}

	validUntil := time.Now().UTC().Add(d.Cfg.QuoteValidity)
	if vendorQuote.ValidUntil.Before(validUntil) {
		validUntil = vendorQuote.ValidUntil
	}
	return Quote{
		Direction: req.Direction, AmountIn: amountIn, OurFee: ourFee, Forward: forward,
		VendorFee: vendorFee, AmountOut: vendorQuote.AmountOut, Vendor: vendorQuote.ProviderName,
		ValidUntil: validUntil, Pricing: cfg,
	}, nil
}

func formatUnits(units int64, asset money.Asset) string {
	s, err := money.Format(money.Amount{Asset: asset, Units: units})
	if err != nil {
		return fmt.Sprint(units)
	}
	return s
}

// ValidateDestination checks the customer's payout address is a valid
// address on the network they will be paid on.
func ValidateDestination(direction relay.Direction, destination string) error {
	var err error
	if direction == relay.TRC20ToBEP20 {
		err = addrcheck.EVM(destination)
	} else {
		err = addrcheck.TRON(destination)
	}
	if err != nil {
		return fmt.Errorf("%w: destination_address: %v", ErrBadRequest, err)
	}
	return nil
}

// LedgerCustomerID is the identity a leg is booked under in the ledger.
// Derived, never taken verbatim: what a customer types (a name, an email)
// must not be able to land in, or collide with, another customer's ledger
// accounts -- Model D's customers share the same account namespace.
func LedgerCustomerID(label string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(label))))
	return "relay-" + hex.EncodeToString(sum[:8])
}

type CreateRelayLegRequest struct {
	ExternalID         string
	CustomerLabel      string // what the customer typed to identify themselves
	Direction          relay.Direction
	DestinationAddress string // the customer's OWN wallet on the out-chain
	AmountIn           string // decimal string, in-asset for Direction
}

type CreateRelayLegResult struct {
	ExternalID      string
	OrderID         int64
	DepositAddress  string
	Quote           Quote
	DepositDeadline time.Time
}

// CreateRelayLeg validates the request, prices it, and reserves a
// deposit address: a C1 order, the watcher's address assignment, and the
// local leg -- each step idempotent on ExternalID, so a failed call is
// safe to repeat.
func (d *Driver) CreateRelayLeg(ctx context.Context, req CreateRelayLegRequest) (CreateRelayLegResult, error) {
	if strings.TrimSpace(req.CustomerLabel) == "" {
		return CreateRelayLegResult{}, fmt.Errorf("%w: customer label is required", ErrBadRequest)
	}
	if !validExternalID.MatchString(req.ExternalID) {
		return CreateRelayLegResult{}, fmt.Errorf("%w: external_id must be 16-128 characters of letters, digits, '.', '_', ':' or '-'", ErrBadRequest)
	}
	if err := ValidateDestination(req.Direction, req.DestinationAddress); err != nil {
		return CreateRelayLegResult{}, err
	}
	quote, err := d.Quote(ctx, QuoteRequest{Direction: req.Direction, AmountIn: req.AmountIn})
	if err != nil {
		return CreateRelayLegResult{}, err
	}
	inAsset := quote.AmountIn.Asset
	customerID := LedgerCustomerID(req.CustomerLabel)
	quotedAt := time.Now().UTC()
	depositDeadline := quotedAt.Add(d.Cfg.DepositWindow)

	order, err := d.Ledger.CreateOrder(ctx, req.ExternalID, customerID, quote.AmountIn, quote.AmountOut,
		quote.OurFee, money.Amount{Asset: inAsset, Units: 0}, req.DestinationAddress, quotedAt, depositDeadline,
		"relayd:create-order:"+req.ExternalID)
	if err != nil {
		existing, getErr := d.Ledger.GetOrder(ctx, req.ExternalID)
		if getErr != nil {
			return CreateRelayLegResult{}, fmt.Errorf("driver: creating order in C1: %w", err)
		}
		// A retry of this same request finds its own order; anything else
		// is someone else's order and must not be touched.
		if existing.Tier != "RELAY" || existing.CustomerID != customerID || existing.RecipientAddress != req.DestinationAddress ||
			existing.AmountIn != quote.AmountIn {
			return CreateRelayLegResult{}, fmt.Errorf("%w: %s", ErrConflict, req.ExternalID)
		}
		order = existing
		depositDeadline = existing.QuoteExpiresAt
	}

	watcher := d.BEP20Watcher
	if req.Direction == relay.TRC20ToBEP20 {
		watcher = d.TronWatcher
	}
	addr, err := watcher.AssignAddress(ctx, order.ID, order.ExternalID, order.CustomerID,
		quotedAt, depositDeadline, "relayd:assign-address:"+req.ExternalID)
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: order %d created in C1 (external_id %s) but assigning a deposit address failed -- retry this call, C1's own side is idempotent-safe to repeat: %w",
			order.ID, order.ExternalID, err)
	}

	label := strings.TrimSpace(req.CustomerLabel)
	profitBPS, minProfit := quote.Pricing.ProfitBPS, quote.Pricing.MinProfit
	leg, err := d.Store.Create(ctx, relay.Leg{
		ExternalID: order.ExternalID, OrderID: order.ID, Direction: req.Direction,
		CustomerID: order.CustomerID, CustomerLabel: &label, DestinationAddress: req.DestinationAddress,
		DepositAddress: addr.Address, DepositDerivationIndex: addr.DerivationIndex,
		AmountIn: quote.AmountIn, AmountOutExpected: quote.AmountOut,
		ProfitBPS: &profitBPS, MinProfit: &minProfit,
	})
	if err != nil {
		return CreateRelayLegResult{}, fmt.Errorf("driver: recording relay leg locally: %w", err)
	}
	if leg.OrderID != order.ID || leg.Direction != req.Direction || leg.DestinationAddress != req.DestinationAddress {
		return CreateRelayLegResult{}, fmt.Errorf("%w: %s", ErrConflict, req.ExternalID)
	}

	return CreateRelayLegResult{
		ExternalID: leg.ExternalID, OrderID: leg.OrderID, DepositAddress: leg.DepositAddress,
		Quote: quote, DepositDeadline: depositDeadline,
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

	// Refund what actually arrived -- the deposit sits in the leg's own
	// suspense account -- never the quoted amount, which can differ.
	received, err := d.Ledger.AccountBalance(ctx, refundRelayLegAccountCode(leg.OrderID))
	if err != nil {
		return RefundEntry{}, fmt.Errorf("driver: reading the deposit held for order %d: %w", leg.OrderID, err)
	}
	if received.Units <= 0 {
		return RefundEntry{}, fmt.Errorf("%w: no deposit is held for this order", ErrLegNotEligibleForRefundEntry)
	}
	// Record it on the leg too, so the refund transfer relayd sends once
	// this entry posts returns exactly the amount the entry books.
	sender := ""
	if order.SenderAddress != nil {
		sender = *order.SenderAddress
	}
	if _, err := d.Store.RecordDeposit(ctx, externalID, received, sender, money.Amount{Asset: received.Asset}, received); err != nil {
		return RefundEntry{}, fmt.Errorf("driver: recording the deposit for order %d: %w", leg.OrderID, err)
	}
	negAmountIn, err := received.Neg()
	if err != nil {
		return RefundEntry{}, fmt.Errorf("driver: negating the received amount: %w", err)
	}
	amountInStr, err := money.Format(received)
	if err != nil {
		return RefundEntry{}, fmt.Errorf("driver: formatting the received amount: %w", err)
	}
	negAmountInStr, err := money.Format(negAmountIn)
	if err != nil {
		return RefundEntry{}, fmt.Errorf("driver: formatting the negated received amount: %w", err)
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
