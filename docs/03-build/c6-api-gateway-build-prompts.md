# C6 — API gateway: sequenced build prompts

> **Background (history).** Written during the design and build phase and kept as a
> record. You don't need it to run or change today's product: start at
> [docs/README.md](../README.md).

**Target:** Go + PostgreSQL, matching C1/C2/C3/C4/S1's stack. **Consumer:** an AI coding agent (Claude Code or equivalent), same usage pattern as the prior build-prompt docs.

**How to use this file.** Paste §0 once at the start of the session — standing context the agent must hold for every chunk. Then paste chunks C6.0 → C6.9 one at a time, in order. Do not move to the next chunk until the current chunk's acceptance criteria pass against a real, running C1 and C2 (both are built — see §A), not a mock of your own assumptions about either.

**Before you start:** read the four call-outs below. They are not the architecture recap component-map.md already gives you — they are things that only became visible once C1, C2, C3, and C4 were actually built and this document was written against their real, shipped shapes rather than their original specs.

---

## Read this first — the "pricing library" component-map.md assumes does not exist

`component-map.md`'s one line on C6 says it "calls a pricing library rather than embedding tiers." No such library exists anywhere in this repository, in any language, in any component. There is no shared package computing the bp-tier fee (40 bp under $1k · 25 bp to $10k · 12 bp to $100k · 6 bp to $1M · 3.5 bp above, floor never below $1.80) or the three tier network fees (Direct $2.54 / Standard $1.80 / Sweep $1.00). C1's `PostOrderRequest` schema takes `amount_out`, `fee_units`, and `network_fee_units` as **required, pre-computed inputs** — C1 does not compute them, does not validate them against the pricing model, and does not know the pricing model exists. It trusts whatever number is handed to it and posts an entry against it.

That makes C6 the only place in the entire system where the price a customer is quoted becomes the price the ledger actually books — a wrong number here isn't a display bug, it's a real balance-sheet entry, on every single order, silently. C6.0 below builds this pricing library from scratch, as this component's own foundation, not as a shared dependency borrowed from anywhere else. Get the worked $3,000/Standard example from `findings-and-recommendation.md` (fee $7.50, network fee $1.80, recipient receives $2,990.70) passing as a golden test before writing anything else — if that number doesn't fall out of the general tier formula on the first chunk, nothing built on top of it should be trusted either.

---

## Read this second — order creation is a real two-service choreography with a real partial-failure window

This isn't hypothetical or a component-map guess. C2's own build doc (`c2-deposit-watcher-build-prompts.md`, C2.9's lead-in) states the contract explicitly, because C2 had to design around C6 not existing yet: *"C1 doesn't push order data to C2... the direction of discovery has to run the other way: whoever creates the order (C6, once it exists) must call C2... C2 needs an inbound endpoint of its own — see C2.9 — where the caller hands over `order_id`, `external_id`, `customer_id`, `quoted_at`, `quote_expires_at`, and gets back an assigned deposit address."*

That endpoint is real and already shipped: `POST /v1/addresses` on C2, idempotent on `order_id`. So creating an order from C6's side is **two separate HTTP calls to two separate services with two separate databases**, not one atomic operation:

1. `POST /v1/orders` on C1 — creates the order in `quoted`.
2. `POST /v1/addresses` on C2 — assigns the deposit address the customer actually needs to see.

If step 1 succeeds and step 2 fails (C2 down, network partition, C6 crashes between the two calls), the order exists in C1's ledger but the customer was never given anywhere to send money — a real order, stuck, forever, unless something notices. Both calls are individually idempotent (retry-safe), but nothing makes the *pair* atomic. C6.3 builds the choreography; C6.4 builds the reconciliation loop that is the only thing standing between this gap and a customer who paid a quote and got nothing back. Do not treat C6.4 as optional polish — without it, C6.3 has a silent failure mode on its very first chunk.

---

## Read this third — the sandbox is a named production gate, not a nice-to-have

`product-operations-architecture.md`, decision 5, in full: *"API: quote-then-order, never quote-inside-order. 90s price lock, idempotency keys, HMAC-SHA256 webhooks with 8 retries, sandbox with four deterministic failure triggers (reorg, screening hold, energy exhaustion, retry storm). **No production sign-off without exercising all four.**"* That last sentence is not advisory language anywhere else this phrase appears in the source docs — C1.9, C2.10, C3.9, C4.9, and S1's own replay harness are all described the same way, as the thing that gates going live, not a testing convenience.

The sandbox must therefore be able to deterministically produce all four failure conditions **without touching real C1/C2/C3/C4/C5 state or real vendor calls**, because two of the four (energy exhaustion, retry storm) are conditions this system deliberately tries never to let happen for real — you cannot gate production readiness on a test that only passes by accident when a real vendor happens to be having a bad day. C6.7 builds this as a fully separate code path, keyed off something the sandbox caller opts into explicitly (a sandbox API key, never a flag on a production one), so a sandbox order can never be mistaken for, or silently promoted into, a real one.

---

## Read this fourth — there is still no event bus, and this is the component where that finally hurts a customer directly

C2's own doc already surfaced this once: *"nothing in C1.8 is a webhook or event stream, and building one is explicitly C6's job (decision 5), not C1's or C2's."* C3 and C4 both inherited the same shape — they discover work by polling `GET /v1/orders?state=X`, not by being told. C6 inherits it too, for the opposite direction: to fire a webhook when an order reaches `settled` (or `held`, or `refunded`), C6 has to notice that transition itself, which means polling the same endpoint everyone else polls.

The difference is that for C2/C3/C4 this polling delay is an internal implementation detail nobody outside the system ever sees. For C6 it is directly, contractually customer-visible: decision 11 makes the status page (P50/P95/P99 settlement time, published hourly) *"the primary sales asset"* of the entire business, and a webhook that's slow because its own trigger is a polling loop eats directly into that number. C6.6 builds the webhook trigger as a tight, dedicated poll against `GET /v1/orders?state=settled` (and `held`, `refunded`) — separate from, and much tighter-interval than, C3's or C4's own background loops — precisely because this is the one polling loop in the system with a customer-facing SLA sitting on top of it.

---

## §0 — Standing context (paste once)

```
You are building C6, the API gateway of a USDT cross-network settlement system
(BEP20 -> TRC20 payouts) -- the customer-facing contract: quote -> order ->
status -> webhook. C1 (ledger core), C2 (deposit watcher), C3 (screening), and
C4 (energy broker) are already built and running. S1 (key management/signing)
is built (S1.0-S1.6) against a fake KMS client -- no real cloud KMS adapter
exists yet. C5 (payout dispatcher) has a build spec only, no code, and cannot
reach mainnet without S1's real KMS adapter.

STACK
- Go 1.22+, PostgreSQL 16 -- C6's own database, separate from every other
  component's, same as every sibling here.
- pgx/v5, goose migrations, chi routing, log/slog, testify, testcontainers-go.
- HTTP clients for C1 (order creation/status/halt check) and C2 (address
  assignment) -- both real, already-shipped services, not mocks of them.
- No pricing library exists anywhere else in this repo -- see "Read this
  first". C6 builds and owns it.

WHAT C6 IS
The customer-facing contract for the whole corridor: issues time-locked quotes
from its own pricing model, creates orders against C1 and C2 in one
choreography (see "Read this second"), exposes pull-based order status as the
guaranteed backstop, delivers signed webhooks on state transitions it detects
itself (see "Read this fourth"), and runs an isolated sandbox exercising four
deterministic failure conditions as a named pre-production gate (see "Read
this third").

WHAT C6 IS NOT -- do not build any of this, do not import libraries for it
- No money movement of any kind. C6 never posts a journal entry, never holds
  a balance, never touches a TRON or BSC key. It calls C1's and C2's own
  already-built APIs; it does not reimplement anything either of them owns.
- No order-state transitions past creation. C6 calls POST /v1/orders exactly
  once per order (creates in `quoted`) and never calls
  POST /v1/orders/{id}/transitions for anything -- that belongs to C2, C3,
  C4, C5, same rule C2, C3, and C4 all followed toward C1.
- No screening, no energy acquisition, no payout construction or signing.
  C6 does not call C3, C4, or C5 directly, ever -- they all discover their
  own work from C1 independently. C6's only two service dependencies are
  C1 and C2.
- No solving the deposit-after-expiry policy question. repository.md already
  flags this as an open gap from C2's own spec (#3 of 4). If a quote expires
  before a deposit arrives, that is a real open product decision this
  document does not resolve -- flag it in C6.3/C6.4, do not invent an answer.
If a chunk seems to require any of the above, you have misread it. Stop and
say so.

NON-NEGOTIABLE INVARIANTS
1. A quote is never generated inside an order, and an order is never created
   without a live, unexpired quote row it was issued against -- decision 5's
   own rule, enforced in code, not just at the API-description level.
2. Every write this component makes to C1 or C2 carries an idempotency key,
   and every write a customer makes to C6 requires one too -- the same
   discipline every other component here already applies, extended one hop
   further out to the actual customer's own retries.
3. A webhook is signed (HMAC-SHA256) and is never the only way a customer can
   learn an order's outcome -- GET /v1/orders/{id} is always available as the
   pull-based backstop, and the two must never be able to disagree.
4. Sandbox and production share no state, no customer_id namespace, and no
   code path that reaches a real C1/C2/C4/C5 call -- a sandbox order must be
   structurally incapable of becoming a real one, not just conventionally
   discouraged from it.
5. A price is never honored past its lock window (90s default, configurable).
   An expired quote is rejected at order creation, every time, no exceptions
   -- the same absolute-ceiling discipline C4's own invariant 3 applies to
   price, applied here to time.
6. C6 checks C1's halt state (GET /v1/system/halt) before issuing a quote and
   again before creating an order, and refuses both while halted -- offering
   a price the system cannot currently fulfill is worse than refusing one.
```

---

## §A — Interface contracts (verified against the real shipped code)

### With C1 (`ledger/`, built, `ledger/docs/openapi.yaml`)

- `POST /v1/orders` (header: `Idempotency-Key`) — body: `external_id, customer_id, tier, amount_in, amount_out, fee_units, network_fee_units, recipient_address, quoted_at, quote_expires_at`. `amount_out`/`fee_units`/`network_fee_units` are decimal strings C6's pricing library computes; C1 does not validate them against any model of its own. Returns `201` with the created `Order`, or `423 system_halted` if halted.
- `GET /v1/orders/{external_id}` — customer-facing status backstop reads through this. `state` is one of `quoted, funded, screened, dispatching, settled, held, refunded, expired`.
- `GET /v1/orders?state=X&updated_after=cursor&limit=N` — the same discovery primitive C3 and C4 already use. C6's webhook trigger (C6.6) polls this for `settled`, `held`, and `refunded`.
- `GET /v1/system/halt` — `HaltState`. Check before quoting and before order creation (invariant 6).
- Error shapes: `400` (`invalid_request`/`invalid_amount`), `404` (`order_not_found`), `409` (`idempotency_conflict`/`version_conflict`), `423` (`system_halted`) — all as `{code, message}` per the shared `Error` schema every component here already returns the same way.

### With C2 (`depositwatcher/`, built, C2.9's own HTTP boundary)

- `POST /v1/addresses` — body: `order_id, external_id, customer_id, quoted_at, quote_expires_at`. Idempotent on `order_id`: called twice, same address back, `200` both times. This is the second half of order creation — see "Read this second".
- `POST /v1/addresses/{order_id}/retire` — body: `reason`. Call this when an order expires unfunded (once the deposit-after-expiry policy question is actually resolved — see WHAT C6 IS NOT).
- `GET /v1/addresses/{order_id}` — for C6.4's reconciliation loop to check whether an address was ever actually assigned before retrying.

### Auth posture

Service-to-service bearer tokens against C1 and C2, same posture every sibling component uses (mTLS can replace this later without touching handlers). Customer-facing auth (C6.1) is a distinct, separate concern — customers never see or use the C1/C2 service tokens.

---

## §B — The pricing library's shape

Pure functions, no HTTP, no database — a package other C6 chunks call, tested in complete isolation first (C6.0).

```
ComputeQuote(tier Tier, amountIn money.Amount) (Quote, error)

Tier = DIRECT | STANDARD | SWEEP
  networkFeeUnits: DIRECT=2.54, STANDARD=1.80, SWEEP=1.00  (USDT_TRC20, fixed per tier)

feeUnits = max(floor, tieredBp(amountIn))
  floor = 1.80
  tieredBp: 40bp under $1k, 25bp to $10k, 12bp to $100k, 6bp to $1M, 3.5bp above
  -- apply marginally across the bracket boundaries, not as a single flat
  rate picked by which bracket the total falls in; confirm against the
  worked $3,000 example (25bp bracket, $7.50 fee) before trusting the
  boundary logic on larger tickets.

amountOut = amountIn - feeUnits - networkFeeUnits
```

Golden test: `amountIn=$3,000, tier=STANDARD` → `feeUnits=$7.50, networkFeeUnits=$1.80, amountOut=$2,990.70` (`findings-and-recommendation.md`'s own worked example). This must pass before C6.1 starts.

---

## C6.0 — Scaffold and the pricing library

```
Go module, chi router serving only /healthz initially (same posture every
sibling's own C4.0/C5.0/S1.0 scaffold chunk used). Postgres connection,
goose migrations directory, no tables yet beyond whatever the scaffold
itself needs.

Build internal/pricing exactly per §B, as pure functions with zero I/O.
No HTTP handler calls it yet -- that's C6.2.

ACCEPTANCE
- The $3,000/STANDARD golden test from §B passes exactly.
- Property test: for every tier and a spread of amountIn values (including
  exact bracket boundaries: $999.99/$1,000/$1,000.01, etc.), feeUnits is
  never below the $1.80 floor and amountOut is never negative.
- go vet / lint clean, matching every sibling module's own baseline.
```

---

## C6.1 — Customer auth and rate limiting

```
customers table: id, name, api_key_hash, created_at, status
  (active/suspended -- no deletion, matching this project's own
  never-hard-delete convention from every prior component).

Bearer API keys, hashed at rest (never logged, never returned after
creation). Per-customer rate limiting -- token bucket, config per
customer, sane default for anyone without an override.

Everything from C6.2 onward requires a valid, active customer's API key.
A suspended customer's existing orders remain readable (status, webhooks
already scheduled still fire) but no new quote or order may be created.

ACCEPTANCE
- Invalid/missing key: 401. Suspended customer: 403 on quote/order
  creation, 200 on read/status endpoints.
- Rate limit exceeded: 429 with a Retry-After header.
- API key never appears in any log line, error message, or metric label
  -- grep the test output for the raw key value as part of the test
  itself, don't just eyeball it.
```

---

## C6.2 — Quote issuance

```
quotes table: id, customer_id, tier, amount_in, amount_out, fee_units,
  network_fee_units, recipient_address, created_at, expires_at,
  consumed_at (nullable), consumed_by_order_external_id (nullable).

POST /v1/quotes  body: tier, amount_in, recipient_address
  -> calls internal/pricing (C6.0), persists a row, returns the full
  Quote including quote_id and expires_at = now + 90s (config).

A quote is issued, never computed again at order time -- order creation
(C6.3) reads this row back, it does not recompute the price. This is
what "quote-then-order, never quote-inside-order" means concretely: the
number the customer saw is the exact row that gets used, not a
re-derivation that could silently drift.

ACCEPTANCE
- Two quotes for the same customer/tier/amount get different quote_ids
  and independently expire -- no caching or reuse across requests.
- expires_at is always exactly created_at + the configured lock window,
  computed server-side, never trusting a client-supplied duration.
- GET /v1/system/halt is checked (invariant 6): halted -> 423, no row
  written.
```

---

## C6.3 — Order creation choreography (the C1 + C2 two-call sequence)

```
POST /v1/orders  header: Idempotency-Key  body: quote_id, external_id

1. Load the quote row. Not found -> 404. Already consumed -> 409
   (idempotency_conflict if this is a genuine retry of the same
   external_id+quote_id pair; a real conflict otherwise). Expired ->
   409 quote_expired -- a new quote must be requested, this endpoint
   never re-prices.
2. Halt check (invariant 6) -- refuse before either downstream call.
3. Call C1 POST /v1/orders with the quote's own amount_in/amount_out/
   fee_units/network_fee_units/recipient_address, this order's own
   external_id/customer_id, and quoted_at/quote_expires_at from the
   quote row -- verbatim, never recomputed at this step.
4. Mark the quote row consumed (consumed_at, consumed_by_order_external_id)
   in the same local transaction as step 3's own bookkeeping row (a
   gateway_orders table tracking external_id -> {c1_order_created: bool,
   c2_address_assigned: bool} -- this is what C6.4 reconciles against).
5. Call C2 POST /v1/addresses with order_id (C1's own numeric id from
   step 3's response), external_id, customer_id, quoted_at,
   quote_expires_at.
6. Return the order plus its assigned deposit address to the customer.

If step 5 fails after step 3 succeeded: do NOT fail the request silently
into a stuck state. Return the order with address=null and a
status=address_pending marker, and rely on C6.4 to close the gap
asynchronously -- see "Read this second". Never retry step 3 (C1's order
already exists, idempotently) if only step 5 needs retrying.

ACCEPTANCE
- Same Idempotency-Key + same request body, called twice: identical
  response both times, C1 and C2 each called exactly once (assert by
  call count against fakes in the unit test, matching every sibling
  component's own idempotency-replay test pattern).
- Step 5 forced to fail (fake C2 returns 500): response still comes
  back with address_pending, gateway_orders row shows
  c1_order_created=true, c2_address_assigned=false -- and a second
  identical request does NOT create a second C1 order.
- Expired quote: 409, no C1 or C2 call made at all.
```

---

## C6.4 — Order-creation reconciliation (closes the gap from C6.3)

```
Background loop, default every 30s (config): finds gateway_orders rows
with c1_order_created=true, c2_address_assigned=false, older than a short
grace period (a few seconds -- long enough that C6.3's own step 5 isn't
still in flight when this loop first looks). For each, calls C2's
POST /v1/addresses again (idempotent on order_id, safe to retry
indefinitely) and marks the row resolved on success.

A row stuck past a configured alerting threshold (default 10 minutes)
raises an alert -- same Alerter-interface convention C4's buffer.go
already established (nil-safe, optional, never a panic if unset).

ACCEPTANCE
- A row artificially stuck (fake C2 failing) gets retried on every loop
  tick and resolves the moment the fake starts succeeding, without any
  customer-facing request needing to happen again.
- A row past the alert threshold fires exactly one alert, not one per
  loop tick -- matches C4's own Reconcile "alert once, not per retry"
  discipline.
- Metric: gateway_orders_pending_address gauge, so this gap's own size is
  observable, not just eventually self-healing.
```

---

## C6.5 — Order status (the pull-based backstop)

```
GET /v1/orders/{external_id} -- customer-facing view. Reads through to
C1's own GET /v1/orders/{external_id} plus this gateway's own address
field (from gateway_orders / C2), translated into a customer-safe shape:
no internal C1 numeric id, no internal account codes, no fields a
customer has no reason to see.

This is decision 5's own named backstop for webhook failure (see C6.6
and the scenario-catalog's own C6 row: "Needs a pull-based
reconciliation path... and a test that the backstop actually agrees
with what the webhook would have said").

ACCEPTANCE
- Every state C1 can report translates to a defined customer-facing
  state -- no passthrough of an internal state string the customer has
  no contract for.
- A customer with no orders, or querying another customer's
  external_id, gets 404 -- ownership is checked, not just existence.
```

---

## C6.6 — Webhook delivery

```
webhook_deliveries table: id, customer_id, external_id, event_type,
  payload, created_at, delivered_at (nullable), attempt_count,
  next_attempt_at, last_error.

A tight dedicated poll (default 5s, much tighter than C3's or C4's own
background intervals -- see "Read this fourth" for why) against
GET /v1/orders?state=settled (and held, refunded), using updated_after
cursors per state so this loop's own cost doesn't grow with total order
volume. A newly observed transition enqueues one webhook_deliveries row.

Delivery: HMAC-SHA256 over the payload using a per-customer webhook
secret (set at C6.1's own customer creation, or rotatable via a later
chunk if you find you need one), 8 retries with exponential backoff,
capped. After 8 failed attempts, stop retrying and rely on C6.5's own
backstop -- this is what "webhook delivery exhausts all 8 retries"
in the scenario catalog's own C6 row describes, and the required test
for it: after exhaustion, GET /v1/orders/{id} still reports the correct,
current state, matching what the (failed) webhook would have said.

ACCEPTANCE
- A state transition is delivered exactly once under normal operation;
  a customer endpoint that 500s for the first 3 attempts and then
  succeeds gets exactly one successful delivery, not a duplicate.
- After 8 exhausted attempts, no further attempt is made, an alert
  fires, and GET /v1/orders/{id} independently confirms the same state
  the webhook would have carried.
- Signature verification: a documented, tested example customers can
  use to verify a payload against their own secret.
```

---

## C6.7 — Sandbox: the four deterministic failure triggers

```
A fully separate code path (see "Read this third" -- sandbox API keys,
issued distinctly from production ones at C6.1, never converted between
the two). Sandbox orders never call the real C1/C2; a fake in-process
ledger/address-assignment stands in, scoped entirely to this chunk,
never imported by any production code path (enforce mechanically with a
dependency test, matching every sibling module's own
internal/provider-boundary discipline).

Four triggers, selected by a documented sandbox-only request field
(e.g. amount_in ending in a specific cent pattern, or an explicit
trigger field -- pick one, document it in the OpenAPI spec, and keep it
impossible to hit by accident on a real amount):
  1. reorg          -- order reaches funded, then a simulated BSC reorg
                        reverses it back to quoted, exactly matching
                        C1.6 scenario A's real shape.
  2. screening_hold  -- order reaches funded, then held, with a
                        realistic C3 reason code attached.
  3. energy_exhaustion -- order reaches screened, then dispatching stalls
                        past a simulated deadline the way C4's own
                        ceiling/fallback-ladder exhaustion would.
  4. retry_storm     -- webhook delivery for this order fails
                        deterministically for exactly 8 attempts, so a
                        customer's own retry-handling code gets a real,
                        repeatable exhaustion case to test against.

ACCEPTANCE
- Each trigger is independently deterministic -- same request, same
  sequence of states and webhook attempts, every time, no timing races.
- A sandbox order's external_id is drawn from a namespace (or table)
  that overlaps zero percent with production external_ids -- a
  production GET /v1/orders/{external_id} for a sandbox id returns 404,
  never sandbox data.
- Decision 5's own gate is directly testable: a single test suite run
  exercises all four triggers end to end and asserts each one's
  documented behavior, suitable as the artifact a production sign-off
  actually points at.
```

---

## C6.8 — HTTP boundary and OpenAPI

```
Consolidate: /healthz, /readyz, /metrics, bearer auth middleware (C6.1),
consistent error shapes matching C1/C2/C3/C4's own {code, message}
convention exactly (a customer integrating against this corridor should
not see a different error shape depending on which component's mistake
they tripped over).

OpenAPI spec generated/maintained the same way C1.8 required, with a
test that every route in the router is present in it -- same acceptance
bar every sibling's own HTTP-boundary chunk used.

Metrics: quotes_issued_total, orders_created_total,
orders_address_pending gauge (mirrors C6.4's own metric),
webhook_deliveries_total by result, webhook_retry_exhausted_total,
sandbox_requests_total by trigger.

ACCEPTANCE
- Full route coverage in the OpenAPI spec, verified by test.
- A customer-facing error from any endpoint, for any of C1/C2's own
  underlying failure codes, never leaks an internal service name,
  internal id, or stack trace.
```

---

## C6.9 — Replay harness (the ship gate)

```
C6's acceptance gate, same posture as every prior component's own C*.9:
needs a real running C1 and C2 (testcontainers or real subprocess
binaries, matching whichever convention C2/C3/C4 actually used against
each other), not mocks of either.

cmd/replay/main.go, runnable as a Go test in CI. Seeded, deterministic,
prints the seed on failure.

SCENARIO MIX (mirrors c1-scenario-catalog.md's own C6 row directly -- if
you add a scenario here, add the matching row there too)
  Clean quote -> order -> settled -> webhook delivered path
  Quote expires before order creation -> 409, no C1/C2 call made
  C2's own POST /v1/addresses fails after C1's order succeeds ->
    address_pending -> C6.4 resolves it within the next loop tick
  Webhook endpoint failing for exactly 8 attempts -> exhaustion ->
    GET /v1/orders/{id} backstop still agrees with the true state
  Concurrent order-creation requests racing the same quote_id -> exactly
    one order created, the other gets 409 quote_already_consumed
  System halted mid-quote and mid-order-creation -> both correctly
    refused, no partial state left behind
  All four sandbox triggers (C6.7), run in the same harness to confirm
    they remain isolated from the production scenarios also running

FINAL ASSERTIONS
1. Every settled/held/refunded order in the run has exactly one
   successfully delivered webhook, or (if delivery was deliberately
   forced to fail) exactly 8 attempts and a backstop that agrees.
2. No gateway_orders row is ever left in address_pending past the
   configured alert threshold during the run.
3. No sandbox-triggered order or webhook is ever visible through any
   production-scoped query.
4. Every quote is consumed by at most one order, no exceptions, under
   concurrent load.
5. Zero deadlocks, zero panics, zero unexplained errors.

PERFORMANCE TARGET: same framing as every prior component's own C*.9 --
real volume here is low (~100 orders/day), so this harness is about
correctness under adversarial timing (concurrent requests racing one
quote, C2 failing mid-choreography), not throughput.
```

---

## Suggested sequencing

| Chunk | Days | Notes |
|---|---|---|
| C6.0 Scaffold + pricing library | 1.0 | The golden-test discipline in "Read this first" matters more than the days estimate suggests |
| C6.1 Auth + rate limiting | 0.5 | — |
| C6.2 Quote issuance | 0.5 | — |
| C6.3 Order creation choreography | 1.5 | The crux chunk — "Read this second" is why |
| C6.4 Reconciliation loop | 0.5 | Not optional — see C6.3's own note |
| C6.5 Status (backstop) | 0.5 | — |
| C6.6 Webhook delivery | 1.0 | Tight poll interval, not the sibling default — see "Read this fourth" |
| C6.7 Sandbox (4 triggers) | 1.0 | Named production gate per decision 5 — see "Read this third" |
| C6.8 HTTP boundary | 0.5 | Can run in parallel with C6.6/C6.7 |
| C6.9 Replay harness | 1.0 | Needs a running C1 and C2 |

≈8–8.5 days against the 1.5 + 0.5 (sandbox) eng-week estimate in `component-map.md` — close, with the sandbox's own weight showing up as a real chunk (C6.7) rather than an afterthought, consistent with decision 5 naming it a hard production gate.

**Before starting C6.0:** there is no dependency on C5 or a real S1 KMS adapter — C6 only ever talks to C1 and C2, both real today. It can start immediately, in parallel with whatever closes C3's remaining production-wiring gap or gets a real KMS adapter built for S1. The one real prerequisite is deciding the deposit-after-expiry policy question repository.md already flags as open (#3 of C2's original 4 gaps) — C6.3/C6.4 can be built with that question still open (both chunks above are written to leave it open, not to guess), but going live with real customer money without an answer is a product decision, not an engineering one.

---

## Cross-reference

- C1's full build spec and the exact order/entry contract this document is written against: `c1-ledger-build-prompts.md`, `ledger/docs/openapi.yaml`
- C2's own real HTTP boundary this document's order-creation choreography depends on: `c2-deposit-watcher-build-prompts.md`, C2.9
- The architecture decision this component implements in full (quote-then-order, 90s lock, HMAC webhooks, the sandbox gate): `product-operations-architecture.md`, decision 5
- Where C6's own scenarios live in the corridor-wide risk view: `c1-scenario-catalog.md`, Part 2's C6 section
- Component ownership boundaries this spec expands: `component-map.md`
- Current build/repo status as of the last write-up: `repository.md`
