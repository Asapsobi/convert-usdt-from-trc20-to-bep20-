package orders

import "errors"

var (
	ErrOrderNotFound     = errors.New("orders: order not found")
	ErrIllegalTransition = errors.New("orders: illegal transition")
	ErrVersionConflict   = errors.New("orders: version conflict")
	ErrEntryRequired     = errors.New("orders: this transition requires a journal entry")
	ErrEntryNotAllowed   = errors.New("orders: this transition does not accept a journal entry")
	ErrInvalidParams     = errors.New("orders: invalid parameters")
)

// rule describes one legal (from, to) pair.
//
// HaltBlocked is recorded here as static metadata for C1.7, which does not
// exist yet: this chunk (C1.5) does not check any halt flag, because
// there is no halt flag to check until the reconciler is built. Transition
// never consults HaltBlocked itself. It is here so the single static
// table remains the one source of truth for every property of every
// transition, rather than splitting "is this pair legal" (here) from "is
// this pair halt-blocked" (wherever C1.7 ends up putting it).
type rule struct {
	RequiresEntry bool
	HaltBlocked   bool
}

// pair is a (from, to) state pair, used as the transition table's key.
type pair struct {
	From State
	To   State
}

// transitionTable is the only source of truth for which transitions are
// legal. Any (from, to) pair not present here is illegal, full stop --
// including every self-transition and every attempt to leave a terminal
// state, neither of which needs its own special-case check because
// neither ever appears as a key below.
//
// The two pairs marked "reversal" in the build spec (Funded->Quoted,
// Dispatching->Held) are handled identically to every other
// RequiresEntry transition here: Transition always calls journal.Post
// with whatever EntryRequest the caller supplies via TransitionParams.
// journal.Reverse (C1.6) does not exist yet, and nothing about this
// transition table requires it to -- constructing a reversal-shaped
// entry, once C1.6 provides a way to, is the caller's responsibility, not
// something Transition special-cases by (from, to) pair.
// {Screened, Refunded} (added for Model F): RELAY tier only in practice
// -- relayd gave up on a stuck screened order (its own
// upstream.CreateOrder call never once succeeded) and refunded it
// directly, per docs/02-architecture/model-f-relay-architecture.md §5's
// own reconciliation posture -- see
// relayd/internal/orchestrate/refund.go's own doc comment. Model D's own
// DIRECT/STANDARD/SWEEP tiers never sit in `screened` long enough in
// practice for this to matter (C5 picks a screened order up virtually
// immediately), but this transition is not itself tier-gated -- C1
// reuses its own existing state machine rather than forking it for
// RELAY, per that same architecture doc's own §5.
var transitionTable = map[pair]rule{
	{Quoted, Funded}:        {RequiresEntry: true, HaltBlocked: false},  // deposit reached 15 conf
	{Quoted, Expired}:       {RequiresEntry: false, HaltBlocked: false}, // quote_expires_at passed, nothing received
	{Funded, Screened}:      {RequiresEntry: false, HaltBlocked: false}, // screening verdict pass
	{Funded, Held}:          {RequiresEntry: false, HaltBlocked: false}, // screening verdict hold
	{Funded, Refunded}:      {RequiresEntry: true, HaltBlocked: true},   // customer cancel pre-conversion
	{Funded, Quoted}:        {RequiresEntry: true, HaltBlocked: false},  // deposit reorged out (reversal)
	{Held, Screened}:        {RequiresEntry: false, HaltBlocked: false}, // manual release
	{Held, Refunded}:        {RequiresEntry: true, HaltBlocked: true},   // manual reject
	{Screened, Dispatching}: {RequiresEntry: true, HaltBlocked: true},   // posts the conversion entry (§B)
	{Screened, Refunded}:    {RequiresEntry: true, HaltBlocked: true},   // stuck screened order, gave up (see above)
	{Dispatching, Settled}:  {RequiresEntry: true, HaltBlocked: true},   // payout SR-final
	{Dispatching, Held}:     {RequiresEntry: true, HaltBlocked: false},  // non-retryable failure (reversal)
}
