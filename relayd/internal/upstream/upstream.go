// Package upstream defines the vendor-agnostic instant-exchange
// interface relayd calls, and holds every implementation of it. No
// other package in this module may import a specific vendor's SDK
// directly -- see dependency_test.go, which enforces that mechanically,
// the same way energybroker/internal/provider's own dependency_test.go
// enforces its own vendor-SDK boundary.
//
// Per docs/02-architecture/model-f-relay-architecture.md §3.
package upstream

import (
	"context"
	"errors"
	"time"

	"relayd/internal/money"
)

// Pair is one directed swap pair, e.g. USDT_TRC20 -> USDT_BEP20.
type Pair struct {
	From money.Asset
	To   money.Asset
}

// Quote is one provider's current price for one pair, at one point in
// time -- never mutated after construction, the same discipline
// energybroker/internal/provider.Quote and screening/internal/provider.Verdict
// already use elsewhere in this codebase. A re-check produces a new
// Quote.
type Quote struct {
	ProviderName string
	Pair         Pair
	AmountIn     money.Amount
	AmountOut    money.Amount // what the provider will actually pay out at this quote
	QuotedAt     time.Time
	ValidUntil   time.Time // the PROVIDER's own lock window -- relayd's own quote must never outlive it
}

// SwapStatus is the upstream platform's own lifecycle for one order it
// placed on our behalf -- distinct from (and narrower than) relayd's own
// internal/relay state machine, which also tracks the deposit-detection
// and screening steps that happen before an upstream order ever exists.
type SwapStatus string

const (
	StatusAwaitingDeposit SwapStatus = "awaiting_deposit"
	StatusConfirming      SwapStatus = "confirming"
	StatusExchanging      SwapStatus = "exchanging"
	StatusSending         SwapStatus = "sending"
	StatusComplete        SwapStatus = "complete"
	StatusFailed          SwapStatus = "failed"
	StatusExpired         SwapStatus = "expired"
)

// SwapOrder is one order placed with the upstream platform.
// DestinationAddress is set once, at CreateOrder, and this interface
// deliberately has no method to change it afterward -- if a real
// vendor's API ever allows retargeting an in-flight order, that is
// exactly the kind of implicit assumption energybroker's own Redelegate
// history (see energybroker/internal/provider/provider.go's own doc
// comment) warns against baking in before a real integration confirms
// it.
type SwapOrder struct {
	ProviderName       string
	ProviderOrderID    string
	DepositAddress     string // where WE must send funds -- the provider's own address
	DestinationAddress string // the end customer's own wallet
	Status             SwapStatus
	AmountIn           money.Amount
	AmountOutExpected  money.Amount
	AmountOutActual    *money.Amount // nil until the provider reports the final payout
	CreatedAt          time.Time
}

// SwapProvider is the one thing every instant-exchange integration
// (real or placeholder) must implement. All three calls must respect
// ctx cancellation/deadline -- a vendor call that ignores it and blocks
// past a caller's timeout is a bug in the implementation, not something
// callers should have to guard against separately.
type SwapProvider interface {
	Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error)
	CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error)
	GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error)
}

// ErrNoVendorConfigured is PlaceholderProvider's only possible result --
// same posture as screening's AlwaysCleanProvider and energybroker's
// NoOpProvider: reachable through the real interface, gated behind an
// explicit env var (see cmd/relayd's own upstreamProviderFromEnv, added
// alongside relayd's HTTP boundary), never silently returning a
// fabricated quote.
var ErrNoVendorConfigured = errors.New(
	"upstream: no real swap vendor configured -- set UPSTREAM_PROVIDER and the vendor's credentials before routing real orders")

// PlaceholderProvider is the only implementation this chunk (R0) ships.
// Every call fails with ErrNoVendorConfigured -- reachable through the
// same SwapProvider interface a real vendor would implement, but never
// silently succeeding, so a caller that forgets to gate on this returns
// a loud error instead of believing a swap was actually quoted or
// placed. R2 (choosing a real upstream platform) and R4 (wiring it in)
// are what replace this for real traffic.
type PlaceholderProvider struct{}

// Quote implements SwapProvider.
func (PlaceholderProvider) Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error) {
	return Quote{}, ErrNoVendorConfigured
}

// CreateOrder implements SwapProvider.
func (PlaceholderProvider) CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error) {
	return SwapOrder{}, ErrNoVendorConfigured
}

// GetOrder implements SwapProvider.
func (PlaceholderProvider) GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error) {
	return SwapOrder{}, ErrNoVendorConfigured
}
