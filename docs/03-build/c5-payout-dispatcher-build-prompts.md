# C5 — Payout dispatcher (TRON): sequenced build prompts

**Target:** Go + PostgreSQL, matching C1–C4's stack. **Consumer:** an AI coding agent (Claude Code or equivalent), same usage pattern as the prior build-prompt docs.

**How to use this file.** Paste §0 once at the start of the session. Then paste chunks C5.0 → C5.11 one at a time, in order. Do not move to the next chunk until the current chunk's acceptance criteria pass. **More than any prior component, do not skip the "Read this" sections below** — one of them is not a documentation gap, it is the literal absence of any code anywhere in this repository that can sign a TRON transaction.

**Before you start:** this document was written after inspecting the actual, currently-built code for C1–C4 directly (not just their spec docs, which have drifted — C3 and C4 are both further along than the project workspace's own docs currently say). Every interface claim below — C1's real transition/reversal request shapes, C4's real reservation contract — is taken from the shipped Go code, not re-derived from the original specs. Where the specs and the code disagree, this document follows the code.

---

## Read this first — S1 does not exist. Not "not finished." Not started.

`component-map.md`'s dependency graph says it plainly: *"S1 Keys — required before C5 touches mainnet."* A repo-wide search for anything signing- or key-related turns up exactly one file: `depositwatcher/internal/addresses/no_signing_test.go` — a test that proves C2 *cannot* sign anything, which is correct and deliberate, but it means the search comes up empty everywhere it matters. There is no S1 module, no key-custody design, no split-custody scheme, nothing. Every other component's "Read this first" gap so far has been a missing endpoint or an unconfirmed number — something that blocks *finishing* the component. This one blocks *starting* the one thing C5 exists to do: sign and broadcast a TRON transaction.

This document does not build S1. It builds C5 entirely against a **proposed signing-service contract** — the same move C4's own doc made for C5 before C5 existed, now one level further down the dependency chain. Every chunk that would touch a real key is written against this interface and tested with a fake:

```go
type SigningService interface {
    // Sign returns a raw, broadcast-ready signed transaction. It never
    // returns key material, a seed, or anything from which one could be
    // derived -- C5 holds no more capability after this call than it did
    // before it.
    Sign(ctx context.Context, slotID int, unsignedTx []byte) (signedTx []byte, err error)

    // SlotAddress resolves a slot id (1-6) to its real TRON address --
    // the one piece of slot identity C5 needs that isn't a private
    // detail, so it's part of this interface rather than something C5
    // has to hardcode or duplicate from wherever S1 ends up keeping it.
    SlotAddress(ctx context.Context, slotID int) (string, error)
}
```

**Nothing in this document authorizes building a real implementation of this interface.** That is S1's job, and S1 doesn't have a build-prompts doc yet either — this project's docs list (`repository.md`) has none. Whoever picks up S1 next should treat the interface above as a starting proposal, not a settled contract, the same caveat this document itself applies to what C4 proposed for it.

**Practical consequence for sequencing:** C5.0 through C5.9 below can be built and fully tested against a `FakeSigningService` today. **Nothing in this component can be run against real TRON, at all, until S1 exists and a real implementation is wired in.** Do not let a passing test suite against the fake create the impression this component is closer to production than it is.

---

## Read this second — the reservation contract is real code, but C4 has no HTTP surface yet

Unlike every prior "blocked on an endpoint" gap in this project, this one is small and already half-solved: C4's actual reservation logic (`energybroker/internal/reservations/reservations.go`) is built, tested, and matches its own spec closely. What's missing is purely `energybroker/internal/httpapi` — as of this writing it wires only `/healthz`, none of C4.8's `/reservations` routes.

The real contract, taken directly from the shipped Go types (not re-derived):

```go
// Request — POST /v1/reservations (once C4.8 exists)
type Request struct {
    IdempotencyKey string
    ExternalID     string
    TargetAddress  string    // the slot address C5 selected — see §B
    EnergyUnits    int64
    Tier           string    // "DIRECT" | "STANDARD" | "SWEEP"
    Deadline       time.Time
}

// Reservation — the response, polled via GET /v1/reservations/{id}
type Reservation struct {
    ID, OrderID              int64
    ExternalID, TargetAddress string
    EnergyUnits              int64
    Tier                     string
    Status                   string // "PENDING" | "CONFIRMED" | "FAILED"
    Vendor                   *string
    CostTRX                  *money.Amount
    ConfirmedAt              *time.Time
    FastPath                 *bool  // true = served from C4's buffer, false = slow path, nil until resolved
}
```

Build C5.2 (reservation integration) against an HTTP client for this shape now. It is a safe bet — the Go-level implementation behind it is already built and tested — but confirm the actual route paths and status codes once C4.8 ships, the same "verify before trusting a spec" discipline every prior component in this project has applied to something.

**Update: the concern this paragraph originally raised is now moot, not just unmeasured.** It used to say real on-chain re-delegation latency (buffer's staging address → the specific slot address, on every fast-path confirmation) had never been measured against a real TRON node. `energybroker/internal/reservations`'s own header comment now explains why that measurement is no longer needed at all: the original design (one shared staging address, fast re-delegation to the requested slot at reservation time) assumed a real vendor could retarget an already-issued delegation on request — false against all three real vendors' actual APIs, each of which only sells a new, fixed-receiver order. The package now uses one independently-replenished buffer *per slot address*, pre-acquired already pointed at its final destination — a fast-path confirmation is a pure database transition, zero vendor calls, zero re-delegation step to measure latency on at all.

**What's still genuinely true, and worth keeping:** no component in this repo has ever signed or broadcast through a real (non-fake) S1 KMS adapter, and nothing has been proven end-to-end against an actual TRON node — general chain-timing behavior (confirmation latency, energy-exhaustion races) remains unmeasured against anything real. That's a separate, still-open gap from the re-delegation one above, and it isn't closed by this update.

---

## Read this third — the dispatch-failure path has the exact atomicity gap C1.11 already fixed once, for a different transition

Walk C1's real, shipped HTTP surface (`ledger/internal/httpapi/server.go`):

```
POST /v1/entries/{id}/reversal          -- reverses one entry, NOT idempotent (409 + the existing reversal on retry)
POST /v1/orders/{external_id}/transitions   -- takes either an inline `entry`, or an `entry_id` naming an
                                               already-posted reversal (per C1.5's TransitionParams, exposed
                                               over HTTP only since C1.11)
POST /v1/orders/{external_id}/reorg     -- the ONE dispatch-shaped compound operation that exists: reverses the
                                            deposit_final entry AND transitions the order, atomically, server-side,
                                            branching on the order's own current state (scenario A vs B) so the
                                            caller never nominates the outcome
```

C1.5's transition table has two "reversal-shaped" transitions: `funded -> quoted` (the reorg case, which got the dedicated atomic `/reorg` endpoint above) and **`dispatching -> held`** (a non-retryable dispatch failure, which did not). The code comment on `postTransitionRequest.EntryID` in `ledger/internal/httpapi/orders_handlers.go` says this outright: *"until C1.11 nothing exposed [EntryID], so the only way to drive the two reversal-shaped transitions... was to hand-build a negating entry."* C1.11 fixed the *exposure* problem for both by adding the generic `entry_id` field — but it only built the *atomic, single-call, server-side-branching* version (`/reorg`) for one of the two cases.

That means **C5, dispatching a non-retryable failure, must currently do this as two separate HTTP calls**: `POST /entries/{conversion_entry_id}/reversal`, then `POST /orders/{external_id}/transitions` with that reversal's id. A crash, timeout, or halt between those two calls leaves the books correctly reversed (the reversal itself is atomic and balanced) but the order still shows `dispatching` — a state the order-vs-ledger reconciliation has no name for, because nothing in C1.7's reconciler currently checks "does this order's state agree with what its own entries say happened to it."

This document proposes closing this the same way C1.11 closed the analogous gap for C2 — a small, contained, precedented addition:

```
POST /v1/orders/{external_id}/dispatch-failure
Idempotency-Key: <caller-supplied>

{ "conversion_entry_idempotency_key": "<the E2 entry's own key>",
  "reason": "..." }

-> reverses the conversion entry AND transitions dispatching -> held, atomically,
   server-side, in one transaction -- exactly /reorg's shape, one level later in
   the order lifecycle.
```

Until this exists, build C5.7 against it as a stub (same posture C2.6 took toward the reorg endpoint before C1.11 shipped) and fall back to the two-call sequence with a documented, tested "what if the second call never arrives" recovery path (a reconciliation job, not a hope) as the interim behavior.

---

## Read this fourth — nothing owns slot identity, caps, or rotation yet, and the Sweep-tier energy number is still a written-down assumption

Two smaller but real gaps, both squarely C5's own to close (component-map already scopes them to C5, so these aren't proposals to another component — they're this document's own chunks):

- C1 knows `asset:tron:slot:<id>` only as a parseable account-code string (`ledger/internal/accounts/code.go`) — it has no registry mapping slot id → real address, no concept of the $50k/5,000-tx caps from `product-operations-architecture.md` decision 4, and no rotation state. C5 owns all of it (C5.1).
- `component-map.md` flags outright: *"the week-6 batched-multisend energy measurement (35,000/recipient assumption) is measured here"* — meaning this number is not yet observed, and everything about Sweep-tier batching (decision 6's claim that Sweep is simultaneously the cheapest AND highest-margin tier) rests on it. C5.8 builds the batching mechanism against a configurable per-recipient energy estimate; it does not get to assume 35,000 is correct.

---

## §0 — Standing context (paste once)

```
You are building C5, the payout dispatcher of a USDT cross-network settlement
system (BEP20 -> TRC20 payouts). C1 (ledger), C2 (deposit watcher), C3
(screening), and C4 (energy broker, minus its HTTP boundary) are already built.
S1 (key management/signing) does NOT exist -- see "Read this first." Everything
this component does that would touch a real key is built against a proposed
interface and a fake, never a real implementation.

STACK
- Go 1.22+, PostgreSQL 16 -- C5's own database, separate from every other
  component's.
- pgx/v5, goose migrations, chi routing, log/slog, testify, testcontainers-go.
- An HTTP client for C1 (§A) and C4 (§A) built against their REAL shipped
  request/response shapes, not the original spec docs where the two differ.
- A TRON transaction-construction library for building (never signing)
  multisend/single-send payloads -- research the current standard library for
  this the same way C2.2 was told to verify go-ethereum's ethclient was still
  the right choice; do not assume a library choice made months before this
  chunk is still current.

WHAT C5 IS
The component that turns a screened order into an actual TRC20 payment: selects
a payout slot under its caps, reserves and confirms energy via C4, posts the
conversion entry to C1 and enters `dispatching`, constructs and (via S1)
signs the transaction, broadcasts it, confirms SR finality, and reports
`settled` -- or, on a non-retryable failure, reverses the conversion and
reports `held`.

WHAT C5 IS NOT -- do not build any of this, do not import libraries for it
- No private keys, no signing. Every signature comes from the SigningService
  interface in "Read this first." A key, seed, or anything key-derived
  appearing anywhere in this module's memory or logs is a bug, not a feature
  waiting to be wired up.
- No energy acquisition logic. C4 owns pricing, routing, and the buffer; C5
  only ever calls C4's reservation API and waits for a verdict.
- No screening. C5 only ever dispatches an order already in `screened`.
- No customer-facing auth, no quote/pricing logic. Those are C6's.
- No treasury rebalancing. `position:corridor` and cross-chain inventory are
  S2's problem (a runbook at MVP, not a service).
If a chunk seems to require any of the above, you have misread it. Stop and
say so.

NON-NEGOTIABLE INVARIANTS
1. A broadcast is attempted at most once per (order, dispatch attempt) pair,
   with exactly-once semantics under retry -- component-map's own C5 "hard
   parts" list names this explicitly and it needs its own proof, the same way
   C1.3 proved idempotent Post under concurrency.
2. Energy is confirmed CONFIRMED by C4 before a broadcast is attempted. Never
   the reverse. A broadcast attempted against a PENDING or FAILED reservation
   is a bug this document treats as seriously as C4 treats paying above its
   own price ceiling.
3. The conversion entry (E2, `screened -> dispatching`) and the settlement
   entry (E3, `dispatching -> settled`) are two separate, sequential C1 calls,
   never combined -- this is how C1.5's transition table is actually shaped
   and C5 does not get to invent a shortcut across it.
4. A non-retryable failure after entering `dispatching` MUST reverse the
   conversion entry before (or atomically with, once the "Read this third"
   gap closes) reporting `held`. An order sitting in `dispatching` with an
   un-reversed conversion and no active dispatch attempt is a silent-loss
   shape this system has specifically built machinery elsewhere (C1.6
   scenario B) to make impossible -- do not reintroduce it here by omission.
5. Slot caps ($50k / 5,000 tx per decision 4) are checked against C1's own
   real-time balance for that slot (`GET /v1/accounts/{code}/balance`), never
   against a value C5 cached and could have drifted from the ledger's own
   truth -- the same "cache vs. computed must agree" posture C1.4 established
   for its own balances.
6. Every cost or state-changing call C5 makes to C1 or C4 is idempotent on a
   caller-supplied key, matching this system's universal convention:
   "dispatcher:<domain>:<order_id>:<attempt>".
7. No file outside the S1-facing boundary (a single, narrow package) imports
   anything that could sign a transaction or construct one from key material
   -- enforced the same way C2.0 enforced its own no-signing boundary.

STYLE
- Small packages: internal/slots (registry, caps, rotation), internal/energy
  (C4 client), internal/txbuild (construction, no signing), internal/signing
  (the S1 client -- the ONLY package that ever calls SigningService),
  internal/dispatch (the state machine tying it together), internal/ledgerclient
  (the C1 HTTP contract), internal/httpapi.
- Errors typed, wrapped with %w, one stable code per condition reaching HTTP --
  same discipline as every prior component.
- Tests are the deliverable. A chunk touching broadcast or signing is not done
  until proven against a fake capable of a timeout, a partial multisend
  failure, and a duplicate-broadcast attempt -- not just the happy path.
- Do not add features, endpoints, or abstractions a chunk did not ask for.
```

---

## §A — Interface contracts (verified against the real shipped code)

### With C1

```
GET  /v1/orders/{external_id}                       -- get current state, version
GET  /v1/accounts/{code}/balance                     -- real-time slot balance for caps (invariant 5)

POST /v1/orders/{external_id}/transitions
Idempotency-Key: dispatcher:enter_dispatching:<order_id>
{ "to_state": "dispatching", "expected_version": <v>, "reason": "dispatch_start",
  "entry": { "entry_type": "conversion", "occurred_at": "...",
             "lines": [ ...the E2 shape from c1-ledger-build-prompts.md §B... ] } }

POST /v1/orders/{external_id}/transitions
Idempotency-Key: dispatcher:settle:<order_id>:<attempt>
{ "to_state": "settled", "expected_version": <v>, "reason": "payout_sr_final",
  "entry": { "entry_type": "payout_settled", "occurred_at": "...",
             "lines": [ ...E3 shape... ] } }

-- Non-retryable failure, TODAY (two calls, not atomic -- see "Read this third"):
POST /v1/entries/{conversion_entry_id}/reversal
Idempotency-Key: n/a -- this endpoint is deliberately NOT idempotent; a
                  retry must check for the existing reversal first (GET
                  /v1/entries?idempotency_key=<E2's key> + look for a
                  reversal_of pointing at it) rather than call twice.
{ "reason": "...", "occurred_at": "..." }

POST /v1/orders/{external_id}/transitions
Idempotency-Key: dispatcher:fail:<order_id>:<attempt>
{ "to_state": "held", "expected_version": <v>, "reason": "dispatch_failed",
  "entry_id": <the reversal's id from the call above> }

-- ONCE "Read this third"'s proposed endpoint exists, replace both calls with:
POST /v1/orders/{external_id}/dispatch-failure
{ "conversion_entry_idempotency_key": "dispatcher:enter_dispatching:<order_id>",
  "reason": "..." }
```

Note what's real and verified here versus this document's own extrapolation: the transition and reversal shapes above are taken directly from `ledger/internal/httpapi/orders_handlers.go` and `reversals.go`. **`actor` is never a request field on any C1 write** — it comes from `actorFromContext`, resolved from the bearer token by `authMiddleware`, full stop. (This corrects an open question the C3 build-prompts doc raised and guessed at before C3 was built; the real code confirms actor is bearer-derived everywhere, and C3's own shipped `holds` package worked around it correctly by keeping `resolved_by` as its own local field rather than trying to pass a reviewer identity to C1. C5 needs no equivalent workaround — nothing in C5 has a human-in-the-loop actor, every C5 write is machine-attributed to whatever service identity `dispatcher`'s own bearer token carries.)

### With C4 (energy broker)

The request/response shapes in "Read this second," verbatim from `energybroker/internal/reservations/reservations.go`. C5 calls `POST /v1/reservations` once C4.8 exists; until then, C5.2 is built and tested against a fake HTTP server serving exactly this contract, so the swap-in is mechanical once C4.8 ships.

### With S1 (proposed — S1 does not exist)

The `SigningService` interface in "Read this first." This is a proposal, not a contract anyone has agreed to yet — flag it to whoever picks up S1 next rather than treating it as settled the way C1's frozen HTTP surface is.

---

## §B — Slot management, caps, and the batching model

**Slot registry** (C5's own database — nothing upstream provides this):

```
TABLE slots
  id              smallint primary key           -- 1-6, per decision 4
  tron_address    text not null unique
  status          text not null                  -- ACTIVE, RETIRING, RETIRED
  activated_at    timestamptz not null
  retired_at      timestamptz null
  tx_count        bigint not null default 0       -- since activation, never reset on rotation start
```

**Cap check, per dispatch:** before selecting a slot, query C1's real `GET /v1/accounts/asset:tron:slot:<id>/balance` (invariant 5) and this table's own `tx_count`. A slot within both the $50k balance ceiling and the 5,000-tx ceiling is eligible; nothing else is. Selection among eligible slots is round-robin or least-recently-used — this document does not prescribe one, but whichever is chosen must be deterministic and tested, not "whichever happens to be first in a map iteration."

**Rotation:** when a slot approaches either cap, mark it `RETIRING` (accepts no new dispatches, still finishes anything already in flight) rather than `RETIRED` outright — component-map's own C5 hard-parts list names *"slot rotation stranding a small balance below the dust threshold in a retiring slot"* as a known failure mode. This document does not solve dust-stranding (it's a small, real, unsized cost, not a correctness bug) but the rotation state machine must have a place for a retiring slot's dust to be *visible*, not silently lost track of.

**Batching (Sweep tier):** a Sweep-tier dispatch is N orders, one slot, one multisend transaction. C5 owns the batching window (how long to accumulate orders before cutting a batch — a real latency/efficiency tradeoff this document leaves as config, not a fixed number) and the partial-failure handling for "40 of 50 recipients settle, 10 fail" (`c1-scenario-catalog.md`'s own named scenario, mirrored in C1.9's replay mix at 2%). A partial multisend failure means: the 40 that succeeded get their own `settled` transitions; the 10 that failed get retried in the *next* batch window, never silently dropped, and never re-attempted inside the same broadcast (invariant 1's exactly-once applies per-recipient, not per-batch).

---

## C5.0 — Scaffold, the signing interface, and the slot registry

```
Build the repository skeleton, the SigningService interface with a fake, and
slot storage. Nothing that touches C1, C4, or a real chain yet.

1. Go module `dispatcher`. Layout mirrors C1-C4:
   cmd/dispatchd/main.go
   internal/signing/    -- SigningService interface + FakeSigningService
   internal/slots/
   internal/db/
   migrations/
   docker-compose.yml (Postgres 16 only, port 5436 -- distinct from all four
     prior services' ports, so all five can run together)
   Makefile (build, test, test-integration, migrate-up, migrate-down, lint)

2. internal/signing:
   - SigningService interface from "Read this first."
   - FakeSigningService: deterministic on a seeded PRNG, returns a
     syntactically valid signed-tx byte string for any input; configurable
     to return an error, a timeout, or (for the exactly-once test in C5.5)
     the exact same signature twice for the exact same unsigned tx.
   - A dependency test (same pattern as C2.0/C3.0/C4.0): nothing outside
     internal/signing imports anything that could plausibly hold or derive
     key material.

3. internal/slots: the TABLE from §B, Create/Get/List(status), MarkRetiring,
   IncrementTxCount -- storage only, no cap-checking logic yet (that needs
   C1's balance endpoint, which is C5.1).

4. internal/db, cmd/dispatchd: same pattern as every prior component.

ACCEPTANCE
- `make test` green.
- FakeSigningService: same seed always yields the same signature for the
  same input; forced-duplicate-signature mode actually returns byte-identical
  output twice, for C5.5 to detect.
- The signing dependency-boundary test passes and is specific enough to fail
  if someone later adds a private-key type to a shared package by accident.
- Slot storage: Create rejects a duplicate id or address; MarkRetiring is a
  one-way transition (ACTIVE -> RETIRING -> RETIRED, never backwards) enforced
  at the DB level, same defense-in-depth instinct as C2.1's address lifecycle.

DO NOT
- Do not add cap-checking, C1 integration, or any real chain-construction
  logic yet. This chunk is storage and interfaces only.
```

---

## C5.1 — Slot selection under caps

```
Turn the slot registry into a real selection decision, checked against C1's
live balance.

BUILD internal/slots (extend)
  SelectForDispatch(ctx, ledgerClient BalanceReader, tier string) (Slot, error)
    - Lists ACTIVE slots, queries GET /v1/accounts/asset:tron:slot:<id>/balance
      for each (concurrently -- six slots, this is cheap), filters to those
      under BOTH the $50k balance ceiling and the 5,000-tx ceiling (config,
      not hardcoded, so a real cap change doesn't need a redeploy).
    - Zero eligible slots -> ErrNoEligibleSlot. This is the C5-side version
      of C4's fallback_ladder signal: a real condition to alert on, never a
      silent block-forever.
    - Deterministic selection among eligible slots (documented tie-break,
      e.g. least-recently-used by last dispatch time) -- a test asserts the
      exact rule, not just "some slot came back."

ACCEPTANCE
- Against a fake BalanceReader returning controlled balances/tx-counts: the
  eligible set is exactly the slots under both caps, and selection is
  reproducible given the same inputs.
- A slot at exactly the balance ceiling is excluded (boundary test, same
  discipline as every ceiling/threshold test in this project).
- All six slots over cap: ErrNoEligibleSlot, zero dispatch attempts made.
- SelectForDispatch never trusts a locally cached balance -- a test that
  changes the fake BalanceReader's return value between two calls proves
  the second call reflects the new value, not a stale one.
```

---

## C5.2 — Energy reservation integration

```
BUILD internal/energy
  type Client struct { baseURL string; httpClient *http.Client }
  Reserve(ctx, orderID, targetAddress string, units int64, tier string, deadline time.Time) (Reservation, error)
    - POSTs §A's real request shape. Idempotency-Key per invariant 6.
  Poll(ctx, reservationID int64, deadline time.Time) (Reservation, error)
    - Polls GET /v1/reservations/{id} until CONFIRMED, FAILED, or the
      deadline elapses (returns a typed ErrReservationTimeout distinct from
      a FAILED status -- these are different conditions C5.3 must react to
      differently: FAILED means C4 gave up; a client-side timeout means C5
      doesn't know what happened and must not proceed as if it does).

ACCEPTANCE
- Against a fake C4 HTTP server built exactly to §A's contract: Reserve +
  Poll round-trips a CONFIRMED reservation correctly, including reading
  FastPath and CostTRX for later cost-attribution bookkeeping (C5's own
  audit trail, even though C4 -- not C5 -- posts the actual expense entry
  per its own C4.5).
- A FAILED reservation is surfaced distinctly from a client-side timeout.
- Reserve is idempotent: replaying the same order/attempt does not create
  a second reservation against a real C4 idempotency-key check (test against
  the fake server's own idempotency handling, matching C4's actual behavior).
- This chunk is written to be trivially re-pointed at a real C4 instance once
  C4.8 ships -- a config change, not a code change. Prove this by running the
  same test suite against a manually-started real energybroker binary if one
  is available in the build environment; skip (not fail) if it isn't, with a
  clear log line saying why.
```

---

## C5.3 — Entering dispatching

```
The chunk that commits to the conversion. Everything before this point is
reversible with zero ledger impact (a failed slot selection or a failed
reservation touches nothing in C1). This is not.

BUILD internal/dispatch
  type Attempt struct { OrderID int64; SlotID int; ConversionEntryKey string; ... }

  EnterDispatching(ctx, order Order, slot Slot) (Attempt, error)
    - Builds the E2 conversion entry per c1-ledger-build-prompts.md §B,
      using the order's own amount_in/amount_out/fee_units/network_fee_units
      -- C5 does not recompute pricing, it uses exactly what C1's order
      record already carries (screened orders already have these fields
      frozen from quote time).
    - Calls POST /orders/{external_id}/transitions with to_state:
      "dispatching", the entry inline, Idempotency-Key per invariant 6.
    - On success: records the attempt locally (own DB), including the E2
      entry's idempotency key -- this is what C5.7's reversal call (or the
      proposed dispatch-failure endpoint) will need later.
    - On 409 version_conflict: refetch, retry once (standard pattern).
    - On 423 system_halted: this transition MARKED halt-blocked in C1.5 --
      unlike C4.5's entry post, this one legitimately blocks on a halt.
      Back off and retry on a longer interval; do not treat this as a
      failure requiring dispatch-failure handling.

ACCEPTANCE
- Against a real running C1 instance (testcontainers): a screened order
  correctly enters `dispatching`, the E2 entry lands with both assets
  balancing independently (per §B's worked example), and the order's
  version increments.
- Halted C1: the call retries on backoff and succeeds once cleared, never
  routes to dispatch-failure handling just because of a halt.
- Replaying the same attempt (simulating a C5 restart between reservation
  and this call) hits C1's idempotency path -- no duplicate conversion.
```

---

## C5.4 — Transaction construction (Direct and Standard tiers)

```
Build the unsigned TRC20 transfer, single recipient. Sweep's multisend
variant is C5.8 -- do not generalize to N-recipient here, the two shapes
are different enough that a premature abstraction will cost more than it
saves (the same judgment call C1.5 made explicitly deferring a
dispatch_attempts table until it was actually needed).

BUILD internal/txbuild
  BuildTransfer(slotAddress, recipientAddress string, amount money.Amount) (unsignedTx []byte, err error)
    - Pure construction. No signing, no broadcast, no network call of any
      kind in this package -- the boundary this whole component's
      "Read this first" cares about starts here, not just at internal/signing.
    - Validates the recipient address's format/checksum before construction
      -- a malformed recipient is a data problem C6/C1 should have already
      prevented, but this is the last chance to catch it before a real
      broadcast would burn energy on a doomed transaction.

ACCEPTANCE
- BuildTransfer against known-good inputs produces a syntactically valid
  unsigned TRC20 transfer payload (validate against whatever library C5.0
  brought in, per "verify the current standard library" note in §0).
- A malformed recipient address is rejected before any bytes are built.
- BuildTransfer is deterministic: same inputs, byte-identical output --
  this matters because C5.5's exactly-once test needs to distinguish "the
  same logical transfer, rebuilt on retry" from "a different transfer."
```

---

## C5.5 — Broadcast and exactly-once retry semantics

```
The hard-parts item component-map names explicitly: "duplicate broadcast on
retry -- exactly-once semantics under retry is listed as a hard part and
needs its own proof, analogous to C1.3's concurrent-Post test but at the
chain-broadcast layer."

TABLE dispatch_attempts       -- the table C1.5 explicitly declined to build,
                                -- confirmed still absent from C1 as of this
                                -- writing -- because it's C5's concern, not C1's
  id              bigserial primary key
  order_id        bigint not null
  slot_id         smallint not null
  attempt_number  int not null
  unsigned_tx_hash text not null unique  -- content-addressed, so a rebuild
                                          -- of the identical transfer (C5.4's
                                          -- determinism) collides here
                                          -- rather than silently duplicating
  signed_tx_hash  text null
  broadcast_at    timestamptz null
  tron_txid       text null
  status          text not null    -- BUILT, SIGNED, BROADCAST, CONFIRMED, FAILED
  created_at      timestamptz not null default now()

BUILD internal/dispatch (extend)
  Broadcast(ctx, attempt Attempt, signer SigningService, chain BroadcastClient) error
    - INSERT the dispatch_attempts row FIRST (BUILT), before calling Sign --
      the unique constraint on unsigned_tx_hash is what makes a retried
      Broadcast call for the identical transfer a no-op read of the existing
      row rather than a second signature/broadcast.
    - A row already at BROADCAST or later: return its existing tron_txid,
      never re-sign, never re-broadcast.
    - A row stuck at SIGNED (crashed between sign and broadcast): re-attempt
      the broadcast with the SAME signed_tx_hash -- never re-sign, since a
      second signature over the same payload is a second valid transaction
      from TRON's perspective if the first broadcast actually landed
      despite C5 not observing the response.

ACCEPTANCE
- Two concurrent Broadcast calls for the identical logical transfer (same
  order, same attempt): exactly one BROADCAST row, one real chain call --
  same concurrency-proof discipline as C1.3's 1,000-goroutine idempotency
  test, adapted to this layer.
- A crash simulated between SIGNED and the broadcast call, then a resumed
  Broadcast: re-broadcasts the existing signed_tx_hash, never calls Sign
  again -- verify by asserting the fake SigningService's call count.
- FakeSigningService's forced-duplicate-signature mode (from C5.0) is used
  here to prove the SYSTEM still behaves correctly even if signing itself
  were to misbehave, not just that C5's own retry logic is careful.
```

---

## C5.6 — SR-finality confirmation and settlement

```
BUILD internal/dispatch (extend)
  ConfirmFinality(ctx, attempt Attempt, chain FinalityReader) (bool, error)
    - Polls for TRON Super-Representative finality on tron_txid -- a
      read-only chain query, no different in spirit from C2's own
      watch-only relationship to BSC or C4.3's VerifyOnChain.
    - On finality: marks the attempt CONFIRMED, then calls C1's
      dispatching -> settled transition (§A) with the E3 entry.

ACCEPTANCE
- Against a fake FinalityReader: an attempt reaching finality correctly
  transitions to `settled` in a real C1 instance (testcontainers), E3
  lands with the correct amount_out, and dispatch_attempts is marked
  CONFIRMED.
- Replaying ConfirmFinality for an already-settled attempt hits C1's
  idempotency path on the transition call -- no duplicate E3.
- A tron_txid that never reaches finality within a generous configured
  ceiling is alerted on, not silently polled forever -- same posture as
  C4.1's price-staleness ceiling and C2.5's stale-pending-finality alert.
```

---

## C5.7 — Non-retryable failure handling (the atomicity gap from "Read this third")

```
Builds the dispatching -> held path, against BOTH the interim two-call
sequence (today's real C1) and the proposed atomic endpoint (once it ships),
behind one interface so the swap is a config change.

BUILD internal/ledgerclient (extend)
  type DispatchFailureReporter interface {
    ReportDispatchFailure(ctx, order Order, conversionEntryKey, reason string) error
  }

  twoCallReporter -- today's real behavior:
    1. GET /entries?idempotency_key=<conversionEntryKey> to check for an
       EXISTING reversal first (this endpoint IS idempotent to call
       repeatedly, unlike the reversal endpoint itself) -- if one already
       exists (a retried ReportDispatchFailure after a prior partial
       success), skip straight to step 3 with its id.
    2. POST /entries/{id}/reversal. This call is NOT idempotent (per §A) --
       call it AT MOST ONCE per logical failure, guarded by step 1's check.
    3. POST /orders/{external_id}/transitions {to_state: "held", entry_id:
       <the reversal's id>}.
    - If the process crashes between steps 2 and 3: the NEXT call to
      ReportDispatchFailure (triggered by a reconciliation job, not a human)
      re-runs step 1, finds the existing reversal, and completes step 3 --
      this is the tested recovery path invariant 4 requires, standing in
      for real atomicity until the endpoint exists.

  atomicReporter -- once "Read this third"'s endpoint ships: one call,
    POST /orders/{external_id}/dispatch-failure. Trivial, and this is the
    version that should actually run in production once it's available.

A RECONCILIATION JOB (ticker, e.g. every few minutes): scans for orders
  in `dispatching` whose most recent dispatch_attempts row is FAILED and
  whose conversion entry has no corresponding order_transitions row moving
  them to `held` -- these are exactly the crashed-between-steps-2-and-3
  orders, and this job is what actually closes them, not a hope that
  ReportDispatchFailure gets called again on its own.

ACCEPTANCE
- twoCallReporter against a real C1 instance: a clean failure reverses the
  conversion and transitions to `held`, order_transitions shows the
  reversal entry as the transition's cause.
- Simulated crash between step 2 and step 3 (kill the process, real SIGKILL
  in the integration test): the reconciliation job detects the order,
  completes step 3, and the order reaches `held` -- without ever calling
  the reversal endpoint a second time (assert this by call count against
  a real C1, since a second call would itself 409, but the test should
  prove the code never even tries).
- atomicReporter (built against a stub until the real endpoint exists)
  produces the identical end state as twoCallReporter's happy path, so the
  swap is provably behavior-preserving.
```

---

## C5.8 — Sweep tier: batched multisend

```
Implements §B's batching model and the C1.9-mirrored "40 of 50 settle, 10
fail" scenario.

BUILD internal/txbuild (extend)
  BuildMultisend(slotAddress string, recipients []Recipient) (unsignedTx []byte, err error)
    - Recipients capped per transaction per whatever the chosen TRON
      multisend mechanism actually allows -- verify the real limit, do not
      assume an arbitrary batch size is broadcastable in one transaction.

BUILD internal/dispatch (extend)
  AccumulateForBatch(ctx, order Order) error
    - Adds a screened Sweep-tier order to the current open batch window
      (config: max wait, max batch size -- whichever triggers first cuts
      the batch).
  CutBatch(ctx, window BatchWindow) ([]Attempt, error)
    - One BuildMultisend covering every order in the window, ONE energy
      reservation from C4 sized for the whole batch (the aggregate is what
      decision 6's "batched multisend halves per-recipient energy" claim
      is actually about -- do not reserve energy per-recipient for a Sweep
      batch, that would silently forfeit the entire margin advantage this
      tier exists for).
  HandlePartialSettlement(ctx, batchTxid string, perRecipientResults map[string]bool) error
    - Recipients that succeeded: their own dispatching -> settled transition,
      each with its own E3 entry (their own amount_out, not a shared one).
    - Recipients that failed: NOT retried inside this batch or this
      broadcast (invariant 1). Re-enqueued into the NEXT batch window as a
      fresh attempt, with the batch-level failure logged distinctly from a
      per-recipient one so an operator can tell "this batch had 10 stragglers"
      from "this one order has failed 10 times."

ACCEPTANCE
- BuildMultisend against a real recipient-count limit: rejects an
  over-limit batch before construction, doesn't silently truncate it.
- A simulated 40/50 partial settlement (mirroring C1.9's own 2% scenario):
  the 40 correctly reach `settled` with correct individual E3 entries, the
  10 are re-enqueued, and the batch as a whole is never itself represented
  as a single order-like entity anywhere in C1 (C1 only ever sees individual
  orders transition, per order -- the batch is purely a C5-internal
  broadcast-efficiency concept, invisible to the ledger).
- One energy reservation is made per batch, sized for the batch total, not
  per recipient -- assert this by call count against the fake C4 client.
```

---

## C5.9 — Slot freeze handling (pre-settlement, recoverable)

```
The scenario catalog's own distinction: "A slot gets frozen (Tether action)
mid-flight, between selection and confirmation -- different from the freeze
scenario in Part 3 in that this one is caught pre-settlement and should be
recoverable without a loss entry." Part 3's version (an already-settled
slot getting frozen later) is a terminal, insurable risk with no engineering
fix -- this chunk is NOT that. This is: the freeze happens after C5 selected
the slot but before the payout it just sent from that slot reaches finality.

BUILD internal/slots (extend)
  DetectFreeze(ctx, slot Slot, chain BalanceReader) (bool, error)
    - A frozen slot's balance becomes unreachable/frozen at the chain level
      in a way distinguishable from a normal balance read -- this chunk
      defines the detection, which depends on how TRON actually surfaces a
      Tether freeze at the RPC layer; confirm the real signal (an error
      code, a flag on the account query, etc.) rather than assuming one.
  HandleMidFlightFreeze(ctx, attempt Attempt) error
    - If the attempt hasn't broadcast yet: abandon it, mark the slot
      RETIRED (not just RETIRING -- a frozen slot is not coming back), and
      re-select a fresh slot for the same order, starting a NEW attempt
      (not a retry of the frozen one, since invariant 1's exactly-once is
      about one attempt, and this is legitimately a different attempt).
    - If the attempt already broadcast but hasn't confirmed: this is now
      genuinely ambiguous (did the frozen slot's payout land before the
      freeze or not) and must alert a human rather than guess -- do not
      auto-resolve this by assuming either outcome.

ACCEPTANCE
- A freeze detected before broadcast: order gets a fresh attempt on a
  different slot, no loss entry, no halt -- matches the scenario catalog's
  own "should be recoverable without a loss entry" framing.
- A freeze detected after broadcast, before confirmation: an alert fires,
  and NO automatic C1 transition is attempted in either direction --
  test this by asserting zero transition calls, the same verification
  discipline C3.7's re-screen chunk used for its own "don't guess" case.
```

---

## C5.10 — HTTP boundary

```
Expose C5 as a service, mirroring the shape and discipline of every prior
component's own boundary.

ENDPOINTS (all under /v1)
  POST /dispatch                    body: external_id -- explicit trigger
                                     (C6, once it exists, or an ops tool,
                                     calls this once an order is screened;
                                     C5 does not poll C1 for screened orders
                                     on its own initiative, matching the
                                     same push-not-poll reasoning C4's own
                                     reservation contract already settled on)
  GET  /dispatch/{order_id}         current attempt status
  GET  /slots                       current status/balance/tx_count per slot
  POST /slots/{id}/retire           manual override
  GET  /system/invariants           open dispatching-without-held-or-settled
                                     orders (the C5.7 reconciliation job's
                                     own findings, surfaced), batch queue
                                     depth, slot cap headroom
  GET  /healthz  GET /readyz  GET /metrics

ACCEPTANCE
- OpenAPI spec + route-presence test, same requirement as every prior
  component.
- Metrics: dispatches_total by {settled, held, failed}, broadcast_attempts_total,
  duplicate_broadcast_prevented_total (invariant 1's own proof surfaced live),
  slot_balance gauge per slot, batch_queue_depth gauge.
```

---

## C5.11 — Replay harness (the ship gate)

```
Like every prior component's own gate, but this one needs the most from its
fakes, since NOTHING here can touch a real chain (no S1, and this document
does not assume a reachable TRON node any more than C4's own did).

cmd/replay/main.go, seeded, deterministic, prints the seed on failure.

SCENARIO MIX (mirrors c1-scenario-catalog.md's own C5 rows and Part 2's C5
section, plus C1.9's own 2%/1.5% Sweep-batch and dispatch-failure buckets --
if you add a scenario here, add the matching row there too)
  Clean Direct/Standard dispatch, majority
  Clean Sweep batch, no partial failure
  Sweep batch with 40/50 partial settlement
  Energy reservation FAILED before any C1 commitment -- zero ledger impact
  Energy reservation timeout (client-side, distinct from FAILED)
  Non-retryable dispatch failure -> held, via the two-call path, including
    a simulated crash between the two calls, caught by reconciliation
  Duplicate broadcast attempt (retry storm) -> exactly one real broadcast
  Slot freeze pre-broadcast -> re-attempt on a fresh slot, no loss
  Slot freeze post-broadcast pre-confirmation -> alert, zero automatic
    transitions
  All six slots over cap -> ErrNoEligibleSlot, alerted, zero attempts
  A FakeSigningService duplicate-signature injection -> system still
    broadcasts exactly once (C5.5's own guarantee, exercised end-to-end)

FINAL ASSERTIONS
1. Every dispatched order reaches exactly one of `settled` or `held` --
   never left in `dispatching` with no open attempt and no reconciliation
   finding it (the C5.7 job's own coverage, checked at the end of the run,
   not just during it).
2. Every dispatch_attempts row's unsigned_tx_hash is unique, and no order
   has two BROADCAST-or-later rows for the same logical transfer.
3. Every `settled` order has exactly one E3 entry, amount matching the
   order's own amount_out exactly.
4. Every `held`-via-dispatch-failure order has exactly one reversal of its
   own E2, never zero, never two.
5. Sweep batches never show a per-recipient energy reservation -- every
   batch's reservation count is exactly one, sized for the batch total.
6. Zero deadlocks, zero panics, zero unexplained errors.

PERFORMANCE TARGET: same framing as every prior harness -- real volume here
is ~100 payouts/day; this is about correctness under adversarial timing and
failure injection, not throughput.

CANNOT BE PART OF THIS GATE, and should not be treated as this component's
fault for being missing: end-to-end proof against a real TRON node, a real
SigningService, and a real C4 HTTP boundary. Those require S1 to exist and
C4.8 to ship. This harness proves C5's OWN logic is correct against faithful
fakes of both -- it is not, and cannot yet be, proof the assembled system
works against mainnet.
```

---

## Suggested sequencing

| Chunk | Days | Notes |
|---|---|---|
| C5.0 Scaffold + signing interface + slots | 1.0 | — |
| C5.1 Slot selection under caps | 1.0 | — |
| C5.2 Energy reservation integration | 1.0 | Written against C4's real Go contract; swap-in once C4.8 ships is config-only |
| C5.3 Entering dispatching | 1.0 | The commitment point — everything before is free to abandon |
| C5.4 Transaction construction | 1.0 | Verify the current TRON tx-construction library choice before starting |
| C5.5 Broadcast + exactly-once | 1.5 | The crux chunk — do not compress this |
| C5.6 Finality + settlement | 1.0 | — |
| C5.7 Dispatch-failure handling | 1.5 | Build against the interim two-call path AND the proposed atomic endpoint |
| C5.8 Sweep batching | 1.5 | The tier decision 6 calls the margin's highest-leverage lever — worth the extra time |
| C5.9 Slot freeze handling | 0.5 | Confirm TRON's actual freeze-detection signal before coding |
| C5.10 HTTP boundary | 0.5 | Can run in parallel with C5.8/C5.9 |
| C5.11 Replay harness | 1.5 | The ship gate — see its own caveat on what it cannot prove |

≈13 days against the 2.0 eng-week estimate in `component-map.md` — the largest single component so far, consistent with it carrying "High (money out)" risk and being the one everything else in the dependency graph converges on.

**Before starting C5.0:** this is the one point in the whole project where "start building against a stub and swap in the real thing later" (this project's own recurring pattern, going back to C2.6's blocked reorg call) stops being a scheduling convenience and becomes the only option — S1 has no owner, no timeline, and no spec yet. Whoever is sequencing the actual roadmap should treat "who builds S1, and when" as a decision at least as consequential as the wholesale-pricing calls C4 is still waiting on, not a detail that will sort itself out once C5's code is ready.

---

## Cross-reference

- C1's full build spec and the real, shipped HTTP surface this document verified directly (`ledger/internal/httpapi/`): `c1-ledger-build-prompts.md`
- C4's real reservation contract (`energybroker/internal/reservations/reservations.go`) this document is written against: `c4-energy-broker-build-prompts.md`
- The slot-cap architecture decision (segregated rotating slots vs. pooled treasury) this component implements: `product-operations-architecture.md`, decision 4
- Where these scenarios live in the corridor-wide risk view, including the not-yet-sized reorg and multi-provider-degradation terminal risks that compound with C5's own failure modes: `c1-scenario-catalog.md`, Part 2's C5 section and Part 3
- Component ownership boundaries this spec expands, and the S1 dependency this document proposes a contract ahead of: `component-map.md`
- Current build/repo status as of the last write-up (already stale relative to the actual git history checked while writing this document): `repository.md`
