package orders

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
)

// haltCache is package-level rather than a Transition parameter because
// Transition's signature is fixed by C1.5 and every existing caller
// already matches it. SetHaltCache must be called once, at process
// startup (cmd/ledgerd) or in a test's setup, before any halt-blocked
// transition runs -- see the ErrHaltCacheNotConfigured check in
// Transition for what happens if it hasn't been.
var haltCache *halt.Cache

// SetHaltCache wires up the in-memory halt cache Transition consults for
// halt-blocked transitions (see transitionTable's HaltBlocked field).
// Call once per process.
func SetHaltCache(c *halt.Cache) {
	haltCache = c
}

// ErrHaltCacheNotConfigured means SetHaltCache was never called. Treated
// as a hard error for any halt-blocked transition rather than silently
// treating "no cache configured" as "not halted": failing closed here is
// deliberate -- letting money-moving transitions through because of a
// missing setup call is a worse failure mode than refusing them.
var ErrHaltCacheNotConfigured = errors.New("orders: halt cache not configured; call SetHaltCache at startup")

// ErrSystemHalted is returned by Transition for a halt-blocked pair while
// the ledger is halted.
var ErrSystemHalted = errors.New("orders: system is halted")

// CreateParams is everything Create needs. The new order always starts in
// Quoted with Version 0 -- callers never choose the initial state.
type CreateParams struct {
	ExternalID       string
	CustomerID       string
	Tier             Tier
	AmountIn         money.Amount // must be USDT_BEP20
	AmountOut        money.Amount // must be USDT_TRC20
	FeeUnits         money.Amount // must be USDT_TRC20
	NetworkFeeUnits  money.Amount // must be USDT_TRC20
	RecipientAddress string
	QuotedAt         time.Time
	QuoteExpiresAt   time.Time
}

func (p CreateParams) validate() error {
	if p.ExternalID == "" {
		return fmt.Errorf("%w: empty external_id", ErrInvalidParams)
	}
	if p.CustomerID == "" {
		return fmt.Errorf("%w: empty customer_id", ErrInvalidParams)
	}
	if !p.Tier.Valid() {
		return fmt.Errorf("%w: unknown tier %q", ErrInvalidParams, p.Tier)
	}
	// Model D (DIRECT/STANDARD/SWEEP) is a fixed-direction corridor --
	// BEP20 in, TRC20 out, always -- so those three tiers keep the
	// original hard-coded check unchanged. Model F (RELAY) is
	// bidirectional by design (docs/03-build/model-f-relay-build-prompts.md's
	// own "happy flow" covers TRC20->BEP20 as the PRIMARY example, the
	// mirror image of Model D's own direction), so a RELAY order must
	// accept either pairing -- rejecting TRC20-in/BEP20-out here would
	// silently break exactly the direction that doc walks through first.
	// FeeUnits/NetworkFeeUnits match AmountIn's asset for RELAY --
	// unlike Model D, where C5 itself broadcasts the payout and can
	// literally send the customer less than a pure conversion would
	// (fee_units taken out of amount_out), a RELAY order's upstream
	// vendor pays the customer's own destination wallet directly with
	// 100% of its own conversion output (the whole point of the
	// zero-float relay -- see docs/02-architecture/model-f-relay-architecture.md
	// §1). relayd's only lever is how much of the DEPOSITED (in-asset)
	// amount it forwards to the vendor -- it withholds the fee before
	// forwarding, never after. Found the hard way: an earlier version of
	// this validation matched AmountOut's asset here (copying Model D's
	// convention by analogy without checking it against RELAY's actual
	// money-flow mechanics), which parsed and validated fine but was
	// silently wrong -- caught only by relayd's own real-ledgerd
	// integration test (internal/orchestrate/orchestrate_integration_test.go's
	// TestFullHappyPath_TRC20ToBEP20) failing with an asset mismatch
	// computing (amount_in - fee_units), invisible to any unit test that
	// mocks C1's response shape instead of asking a real one.
	if p.Tier == Relay {
		validPair := (p.AmountIn.Asset == money.USDT_BEP20 && p.AmountOut.Asset == money.USDT_TRC20) ||
			(p.AmountIn.Asset == money.USDT_TRC20 && p.AmountOut.Asset == money.USDT_BEP20)
		if !validPair {
			return fmt.Errorf("%w: relay amount_in/amount_out must be USDT_BEP20/USDT_TRC20 or USDT_TRC20/USDT_BEP20, got %q/%q",
				ErrInvalidParams, p.AmountIn.Asset, p.AmountOut.Asset)
		}
		if p.FeeUnits.Asset != p.AmountIn.Asset {
			return fmt.Errorf("%w: relay fee_units must match amount_in's asset (%q), got %q", ErrInvalidParams, p.AmountIn.Asset, p.FeeUnits.Asset)
		}
		if p.NetworkFeeUnits.Asset != p.AmountIn.Asset {
			return fmt.Errorf("%w: relay network_fee_units must match amount_in's asset (%q), got %q", ErrInvalidParams, p.AmountIn.Asset, p.NetworkFeeUnits.Asset)
		}
	} else {
		if p.AmountIn.Asset != money.USDT_BEP20 {
			return fmt.Errorf("%w: amount_in must be USDT_BEP20, got %q", ErrInvalidParams, p.AmountIn.Asset)
		}
		if p.AmountOut.Asset != money.USDT_TRC20 {
			return fmt.Errorf("%w: amount_out must be USDT_TRC20, got %q", ErrInvalidParams, p.AmountOut.Asset)
		}
		if p.FeeUnits.Asset != money.USDT_TRC20 {
			return fmt.Errorf("%w: fee_units must be USDT_TRC20, got %q", ErrInvalidParams, p.FeeUnits.Asset)
		}
		if p.NetworkFeeUnits.Asset != money.USDT_TRC20 {
			return fmt.Errorf("%w: network_fee_units must be USDT_TRC20, got %q", ErrInvalidParams, p.NetworkFeeUnits.Asset)
		}
	}
	if p.RecipientAddress == "" {
		return fmt.Errorf("%w: empty recipient_address", ErrInvalidParams)
	}
	if p.QuotedAt.IsZero() {
		return fmt.Errorf("%w: zero quoted_at", ErrInvalidParams)
	}
	if !p.QuoteExpiresAt.After(p.QuotedAt) {
		return fmt.Errorf("%w: quote_expires_at must be after quoted_at", ErrInvalidParams)
	}
	return nil
}

// Create inserts a new order in state Quoted, version 0.
func Create(ctx context.Context, q accounts.Queryer, p CreateParams) (Order, error) {
	if err := p.validate(); err != nil {
		return Order{}, err
	}

	// Only a RELAY order ever gets a non-NULL relay_direction -- every
	// other tier relies on the fixed, implicit BEP20-in/TRC20-out
	// convention scanOrder still hard-codes for them. p.validate() above
	// already confirmed a RELAY order's own AmountIn/AmountOut pairing is
	// one of the two legal combinations, so this is a plain lookup, not a
	// second validation.
	var relayDirection *string
	if p.Tier == Relay {
		direction := "BEP20_TO_TRC20"
		if p.AmountIn.Asset == money.USDT_TRC20 {
			direction = "TRC20_TO_BEP20"
		}
		relayDirection = &direction
	}

	row := q.QueryRow(ctx, `
		INSERT INTO orders
			(external_id, customer_id, tier, state, amount_in, amount_out,
			 fee_units, network_fee_units, recipient_address, quoted_at, quote_expires_at,
			 version, relay_direction)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 0, $12)
		RETURNING id, external_id, customer_id, tier, state, amount_in, amount_out,
			fee_units, network_fee_units, recipient_address, sender_address, quoted_at, quote_expires_at,
			version, created_at, updated_at, relay_direction
	`, p.ExternalID, p.CustomerID, string(p.Tier), string(Quoted),
		p.AmountIn.Units, p.AmountOut.Units, p.FeeUnits.Units, p.NetworkFeeUnits.Units,
		p.RecipientAddress, p.QuotedAt, p.QuoteExpiresAt, relayDirection)

	order, err := scanOrder(row)
	if err != nil {
		return Order{}, fmt.Errorf("orders: create %q: %w", p.ExternalID, err)
	}
	return order, nil
}

// Get looks up an order by its internal id.
func Get(ctx context.Context, q accounts.Queryer, orderID int64) (Order, error) {
	row := q.QueryRow(ctx, orderSelectSQL+" WHERE id = $1", orderID)
	order, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("%w: id %d", ErrOrderNotFound, orderID)
	}
	if err != nil {
		return Order{}, fmt.Errorf("orders: get %d: %w", orderID, err)
	}
	return order, nil
}

// GetByExternalID looks up an order by the id C6 gave the customer.
func GetByExternalID(ctx context.Context, q accounts.Queryer, externalID string) (Order, error) {
	row := q.QueryRow(ctx, orderSelectSQL+" WHERE external_id = $1", externalID)
	order, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("%w: external_id %q", ErrOrderNotFound, externalID)
	}
	if err != nil {
		return Order{}, fmt.Errorf("orders: get by external id %q: %w", externalID, err)
	}
	return order, nil
}

// ListByState returns every order currently in state, ordered by id.
func ListByState(ctx context.Context, q accounts.Queryer, state State) ([]Order, error) {
	rows, err := q.Query(ctx, orderSelectSQL+" WHERE state = $1 ORDER BY id", string(state))
	if err != nil {
		return nil, fmt.Errorf("orders: list by state %s: %w", state, err)
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("orders: list by state %s: %w", state, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orders: list by state %s: %w", state, err)
	}
	return out, nil
}

// TransitionParams is what a caller supplies to Transition beyond the
// (orderID, toState, expectedVersion) triple.
type TransitionParams struct {
	Actor      string
	Reason     string
	OccurredAt time.Time
	// Entry is posted via journal.Post in the same transaction as the
	// state change, atomically, when the transition rule requires one.
	// Use this when the transition itself is what should cause the entry
	// to exist.
	Entry *journal.EntryRequest
	// EntryID names an entry ALREADY posted earlier in the same
	// transaction -- for example by journal.Reverse, called directly by
	// C1.6's reorg handling before it calls Transition -- to be recorded
	// as this transition's cause without posting anything new. At most
	// one of Entry / EntryID may be set, and exactly one must be set
	// when the transition rule requires an entry.
	EntryID *int64
	// SenderAddress is the BSC address that funded this order's
	// deposit, supplied by C2 alongside the quoted->funded transition's
	// entry (see docs/03-build/c3-screening-build-prompts.md §A). Only
	// accepted when toState is Funded -- nil on every other transition,
	// which is also what every caller that predates this field already
	// sends, so this is additive. A non-nil value always overwrites
	// whatever was there (including a prior non-nil value): the only
	// path that can reach the write is a real quoted->funded transition,
	// which after a funded->quoted reversal (a reorg) can legitimately
	// happen a second time for the same order, from a possibly different
	// sender -- there is no separate "immutable" guard beyond that, since
	// no other code path ever writes this column at all.
	SenderAddress *string
}

func (p TransitionParams) validate() error {
	if p.Actor == "" {
		return fmt.Errorf("%w: empty actor", ErrInvalidParams)
	}
	if p.Reason == "" {
		return fmt.Errorf("%w: empty reason", ErrInvalidParams)
	}
	if p.OccurredAt.IsZero() {
		return fmt.Errorf("%w: zero occurred_at", ErrInvalidParams)
	}
	if p.Entry != nil && p.EntryID != nil {
		return fmt.Errorf("%w: both Entry and EntryID set", ErrInvalidParams)
	}
	if p.SenderAddress != nil && *p.SenderAddress == "" {
		return fmt.Errorf("%w: sender_address, if set, must not be empty", ErrInvalidParams)
	}
	return nil
}

// Transition moves an order from its current state to toState, per
// transitionTable, the only source of truth for which moves are legal.
//
// Concurrency is optimistic: expectedVersion must match the order's
// current version, checked twice -- once as an early snapshot read (a
// fast, clear rejection in the common uncontended case) and once, the
// guarantee that actually matters, as the WHERE clause of the UPDATE that
// performs the transition. A CAS miss there is ErrVersionConflict; this
// function never retries on the caller's behalf.
//
// For a HaltBlocked pair (see transitionTable), halt state is checked
// twice: first the in-memory Cache (haltCache), for fast rejection with
// up to 1 second of staleness; then, only if that says "not halted," the
// authoritative transactional read against system_state using this same
// tx -- because a fast-but-stale "not halted" is not good enough to
// actually let money leave. A pair that is not HaltBlocked never checks
// either: quoted->funded (deposit recording) must keep working while
// halted, and neither read-only calls nor journal.Reverse ever reach
// this function at all.
//
// tx is a transaction the caller already opened. If Transition returns a
// non-nil error, the caller must roll back rather than commit -- that is
// what makes a losing attempt in a concurrent-transition race (which may
// have already called journal.Post before losing the final CAS) leave no
// trace: the whole attempt, including any entry it posted, is discarded
// with the transaction.
func Transition(ctx context.Context, tx pgx.Tx, orderID int64, toState State, expectedVersion int32, p TransitionParams) (Order, error) {
	if !toState.Valid() {
		return Order{}, fmt.Errorf("%w: unknown target state %q", ErrInvalidParams, toState)
	}
	if err := p.validate(); err != nil {
		return Order{}, err
	}

	current, err := Get(ctx, tx, orderID)
	if err != nil {
		return Order{}, err
	}
	if current.Version != expectedVersion {
		return Order{}, fmt.Errorf("%w: order %d has version %d, expected %d",
			ErrVersionConflict, orderID, current.Version, expectedVersion)
	}

	r, legal := transitionTable[pair{From: current.State, To: toState}]
	if !legal {
		if toState == current.State && p.Entry != nil {
			replayed, ok, err := replayIfAlreadyPosted(ctx, tx, current, *p.Entry)
			if err != nil {
				return Order{}, err
			}
			if ok {
				return replayed, nil
			}
		}
		return Order{}, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, current.State, toState)
	}

	if r.HaltBlocked {
		if haltCache == nil {
			return Order{}, ErrHaltCacheNotConfigured
		}
		fastHalted, err := haltCache.IsHalted(ctx)
		if err != nil {
			return Order{}, fmt.Errorf("orders: checking cached halt state: %w", err)
		}
		if fastHalted {
			return Order{}, fmt.Errorf("%w: %s -> %s", ErrSystemHalted, current.State, toState)
		}

		authHalted, err := halt.IsHalted(ctx, tx)
		if err != nil {
			return Order{}, fmt.Errorf("orders: checking authoritative halt state: %w", err)
		}
		if authHalted {
			return Order{}, fmt.Errorf("%w: %s -> %s", ErrSystemHalted, current.State, toState)
		}
	}

	switch {
	case r.RequiresEntry && p.Entry == nil && p.EntryID == nil:
		return Order{}, fmt.Errorf("%w: %s -> %s", ErrEntryRequired, current.State, toState)
	case !r.RequiresEntry && (p.Entry != nil || p.EntryID != nil):
		return Order{}, fmt.Errorf("%w: %s -> %s", ErrEntryNotAllowed, current.State, toState)
	}

	if p.SenderAddress != nil && toState != Funded {
		return Order{}, fmt.Errorf("%w: sender_address is only accepted on a transition into funded, got %s -> %s",
			ErrInvalidParams, current.State, toState)
	}

	entryID := p.EntryID
	if p.Entry != nil {
		entry, err := journal.Post(ctx, tx, *p.Entry)
		if err != nil {
			return Order{}, fmt.Errorf("orders: posting entry for %s -> %s: %w", current.State, toState, err)
		}
		entryID = &entry.ID
	}

	row := tx.QueryRow(ctx, `
		UPDATE orders
		SET state = $1, version = version + 1, updated_at = now(),
			sender_address = COALESCE($4, sender_address)
		WHERE id = $2 AND version = $3
		RETURNING id, external_id, customer_id, tier, state, amount_in, amount_out,
			fee_units, network_fee_units, recipient_address, sender_address, quoted_at, quote_expires_at,
			version, created_at, updated_at, relay_direction
	`, string(toState), orderID, expectedVersion, p.SenderAddress)

	updated, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("%w: order %d version %d (lost the race)", ErrVersionConflict, orderID, expectedVersion)
	}
	if err != nil {
		return Order{}, fmt.Errorf("orders: updating order %d: %w", orderID, err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO order_transitions (order_id, from_state, to_state, entry_id, actor, reason, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, orderID, string(current.State), string(toState), entryID, p.Actor, p.Reason, p.OccurredAt)
	if err != nil {
		return Order{}, fmt.Errorf("orders: logging transition for order %d: %w", orderID, err)
	}

	return updated, nil
}

// replayIfAlreadyPosted handles the one case Transition's own
// transitionTable check cannot: a caller (C2's deposit-final report,
// concretely) redelivering a transition-with-entry request after it
// ALREADY succeeded and the order left the state that pair was legal
// from -- e.g. Funded -> Funded is not a legal transitionTable pair, so
// a redelivered "fund this order" request would otherwise fall straight
// through to ErrIllegalTransition. That is correct for a genuinely new
// entry request that happens to name the order's current state, but
// wrong for an exact replay of a request that already succeeded --
// found while building C2's ReportDepositFinal against a real running
// ledgerd: C2 has no persistent record of which candidates it already
// reported (by design, see the C2 build spec's own finality.Tracker), so
// a process restart re-observing an already-final on-chain deposit
// produces exactly this redelivery.
//
// This only ever acts when an entry with entryReq.IdempotencyKey ALREADY
// EXISTS -- for a brand-new key it returns (Order{}, false, nil)
// unconditionally, deferring to the caller's own ErrIllegalTransition,
// and never posts anything through this path. Whether a found key's
// payload actually matches (a genuine replay) or not (a caller bug reusing
// a key for a different request) is decided by journal.Post's own
// ON CONFLICT + payload-hash comparison -- reused here, not
// reimplemented, so that comparison lives in exactly one place: matching
// payload returns Outcome=Replayed and no error; a mismatched one returns
// ErrIdempotencyConflict, propagated up as the P1-bug signal it is meant
// to be, never silently treated as success.
func replayIfAlreadyPosted(ctx context.Context, tx pgx.Tx, current Order, entryReq journal.EntryRequest) (Order, bool, error) {
	if _, err := journal.GetEntryByIdempotencyKey(ctx, tx, entryReq.IdempotencyKey); err != nil {
		if errors.Is(err, journal.ErrEntryNotFound) {
			return Order{}, false, nil
		}
		return Order{}, false, fmt.Errorf("orders: checking for an already-posted entry %q: %w", entryReq.IdempotencyKey, err)
	}

	if _, err := journal.Post(ctx, tx, entryReq); err != nil {
		return Order{}, false, fmt.Errorf("orders: replaying entry %q: %w", entryReq.IdempotencyKey, err)
	}
	return current, true, nil
}

const orderSelectSQL = `
	SELECT id, external_id, customer_id, tier, state, amount_in, amount_out,
		fee_units, network_fee_units, recipient_address, sender_address, quoted_at, quote_expires_at,
		version, created_at, updated_at, relay_direction
	FROM orders`

type scanRow interface {
	Scan(dest ...any) error
}

func scanOrder(row scanRow) (Order, error) {
	var o Order
	var tier, state string
	var amountIn, amountOut, feeUnits, networkFeeUnits int64
	var relayDirection *string
	err := row.Scan(
		&o.ID, &o.ExternalID, &o.CustomerID, &tier, &state, &amountIn, &amountOut,
		&feeUnits, &networkFeeUnits, &o.RecipientAddress, &o.SenderAddress, &o.QuotedAt, &o.QuoteExpiresAt,
		&o.Version, &o.CreatedAt, &o.UpdatedAt, &relayDirection,
	)
	if err != nil {
		return Order{}, err
	}
	o.Tier = Tier(tier)
	o.State = State(state)

	// Every tier except RELAY is a fixed-direction corridor -- amount_in
	// is always USDT_BEP20, the other three always USDT_TRC20 -- so the
	// asset is safely hard-coded for them, unchanged from before RELAY
	// existed. RELAY orders record which direction they actually are in
	// relay_direction (migration 0011); a RELAY row with a NULL
	// relay_direction would violate that migration's own CHECK
	// constraint, so amountInAsset/amountOutAsset below is never reached
	// with an inconsistent combination.
	amountInAsset, amountOutAsset := money.USDT_BEP20, money.USDT_TRC20
	if o.Tier == Relay && relayDirection != nil && *relayDirection == "TRC20_TO_BEP20" {
		amountInAsset, amountOutAsset = money.USDT_TRC20, money.USDT_BEP20
	}
	o.AmountIn = money.Amount{Asset: amountInAsset, Units: amountIn}
	o.AmountOut = money.Amount{Asset: amountOutAsset, Units: amountOut}

	// fee_units/network_fee_units match amount_out's asset for every
	// tier except RELAY, which matches amount_in's instead -- see
	// CreateParams.validate's own doc comment for exactly why RELAY's
	// money-flow mechanics require the opposite convention from Model
	// D's.
	feeAsset := amountOutAsset
	if o.Tier == Relay {
		feeAsset = amountInAsset
	}
	o.FeeUnits = money.Amount{Asset: feeAsset, Units: feeUnits}
	o.NetworkFeeUnits = money.Amount{Asset: feeAsset, Units: networkFeeUnits}
	return o, nil
}

// DefaultListLimit and MaxListLimit bound ListByStateAfter's page size --
// a safety net for any caller (HTTP or direct Go) that doesn't apply its
// own bound. C3's discovery poll (component-map.md's own volume figures,
// ~4 deposits/hour peak) will never come close to MaxListLimit; it exists
// so a malformed or malicious limit can't force an unbounded scan.
const (
	DefaultListLimit = 100
	MaxListLimit     = 500
)

// Cursor is an opaque pagination bookmark for ListByStateAfter: the
// (updated_at, id) of the last row a caller has already seen. Callers
// must treat the string form as opaque -- constructed and parsed only
// through Cursor.String and ParseCursor, never assembled by hand.
type Cursor struct {
	UpdatedAt time.Time
	ID        int64
}

// String encodes c as an opaque, URL-safe token.
func (c Cursor) String() string {
	raw := fmt.Sprintf("%s|%d", c.UpdatedAt.UTC().Format(time.RFC3339Nano), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor decodes a token previously produced by Cursor.String.
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidParams)
	}
	updatedAtRaw, idRaw, ok := strings.Cut(string(raw), "|")
	if !ok {
		return Cursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidParams)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: malformed cursor timestamp", ErrInvalidParams)
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: malformed cursor id", ErrInvalidParams)
	}
	return Cursor{UpdatedAt: updatedAt, ID: id}, nil
}

// ListByStateAfter returns up to limit orders in state, ordered by
// (updated_at, id) ascending, strictly after the given cursor -- keyset
// pagination, not OFFSET, so a page's cost doesn't grow with how deep
// into the result set a caller has already paged. after == nil starts
// from the beginning. limit <= 0 uses DefaultListLimit; anything above
// MaxListLimit is clamped down to it.
//
// This is C3's own discovery mechanism (§0/C3.3 of
// docs/03-build/c3-screening-build-prompts.md): polling was chosen over
// C1 pushing to C3 specifically so this stays a small, generically
// reusable "list by state" primitive, not a point-to-point coupling to
// one downstream service.
func ListByStateAfter(ctx context.Context, q accounts.Queryer, state State, after *Cursor, limit int) ([]Order, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	var afterUpdatedAt *time.Time
	var afterID *int64
	if after != nil {
		afterUpdatedAt = &after.UpdatedAt
		afterID = &after.ID
	}

	rows, err := q.Query(ctx, orderSelectSQL+`
		WHERE state = $1
		  AND ($2::timestamptz IS NULL OR (updated_at, id) > ($2, $3))
		ORDER BY updated_at ASC, id ASC
		LIMIT $4
	`, string(state), afterUpdatedAt, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("orders: list by state %s after cursor: %w", state, err)
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("orders: list by state %s after cursor: %w", state, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orders: list by state %s after cursor: %w", state, err)
	}
	return out, nil
}
