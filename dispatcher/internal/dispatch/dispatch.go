// Package dispatch is C5's own state machine, tying slot selection,
// energy reservation, C1's ledger, and (from C5.4 onward) transaction
// construction, signing, and broadcast together. EnterDispatching is the
// first piece: the chunk that commits an order to conversion. Everything
// before this point (slot selection, energy reservation) is reversible
// with zero ledger impact -- a failed reservation touches nothing in C1.
// This is not: once the E2 entry lands, invariant 4 requires an explicit
// reversal to undo it, never a silent drop.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dispatcher/internal/ledgerclient"
)

// Status is a dispatch_state row's own lifecycle: DISPATCHING from the
// moment E2 lands, then exactly one of SETTLED (C5.6) or HELD (C5.7) --
// never both, per component-map's own C5 final assertion 1.
type Status string

const (
	StatusDispatching Status = "DISPATCHING"
	StatusSettled     Status = "SETTLED"
	StatusHeld        Status = "HELD"
)

// Attempt is one order's local dispatch record: which slot it committed
// to, and the conversion entry's own idempotency key -- what C5.7's
// failure path needs to reverse E2, without having to re-derive or trust
// a re-derivation of dispatcher:enter_dispatching:<order_id>.
type Attempt struct {
	OrderID              int64
	ExternalID           string
	SlotID               int
	ConversionEntryKey   string
	Status               Status
	EnteredDispatchingAt time.Time
}

// LedgerClient is the narrow slice of ledgerclient.Client this package
// needs -- a consumer-defined interface, per this project's convention,
// so a fake can stand in for tests without depending on ledgerclient's
// own HTTP machinery.
type LedgerClient interface {
	GetOrder(ctx context.Context, externalID string) (ledgerclient.Order, error)
	EnsureAccount(ctx context.Context, code string, accountType ledgerclient.AccountType, asset string, idempotencyKey string) error
	TransitionWithEntry(ctx context.Context, externalID, toState string, expectedVersion int32, reason, entryType string, occurredAt time.Time, lines []ledgerclient.EntryLine, idempotencyKey string) (ledgerclient.Order, error)
}

// Dispatcher holds what EnterDispatching and Broadcast need to talk to
// C1, S1, and this service's own database.
type Dispatcher struct {
	Ledger   LedgerClient
	Store    *Store
	Attempts *AttemptStore
	Signer   SigningService

	// Batches is C5.8's own Sweep-tier batching store. Only
	// AccumulateForBatch, CutBatch, and HandlePartialSettlement need it --
	// nil is fine for a Dispatcher that only ever handles Direct/Standard
	// orders, matching NewDispatcher's own signature not requiring it
	// (added after this project's Direct/Standard pipeline was already
	// built and tested; making it a required constructor argument would
	// have meant touching every existing call site for a dependency most
	// of them don't use).
	Batches *BatchStore

	// HaltBackoff is how long to wait between retries while C1 reports
	// system_halted -- invariant says only "back off ... on a longer
	// interval," not a specific duration; kept a field (not a const) so
	// tests can set it well below production's real interval.
	HaltBackoff time.Duration

	// FinalityStaleCeiling is how long ConfirmFinality tolerates a
	// broadcast tron_txid not yet reaching finality before returning
	// ErrFinalityStale -- an alert signal, not a giving-up point (the
	// caller keeps polling regardless). Zero disables the ceiling
	// entirely (never stale), which is never the production setting.
	FinalityStaleCeiling time.Duration
}

// NewDispatcher wires a Dispatcher with production-sane defaults.
func NewDispatcher(ledger LedgerClient, store *Store, attempts *AttemptStore, signer SigningService) *Dispatcher {
	return &Dispatcher{
		Ledger: ledger, Store: store, Attempts: attempts, Signer: signer,
		HaltBackoff: 30 * time.Second, FinalityStaleCeiling: 10 * time.Minute,
	}
}

func conversionIdempotencyKey(orderID int64) string {
	return fmt.Sprintf("dispatcher:enter_dispatching:%d", orderID)
}

func customerAccountCode(customerID, asset string) string {
	return fmt.Sprintf("liability:customer:%s:%s", customerID, asset)
}

// buildConversionLines builds the E2 entry per c1-ledger-build-prompts.md
// §B, using exactly the order's own frozen amount_in/amount_out/
// fee_units/network_fee_units -- C5 does not recompute pricing.
// position:corridor's own two accounts are part of the fixed chart
// internal/accounts.Seed creates up front (ledger/internal/accounts/seed.go),
// so only the customer's TRC20 liability account is ever new here; the
// BEP20 side already exists from C2's own E1 (deposit_final).
func buildConversionLines(order ledgerclient.Order) []ledgerclient.EntryLine {
	bepLiability := customerAccountCode(order.CustomerID, "USDT_BEP20")
	trcLiability := customerAccountCode(order.CustomerID, "USDT_TRC20")
	return []ledgerclient.EntryLine{
		{AccountCode: bepLiability, Asset: "USDT_BEP20", Amount: order.AmountIn},
		{AccountCode: "position:corridor:USDT_BEP20", Asset: "USDT_BEP20", Amount: -order.AmountIn},
		{AccountCode: "position:corridor:USDT_TRC20", Asset: "USDT_TRC20", Amount: order.AmountIn},
		{AccountCode: trcLiability, Asset: "USDT_TRC20", Amount: -order.AmountOut},
		{AccountCode: "revenue:fee", Asset: "USDT_TRC20", Amount: -order.FeeUnits},
		{AccountCode: "revenue:network_fee", Asset: "USDT_TRC20", Amount: -order.NetworkFeeUnits},
	}
}

// ErrNotDispatchable means an order's tier is paid out by another
// service, never by this dispatcher.
var ErrNotDispatchable = errors.New("dispatch: this order's tier is not paid out by the dispatcher")

// DispatchableTier reports whether this dispatcher pays out orders of
// tier. Every other tier sharing the ledger belongs to another service --
// above all RELAY, which relayd (Model F) forwards itself: dispatching a
// RELAY order here as well would pay its customer twice.
func DispatchableTier(tier string) bool {
	return tier == "DIRECT" || tier == "STANDARD"
}

// EnterDispatching posts the E2 conversion entry and transitions order
// into `dispatching`, then records the attempt locally. order must
// already be in `screened` (the caller's job to have gotten it there);
// slotID is whichever slot C5.1/C5.2's own selection already committed
// to for this dispatch -- EnterDispatching does not choose a slot, only
// records the choice.
//
// occurredAt is supplied by the caller, not computed here, and MUST be
// stable across every retry of this same logical attempt (including a
// fresh process after a crash) -- discovered against a real ledgerd: the
// conversion entry's own idempotency-key replay check compares a hash of
// the full payload, occurred_at included, so a retry that recomputes
// time.Now() produces a different hash and is rejected as
// idempotency_conflict rather than replayed. There is no natural external
// timestamp for "when C5 decided to dispatch this order" the way E1 has
// the deposit's own block time, so the caller owns picking one value once
// (e.g. when it first selects a slot for this order) and supplying that
// same value on every subsequent call -- the same determinism C5.4's own
// BuildTransfer is required to have, for the same reason.
//
//   - On 409 version_conflict: refetch the order once and retry with its
//     current version. If the order is already `dispatching` (this exact
//     call already succeeded and C5 simply didn't observe the response),
//     the retry lands on C1's own replay path -- (dispatching,
//     dispatching) is not a legal transition pair, but an identical
//     entry payload under the same idempotency key replays instead of
//     erroring (ledger/internal/orders/store.go's own
//     replayIfAlreadyPosted). A second version conflict after the retry
//     is returned as an error, per invariant 6's "retry once" -- anything
//     racier than that is a caller bug, not a condition this function
//     papers over.
//   - On 423 system_halted: this transition is halt-blocked in C1 by
//     design (unlike C4's own energy-cost entries). Back off and retry
//     indefinitely on HaltBackoff -- never treated as a failure requiring
//     C5.7's dispatch-failure handling; ctx cancellation is the only way
//     out of this loop.
func (d *Dispatcher) EnterDispatching(ctx context.Context, order ledgerclient.Order, slotID int, occurredAt time.Time) (Attempt, error) {
	if !DispatchableTier(order.Tier) {
		return Attempt{}, fmt.Errorf("%w: order %s is tier %q", ErrNotDispatchable, order.ExternalID, order.Tier)
	}
	conversionKey := conversionIdempotencyKey(order.ID)
	trcLiability := customerAccountCode(order.CustomerID, "USDT_TRC20")

	ensureKey := "dispatcher:ensure-account:" + trcLiability
	if err := d.Ledger.EnsureAccount(ctx, trcLiability, ledgerclient.AccountLiability, "USDT_TRC20", ensureKey); err != nil {
		return Attempt{}, fmt.Errorf("dispatch: ensuring %s exists before entering dispatching: %w", trcLiability, err)
	}

	lines := buildConversionLines(order)
	expectedVersion := order.Version
	versionConflictRetried := false

	for {
		_, err := d.Ledger.TransitionWithEntry(ctx, order.ExternalID, "dispatching", expectedVersion,
			"dispatch_start", "conversion", occurredAt, lines, conversionKey)
		if err == nil {
			break
		}

		if errors.Is(err, ledgerclient.ErrVersionConflict) && !versionConflictRetried {
			versionConflictRetried = true
			refetched, getErr := d.Ledger.GetOrder(ctx, order.ExternalID)
			if getErr != nil {
				return Attempt{}, fmt.Errorf("dispatch: refetching order %s after version conflict: %w", order.ExternalID, getErr)
			}
			expectedVersion = refetched.Version
			continue
		}

		if errors.Is(err, ledgerclient.ErrSystemHalted) {
			select {
			case <-ctx.Done():
				return Attempt{}, ctx.Err()
			case <-time.After(d.HaltBackoff):
			}
			continue
		}

		return Attempt{}, fmt.Errorf("dispatch: entering dispatching for order %s: %w", order.ExternalID, err)
	}

	return d.Store.Create(ctx, Attempt{
		OrderID:              order.ID,
		ExternalID:           order.ExternalID,
		SlotID:               slotID,
		ConversionEntryKey:   conversionKey,
		Status:               StatusDispatching,
		EnteredDispatchingAt: occurredAt,
	})
}
