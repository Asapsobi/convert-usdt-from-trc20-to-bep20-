# C4 — Energy broker: sequenced build prompts

> **Background (history).** Written during the design and build phase and kept as a
> record. You don't need it to run or change today's product: start at
> [docs/README.md](../README.md).

**Target:** Go + PostgreSQL, matching C1/C2/C3's stack. **Consumer:** an AI coding agent (Claude Code or equivalent), same usage pattern as the prior build-prompt docs.

**How to use this file.** Paste §0 once at the start of the session — standing context the agent must hold for every chunk. Then paste chunks C4.0 → C4.9 one at a time, in order. Do not move to the next chunk until the current chunk's acceptance criteria pass against a real (or sandbox) vendor endpoint, not a mock of your own assumptions about one.

**Before you start:** read the three call-outs below. The first is the one that should actually block starting, not just be noted in passing — the number this whole component is built around has never been confirmed at the rate this business would actually pay. The second is a real asymmetry in the four candidate vendors that the clean "one interface, four providers" story glosses over. The third is a design decision this document makes explicitly, because component-map's own wording ("acquire energy... *before* every payout") only makes sense once you pick one of two very different architectures.

---

## Read this first — the routing weights are built on unconfirmed pricing

`component-map.md` says it outright: **"Blocked on: the week-2 wholesale pricing calls. Retail-quote pricing invalidates the routing weights."** `findings-and-recommendation.md`'s Addendum 2 is more specific about why: the 24/28/30 sun figures for Tronsell/Netts/CatFee are **retail-facing quotes from a July 2026 market survey**, not confirmed partner pricing — *"Before committing, get actual wholesale/partner-tier pricing from Netts and Tronsell directly (both require contacting a business channel rather than publishing bulk rates) — the sun rates above are retail-facing quotes, not confirmed partner pricing."*

This matters more than a normal "verify before coding" footnote (like C2.4's BscScan contract-address check) because **the entire margin case for this business runs through this number.** Decision 2 in `product-operations-architecture.md` is explicit: the 60/35/5 blend at 25.7 sun ($0.6532/payout) against the market's 41 sun ($1.0420/payout) is *"the margin engine"* — a $0.389/payout gap that's supposed to be ticket-invariant. If real partner pricing comes back materially different from the retail survey (higher, or with minimum-volume commitments, or requiring prepayment terms that change the working-capital math), the 60/35/5 weighting itself may be wrong, not just a config constant that needs updating.

**This document does not treat 24/28/30 sun as ground truth.** Every chunk below reads pricing live (C4.1) and treats the weights as config (C4.2), not a hardcoded assumption — but that only protects the *code* from being wrong. It does not protect the *business* from having been sized on a number nobody has actually gotten a vendor to commit to. Do not let this component ship to production against retail rates while believing it's earning the modeled margin. Get the wholesale calls done, or explicitly flag every cost figure this component reports as provisional until they are.

---

## Read this second — the fourth "vendor" isn't a vendor, it's a smart contract

Tronsell, Netts, and CatFee are described consistently across the source docs as API-driven marketplaces: Tronsell's own listing advertises "sub-5-minute rental windows, per-call energy/time parameters, up to 300 QPS" — this is a request/response integration against an account with a pre-funded balance, no different in shape from C3's vendor-agnostic screening call.

**JustLendDAO is not that.** It's TRON's own on-chain lending protocol — energy there is acquired by interacting with a smart contract (stake collateral, borrow/delegate resources), not by calling a REST API against a business account. `findings-and-recommendation.md`'s Addendum 2 holds it as "a backstop route for reliability rather than cost" specifically because it's non-custodial and trusted, not because it's operationally similar to the other three.

This means a single `EnergyProvider` interface covering all four (as §B below proposes) is real, but it hides a discontinuity: three implementations make an HTTP call with an API key, and the fourth needs to **sign and broadcast a TRON transaction against a third-party contract** — a capability nothing else in C4's scope otherwise requires (component-map's own "does not own" line for C4 only excludes *"broadcasting the USDT transfer"*, which leaves this ambiguous rather than settling it either way).

Automating that fourth path means C4 needs some signing capability from S1, for a path that — being the most expensive, least-used option by design — will exercise that capability rarely and therefore rustily, which is its own risk (an untested signing path is worse than no signing path, in a system whose S1 component is independently flagged "High (terminal)" risk). This document's default, built into C4.6: **ship JustLendDAO as a manual/ops runbook fallback, not an automated fourth provider, at MVP.** C4 detects the "all three primary vendors degraded" condition and alerts a human with a runbook link, rather than reaching for on-chain signing to solve a rare-path problem. Automating it later is a deliberate, reviewed decision for whoever owns S1's signing surface — not a default this component should back into because the interface happened to make it look uniform.

---

## Read this third — "acquire energy before every payout" means a buffer, not a per-order round-trip

Component-map's one-line purpose for C4 is *"Acquire TRON energy at the 25.7 sun blend **before** every payout"* — and the hard-parts list warns that *"delegation must land before broadcast or the payout burns TRX at market rate."* Put together, these rule out the naive design (C5 asks C4 for energy, C4 makes a live vendor call, C5 waits for it) because that puts a vendor's live API latency — and any transient vendor slowness — directly in the critical path of every single payout, which is exactly the timing race the hard-parts list is warning about.

**This document builds C4 as a small inventory system, not a pass-through broker:**

- C4 continuously maintains a **buffer** of already-delegatable energy capacity, sized to cover some rolling number of upcoming payouts (config — at ~4 deposits/hour peak per component-map, a buffer covering even 30–60 minutes of peak demand is not a large number in absolute energy units).
- C4's routing/pricing loop (C4.1–C4.2) replenishes that buffer continuously, in the background, weighted 60/35/5 across the three live-priced vendors, respecting the ceiling.
- When C5 requests a reservation for a specific order (C4.4), the **fast path** serves it out of the already-provisioned buffer — no vendor round-trip, no timing race, delegation can be confirmed on-chain and handed back to C5 with real lead time.
- A **slow path** exists for when the buffer is insufficient (a demand spike, or the replenishment loop itself falling behind) and must make a live vendor call synchronously — this is the one place the original timing race still exists, and it is treated as a degraded, alerted condition, not routine operation.

If your own read of "pre-order lead time" in component-map differs from this, the buffer design is this document's interpretation, not something already decided elsewhere in the project docs — flag it before building if you think the per-order-live-call design was actually intended; it is meaningfully simpler to build (no buffer state, no replenishment loop) at the cost of reintroducing the exact timing race decision 2's margin case doesn't want anywhere near production.

---

## Read this fourth — real vendor APIs settled the "how does the fast path retarget the buffer" question (added after building C4's real vendor integrations)

C4.4's own design comment picked design (b) for how the fast path turns pre-provisioned buffer capacity into a delegation at the reservation's own target address: one shared buffer against a broker-controlled staging address, retargeted per reservation via an `EnergyProvider.Redelegate` call, deferring design (a) ("six parallel buffers, one pre-delegated per known payout slot") as the documented fallback "if real re-delegation latency turns out to be too high."

Building real, HTTP-calling `EnergyProvider` implementations against Tronsell's, Netts's, and CatFee's actual current APIs (not the retail-survey summaries in "Read this first") answered that question differently than latency: **none of the three expose anything that retargets an already-issued delegation to a new address at all.** Each vendor's own API is strictly "buy a new, fixed-receiver, vendor-priced order" (CatFee: `POST /v1/order`, only `duration=1h`; Netts: `POST /order1h`/`POST /order5m`; Tronsell: `POST /v1/order/rent`/`useRent`, with a genuinely custom `leaseDurationSecond`). A same-shaped `Redelegate` against any of them could only mean a second, live, full-price purchase — which would put vendor latency straight back into the fast path's own critical path (the exact thing this section exists to keep out) and pay for the same energy twice.

This document now adopts **design (a)**, not as a latency fallback but as the only one buildable against real vendor capability: `internal/buffer.Buffer` keeps one independently-sized, independently-replenished pool per known payout slot address (`Config.SlotAddresses`, sourced from whoever owns the payout wallet roster — never fabricated by C4 itself), each pre-acquired already pointed at its own final destination. A fast-path reservation is a pure database claim against an already-verified row — no vendor call, no retarget, ever. `EnergyProvider.Redelegate` has been removed from the interface entirely: every real vendor client (and `MockProvider`) now implements only `Quote` and `Delegate`, matching exactly what the vendors themselves offer.

Two further real constraints this same integration work surfaced, both handled in `internal/provider`, not worth their own top-level section:

- **Delegation duration isn't uniform across vendors.** Only Tronsell accepts an arbitrary lease length; CatFee and Netts's automated-failover product are both fixed at 1 hour. `provider.Delegation` now carries its own `ExpiresAt`, set by each vendor client to what it actually granted — `internal/buffer` records that real expiry, never a value it recomputes from the caller's own requested duration.
- **Tronsell has no free price-quote endpoint.** Unlike CatFee's `GET /v1/estimate` or Netts's `GET /pricing`, Tronsell's public API has nothing that answers "what does energy cost right now" without placing a real, paid order. `TronsellProvider.Quote` reports the price observed on the most recent real `Delegate` call and errors before one has happened, rather than spending real TRX as a side effect of a routine ~1-minute price poll — see `provider.ErrTronsellNoPriceObserved`'s own doc comment.

`internal/buffer.TronGridReader` is this same work's answer to `TronEnergyReader`'s own "ships no real TRON RPC client" gap noted in C4.3 below: a real, minimal client against TronGrid's public `getaccountresource` endpoint (the same one both CatFee's and Tronsell's own docs point integrators at for independent verification), wired into `cmd/brokerd` alongside the three vendor clients.

---

## §0 — Standing context (paste once)

```
You are building C4, the energy broker of a USDT cross-network settlement
system (BEP20 -> TRC20 payouts). C1 (ledger core) and C2 (deposit watcher) are
already built; C3 (screening) has a build spec but is not yet built; C5
(payout dispatcher) does not exist yet and will be written AGAINST this
document's reservation contract (§A), the same way C2 and C4 were written
against C1's already-frozen contract.

STACK
- Go 1.22+, PostgreSQL 16 — C4's own database, separate from every other
  component's.
- pgx/v5, goose migrations, chi routing, log/slog, testify, testcontainers-go —
  same toolchain discipline as C1/C2/C3.
- An HTTP client for calling C1's API (cost attribution only — see §A; C4
  needs no new C1 endpoints, unlike C2 and C3 before it).
- A vendor-agnostic energy provider interface. No specific vendor SDK, and no
  TRON signing library, is a hard dependency of the core package — see C4.0.

WHAT C4 IS
The service that keeps a standing buffer of TRON energy available, routed
60/35/5 across Tronsell/Netts/CatFee (weights and prices are CONFIG, not
constants baked from the retail survey — see "Read this first"), serves
reservation requests from C5 against that buffer with on-chain delegation
confirmation, falls back to a manual runbook when all three primary vendors
are degraded (see "Read this second"), and attributes the real cost of every
delegation to the ledger.

WHAT C4 IS NOT — do not build any of this, do not import libraries for it
- No broadcasting of the USDT payout transfer. That is C5's entire job; C4
  only ever delegates ENERGY, a distinct TRON resource, never moves USDT.
- No private keys or TRON transaction signing for the three primary vendors —
  those are plain authenticated HTTP API calls against a funded account
  balance, no different in shape from any other vendor integration in this
  system. (JustLendDAO is the one exception flagged above, and this document
  defaults to NOT automating it — see C4.6.)
- No slot selection. C5 decides which of the six payout slots is dispatching
  and tells C4 the target address in the reservation request. C4 never picks
  a slot on its own initiative.
- No screening, no order-state transitions of any kind. C4 posts a cost
  ENTRY to C1 (which requires no transition) and never calls
  POST /orders/{id}/transitions.
- No customer-facing auth. Same posture as every other component here.
If a chunk seems to require any of the above, you have misread it. Stop and
say so.

NON-NEGOTIABLE INVARIANTS
1. A reservation is never marked confirmed for C5 until the delegation is
   independently verified ON-CHAIN (the target address's available energy
   actually reflects it) — never on the strength of a vendor API's 200
   response alone. A vendor claiming success and the delegation actually
   landing are different facts, and only the second one is safe to act on.
2. The live-vendor price used for any buffer purchase or slow-path
   reservation is NEVER trusted from a stale cache beyond a short, configured
   staleness window (see C4.1) — TRON energy pricing moves, and a stale price
   feeding the routing weights silently breaks the margin math "Read this
   first" already flags as fragile.
3. A price above the configured ceiling is NEVER paid, under any
   circumstance, including "just this once to unblock a payout." The
   fallback ladder (C4.6) and the hold behavior (C4.7) exist precisely so
   that "pay anyway" is never the only lever available when a price spikes.
4. Every cost entry C4 posts to C1 is idempotent on delegation id, matching
   C1's own convention: "broker:energy_cost:<delegation_id>".
5. C4 never invents an entry shape or account code C1 doesn't already define
   (same rule C2 and C3 followed). The E4 shape in C1's §B
   (`DR expense:energy / CR asset:tron:energy_wallet`, in TRX) is the only
   entry C4 ever posts.
6. Buffer state (how much energy is currently provisioned, to which
   addresses, expiring when) lives entirely in C4's own database. C4 never
   treats its buffer bookkeeping as authoritative over what's actually true
   on-chain — a periodic reconciliation (C4.3) against real delegation
   records is required, the same "cache vs. computed, they must agree"
   discipline C1.4 established for balances.
7. JustLendDAO integration, if and when it is automated, requires an
   explicit, separate, reviewed decision — see "Read this second." Nothing
   in this document authorizes building TRON transaction signing into C4 by
   default.

STYLE
- Small packages: internal/provider (vendor abstraction), internal/pricing
  (polling + ceiling), internal/routing (weighted selection + fallback),
  internal/buffer (provisioning + reconciliation), internal/reservations
  (the C5-facing contract), internal/ledgerclient (the C1 HTTP contract,
  isolated exactly as C2 and C3 isolated their own), internal/httpapi.
- Errors are typed, wrapped with %w, one stable error code per condition
  reaching the HTTP layer — same discipline as every prior component.
- Tests are the deliverable. A chunk touching a vendor call is not done
  until it's proven against fakes capable of producing a timeout, a price
  spike past the ceiling, and a malformed response — not just the happy path.
- Do not add features, endpoints, or abstractions a chunk did not ask for.
```

---

## §A — Interface contracts

### With C1 (already built — one call, no new endpoints needed)

Unlike C2 and C3, C4 needs **zero additions to C1's already-shipped HTTP surface.** The only call C4 ever makes to C1 is the existing, general-purpose entry endpoint:

```
POST /v1/entries
Idempotency-Key: broker:energy_cost:<delegation_id>

{
  "idempotency_key": "broker:energy_cost:<delegation_id>",
  "entry_type": "energy_cost",
  "actor": "energy-broker",
  "order_id": <C1's internal order id>,
  "occurred_at": "<delegation confirmation time, RFC3339 UTC>",
  "lines": [
    {"account_code": "expense:energy", "amount": "<decimal string>"},
    {"account_code": "asset:tron:energy_wallet", "amount": "-<decimal string>"}
  ]
}
```

This is exactly entry E4 from `c1-ledger-build-prompts.md` §B. C4 resolves the internal `order_id` via a single `GET /v1/orders/{external_id}` call, cached for the lifetime of one reservation — no polling loop, no discovery mechanism, because C4 never acts on an order's state on its own initiative; it only reacts to a reservation request from C5 that already names the order (§A below, "With C5").

Amounts are TRX, decimal strings, 6 decimal places — same discipline as every other amount in this system.

### With C5 (not yet built — this document proposes the contract)

C5 doesn't exist yet, so this isn't a gap to work around, it's a contract to hand forward — same spirit as C1.8's "freeze point" language. Whoever builds C5 next should be pointed at this section, and this document's own C4.4 is the acceptance gate proving C4 honors it.

```
POST /v1/reservations
Idempotency-Key: <caller-supplied, e.g. "dispatch:<order_id>:<attempt>">

{
  "external_id": "<order external id>",
  "target_address": "<TRON address of the slot C5 selected>",
  "energy_units": <integer>,
  "tier": "DIRECT" | "STANDARD" | "SWEEP",
  "deadline": "<RFC3339 — how long C5 is willing to wait before giving up>"
}

-> 202 { "reservation_id": ..., "status": "pending" }
```

```
GET /v1/reservations/{reservation_id}

-> { "status": "pending" | "confirmed" | "failed", "confirmed_at": ...,
     "vendor": "tronsell" | "netts" | "catfee" | "justlend_manual",
     "cost_trx": "<decimal string>" }   -- present once confirmed
```

**C5 must not broadcast the payout until it observes `status: confirmed`.** A `failed` status (buffer exhausted AND the slow path also failed, or every vendor including the fallback ladder exhausted before `deadline`) means C5 does not have usable energy and must not attempt to broadcast — what C5 does instead (retry, hold the order, escalate) is C5's design problem, not C4's, but C4's contract guarantees it will never return `confirmed` on anything less than on-chain-verified delegation (invariant 1).

---

## §B — The provider abstraction and routing model

```go
type Verdict = // n/a, this is C3's type — do not import it, C4 has its own

type Quote struct {
    ProviderName string
    PricePerUnitSun float64
    QuotedAt        time.Time
    MaxUnitsAvailable int64   // some providers cap a single call's size
}

type Delegation struct {
    ID           string
    ProviderName string
    TargetAddress string
    EnergyUnits  int64
    CostTRX      money.Amount
    RequestedAt  time.Time
    ConfirmedAt  *time.Time   // nil until on-chain-verified
}

type EnergyProvider interface {
    Quote(ctx context.Context) (Quote, error)
    Delegate(ctx context.Context, target string, units int64, duration time.Duration) (Delegation, error)
    // Verify is deliberately NOT part of this interface — on-chain
    // verification is provider-agnostic (it's a TRON resource query against
    // the target address, not something any vendor's API tells you) and
    // lives in internal/buffer instead. Do not let a vendor's own "success"
    // response substitute for it (invariant 1).
}
```

Three implementations (`TronsellProvider`, `NettsProvider`, `CatfeeProvider`) satisfy this cleanly — authenticated HTTP clients against a funded account balance, per "Read this second." `JustLendProvider` either isn't built at MVP (this document's default) or, if it is, is the one implementation that also needs a TRON signing dependency injected — keep that dependency injectable and absent by default, never a package-level import that drags a signing library into every other provider's binary.

**Routing weights (config, not constant):**

```
routing_weights:
  tronsell: 0.60
  netts:    0.35
  catfee:   0.05
ceiling_sun_per_unit: <config — set only after the wholesale calls in "Read
  this first" complete; do not ship a production default derived from the
  retail survey without saying so out loud in the config file itself>
```

A weighted, price-aware selection: among providers currently quoting at or below the ceiling, pick with probability proportional to the configured weight — not simply "always cheapest" (that would abandon the diversification the findings doc's Addendum 2 explicitly wanted: *"diversification protects against a single provider losing liquidity mid-day"*) and not "fixed weights regardless of price" (that would ignore invariant 3 the moment one provider's weight-bucket happens to be the one that spiked).

---

## C4.0 — Scaffold and the provider interface

```
Build the repository skeleton and the vendor-agnostic interface. Nothing else.

1. Go module `energybroker`. Layout mirrors C1/C2/C3:
   cmd/brokerd/main.go
   internal/provider/
   internal/db/
   migrations/
   docker-compose.yml (Postgres 16 only)
   Makefile (build, test, test-integration, migrate-up, migrate-down, lint)

2. internal/provider:
   - The EnergyProvider interface, Quote, and Delegation types from §B.
   - MockProvider: deterministic on a seeded PRNG per provider name; supports
     forcing a price (including above any configured ceiling), a timeout, a
     partial-fill (MaxUnitsAvailable below the requested amount), and a
     malformed response.
   - Four named provider slots in config (tronsell, netts, catfee,
     justlend_manual) — the fourth wired to a NoOpProvider that always
     returns an error directing the caller to the manual runbook (C4.6),
     never a real API client, matching "Read this second"'s default.

3. internal/db: pgx pool, config from env, Tx helper — same pattern as prior
   components.

4. cmd/brokerd: starts, connects, serves GET /healthz, shuts down on SIGTERM.

ACCEPTANCE
- `make test` green.
- MockProvider: same seed always yields the same quote sequence; forced
  timeout respects ctx cancellation; forced malformed response is a typed
  parse error, never a zero-value Quote/Delegation treated as valid.
- justlend_manual's NoOpProvider is reachable through the same interface but
  every call returns a distinct, documented error type
  (ErrManualFallbackRequired), never silently succeeds.
- No file outside a real vendor's own provider implementation imports that
  vendor's SDK — enforced the same way C2.0 and C3.0 enforced their own
  import boundaries.

DO NOT
- Do not build TRON signing anywhere in this chunk, including for
  justlend_manual. That capability, if it is ever added, is a separate,
  later, explicitly-reviewed addition per "Read this second."
- Do not add pricing/routing/buffer logic yet. Interface only.
```

---

## C4.1 — Price polling and the ceiling

```
Build continuous price awareness for the three live providers. No routing
decisions yet — this chunk turns vendor quotes into a trustworthy, fresh
price feed.

TABLE price_observations
  id             bigserial primary key
  provider_name  text not null
  price_sun      double precision not null
  observed_at    timestamptz not null
  max_units      bigint null

BUILD internal/pricing
  PollAll(ctx) error
    - Calls Quote() on every configured provider concurrently, on a ticker
      (default: every 30-60s — energy prices don't move fast enough at this
      volume to need sub-minute polling, but "not stale" per invariant 2
      means picking a number and defending it, not leaving it open-ended).
    - Stores every observation (append-only, same instinct as C1's journal
      and C3's screening_results — history matters for after-the-fact
      "was this vendor actually reliable" analysis).
    - A provider that errors or times out on a poll is marked unhealthy for
      routing purposes (C4.2) starting from that tick, not retroactively.

  CurrentPrice(ctx, provider) (Quote, error)
    - Returns the most recent observation IF within the configured staleness
      window (invariant 2). Past that window, returns ErrPriceStale rather
      than the last-known value — a caller silently using a 10-minute-old
      quote is exactly the failure mode this exists to prevent.

  UnderCeiling(quote Quote, ceiling float64) bool
    - Pure function, trivially testable, deliberately factored out so
      routing (C4.2) and the slow path (C4.4) both call the exact same
      ceiling check rather than two implementations that could drift.

ACCEPTANCE
- Against three fake providers with independently varying prices: PollAll
  captures all three every tick, CurrentPrice returns the freshest for each.
- A provider frozen mid-test (never responds again): CurrentPrice for it
  returns ErrPriceStale once the staleness window elapses, even though old
  rows remain in price_observations for audit.
- UnderCeiling table-driven test including the exact-boundary case (price
  equal to ceiling — document and test which side "at the ceiling" falls on,
  the same boundary-behavior discipline C3.2 required of its own thresholds).
- A provider erroring on Quote() is excluded from CurrentPrice results (not
  a stale/zero value standing in for "unhealthy") and flagged for C4.2's
  routing to see.
```

---

## C4.2 — Routing and the fallback ladder

```
Turn live, fresh, under-ceiling prices into an actual provider selection.

BUILD internal/routing
  type Selection struct {
    Provider string
    Reason   string   // "weighted" | "fallback_ladder" | "manual_required"
  }

  SelectProvider(ctx, weights RoutingWeights, ceiling float64) (Selection, error)
    - Filters to providers that are (a) healthy per C4.1 and (b) under
      ceiling per C4.1's UnderCeiling.
    - Among survivors, weighted-random selection per §B's configured weights,
      RENORMALIZED over just the survivors (if catfee is down, tronsell:netts
      keeps its 60:35 ratio between them, not 60:35 against a phantom 5% that
      no longer has anywhere to go).
    - Zero survivors among the three primaries -> Selection{Provider:
      "justlend_manual", Reason: "fallback_ladder"} — this is the trigger
      condition for C4.6's manual runbook path, not an automatic delegation
      attempt.
    - All three primaries healthy but ALL priced above ceiling (a genuine
      market-wide spike, not a single provider's problem) -> also routes to
      the fallback signal, distinct in the returned Reason from "providers
      down" so C4.6's alerting can tell an outage from a price event.

ACCEPTANCE
- 10,000 SelectProvider calls with all three healthy and under ceiling: the
  empirical distribution matches the configured weights within a documented
  tolerance (not exact — it's random — bounded and asserted, not eyeballed).
- CatFee marked unhealthy: the empirical Tronsell:Netts ratio over 10,000
  calls matches 60:35 renormalized, not 60:35:0 with 5% of calls simply
  failing.
- All three unhealthy: every call returns the fallback_ladder Selection, zero
  attempts to call any primary provider's Delegate.
- All three healthy but all quoting above a deliberately low test ceiling:
  fallback_ladder Selection with Reason distinguishing this from the
  all-unhealthy case.
```

---

## C4.3 — The buffer and its reconciliation

```
Implements "Read this third": a standing, continuously-replenished pool of
delegatable energy, reconciled against on-chain truth rather than trusted as
bookkeeping alone.

TABLE energy_buffer
  id             bigserial primary key
  provider_name  text not null
  units          bigint not null
  acquired_at    timestamptz not null
  cost_trx       numeric(38,0) not null      -- minor units, matching C1
  expires_at     timestamptz not null        -- delegated capacity is time-
                                              -- bounded per provider terms
  status         text not null               -- AVAILABLE, RESERVED, SPENT,
                                              -- EXPIRED

BUILD internal/buffer
  TargetLevel(ctx) (int64, error)
    - Configured as a function of recent observed demand (a rolling window
      of actual reservation volume — this document does not hardcode a
      number, because ~4/hour peak today is not a permanent constant and a
      hardcoded target either over-buys (wasted cost, capital tied up with a
      vendor) or under-buys (forces the slow path routinely, which is the
      exact timing risk this design exists to avoid).
    - A configured minimum floor regardless of recent demand, so a quiet
      period doesn't let the buffer drain to zero right before a burst.

  Replenish(ctx) error
    - Compares current AVAILABLE total against TargetLevel; if short, calls
      routing.SelectProvider and provider.Delegate for the shortfall, in
      chunks bounded by each provider's MaxUnitsAvailable.
    - Every successful Delegate call is verified on-chain (see below) before
      being marked AVAILABLE — a vendor's 200 response alone never
      populates this table (invariant 1 applies here just as much as it
      does to the reservation fast path in C4.4).

  Reserve(ctx, orderID, units) (*BufferAllocation, error)
    - Atomically claims `units` worth of AVAILABLE rows (oldest-expiry-first,
      to avoid stranding soon-to-expire capacity), marks them RESERVED.
    - Insufficient AVAILABLE capacity -> returns ErrBufferExhausted, the
      signal that triggers C4.4's slow path.

  VerifyOnChain(ctx, delegation Delegation) (bool, error)
    - Queries TRON for the target address's current delegated-energy total
      and confirms the expected units are actually present. This is the one
      piece of TRON chain-read C4 needs — read-only, no signing, same
      posture C2 took toward BSC (watch, never write) even though C4's
      "chain" is TRON, not BSC.

RECONCILIATION (a ticker, default every few minutes)
  - For every RESERVED or AVAILABLE row nearing its expires_at, re-verify
    on-chain. A row whose on-chain reality no longer matches the buffer's
    bookkeeping (the delegation expired early, or a vendor's own dashboard
    revoked it — this happens in the real energy-rental market) is corrected
    immediately: marked EXPIRED, and if the underlying order still needs it,
    an alert fires rather than the corrected shortfall being silently
    absorbed into the next Replenish cycle.

ACCEPTANCE
- Replenish against a MockProvider: buffer reaches TargetLevel, respecting
  each provider's MaxUnitsAvailable chunking, weighted per C4.2.
- Reserve against a buffer with exactly enough AVAILABLE units: succeeds,
  marks exactly those rows RESERVED, no over- or under-claim under 50
  concurrent Reserve calls for different orders (a deadlock/race test, same
  posture as C1.4's concurrent-balance test).
- Reserve against an exhausted buffer: ErrBufferExhausted, zero rows touched.
- VerifyOnChain against a forked/simulated TRON node: correctly distinguishes
  present, absent, and partially-present delegation.
- Reconciliation catches a delegation that a fake "vendor revokes early"
  test condition removes mid-window, corrects the row, and alerts.
```

---

## C4.4 — Reservation API and delegation confirmation (the C5 contract)

```
Implements §A's "With C5" contract. This is the chunk C5 will actually be
built against once it exists.

BUILD internal/reservations
  Create(ctx, ReservationRequest) (Reservation, error)
    - Idempotent on the caller-supplied Idempotency-Key (same convention as
      every other write in this system).
    - Resolves external_id -> C1's internal order id via GET
      /v1/orders/{external_id} (§A), cached for this reservation's lifetime.
    - FAST PATH: buffer.Reserve for the requested units. Success -> status
      immediately confirmed once VerifyOnChain re-confirms the already-
      buffered delegation still holds for the specific target_address
      (note: buffered energy is typically delegated to a HOLDING address,
      not yet the specific slot — see the open question below).
    - SLOW PATH: buffer.Reserve returns ErrBufferExhausted -> routing.
      SelectProvider + a direct, synchronous provider.Delegate against the
      caller's target_address, bounded by the request's `deadline`. This is
      the one path that still carries real vendor-latency risk, and it is
      metered separately (C4.8) so operators can see how often the buffer
      design is actually doing its job.
    - Exceeding `deadline` on the slow path, or a fallback_ladder Selection
      with no manual resolution in time -> status failed.

  Get(ctx, reservationID) (Reservation, error)

  OPEN QUESTION this chunk must resolve, not inherit silently: energy
  delegated to a BUFFER holding address is not automatically usable by the
  specific TRON slot address C5 names in its request — TRON resource
  delegation targets a specific address. Either (a) the buffer pre-delegates
  to each of the six known slot addresses in parallel (six smaller buffers,
  more complex replenishment, zero re-delegation latency at reservation
  time), or (b) the buffer holds undedicated capacity and Create performs a
  fast RE-delegation from the buffer's holding address to the requested slot
  at reservation time (one buffer, one extra on-chain step per reservation,
  still much cheaper than a cold vendor call). This document does not pick
  for you — the six-slot design in `product-operations-architecture.md`
  decision 4 makes (a) attractive (six known, stable addresses), but
  confirm the actual re-delegation latency on testnet before committing,
  the same "verify before coding" discipline C2.4 applied to the BSC
  contract address.

ACCEPTANCE
- Fast path: a reservation against a warm buffer returns confirmed well
  within a tight test deadline (assert an actual wall-clock bound, not just
  "eventually confirmed") and zero calls reach any EnergyProvider.Delegate.
- Slow path: a reservation against a deliberately exhausted buffer
  correctly falls through to a live provider call, respects the ceiling
  (a forced-above-ceiling MockProvider on the slow path routes to
  fallback_ladder, never pays through), and returns failed if the deadline
  elapses first.
- Replaying the same Idempotency-Key returns the original reservation, never
  a second delegation for the same logical request.
- Whichever design the open question above resolves to has an explicit,
  testable re-delegation-latency assertion if (b), or a six-buffer
  replenishment-fairness test if (a).
```

---

## C4.5 — Cost attribution to C1

```
The only chunk that writes to C1. Turns a confirmed delegation into entry E4.

BUILD internal/ledgerclient
  ReportEnergyCost(ctx, delegation Delegation, orderID int64) error
    - POSTs the exact request in §A. Idempotency-Key = the delegation's own
      id, per invariant 4.
    - On 423 system_halted: back off and retry — a halt blocks money leaving
      via a transition, but does an EXPENSE entry unrelated to any
      transition get blocked too? Verify against C1's actual halt semantics
      (C1.7 says halt blocks "every transition marked halt-blocked... does
      NOT block... reads... snapshot ingestion" — POST /entries in general is
      NOT listed as always halt-blocked, but confirm this specific entry
      type isn't special-cased before assuming retry-on-halt is even the
      right behavior here, rather than treating a halt as unrelated noise).
    - On 409 idempotency_conflict: P1 bug alert, same posture C2.7 took —
      this should be structurally impossible if delegation ids are unique.

ACCEPTANCE
- Against a real running C1 instance (testcontainers): a confirmed
  reservation results in exactly the E4 entry landing in C1's journal, TRX
  balances move by the exact delegation cost, order_id correctly links it.
- Replaying the same delegation's report (simulating a C4 restart) hits
  C1's idempotency path, no double-charge to expense:energy.
- The system_halted question above is answered with a real test against a
  halted C1 instance, not assumed either way.
```

---

## C4.6 — Vendor outage and the manual fallback runbook

```
Builds out what happens when routing (C4.2) signals fallback_ladder.

TABLE manual_fallback_events
  id            bigserial primary key
  triggered_at  timestamptz not null default now()
  reason        text not null           -- "all_unhealthy" | "all_over_ceiling"
  order_id      bigint null             -- set if a specific reservation
                                          -- triggered this, null if detected
                                          -- by the buffer replenishment loop
                                          -- before any order needed it
  resolved_at   timestamptz null
  resolution    text null               -- e.g. "manual JustLend delegation
                                          -- via runbook", "vendor recovered"

BUILD internal/routing (extend)
  OnFallbackTriggered(ctx, reason, orderID *int64) error
    - Records a manual_fallback_events row, fires an alert (structured log
      minimum, real alerting hook for later — same posture as C2.8's
      orphaned-deposit alerting).
    - Does NOT attempt any JustLendDAO call — per "Read this second," this
      is a human-executed runbook step at MVP, not an automated path.
    - If triggered by a live reservation request (order_id set): the
      reservation (C4.4) still resolves to `failed` once its deadline
      elapses without a human resolving the event in time — C4 does not
      invent a way to make the order succeed on its own.

docs/runbook-energy-fallback.md: written for an operator with no context —
  what "all three vendors degraded" or "all three over ceiling" actually
  means, how to manually acquire energy via JustLendDAO's UI as a stopgap,
  how to mark a manual_fallback_events row resolved, and — critically — the
  actual step-by-step for the scenario Part 3 of `c1-scenario-catalog.md`
  calls out as **not yet sized**: "Two or three providers degraded
  simultaneously... this tips into terminal risk if it happens for long
  enough." This runbook is where that risk becomes something a human can
  actually act on rather than a line in a risk table.

ACCEPTANCE
- All three primaries forced unhealthy: exactly one manual_fallback_events
  row per triggering condition (not one per failed reservation attempt if
  several arrive during the same outage window — de-duplicate against an
  already-open event).
- A live reservation during an unresolved fallback event correctly reports
  failed at its deadline, never silently blocks forever.
- Every reason constant in code has a corresponding runbook section,
  enforced by a grep-based test — the same documentation-drift guard C1.10
  applied to halt reasons.
```

---

## C4.7 — Price-spike ceiling behavior

```
A focused chunk on the specific worry `c1-scenario-catalog.md` names for C4:
"Price spike above the configured ceiling — broker must be able to hold
rather than overpay; the failure mode to test is whether 'hold' here ever
silently becomes 'pay anyway.'" Most of the mechanism already exists
(C4.1's UnderCeiling, C4.2's routing away from over-ceiling providers) — this
chunk is about proving the boundary holds under adversarial pressure, not
adding new mechanism.

ADVERSARIAL TEST SCENARIOS (this chunk is tests-and-audit, not new code,
unless the tests below surface a real gap in C4.1/C4.2/C4.4)
  - A price spike that clears the ceiling by exactly one unit of precision —
    boundary test, same discipline as every other boundary test in this
    system.
  - A price that starts under ceiling, is used to begin a Delegate call, and
    the PROVIDER'S RESPONSE reports a higher actual charged price than
    quoted (a real vendor-integrity failure mode, not hypothetical — quoted
    vs. charged mismatches happen). C4 must reconcile the two and refuse to
    silently accept a charge above what UnderCeiling would have allowed,
    even after the vendor call already "succeeded."
  - A ceiling misconfigured to a nonsensical value (zero, negative) — C4
    should refuse to start rather than silently treat every price as over
    ceiling and permanently fallback-ladder, or worse, treat a broken
    comparison as always-true and pay anything.
  - Concurrent buffer replenishment and slow-path reservation racing on the
    same ceiling check during a price update — no window where a stale
    "under ceiling" read from one path lets a since-spiked price through.

ACCEPTANCE
- Every scenario above has a named, passing test. The vendor-charged-more-
  than-quoted scenario in particular must prove the delegation is flagged
  and the entry posted to C1 (C4.5) reflects the ACTUAL charge, never the
  stale quote — a ledger that records what was quoted rather than what was
  paid is a silent, compounding drift bug, the exact class of thing C1.4's
  own "cache vs. computed must agree" discipline exists to catch elsewhere.
- A full audit log line exists for every ceiling check performed, pass or
  fail — this is the artifact that lets someone answer "did we ever overpay"
  definitively rather than by inference.
```

---

## C4.8 — HTTP boundary

```
Expose C4 as a service, mirroring C1.8/C2.9/C3.8's shape and discipline.

ENDPOINTS (all under /v1)
  POST /reservations                  §A's C5-facing contract
  GET  /reservations/{id}
  GET  /buffer                        current AVAILABLE/RESERVED totals by
                                       provider, for ops visibility
  GET  /manual-fallback-events        ?resolved=false filter
  POST /manual-fallback-events/{id}/resolve   body: resolution, actor
  GET  /system/prices                 latest CurrentPrice per provider,
                                       health flags, ceiling
  GET  /system/invariants             buffer level vs target, reconciliation
                                       lag, fast-path vs slow-path ratio
  GET  /healthz  GET /readyz  GET /metrics

AUTH
- Service-to-service bearer token, same posture as every prior component.

ACCEPTANCE
- OpenAPI spec generated/maintained, with a test that every route is present
  in it — same requirement as C1.8/C2.9/C3.8.
- Metrics exposed: reservations_total by {fast_path, slow_path, failed},
  buffer_level gauge by provider, buffer_target gauge, replenish_cost_trx_total
  by provider, manual_fallback_events_total, ceiling_rejections_total,
  price_staleness_seconds gauge per provider.
```

---

## C4.9 — Replay harness against fakes and a real C1

```
C4's acceptance gate. Like C3.9, this needs a real running C1 (testcontainers)
but not a real vendor or a real TRON node for most of it — VerifyOnChain
(C4.3) needs a forked/simulated TRON node the same way C2.10 needed a
forked/simulated BSC node, but the routing/pricing/ceiling logic that carries
most of this component's actual risk does not.

cmd/replay/main.go, runnable as a Go test in CI. Seeded, deterministic,
prints the seed on failure — same posture as every prior harness.

SCENARIO MIX (mirrors c1-scenario-catalog.md's own C4 rows and Part 2's C4
section directly — if you add a scenario here, add the matching row there too)
  Clean fast-path reservations, majority, buffer stays near target
  Buffer exhaustion under a demand burst -> slow path -> success
  Single provider unhealthy -> renormalized 60/35/5 routing continues
  All three primaries unhealthy -> manual_fallback_events, reservations
    correctly fail at deadline with zero automated JustLend attempts
  Price spike above ceiling on all three simultaneously -> same fallback
    path, distinguished by reason from the outage case
  Vendor-charged-more-than-quoted -> flagged, correct actual cost posted
  Delegation that on-chain verification shows never actually landed despite
    a 200 from the vendor -> never marked AVAILABLE/confirmed, alerted
  Reconciliation catching an early-revoked delegation mid-window
  Concurrent reservations racing the buffer under both plenty and scarcity

FINAL ASSERTIONS
1. Every confirmed reservation has a corresponding E4 entry in C1, exactly
   once, with the correct actual (not quoted) cost.
2. No reservation ever confirms without a successful VerifyOnChain call
   backing it — assert by call-count/audit-trail, not by outcome alone.
3. No delegation is ever paid for above the configured ceiling, across every
   injected price condition.
4. Every manual_fallback_events-triggering condition produces exactly one
   event, never a duplicate per retry and never a missed one.
5. Buffer bookkeeping (AVAILABLE/RESERVED/SPENT/EXPIRED totals) matches
   on-chain reality at the end of the run, within reconciliation's own
   correction lag — the C4 analogue of C1.4's TrialBalance() == 0.
6. Zero deadlocks, zero panics, zero unexplained errors.

PERFORMANCE TARGET: same framing as C2.10/C3.9 — real volume here is low
(~100 payouts/day), so this harness is about correctness under adversarial
timing (price updates racing reservations, replenishment racing consumption),
not throughput.
```

---

## Suggested sequencing

| Chunk | Days | Notes |
|---|---|---|
| C4.0 Scaffold + provider interface | 0.5 | — |
| C4.1 Price polling + ceiling | 1.0 | Do not hardcode 24/28/30 sun as defaults — see "Read this first" |
| C4.2 Routing + fallback ladder | 1.0 | — |
| C4.3 Buffer + reconciliation | 1.5 | The crux chunk — the whole point of "Read this third" |
| C4.4 Reservation API (C5 contract) | 1.5 | Resolve the buffer-vs-slot re-delegation open question before finishing |
| C4.5 Cost attribution to C1 | 0.5 | Needs a running C1 instance |
| C4.6 Manual fallback runbook | 0.5 | Mechanism + docs only — no JustLendDAO automation by default |
| C4.7 Ceiling adversarial tests | 0.5 | Mostly test-writing against C4.1/C4.2/C4.4's existing mechanism |
| C4.8 HTTP boundary | 0.5 | Can run in parallel with C4.6/C4.7 |
| C4.9 Replay harness | 1.0 | Needs a running C1 and, for VerifyOnChain, a forked/simulated TRON node |

≈8.5–9 days against the 1.5 eng-week estimate in `component-map.md` — slightly over, mostly because of C4.3/C4.4's buffer design, which the component map's one-line description gestures at ("before every payout") without spelling out. If the buffer design in "Read this third" turns out to be more than this business needs at MVP volume, the simpler per-order-live-call design cuts C4.3 entirely and probably brings this back under 1.5 weeks — but re-read the timing-race hard-part before making that call, not just the schedule pressure.

**Before starting C4.0:** get real movement on the wholesale pricing calls ("Read this first") — this is the one blocker that isn't engineering's to resolve, and building a working, well-tested C4 against numbers the business hasn't actually secured is a real risk of shipping the wrong margin, not just a documentation gap. And get an explicit decision from whoever owns S1's signing surface on whether JustLendDAO is ever going to be automated ("Read this second") — this document defaults to no, but that default should be a decision, not an accident of which chunk got written first.

---

## Cross-reference

- C1's full build spec and the exact entry shape (§B, E4) this document is written against: `c1-ledger-build-prompts.md`
- The vendor survey and the retail-vs-wholesale pricing gap this document is built around: `findings-and-recommendation.md`, Addendum 2
- The architecture decision this component implements (rent, never stake; the 60/35/5 blend; the margin math): `product-operations-architecture.md`, decision 2
- Where these scenarios live in the corridor-wide risk view, including the not-yet-sized multi-provider-degradation terminal risk: `c1-scenario-catalog.md`, Part 2's C4 section and Part 3
- Component ownership boundaries this spec expands, and the C5 dependency this document's §A contract is written ahead of: `component-map.md`
- Current build/repo status as of the last write-up: `repository.md`
