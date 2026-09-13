// Package orders owns the order state machine: the C1.5 record of which
// step of quote -> funded -> ... -> settled/refunded/expired each customer
// order is at, and the only legal way to move it between steps.
package orders

import (
	"errors"
	"time"

	"ledger/internal/money"
)

// State is the closed set of order states. The legal transitions between
// them live entirely in transitionTable (transitions.go) -- State itself
// carries no transition logic.
type State string

const (
	Quoted      State = "quoted"
	Funded      State = "funded"
	Screened    State = "screened"
	Dispatching State = "dispatching"
	Settled     State = "settled"
	Held        State = "held"
	Refunded    State = "refunded"
	Expired     State = "expired"
)

var allStates = []State{Quoted, Funded, Screened, Dispatching, Settled, Held, Refunded, Expired}

var ErrUnknownState = errors.New("orders: unknown state")

func (s State) Valid() bool {
	for _, v := range allStates {
		if s == v {
			return true
		}
	}
	return false
}

// Terminal reports whether s has no legal outgoing transition. This is
// purely descriptive -- Transition never consults it directly, because
// transitionTable already has zero entries with a terminal state as the
// "from" side, so the table lookup alone is sufficient to reject any
// attempt to leave one. It exists so callers (and this package's own
// tests) can ask the question without re-deriving it from the table.
func (s State) Terminal() bool {
	switch s {
	case Settled, Refunded, Expired:
		return true
	default:
		return false
	}
}

// Tier is the closed set of service tiers, per
// docs/02-architecture/product-operations-architecture.md decision 6.
// RELAY is Model F's own tier (docs/02-architecture/
// model-f-relay-architecture.md §5) -- added alongside Model D's three,
// reusing this same state machine and reconciliation machinery rather
// than forking it. Purely additive: nothing in this package (or,
// confirmed by exhaustive grep across ledger/, anywhere else in this
// module) exhaustively switches on Tier -- only orders.State drives the
// transition table -- so RELAY needed no other production-code change
// here. The Postgres side is not purely additive though: order_tier is
// a real ENUM type (ledger/migrations/0006_orders.sql), so adding this
// value also needed migration 0010's own ALTER TYPE.
type Tier string

const (
	Direct   Tier = "DIRECT"
	Standard Tier = "STANDARD"
	Sweep    Tier = "SWEEP"
	Relay    Tier = "RELAY"
)

var allTiers = []Tier{Direct, Standard, Sweep, Relay}

var ErrUnknownTier = errors.New("orders: unknown tier")

func (t Tier) Valid() bool {
	for _, v := range allTiers {
		if t == v {
			return true
		}
	}
	return false
}

// Order is an orders row. AmountIn/AmountOut/FeeUnits/NetworkFeeUnits are
// money.Amount rather than bare integers so their asset travels with
// them, the same discipline the rest of this codebase uses everywhere
// money appears -- even though each field's asset is fixed by column
// convention (AmountIn is always USDT_BEP20, the other three always
// USDT_TRC20, matching §B), not stored as a separate column.
type Order struct {
	ID               int64
	ExternalID       string
	CustomerID       string
	Tier             Tier
	State            State
	AmountIn         money.Amount
	AmountOut        money.Amount
	FeeUnits         money.Amount
	NetworkFeeUnits  money.Amount
	RecipientAddress string
	// SenderAddress is the BSC address that funded this order's deposit
	// -- nil until the quoted->funded transition sets it (see
	// TransitionParams.SenderAddress), and never cleared by anything
	// else afterward. Added for C3 (screening), which needs the
	// deposit's sender to call a screening provider and has no chain
	// access of its own to derive it -- see
	// docs/03-build/c3-screening-build-prompts.md's "Read this first".
	SenderAddress  *string
	QuotedAt       time.Time
	QuoteExpiresAt time.Time
	Version        int32
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
