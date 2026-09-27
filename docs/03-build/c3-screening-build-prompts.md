# C3 — Screening: sequenced build prompts

> **Background (history).** Written during the design and build phase and kept as a
> record. You don't need it to run or change today's product: start at
> [docs/README.md](../README.md).

**Target:** Go + PostgreSQL, matching C1's and C2's stack. **Consumer:** an AI coding agent (Claude Code or equivalent), same usage pattern as `c1-ledger-build-prompts.md` and `c2-deposit-watcher-build-prompts.md`.

**How to use this file.** Paste §0 once at the start of the session — standing context the agent must hold for every chunk. Then paste chunks C3.0 → C3.9 one at a time, in order. Do not move to the next chunk until the current chunk's acceptance criteria pass against a real running C1 instance and a real (or sandbox) screening vendor — not a mock of your own assumptions about either.

**Before you start:** read the four call-outs immediately below. Two are interface gaps this document cannot close by itself — one of them touches C1 *and* C2, both of which are already built. One is a business decision no engineering chunk can resolve. One is good news: unlike C2, C3 needs zero new C1 endpoints for the release action itself.

---

## Read this first — two interface gaps, one already touching shipped code

C1 is built (C1.0–C1.11) and C2 is built through C2.8 (`repository.md`, 5 Sep 2026). That means C3, like C2 before it, is being written against components that already exist rather than components still being designed — so its gaps are gaps in what's *already shipped*, not blank space to fill in freely.

**1. Nothing carries the deposit's sender address from C2 through C1 to C3 — and C3 cannot screen without it.**

Walk the actual schemas. C2.4's `ParseTransferLog` extracts `from` (the sender) on every Transfer log — C2 is the only component that ever observes it. C2.7's `ReportDepositFinal` call (§A of `c2-deposit-watcher-build-prompts.md`) sends `to_state`, `entry.lines`, `entry.metadata` — nowhere in that payload is the sender address, and C1.5's `orders` table (`c1-ledger-build-prompts.md`) has no column for it either: `external_id, customer_id, tier, state, amount_in, amount_out, fee_units, network_fee_units, recipient_address, quoted_at, quote_expires_at, version`. `recipient_address` is the TRON *payout* address, not the BSC *deposit* sender — a different address on a different chain, serving the opposite leg of the order.

Component-map's C3 dimension says C3 "owns... result caching by sender address" — but as things stand today, the sender address is observed once, by C2, at deposit time, and then **is discarded**. C3 has no chain access of its own (deliberately, per its scope) and cannot re-derive it.

This needs a small, contained extension to two already-built components, the same posture as C1.11's addition after C2 was underway:

- **C1**: add a nullable `sender_address` column to `orders`, populated exactly once, on the `quoted → funded` transition, immutable after. Expose it on `GET /v1/orders/{external_id}`. This is additive — no existing caller breaks.
- **C2**: C2.7's transition request needs one more field alongside `entry` — `sender_address` — populated from the same Transfer log C2.4 already parsed. This is a payload addition to an endpoint C2 already calls, not new scope.

Until both land, build C3 against a `SenderAddressLookup` interface (C3.0) with a stub/fake implementation, exactly the posture C2.6 took with the reorg endpoint it was blocked on. Do not invent an alternate path (e.g., re-deriving the sender from a fresh RPC call inside C3) — that would silently re-create C2's chain-access responsibility inside a component whose entire scope is "no chain access."

**2. Nothing tells C3 when an order becomes eligible for screening.**

C1.8's HTTP surface has no `GET /orders?state=funded` — only `GET /orders/{external_id}`, one at a time, by an id the caller must already know. C3 needs to discover *which* external_ids just entered `funded`, and nothing currently produces that stream. There is no event bus in this system by design (webhooks are C6's job, per decision 5 — not C1's, not a general pub/sub).

Two ways to close it, and this document picks one:

- **(a) C1 gains a polling endpoint** — `GET /v1/orders?state=funded&updated_after=<cursor>` — and C3 polls it on an interval, the same posture C2's own doc used for order discovery ("a real cost, a placeholder for a proper event mechanism once C6 exists, not a permanent design").
- **(b) C2 pushes directly to C3** the moment its own `funded` transition succeeds, bundling the sender-address gap and the discovery gap into one call.

This document builds **(a)**, polling against a new C1 endpoint, not (b). Reason: C2 is already shipped and tested through C2.8; wiring a new outbound call to a specific downstream service (C3) inside C2 makes C2 responsible for knowing C3 exists, which the dependency graph in `component-map.md` never asked it to be — C2 is "deposit watcher," full stop. C1 gaining a generic list-by-state endpoint is a small, contained, reusable addition (ops tooling and C6 will both eventually want it too) versus a point-to-point coupling between two specific services. If (a) turns out to be too slow once real volume exists, revisit — at ~4 deposits/hour peak (component-map.md), a 5–10s poll interval is not a real cost yet.

**Neither gap blocks starting.** Build C3.0–C3.2 now against stub interfaces for both; C3.3 is the chunk that goes live once C1's two additions ship.

---

## Read this second — good news: the release action needs no new C1 endpoint

Unlike C2 (which needed a whole new reorg endpoint from C1), every transition C3 will ever call already exists in C1.5's transition table and is already reachable through C1.8's generic `POST /v1/orders/{external_id}/transitions`:

```
funded    -> screened   no entry, not halt-blocked   screening verdict pass
funded    -> held       no entry, not halt-blocked   screening verdict hold
held      -> screened   no entry, not halt-blocked   manual release, actor required
held      -> refunded   YES entry, halt-blocked       manual reject
```

C3 is a pure *caller* of an interface that is already frozen and already tested (C1.9's replay mix already includes 5% "screening hold, then release" and 2% "screening hold, then reject and refund"). This is the one part of C3's build with essentially zero C1-side risk.

One open question this document flags rather than guesses past: C1.5's table notes `held -> screened` "requires... actor" as a callout distinct from every other transition, which only makes sense if `TransitionParams` carries an explicit, caller-supplied `actor` string (mirroring `journal.EntryRequest`'s own caller-supplied `Actor` field in C1.2) rather than actor always being derived from the calling service's bearer-token identity as C1.8's auth section states in general. This document assumes the explicit-field reading — C3 sets `actor` to the human reviewer's identity on a manual release/reject, and to a fixed system value (e.g. `"screening-svc"`) on an automated verdict — but **verify this against C1's actual `TransitionParams` struct before C3.6 ships**, the same way C2.4 was told to verify the USDT contract address against BscScan rather than trust the spec. If actor turns out to be bearer-derived only, C3.6's manual-review audit trail needs a different design (e.g. a `reviewed_by` field in C3's own hold-resolution record, separate from what C1 logs).

**Also out of scope, flagged and left alone:** `held -> refunded` requires a journal entry, but *physically returning* BEP20 to the sender's wallet is an outbound chain transaction that nothing in this system currently owns (the same shape of gap as C2's spec calling out that nobody owns sweeping deposits). C3 records the *decision* to reject and refund; it does not construct, sign, or broadcast anything. Whoever executes the physical refund is an open question for S1/ops, not for this document.

---

## Read this third — one business decision no chunk below can make for you

`c1-scenario-catalog.md` names this outright: *"Vendor (Chainalysis/TRM/Elliptic) outage or timeout at the moment a decision is needed — does the order sit in `funded` indefinitely, or is there a hold-and-retry policy? Not yet specified."*

This is not an engineering gap, it's a risk-appetite decision with real money on both sides:

- **Fail-closed** (timeout → `held`, routed to manual review): never releases funds without a verdict, but turns every vendor blip into a human queue item and a slower customer experience — at 100 calls/day, a bad vendor hour could hold a real fraction of a day's volume.
- **Fail-open** (timeout → `screened` after N retries, logged as `verdict: unavailable`): keeps the tier SLAs intact through a vendor outage, but means money can move on zero screening signal during exactly the window when a bad actor might be counting on it.

C3.5 below builds the mechanism for both, selectable by config, with **fail-closed as the shipped default** — consistent with C1's own posture everywhere else in this system (halt rather than guess, e.g. C1.7's reconciler). Do not ship fail-open as the default; that is a decision for whoever owns AML/compliance sign-off to make explicitly and in writing, not a default an engineering chunk should quietly pick.

---

## §0 — Standing context (paste once)

```
You are building C3, the screening service of a USDT cross-network settlement
system (BEP20 -> TRC20 payouts). C1 (ledger core) and C2 (deposit watcher) are
already built. C3 sits between them and C5 (payout dispatcher): it decides
whether a funded deposit's provenance is clean enough to convert and pay out.

STACK
- Go 1.22+, PostgreSQL 16 — C3's own database, separate from C1's and C2's.
- pgx/v5, goose migrations, chi routing, log/slog, testify, testcontainers-go —
  same toolchain discipline as C1 and C2, for the same reasons.
- An HTTP client for calling C1's API, built against the exact contract in §A,
  including the two additions this document proposes to C1 (sender_address on
  orders, GET /orders?state=).
- A vendor-agnostic screening client interface. No specific vendor SDK is a hard
  dependency of the core package — see C3.0.

WHAT C3 IS
The release/hold decision on deposit provenance. It calls a screening provider
for every newly funded order's sender address, caches results by address,
classifies the verdict into pass/hold/reject with a reason code, calls C1 to
transition the order accordingly, and exposes a hold queue for manual review.

WHAT C3 IS NOT — do not build any of this, do not import libraries for it
- No chain access of any kind. C3 never talks to BSC or TRON, never derives or
  observes a sender address itself — it receives one, from C1, which received
  it from C2 (see the gap this document flags above).
- No payout, no energy, no pricing (C4, C5, C6's jobs).
- No transaction construction of any kind, including refund transactions. C3
  records the DECISION to reject and refund. It never moves money.
- No customer-facing auth. Same posture as C1 and C2: service-to-service only.
- No second source of truth about order state. C3's own database holds
  screening-specific state (verdicts, cache, hold queue) — never a balance,
  never something that could be mistaken for overriding what C1 says an order's
  state is.
If a chunk seems to require any of the above, you have misread it. Stop and say so.

NON-NEGOTIABLE INVARIANTS
1. C3 never releases an order on its own authority alone for a HOLD verdict.
   funded -> held is C3's call; held -> screened (release) requires a distinct,
   separately-authorized manual-review action, never an automatic re-verdict
   that flips a hold back to pass without a human actor in the transition.
2. A verdict, once cached for a sender address, has an explicit, configured
   expiry. It is never treated as permanent — see C3.1.
3. Every call C3 makes to C1 is idempotent, matching C1's own convention:
   key format "screening:<verdict>:<order_id>:<screening_result_id>".
4. C3 never invents a transition, entry shape, or account code C1 doesn't
   already define (same rule C2 followed). C3 calls existing transitions only —
   see "Read this second" above. It never calls POST /entries directly.
5. A vendor timeout or outage NEVER silently becomes a pass. The fail-closed
   default (see "Read this third") is encoded in config, not scattered through
   conditionals — one place decides what "no verdict available" means.
6. Every hold has a recorded, non-empty reason code. "held, no reason given" is
   not a legal state in C3's own database, even transiently.
7. A cached verdict is per (provider, sender_address) pair. Switching providers,
   or a provider revising its own model, must not silently reuse a stale verdict
   from a different provider under the same cache key.

STYLE
- Small packages: internal/provider (vendor abstraction), internal/cache
  (result caching), internal/verdict (classification + reason codes),
  internal/holds (queue + manual review), internal/ledgerclient (the C1 HTTP
  contract, isolated exactly as C2 isolated its own), internal/httpapi.
- Errors are typed, wrapped with %w, one stable error code per condition reaching
  the HTTP layer — same discipline as C1 and C2.
- Tests are the deliverable. A chunk touching the vendor call is not done until
  it's proven against a fake vendor capable of producing a timeout, a malformed
  response, and a flagged verdict — not just the happy path.
- Do not add features, endpoints, or abstractions a chunk did not ask for.
```

---

## §A — Interface contract with C1 (including the two proposed additions)

Everything here is derived from C1's own published contract (`c1-ledger-build-prompts.md` §A/C1.5/C1.8) plus the two additions flagged above. If anything here looks inconsistent with the C1 doc, the C1 doc wins and this one is wrong — same rule C2's spec used.

### Proposed C1 additions (build or confirm before C3.3)

```
ALTER TABLE orders ADD COLUMN sender_address text NULL;  -- set once, on quoted->funded

GET /v1/orders/{external_id}   -- extend response to include sender_address

GET /v1/orders?state=<state>&updated_after=<cursor>&limit=<n>
  -- paginated, ordered by updated_at, cursor is opaque (last row's updated_at+id)
```

C2's `POST /v1/orders/{external_id}/transitions` call for `quoted -> funded` gains one new top-level field, `sender_address`, sibling to `entry` — not inside `entry.lines` (it isn't a ledger amount) and not buried in `entry.metadata` (C3 needs to query it structurally, not parse a jsonb blob C1 makes no promise about the shape of).

### The two calls C3 makes to credit a verdict

```
POST /v1/orders/{external_id}/transitions
Idempotency-Key: screening:pass:<order_id>:<screening_result_id>

{
  "to_state": "screened",
  "expected_version": <order's current version, from the GET that surfaced it>,
  "reason": "screening_pass",
  "actor": "screening-svc"
}
```

```
POST /v1/orders/{external_id}/transitions
Idempotency-Key: screening:hold:<order_id>:<screening_result_id>

{
  "to_state": "held",
  "expected_version": <current version>,
  "reason": "screening_hold:<reason_code>",
  "actor": "screening-svc"
}
```

Neither requires an `entry` — C1.5's table marks both `funded -> screened` and `funded -> held` as not requiring one. Do not construct a journal entry for either; C1.8 will reject an entry on a transition that doesn't call for one exactly the way it rejects a JSON number amount — loudly, not silently ignored.

### The two calls the manual-review path makes (C3.6)

```
POST /v1/orders/{external_id}/transitions
Idempotency-Key: screening:release:<order_id>:<hold_id>

{ "to_state": "screened", "expected_version": <v>, "reason": "manual_release",
  "actor": "<reviewer identity>" }
```

```
POST /v1/orders/{external_id}/transitions
Idempotency-Key: screening:reject:<order_id>:<hold_id>

{ "to_state": "refunded", "expected_version": <v>, "reason": "manual_reject",
  "actor": "<reviewer identity>",
  "entry": { ... refund entry, shape TBD — see "out of scope" note above ... } }
```

The reject call's `entry` shape is the one piece of §A this document cannot fully specify, because nothing yet owns the physical refund. Build C3.6's reject path against a `RefundEntryBuilder` interface with the entry construction stubbed, the same posture as every other blocked piece in this document.

---

## §B — The verdict and outage-policy model (crux decision — read "Read this third" above first)

Three states a screening attempt can end in, and only three:

- **Pass** — provider returned a clean result within the timeout. `funded -> screened`.
- **Hold** — provider returned a flagged result, OR the fail-closed timeout policy fired, OR the result is ambiguous per the provider's own risk-score thresholds (config, not hardcoded — the specific vendor is not chosen yet, see below). `funded -> held`, with a reason code that distinguishes "vendor flagged this address" from "vendor unavailable" from "score in the ambiguous band" — these are different operational responses even though they land in the same C1 state.
- **Reject-on-review** — never an automatic C3 outcome. Only ever reached via C3.6's manual path, from `held`.

**Vendor abstraction, not vendor choice.** Component-map lists Chainalysis, TRM, and Elliptic as the candidate vendors and does not pick one — this is explicitly a "Low–med (vendor)" risk item in the same doc, meaning the choice matters less than the abstraction around it. Build:

```go
type Verdict struct {
    RiskScore    float64          // 0.0-1.0, provider-normalized
    Flagged      bool
    ReasonCodes  []string
    RawResponse  json.RawMessage  // stored verbatim for audit, never parsed downstream
    ProviderName string
    CheckedAt    time.Time
}

type ScreeningProvider interface {
    Screen(ctx context.Context, address string) (Verdict, error)
}
```

C3.0 ships a `MockProvider` (deterministic, seeded, can be configured to flag specific addresses, time out, or return malformed data) as the only implementation exercised by the acceptance harness (C3.9) — real-vendor integration is a separate, later swap-in behind the same interface, not a prerequisite for C3 shipping its own gate.

---

## C3.0 — Scaffold and the provider interface

```
Build the repository skeleton and the vendor-agnostic screening interface.
Nothing else.

1. Go module `screening`. Layout mirrors C1 and C2:
   cmd/screend/main.go
   internal/provider/
   internal/db/
   migrations/
   docker-compose.yml (Postgres 16 only)
   Makefile (build, test, test-integration, migrate-up, migrate-down, lint)

2. internal/provider:
   - The ScreeningProvider interface and Verdict struct from §B.
   - MockProvider: deterministic on a seeded PRNG; supports a test-only
     configuration to force Flagged=true for specific addresses, force a
     context-deadline timeout, and force a malformed/empty response.
   - A SenderAddressLookup interface (stub for the C1 gap):
       GetSenderAddress(ctx, externalID string) (string, error)
     Real implementation added in C3.3 once C1's addition ships; C3.0-C3.2 use
     a fake that returns a configured address per external_id.

3. internal/db: pgx pool setup, config from env, Tx helper — same pattern as
   C1 and C2.

4. cmd/screend: starts, connects, serves GET /healthz, shuts down on SIGTERM.

ACCEPTANCE
- `make test` green.
- MockProvider: same address always yields the same verdict (deterministic);
  the forced-timeout config actually respects ctx cancellation rather than
  sleeping past it; the forced-malformed config returns a typed parse error,
  never a zero-value Verdict masquerading as a clean pass.
- No file outside internal/provider imports a specific vendor's SDK — enforced
  the same way C2.0 enforced no private-key-capable import in its own
  dependency-graph check.

DO NOT
- Do not integrate a real vendor SDK yet, even behind a flag. This chunk is
  interface-only.
- Do not add caching yet — that's C3.1, and doing it here will tangle the two
  concerns.
```

---

## C3.1 — Result cache by sender address

```
Build the cache. No verdict classification yet — this chunk stores and expires
raw provider results.

TABLE screening_results
  id              bigserial primary key
  provider_name   text not null
  sender_address  text not null
  risk_score      double precision not null
  flagged         boolean not null
  reason_codes    text[] not null default '{}'
  raw_response    jsonb not null
  checked_at      timestamptz not null
  expires_at      timestamptz not null      -- checked_at + configured TTL
  created_at      timestamptz not null default now()

  -- no unique constraint on (provider_name, sender_address) alone: a fresh
  -- check after expiry is a NEW row, not an update. History is kept, never
  -- overwritten — same append-only instinct as C1's journal, for the same
  -- audit reason.

BUILD internal/cache
  Get(ctx, provider, address) (*Verdict, error)  -- nil if no unexpired row
  Put(ctx, provider, address, Verdict, ttl) error
  Invalidate(ctx, provider, address, reason, actor) error
    -- explicit invalidation path for a manual override (invariant 7's flip
    -- side: an operator who has reason to distrust a cached pass can force a
    -- re-check without waiting for natural expiry). Recorded, never silent.

RULES
- TTL is configurable per verdict outcome, not a single global constant — a
  flagged result and a clean result do not need the same cache lifetime, and
  this document does not pick specific durations (that's a product/compliance
  call, same flavor as the outage-policy decision, just lower stakes). Ship a
  conservative default (e.g. 24h clean / 7d flagged) and make it loud in config
  that these are placeholders pending a real decision.
- A cache hit is still logged with which cached result it reused and for which
  order — the scenario catalog's own worry ("cache-by-sender false
  positive/negative... one wrong verdict can propagate across multiple orders")
  is only debuggable if every reuse is traceable back to its origin.

ACCEPTANCE
- Get on an unexpired row returns it; on an expired row returns nil (not the
  stale row) and does not delete it (history is kept).
- Put never overwrites a prior row for the same (provider, address) — always a
  new insert. A time-ordered query for one address shows its full verdict
  history.
- Invalidate marks a row (or the concept of "this cache key") as no longer
  trusted without deleting audit history, and the very next Get for that key
  returns nil even though expires_at hasn't naturally passed.
- 100 concurrent Get calls for the same address while a Put for that address is
  in flight: no partial/corrupt read, no deadlock.
```

---

## C3.2 — Verdict classification and reason codes

```
Turn a raw provider Verdict into one of Pass / Hold / RejectPending, with a
reason code — the thing C3.4 actually reports to C1.

BUILD internal/verdict
  type Classification int
  const (
    Pass Classification = iota
    Hold
  )
  // Reject is never produced here — see §B, it's manual-only.

  type Decision struct {
    Classification Classification
    ReasonCode     string   // stable, documented strings — see below
    ScreeningResultID int64 // FK to screening_results, or 0 for a
                             // provider-unavailable decision with no result row
  }

  Classify(v provider.Verdict, thresholds Thresholds) Decision

REASON CODES — stable strings, documented in docs/reason-codes.md, one per
condition, never reused (same discipline as C1's error codes):
  screening_pass
  screening_hold_flagged        -- provider explicitly flagged the address
  screening_hold_ambiguous      -- risk score in the configured grey band
  screening_hold_unavailable    -- provider timeout/outage, fail-closed fired
  screening_hold_stale_cache_invalidated  -- an operator invalidated a prior
                                             pass and the order is re-held
                                             pending re-screen (see C3.7)

RULES
- Thresholds (the risk-score cutoffs between pass/ambiguous/flagged) are
  config, not a hardcoded constant — they are the one part of this chunk most
  likely to need tuning against a real vendor's actual score distribution,
  which nobody has seen yet since no vendor is chosen (§B).
- Classify is a pure function: given the same Verdict and Thresholds, always
  the same Decision. All the impure parts (calling the provider, hitting the
  cache, calling C1) live in other packages that call this one.

ACCEPTANCE
- Table-driven test over the threshold boundaries: a score exactly at a
  boundary is deterministic and documented which side it falls on (do not
  leave a boundary's behavior to floating-point luck).
- A Flagged=true verdict always classifies Hold regardless of score, even a
  score of 0.0 — an explicit flag from the vendor overrides the numeric band.
- Classify never returns Reject. A test asserts the return type structurally
  cannot represent it (or, if Go's type system makes that awkward, asserts by
  exhaustive enumeration that no code path produces it).
```

---

## C3.3 — Discovery (blocked on the two C1 additions)

```
This chunk cannot be fully completed until C1 ships the sender_address column
and the GET /orders?state= endpoint from §A. Build everything up to the real
HTTP calls now, behind the interfaces from C3.0; swap the fakes for real
clients once C1's additions exist.

BUILD internal/ledgerclient (new package, mirrors C2's own)
  PollFundedOrders(ctx, cursor) ([]OrderRef, newCursor, error)
    -- GET /v1/orders?state=funded&updated_after=<cursor>
  GetSenderAddress(ctx, externalID) (string, error)
    -- part of GET /v1/orders/{external_id} once extended

POLLING LOOP
  RunDiscoveryLoop(ctx, interval time.Duration)
    - Each tick: PollFundedOrders from the last saved cursor, for each new
      order not already in C3's own `screening_queue` table, enqueue it.
    - Idempotent and resumable, same posture as C2.3's block-ingestion loop:
      a restart resumes from the saved cursor, never re-enqueues a
      already-processed order twice into a way that double-screens it (a
      duplicate enqueue is fine as long as C3.4's idempotency key collapses it
      to one C1 call — enqueue dedup is a nice-to-have, not the correctness
      boundary).

TABLE screening_queue
  order_id        bigint primary key
  external_id     text not null unique
  sender_address  text null           -- null until GetSenderAddress resolves
  enqueued_at     timestamptz not null default now()
  status          text not null       -- PENDING, SCREENING, DONE
  updated_at      timestamptz not null default now()

ACCEPTANCE (of the parts that don't depend on the missing C1 endpoints)
- PollFundedOrders against a fake C1 client: cursor advances correctly, no
  order is fetched twice across a restart.
- An order whose sender_address lookup fails (fake returns an error) is
  retried on the next tick, not dropped, not enqueued with a permanently null
  address.
- Once C1's additions exist: a live integration test against a real C1
  instance confirms a genuinely funded order appears within one poll interval
  and its sender_address round-trips correctly from what C2 originally
  observed.
```

---

## C3.4 — Emission to C1

```
The chunk that actually calls the screening pipeline end to end and reports
the automatic verdicts (pass/hold) to C1.

BUILD internal/ledgerclient (extend)
  ReportVerdict(ctx, order OrderRef, decision verdict.Decision) error
    - Pass -> POST .../transitions {to_state: "screened", reason:
      "screening_pass", actor: "screening-svc"}
    - Hold -> POST .../transitions {to_state: "held", reason:
      "screening_hold:<reason_code>", actor: "screening-svc"}
    - Idempotency-Key per §A's format.
    - On 409 illegal_transition: the order left `funded` before C3 got to it
      (a legitimate race — e.g. a customer cancel landed first). Log and mark
      the queue row DONE without retrying; this is not an error in C3.
    - On 409 version_conflict: re-fetch current version, retry once.
    - On 423 system_halted: back off, retry on a longer interval — same
      posture as C2.7. Note funded->held and funded->screened are NOT
      halt-blocked per C1.5's table, so a 423 here would be surprising; if it
      ever happens, treat it as a P1 alert, not routine backoff (this differs
      from C2.7's identical-looking case, where system_halted IS expected for
      some of its calls — read the transition table, don't copy C2.7's handler
      verbatim).

PIPELINE (ties C3.1-C3.4 together)
  For each PENDING queue row with a resolved sender_address:
    1. cache.Get(provider, address) — if an unexpired hit exists, skip the
       vendor call entirely (this is the whole point of caching by sender).
    2. Otherwise, provider.Screen(ctx, address) with the configured timeout.
       On success: cache.Put, then verdict.Classify.
       On timeout/error: verdict.Classify still runs, but with a synthetic
       Verdict representing "unavailable" (see C3.5) rather than skipping
       classification.
    3. ledgerclient.ReportVerdict with the classified Decision.
    4. Mark the queue row DONE.

ACCEPTANCE
- Against a real running C1 instance (testcontainers, same pattern as C1 and
  C2 use): an order enqueued via C3.3 ends up in `screened` or `held`
  correctly, matching the MockProvider's configured verdict for its address.
- A cache hit from C3.1 results in zero calls to the provider for the second
  order from the same sender — verified by asserting the mock provider's call
  count, not just the outcome.
- Replaying the same queue row (simulating a C3 restart mid-pipeline) hits
  C1's idempotency path and does not attempt a second, conflicting transition.
- illegal_transition is handled without retry-storming; system_halted is
  retried on backoff and eventually succeeds once the test harness clears it.
```

---

## C3.5 — Vendor outage and timeout policy

```
Builds the mechanism from "Read this third" above. Ships with fail-closed as
the default, configurable to fail-open only via an explicit, logged config
flag — never a silent code-level default.

BUILD internal/provider (extend)
  type OutagePolicy int
  const (
    FailClosed OutagePolicy = iota  // default
    FailOpen
  )

  ScreenWithPolicy(ctx, provider ScreeningProvider, address string,
                    timeout time.Duration, retries int, policy OutagePolicy,
                    ) (Verdict, error)
    - Retries with backoff up to `retries` attempts, each bounded by `timeout`.
    - All attempts exhausted, FailClosed: returns a synthetic Verdict{Flagged:
      false, RiskScore: 0, ReasonCodes: ["provider_unavailable"]} tagged so
      C3.2's Classify produces screening_hold_unavailable, never
      screening_pass — the "unavailable" signal must never be classifiable as
      clean by accident of how the struct is populated.
    - All attempts exhausted, FailOpen: returns a synthetic Verdict tagged so
      Classify produces Pass, but the decision record and the C1 transition's
      `reason` field both explicitly say screening_pass_vendor_unavailable —
      never just screening_pass — so this path is grep-able and auditable
      after the fact, distinguishable from an actual clean vendor result.

CONFIG
- outage_policy: fail_closed | fail_open, default fail_closed. Logged loudly
  at startup, exactly like C1.7 logs a non-default TRX reconciliation
  tolerance — this is the same category of "someone made a deliberate
  risk-tradeoff choice, make it visible" decision.
- A metric (screening_vendor_unavailable_total) incremented on every exhausted-
  retries event regardless of policy, so an operator sees vendor degradation
  even when fail-open is masking it from the order pipeline's own behavior.

ACCEPTANCE
- Against a MockProvider forced to time out on every call: FailClosed policy
  produces screening_hold_unavailable for every affected order, never a pass.
- Same setup, FailOpen policy: produces screening_pass_vendor_unavailable, and
  the order does reach `screened` in C1 — but the reason string on the
  transition record makes the vendor-unavailable origin unambiguous to anyone
  auditing later.
- A provider that fails N-1 times then succeeds on the last retry: uses the
  real verdict from that success, not the outage path — outage handling only
  fires when every retry is exhausted.
- Switching outage_policy requires a config change and a restart, not a
  runtime toggle — this is deliberate friction on a decision that should not
  be flipped casually mid-incident.
```

---

## C3.6 — Hold queue and manual review path

```
Exposes the held orders for a human to act on, and performs the C1 calls on
their behalf per §A's "release/reject" calls.

TABLE holds
  id              bigserial primary key
  order_id        bigint not null
  external_id     text not null
  reason_code     text not null       -- from C3.2's stable list
  screening_result_id bigint null     -- FK, null for *_unavailable holds
  opened_at       timestamptz not null default now()
  status          text not null       -- OPEN, RELEASED, REJECTED
  resolved_by     text null
  resolved_at     timestamptz null
  resolution_note text null

BUILD internal/holds
  Open(ctx, order OrderRef, decision verdict.Decision) (Hold, error)
    -- called from C3.4 whenever a Hold classification is reported to C1.
  Release(ctx, holdID, reviewer, note) error
    -- calls ledgerclient's held->screened transition with actor=reviewer.
  Reject(ctx, holdID, reviewer, note) error
    -- calls ledgerclient's held->refunded transition with actor=reviewer,
    -- entry from the stubbed RefundEntryBuilder (see §A's "out of scope" note
    -- — this call is expected to fail against a real C1 until a refund-entry
    -- owner exists; build and test it against a fake ledgerclient meanwhile).
  ListOpen(ctx) ([]Hold, error)

RULES
- Release and Reject are the ONLY paths that ever move an order out of `held`.
  Nothing in C3.4's automatic pipeline touches an order already in `held` —
  invariant 1 from §0, restated here because this is the chunk where it would
  be easiest to accidentally violate (e.g. "just re-run the pipeline on stale
  holds" is exactly the kind of shortcut that turns into a silent auto-release).
- A hold's `reviewer` identity is never inferred or defaulted — Release/Reject
  reject an empty reviewer argument before calling C1 at all, per the actor
  question flagged in "Read this second."

ACCEPTANCE
- Open creates exactly one holds row per hold-triggering verdict; calling it
  twice for the same order_id (simulating a retried C3.4 pipeline run) does
  not create a duplicate OPEN hold — idempotent on order_id while a hold is
  OPEN.
- Release against a real C1 instance (testcontainers): order moves held ->
  screened, C1's order_transitions row records the reviewer as actor (pending
  the verification flagged in "Read this second" — if C1 does NOT support a
  caller-supplied actor, this test is the one that discovers it, and this
  chunk is not done until that's resolved one way or the other).
- Reject is fully tested against a fake ledgerclient (real integration blocked
  on the refund-entry owner question, same as C2.6 was blocked on its endpoint).
- ListOpen never returns a RELEASED or REJECTED hold.
```

---

## C3.7 — Re-screen path (mechanism only, policy TBD)

```
Builds the mechanism for `c1-scenario-catalog.md`'s open item: "Verdict
changes between initial screen and eventual release — a sender address gets
flagged after an order already passed. No re-screen path exists yet." Same
posture as C2.8's orphaned-deposit handling: this chunk builds the capture-
and-surface mechanism, not the policy for what automatically happens next,
because that's a product/compliance decision, not an engineering one.

MECHANISM
- A scheduled job (interval configurable, default hourly — screening volume is
  ~100/day per component-map, this is cheap) re-checks the provider for every
  DISTINCT sender_address behind an order currently in `screened` or
  `dispatching` (i.e., already past C3's gate but not yet `settled`) using
  the same provider.Screen path as C3.4, bypassing the cache on purpose (the
  whole point is to notice a change the cache would hide).
- If the fresh verdict disagrees with the cached verdict that let the order
  through (a newly Flagged result where none existed before): do NOT
  automatically transition the order. There is no `screened -> held` or
  `dispatching -> held` pair in C1.5's transition table for this reason (the
  only dispatching->held path requires a reversal, for a dispatch failure, not
  a provenance re-flag) — inventing one would be exactly the kind of
  unilateral C1 change this document's own "Read this second" section
  celebrated NOT needing elsewhere. Instead: record a `rescreen_flags` row and
  alert loudly. A human decides what happens to an order already mid-flight.

TABLE rescreen_flags
  id              bigserial primary key
  order_id        bigint not null
  external_id     text not null
  order_state_at_detection text not null
  previous_verdict_id bigint not null references screening_results(id)
  new_verdict_id  bigint not null references screening_results(id)
  detected_at     timestamptz not null default now()
  resolution      text null
  resolved_at     timestamptz null

ACCEPTANCE
- An order whose sender re-screens clean (no change) produces no
  rescreen_flags row — this job is silent on the non-event, same as C2.5's
  silent-on-routine-pre-final-reorg behavior.
- An order whose sender re-screens flagged after passing produces exactly one
  rescreen_flags row, an alert fires, and — critically — no call to C1 is
  made that changes the order's state. Verify this last part explicitly: the
  test asserts zero transition calls, not just that the "right" one wasn't
  made.
- The job never re-screens an order already `settled`, `refunded`, or
  `expired` — those are terminal, and a post-hoc flag on a terminal order is
  a different (and currently entirely unspecified) problem this chunk does
  not attempt to solve.
```

---

## C3.8 — HTTP boundary

```
Expose C3 as a service, mirroring C1.8's and C2.9's shape and discipline.

ENDPOINTS (all under /v1)
  GET  /holds                    ?status=open filter
  POST /holds/{id}/release       body: reviewer, note
  POST /holds/{id}/reject        body: reviewer, note
  GET  /screening-results        ?sender_address= filter, for audit lookups
  POST /screening-results/{id}/invalidate   body: reason, actor
  GET  /rescreen-flags           ?resolved=false filter
  POST /rescreen-flags/{id}/resolve         body: resolution, actor
  GET  /system/queue              queue depth, oldest pending age, by status
  GET  /healthz  GET /readyz  GET /metrics

AUTH
- Service-to-service bearer token, same posture as C1.8/C2.9. The
  release/reject endpoints are the ones a human-facing ops tool calls through,
  so that tool's own auth is what establishes the reviewer identity C3 passes
  through to C1 — C3 itself does not authenticate individual humans.

ACCEPTANCE
- OpenAPI spec generated/maintained, same requirement as C1.8/C2.9, with a
  test that every route is present in it.
- Metrics exposed: verdicts_total by classification, holds_opened_total,
  holds_released_total, holds_rejected_total, rescreen_flags_total,
  screening_vendor_unavailable_total (from C3.5), queue_depth gauge,
  queue_oldest_pending_age_seconds gauge.
```

---

## C3.9 — Replay harness against a fake vendor and a real C1

```
C3's acceptance gate. Unlike C2 (which needed a controllable forked chain),
C3's hard dependency is a controllable vendor, which MockProvider already is —
this harness needs a real running C1 (testcontainers, same as C1.9 and C2.10
each used) but not a real screening vendor.

cmd/replay/main.go, runnable as a Go test in CI. Seeded, deterministic, prints
the seed on failure — same posture as C1.9 and C2.10.

SCENARIO MIX (mirrors c1-scenario-catalog.md's own C3 rows and Part 2's C3
section directly — if you add a scenario here, add the matching row there too)
  Clean pass, first screen                                    majority
  Flagged on first screen -> held -> manually released
  Flagged on first screen -> held -> manually rejected/refunded
  Cache hit: second order from an already-screened sender skips the vendor call
  Vendor timeout under FailClosed -> held with screening_hold_unavailable
  Vendor timeout under FailOpen -> screened with
    screening_pass_vendor_unavailable, reason string auditable
  Ambiguous risk-score band -> held with screening_hold_ambiguous
  Re-screen: sender flagged after already passing and reaching `dispatching`
    -> rescreen_flags row, zero C1 transition calls
  Illegal-transition race: order leaves `funded` (e.g. customer cancel) between
    enqueue and C3.4's call -> handled without retry storm, queue row DONE
  Manual cache invalidation followed by a fresh screen for the same address

FINAL ASSERTIONS
1. Every order that reached a terminal state in C1 has exactly one
   corresponding DONE queue row or resolved hold in C3 — no order is left
   permanently PENDING or OPEN with nothing further happening to it.
2. No FailClosed-policy order ever reaches `screened` via the automatic
   pipeline with a screening_hold_unavailable-flavored verdict — those two
   must never co-occur; if they do, the test fails loudly rather than the
   harness quietly averaging it away.
3. Every cache hit is traceable: for each order that skipped a vendor call,
   the screening_results row it reused is identifiable and belongs to the
   same sender_address.
4. Zero automatic C1 transition calls originate from C3.7's re-screen job —
   assert this by call-count on the fake/real ledgerclient, not by outcome.
5. Every holds row that reaches RELEASED or REJECTED has a non-empty reviewer.
6. Zero deadlocks, zero panics, zero unexplained errors.

PERFORMANCE TARGET: like C2.10, this is about correctness under adversarial
conditions at real scale (~100 calls/day per component-map), not throughput.
Inject real concurrency in the queue-processing loop (multiple orders
resolving in the same tick, a Release racing an in-flight automatic pipeline
run for the same order) rather than trusting low-concurrency test runs.
```

---

## Suggested sequencing

| Chunk | Days | Notes |
|---|---|---|
| C3.0 Scaffold + provider interface | 0.5 | — |
| C3.1 Result cache | 0.5 | — |
| C3.2 Verdict classification | 0.5 | Thresholds are placeholders until a vendor is chosen |
| C3.3 Discovery | 1.0 build + wait | Blocked on the two C1 additions; build everything testable now |
| C3.4 Emission to C1 | 1.0 | Needs a running C1 instance to test against for real |
| C3.5 Outage policy | 0.5 | The flagged business decision — ship fail-closed default, do not guess fail-open |
| C3.6 Hold queue + manual review | 1.0 | Also where the actor-field question in "Read this second" gets resolved for real |
| C3.7 Re-screen (mechanism only) | 0.5 | Policy for what happens next is explicitly out of scope |
| C3.8 HTTP boundary | 0.5 | Can run in parallel with C3.6/C3.7 |
| C3.9 Replay harness | 1.0 | Needs a running C1, not a forked chain — lighter lift than C2.10 |

≈6.5–7 days against the 1.0 eng-week estimate in `component-map.md` — slightly over, almost entirely because of the two C1-side additions this document had to propose rather than simply consume, the same way C2's own estimate assumed its C1 endpoint gap would be closed early rather than discovered mid-build.

**Before starting C3.0:** get an explicit answer on the outage-policy decision ("Read this third") from whoever owns compliance sign-off — engineering can and should build both paths, but should not be the one deciding which ships as default in production even though this document names fail-closed as the safer starting point. And get the two C1 additions ("Read this first," gap 1) scheduled — C3.3 and C3.4 cannot reach a real C1 without them.

---

## Cross-reference

- C1's full build spec and the exact transition table this document is written against: `c1-ledger-build-prompts.md`
- C2's build spec — the component that owns the sender address this document depends on: `c2-deposit-watcher-build-prompts.md`
- Where these scenarios live in the corridor-wide risk view: `c1-scenario-catalog.md`, Part 2's C3 section
- Component ownership boundaries this spec expands: `component-map.md`
- Current build/repo status as of this write-up: `repository.md`
