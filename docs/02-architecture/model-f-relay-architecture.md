# Model F — zero-float relay: architecture

_13 Sep 2026. Decomposition for `docs/01-strategy/model-f-relay-findings.md`'s decision
to build. Does not re-open `component-map.md` or `product-operations-architecture.md`
— this is a second product line reusing C1/C3/C4/S1 as a substrate, not a replacement
for any of them. Build sequencing lives in
`docs/03-build/model-f-relay-build-prompts.md`._

## 1. What makes this different from Model D, mechanically

Model D pre-funds both sides of the corridor so it can pay out instantly while
deposits settle behind it — that's the capital ladder. Model F never takes a
position: every unit of USDT it touches is forwarded to an upstream instant-exchange
platform's own liquidity before this system owes anyone anything. It holds a
customer's deposit for the minutes it takes to relay it onward; the receiving side is
paid from the upstream platform's balance sheet, not this system's.

## 2. Component disposition

| Component | Model D role | Model F role |
|---|---|---|
| **C1 Ledger** | Tracks treasury/float balances on both chains, order state | Same double-entry discipline. New account tree — no `asset:tron:slot:N` treasury accounts; add a short-lived `asset:relay:leg:<order_id>` suspense account per in-flight order (§5). |
| **C2 Deposit watcher (BSC)** | Detects inbound BEP20 deposits into the float | Unchanged. Front door for the BEP20→TRC20 direction. |
| **C2′ Deposit watcher (TRON)** *(new)* | N/A — reverse flow deferred | Required from day one — mirrors C2's own address-derivation, reorg-detection, and finality logic against TRON/TRC20 instead of BSC/BEP20. |
| **C3 Screening** | Screens the deposit before releasing a float-funded payout | Same call, same gate — higher stakes with no B2B framing softening the regulatory read. |
| **C4 Energy broker** | Wholesale energy for payout to an arbitrary recipient | Still needed for one outbound TRC20 leg per order — forwarding the received amount on to the upstream platform's own deposit address. No sweep-tier batching. |
| **C5 Payout dispatcher** | Builds/signs/broadcasts the payout to the customer's own destination | Replaced by a relay forwarder (new service, `relayd`). Destination is never arbitrary — always "the upstream platform's deposit address for this order." Strip Sweep-tier/slot-cap/multisend logic rather than port it. |
| **S1 Key management** | Custody + signing for the final payout to the customer | Same custody model, smaller blast radius — signs only the forward-leg transfer to the upstream platform, never a transfer that directly reaches an arbitrary customer address. |
| **C6 API gateway** | Quote-then-order against your own energy cost, 90s lock | Same shape, different price source and a shorter lock (§6) — the cost underneath the quote is someone else's live rate, not yours to hold. |
| **`relayd`** *(new service)* | N/A | Owns the state machine in §4, calls C1/C3/C4/S1 the way `dispatcher/` does today, and owns `internal/upstream` (§3). |

## 3. `internal/upstream` — vendor-agnostic swap interface

Mirrors `energybroker/internal/provider`'s own `EnergyProvider` convention: one
interface, a dependency-boundary test forbidding any other package from importing a
specific vendor's SDK directly, one placeholder implementation gated behind an
explicit env var for proof-running before a real vendor is contracted.

```go
// Package upstream defines the vendor-agnostic instant-exchange interface
// relayd calls, and holds every implementation of it. No other package
// may import a specific vendor's SDK directly -- enforce this the same
// way energybroker/internal/provider's own dependency_test.go does.
package upstream

import (
	"context"
	"errors"
	"time"

	"relayd/internal/money"
)

type Pair struct {
	From money.Asset // e.g. USDT_TRC20
	To   money.Asset // e.g. USDT_BEP20
}

// Quote is one provider's current price for one pair, at one point in
// time -- never mutated after construction, the same discipline
// provider.Quote and provider.Verdict already use elsewhere in this
// codebase. A re-check produces a new Quote.
type Quote struct {
	ProviderName string
	Pair         Pair
	AmountIn     money.Amount
	AmountOut    money.Amount // what the provider will actually pay out at this quote
	QuotedAt     time.Time
	ValidUntil   time.Time // the PROVIDER's own lock window -- your own C6 quote must never outlive it
}

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
// vendor's API ever allows retargeting an in-flight order, that's the
// kind of implicit assumption C4's own Redelegate history (see
// energybroker/internal/provider/provider.go) warns against baking in
// before a real integration confirms it.
type SwapOrder struct {
	ProviderName       string
	ProviderOrderID    string
	DepositAddress     string // where YOU must send funds -- the provider's own address
	DestinationAddress string // the end customer's own wallet
	Status             SwapStatus
	AmountIn           money.Amount
	AmountOutExpected  money.Amount
	AmountOutActual    *money.Amount // nil until the provider reports the final payout
	CreatedAt          time.Time
}

// SwapProvider is the one thing every instant-exchange integration
// (real or placeholder) must implement. All three calls must respect
// ctx cancellation/deadline.
type SwapProvider interface {
	Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error)
	CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error)
	GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error)
}

// ErrNoVendorConfigured is PlaceholderProvider's only possible result --
// same posture as C3's AlwaysCleanProvider and C4's NoOpProvider:
// reachable through the real interface, gated behind an explicit env
// var, never silently returning a fabricated quote.
var ErrNoVendorConfigured = errors.New(
	"upstream: no real swap vendor configured -- set UPSTREAM_PROVIDER and the vendor's credentials before routing real orders")

type PlaceholderProvider struct{}

func (PlaceholderProvider) Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error) {
	return Quote{}, ErrNoVendorConfigured
}
func (PlaceholderProvider) CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error) {
	return SwapOrder{}, ErrNoVendorConfigured
}
func (PlaceholderProvider) GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error) {
	return SwapOrder{}, ErrNoVendorConfigured
}
```

## 4. State machine

```
              ┌────────────────────┐
   quote ───▶ │  AWAITING_DEPOSIT  │
              └─────────┬──────────┘
                         │ deposit seen + finalized
                         ▼
              ┌────────────────────┐
              │  DEPOSIT_DETECTED  │
              └─────────┬──────────┘
                         │ C3 verdict
                 ┌───────┴────────┐
                 ▼                ▼
            ┌─────────┐      ┌────────┐
            │SCREENED │      │  HELD  │──▶ manual review ──▶ SCREENED | REFUNDED
            └────┬────┘      └────────┘
                 │ upstream.CreateOrder ok
                 ▼
          ┌──────────────┐
          │  FORWARDING  │──(broadcast fails, retry)──┐
          └──────┬───────┘                            │
                 │ forward tx confirmed                │◀────────────┘
                 ▼
          ┌──────────────┐
          │   FORWARDED  │
          └──────┬───────┘
                 │ poll/webhook
        ┌────────┼─────────┐
        ▼        ▼         ▼
   ┌─────────┐ ┌────────┐ ┌─────────┐
   │ SETTLED │ │ FAILED │ │ EXPIRED │──▶ REFUND_PENDING ──▶ REFUNDED | UNRECOVERABLE
   └─────────┘ └────────┘ └─────────┘
   (terminal)   (needs a per-upstream-        (terminal, only reachable if the
                vendor decision, see            upstream never actually received
                build-prompts §"open items")     the forward tx)
```

`UNRECOVERABLE` is the state that doesn't exist in Model D: it's what an order is
logged as if the upstream platform accepted the forward transfer and then failed
*after* accepting it, with no refund mechanism on their side. Once the forward
transaction confirms on-chain, this system has no on-chain lever left to pull — the
funds' delivery is the upstream platform's problem, not this system's. This needs an
explicit operational answer before go-live (a support runbook with the vendor, a
compensation reserve, or a minimum order size below which the probability-weighted
loss doesn't matter) — see `model-f-relay-build-prompts.md`'s open items.

## 5. Ledger changes (C1)

- New account category: `asset:relay:leg:<order_id>` — opened when the deposit is
  confirmed, closed when the forward transfer confirms or a refund posts. A
  reconciliation check that finds one of these accounts still open after, say, one
  hour is an operational alarm — nothing in a working relay should sit here that long.
- New fee-revenue accounts, kept separate even if only one is used at launch:
  `revenue:relay_commission` (mechanism 1) and `revenue:relay_margin` (mechanism 2) —
  see `model-f-relay-findings.md` §"two open pricing mechanisms."
- `orders.tier` gets a new value, `RELAY`, alongside the existing `DIRECT` /
  `STANDARD` / `SWEEP` — reuses C1's existing state-machine and reconciliation
  machinery rather than forking it (`ledger/internal/orders/order.go`'s own `Tier`
  type).
- `UpstreamProviderName` and `UpstreamOrderID` belong on `relayd`'s **own** orders
  table, correlated to C1's order by `ExternalID` — not as new C1 columns. Services in
  this repo talk to each other over HTTP rather than sharing a database (the same
  reason `ledgerclient` packages exist at all); adding vendor-specific columns to C1
  would break its own stated boundary ("Does not own: chain access, keys, pricing").

## 6. Pricing / quote-lock

Model D's 90s lock works because the cost underneath it — its own energy cost — is
something it controls. Model F's cost underneath the quote is someone else's live
rate, over two windows it doesn't fully control: between quoting the customer and
their deposit landing, and between the deposit finalizing and the order actually being
placed with the upstream platform.

- **Shorten the lock.** A resale has no cost control — keep the quote window near the
  minimum the upstream platform's own `Quote.ValidUntil` allows, not a fixed 90s by
  default.
- **Re-quote at forward-time.** Call `upstream.Quote()` again immediately before
  `CreateOrder`, and only proceed if it's within a small tolerance of what was
  promised. A move outside tolerance is a real operational decision (eat it, hold and
  retry, or refund) — not something to paper over with a bigger blanket margin.
- **Operational gas buffer, not treasury float.** TRX for the TRC20 forward leg's
  bandwidth, BNB for the BEP20 leg's gas — typically under $50–100, held at all times,
  tracked like any ops account. This never touches the swap amount and should not be
  conflated with the capital-ladder discussion in `findings-and-recommendation.md`.

## 7. Dependency graph

```
C1 Ledger (existing) ──┬─→ C2 Deposit watcher (existing, BSC leg)
                        ├─→ C2′ Deposit watcher (new, TRON leg) ──┐
                        ├─→ C3 Screening (existing) ──────────────┼─→ relayd ──→ internal/upstream ──→ real vendor
                        └─→ C4 Energy broker (existing) ──────────┘
                             S1 Keys (existing) — required before relayd signs a forward leg on mainnet
```
