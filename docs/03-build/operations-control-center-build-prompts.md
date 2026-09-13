# Build prompt: Operations Control Center for `usdt-settlement-corridor`

> **Status: not built, not reconciled.** While this doc was being written on
> `origin/main`, a simpler operator dashboard covering much of the same need
> was independently designed and built on another branch — see
> `ops-console-build-prompts.md` and `opsconsole/`, which is the thing that
> actually runs today. Merge commit `3aed5fd` kept both docs side by side
> rather than picking one, and explicitly left reconciling them as a separate,
> still-open decision. Read this as an unbuilt superset proposal (RBAC, a
> dedicated Ops BFF, a React frontend, customer CRUD, an audit log) to weigh
> against extending the already-shipped `opsconsole/` instead of building this
> from scratch — not as a description of anything currently running.

> Rewritten against the real, current codebase (checked directly against git on
> `main`, commit `18edadb`) rather than assumptions. Everywhere the original ask
> described something that doesn't exist yet, this version says so explicitly and
> proposes the smallest real addition — it does not silently invent an API,
> a state machine, or a permissions model that isn't there. Paste this whole
> document as the prompt.

---

## 0. Ground truth: what this repository actually is today

Before touching UI code, internalize this. It is not a framing exercise — every
number and name below is checked against real code, not docs (the docs drift;
`README.md` currently still says C6 has no code, but the gateway is fully built
as of commit `18edadb` — **trust `git log` and the code over any `.md` file,
including this one, if they ever disagree**).

### It is seven independent Go services, zero UI, zero shared auth

| Service | Dir | Binary | Port | Own Postgres DB | Purpose |
|---|---|---|---|---|---|
| C1 Ledger | `ledger/` | `ledgerd` | 8080 | `ledger_dev` | source of truth for money + order state |
| C2 Deposit watcher | `depositwatcher/` | `watcherd` | 8082 | `watcher_dev` | BSC HD addresses, ingestion, finality |
| C3 Screening | `screening/` | `screend` | 8083 | `screening_dev` | AML holds (currently a placeholder provider — see §2) |
| C4 Energy broker | `energybroker/` | `brokerd` | 8084 | `broker_dev` | TRON energy rental/reservation |
| S1 Key mgmt | `s1/` | `s1d` | 8085 | `s1_dev` | signing requests, human approval queue |
| C5 Payout dispatcher | `dispatcher/` | `dispatchd` | 8086 | `dispatcher_dev` | slots, dispatch, broadcast, finality |
| C6 API gateway | `gateway/` | `gatewayd` | 8087 | *(own DB, not yet wired into root `docker-compose.yml`)* | customer-facing quote/order/status/sandbox |
| proofrun | `proofrun/` | `proofrund` | 8090 | none (reads through) | throwaway 2-route driver for the mainnet proof run — **not** C6, has no auth, do not build on it |

Each service: Go, `chi` router, `pgx/v5`, `goose` migrations, no ORM, its own
`docs/openapi.yaml` (gateway/ledger/depositwatcher/screening/energybroker/dispatcher
all have one — read them, they are accurate to the code). `s1` and `proofrun` have
no openapi.yaml; routes were confirmed by grepping `internal/httpapi`.

**Every service-to-service call authenticates with a single shared bearer token per
service identity** (e.g. `LEDGER_API_TOKENS=ledgertoken:proofrun` — token maps to an
actor *string*, not a person). `gateway` additionally has customer-facing
`sk_live_*`/`sk_test_*` API keys, stored as `api_key_hash` only. **There is no
concept of a human operator, a login, a session, or a role anywhere in this
codebase.** Section 29's Viewer/Operator/Admin/Security-Finance RBAC does not
exist and cannot be "reflected" by a frontend — it has to be built, for real, as a
new backend concern. See §3.

**There is no frontend of any kind in this repo** — no `package.json`, no `/web`,
no `/ui`, nothing. "Use the existing frontend technology stack" (original §30)
doesn't apply; a stack has to be chosen. See §4.

### The real order lifecycle (not the invented one)

C1 (`ledger/internal/orders`) owns the *only* order state machine that exists.
The states are exactly these eight — nothing else:

```
quoted → funded → screened → dispatching → settled
                                          ↘ held
                            ↘ refunded
   (expired is also reachable from quoted)
```

- `quoted → funded`: a BSC deposit was detected and credited (C2's write).
- `funded → screened`: screening cleared it, or a hold was released (C3's write).
- `funded → held` / anything `→ held`: screening or another gate stopped it — see
  `screening/internal/holds`.
- `screened → dispatching`: C5's `POST /v1/dispatch` posted the E2 conversion
  entry (synchronous half only).
- `dispatching → settled`: C5's background `internal/orchestrate` loop confirmed
  TRON finality.
- `dispatching → held`: a non-retryable dispatch failure.
- any state `→ refunded`: currently has **no implemented path** — screening's own
  `POST /v1/holds/{id}/reject` is wired but returns `501 refund_entry_not_implemented`
  for every call, always, because nothing owns constructing the BEP20 refund entry
  yet. **The UI must show this as a real, currently-broken capability, not hide it
  or fake success.**

Reject the original prompt's invented pipeline
(`QUOTED/WAITING/DETECTED/CONFIRMING/SCREENED/ENERGY/DISPATCHING/BROADCAST/COMPLETED`)
outright — no such states exist anywhere in C1. What the UI can show as
sub-stage detail *underneath* an order's real C1 state, pulled from other
services, is:

- Within `quoted`, before `funded`: C2's deposit-address status
  (`WATCHING → FUNDED → RETIRED`, from `GET /v1/addresses/{order_id}` on C2)
  and, once a deposit lands, whether it's still accumulating confirmations
  (no explicit field exposed for this today — C2 tracks it internally via
  `seen_blocks`/finality policy but does not surface a confirmation counter on
  any route; **this is a real gap**, see §3).
- Within `dispatching`: C5's own sub-status,
  `latest_attempt_status ∈ {BUILT, SIGNED, BROADCAST, CONFIRMED, FAILED}` from
  `GET /v1/dispatch/{order_id}`, plus `dispatch_attempts` rows if you need full
  attempt history (schema: `order_id, attempt_number, status, ...` — one row per
  retry).
- Energy: a reservation's own `status ∈ {PENDING, CONFIRMED, FAILED}` and
  `fast_path` (bool — served from the standing buffer vs. bought live), from C4's
  `GET /v1/reservations/{id}`.
- Signing: S1's `signing_requests.status ∈ {PENDING, SIGNED, REJECTED}`, with a
  2-of-N human-approval sub-flow above a configurable USD threshold
  (`S1_APPROVAL_THRESHOLD_USD`).

There is no `GET /v1/dispatch?order_id=` cross-reference from a raw order id back
to which reservation/signing-request funded it purely from C1's own `Order` object
— **C1's `Order` has no `reservation_id` or `signing_request_id` field.** The link
has to be made by whatever service already knows it (C5 knows both, since it calls
both) or by a new aggregation layer joining on `external_id`/`order_id`. Design
around this rather than assuming a single order document exists anywhere.

### Real entity shapes (use these field names, not invented ones)

**Order** (`GET /v1/orders/{external_id}` on C1 — the canonical shape):
`id, external_id, customer_id, tier (DIRECT|STANDARD|SWEEP), state, amount_in,
amount_out, fee_units, network_fee_units, recipient_address, sender_address
(nullable, set at funded), quoted_at, quote_expires_at, version, created_at,
updated_at`. **No deposit_tx_hash or payout_tx_hash field exists on the order
itself** — those live on C2's deposit/candidate records and C5's
`dispatch_attempts.tron_txid` respectively, and must be joined in by whatever
BFF you build.

**Gateway's own customer-facing `Order`** (`GET /v1/orders/{external_id}` on C6) is
a *different*, smaller shape: `external_id, tier, amount_in, amount_out, fee_units,
network_fee_units, recipient_address, deposit_address, status` — where `status` is
either the pre-address view (`address_pending`/`created`) or the real C1 state.
Don't conflate the two `Order` types when specifying UI columns.

**Deposit address** (C2, `GET /v1/addresses/{order_id}`):
`address, order_id, external_id, customer_id, status (WATCHING|FUNDED|RETIRED),
quoted_at, quote_expires_at, assigned_at, retired_at, retired_reason`.

**Orphaned deposit** (C2, `GET /v1/orphaned-deposits`) — a deposit that finalized
on-chain for an order C1 no longer has open, never auto-resolved:
`id, order_id, external_id, tx_hash, log_index, amount, detected_at,
order_state_at_detection, resolution, resolved_at, resolved_by`.

**Screening hold** (C3, `GET /v1/holds`):
`id, order_id, external_id, reason_code, screening_result_id, opened_at,
status (OPEN|RELEASED|REJECTED), resolved_by, resolved_at, resolution_note`.
Release requires a `reviewer` field the UI must collect and send (never inferred).
The screening provider today is literally `provider.AlwaysCleanProvider` — an
explicitly-labeled non-production placeholder, gated behind
`SCREENING_PROVIDER=always_clean` **and** `SCREENING_ALLOW_ALWAYS_CLEAN=true` so it
can't be reached by accident. Original §11 already asked for this to be shown
honestly in the UI — good instinct, it maps exactly to this real flag; show the
active provider name and, when it's `always_clean`, a persistent "NOT A REAL AML
VENDOR" banner, not a quiet label.

**Energy reservation** (C4, `GET /v1/reservations/{id}`):
`id, external_id, order_id, target_address, energy_units, tier, status
(PENDING|CONFIRMED|FAILED), vendor, cost_trx, confirmed_at, deadline, created_at,
fast_path`. Buffer levels: `GET /v1/buffer` → per-provider `available/reserved`.
Vendor price/health: `GET /v1/system/prices` → per-vendor
`price_per_unit_sun, observed_at, healthy, error`, plus the configured
`ceiling_sun_per_unit` and routing `weights`. Manual fallback events (energy
exhausted, all vendors unhealthy/over ceiling):
`GET /v1/manual-fallback-events`.

**Dispatch / payout** (C5, `GET /v1/dispatch/{order_id}`):
`order_id, slot_id, conversion_entry_key, status (DISPATCHING|SETTLED|HELD),
entered_dispatching_at, latest_attempt_number, latest_attempt_status
(BUILT|SIGNED|BROADCAST|CONFIRMED|FAILED), tron_txid, broadcast_at`.

**Payout slot / wallet** (C5, `GET /v1/slots`):
`id, tron_address, status (ACTIVE|RETIRING|RETIRED), balance, tx_count,
activated_at, retired_at, last_dispatch_at`, capped at
`DISPATCHER_SLOT_BALANCE_CEILING` (default $50,000) and
`DISPATCHER_SLOT_TX_COUNT_CEILING` (default 5,000) — these are the real caps
decision 4 (segregated slots, never a pooled treasury) put in place. There is no
equivalent "wallets" endpoint for BSC deposit addresses beyond C2's per-order
`watched_addresses` — a "Wallets" page has to merge C5 slots (payout side) with a
paginated view over C2's addresses (deposit side); no single existing endpoint
lists all deposit addresses at once (only per-order lookup) — that's a real gap,
see §3.

**Customer** (gateway DB, `customers` table — **no HTTP route exposes this at
all today**, not even read-only):
`id, name, api_key_hash, status (active|suspended), rate_limit_per_minute
(nullable), webhook_url (nullable), created_at, updated_at`. Never store or
render `api_key_hash`. There is currently no way to create a customer except a
direct SQL insert — no seed CLI, no admin route. This blocks the entire
Customers section of the UI until built — see §3.

**Webhook deliveries** (gateway DB, `webhook_deliveries` — also no HTTP route
yet): `id, customer_id, external_id, event_type, payload, created_at,
delivered_at, attempt_count, next_attempt_at, last_error`.

### Health / observability surfaces that actually exist

Every service exposes `/healthz`, `/readyz` (checks DB connectivity), and
`/metrics` (Prometheus text — not JSON, needs a scraper or a text-parsing shim if
the UI wants to chart it directly rather than through Prometheus/Grafana).
Beyond that, each service exposes **different**, non-uniform operational
endpoints — there is no single "system health" endpoint anywhere:

- C1 ledger: `GET /v1/system/halt` (current halt state), `GET /v1/trial-balance`
  (per-asset sum across every journal line — "should be exactly 0.000000
  always," the strongest single health signal in the system),
  `GET /v1/system/invariants`.
- C2 watcher: `GET /v1/system/providers` (per-RPC-provider health — `healthy,
  consecutive_failures, total_rounds, total_failures, last_error`; the pool
  **refuses to construct with fewer than 2 providers**, so this is exactly where
  original §16's "detect two configured providers that are actually the same
  underlying provider" belongs — it doesn't exist today and would need a real
  check added, e.g. comparing resolved chain-id + genesis hash or endpoint host
  across configured providers), `GET /v1/system/invariants` (`cursor_lag_blocks,
  pending_finality_count, oldest_pending_candidate_age_seconds`).
- C3 screening: `GET /v1/system/queue` (queue depth by status + oldest pending
  age) — **note the different path shape**, no `/v1/system/invariants` here.
- C4 broker: `GET /v1/system/prices`, `GET /v1/system/invariants` (`buffer_available,
  buffer_target, reconciliation_lag_seconds, fast_path_ratio,
  open_manual_fallback_events`).
- C5 dispatcher: `GET /v1/system/invariants` (`open_dispatching_orders,
  stuck_pending_reconciliation, batch_queue_depth, slot_headroom[]`).
- S1, C6 gateway, proofrun: no `/v1/system/*` at all today.

There is no TRON-node-health endpoint anywhere (dispatcher's gRPC client to
`grpc.trongrid.io:50051` has no exposed health check), and no unified "is the
whole settlement system healthy" boolean — that has to be computed by whatever
aggregation layer the UI sits behind, from the pieces above.

---

## 2. What this means for the UI's honesty requirements

Original §31 ("no fake data") and §37 ("make incidents understandable") are
right, and they cut harder than the original prompt's own example incidents
suggest, because several of the "degraded" states it describes as hypothetical
are **currently permanent, documented facts about this system**, not incidents
to detect:

- Screening is running on a labeled placeholder (`AlwaysCleanProvider`), not a
  real AML vendor, whenever `SCREENING_PROVIDER=always_clean` — show this as a
  standing banner, not a transient alert.
- S1 signs against `FakeKMSClient` in every environment that exists today (no
  real cloud KMS adapter is built) — this produces **real, valid signatures**
  (verified against a real secp256k1/TRON signing path), just without HSM
  custody. The UI's Wallets/Keys view must say this plainly rather than imply
  hardware custody exists.
- Sweep-tier batching (`internal/txbuild.BuildMultisend`) is built against a
  *proposed* multisend contract that is **not deployed anywhere** — no Sweep
  order can ever actually complete via the batched path today. Show Sweep-tier
  orders' batching step as blocked-by-design, not stalled.
- Screening's `reject` action (`held → refunded`) always 501s — see §0.
- The `gateway` (C6) service is **not part of the root `docker-compose.yml`
  proof-run stack at all** — if you stand up the real backend for this UI to
  talk to, you must add it yourself (own DB, own migration, own env block,
  following the exact pattern the other six services already use).

---

## 3. Backend additions required before the UI can be real (do these first)

The original prompt's own §32 already commits to exactly this discipline
("identify the missing capability, implement the smallest clean addition").
Here is the concrete, scoped list for *this* codebase — nothing invented beyond
what's needed to make the spec in §5 true:

1. **A new, thin Ops/BFF service** (`ops/` or similar, its own Go module,
   matching every other service's conventions: `chi`, `pgx/v5`, `goose`,
   `/healthz`/`/readyz`/`/metrics`). This is the only piece of new
   infrastructure of any real size, and it is what makes an actual "Operations
   Control Center" possible instead of a UI directly juggling seven bearer
   tokens in the browser (which would also violate §29 — never trust the
   frontend with service-to-service credentials). It owns:
   - **Real operator authentication** — a login, sessions or JWTs, and the
     four roles (Viewer / Operator / Admin / Security-Finance) from original
     §29. This does not exist anywhere today; build the smallest real version
     (a Postgres `operators` table + hashed passwords + session tokens is
     enough — don't over-engineer SSO for a v1).
   - **Server-side enforcement of every mutating action** — a request to retry
     an order, release a hold, adjust a config value, etc. passes through this
     service, which checks the caller's role, calls the real downstream
     service with *its own* service-to-service bearer token (never exposed to
     the browser), and only then acts. The frontend must never hold a
     `LEDGER_API_TOKENS`-style secret.
   - **The audit log** (original §21) — genuinely does not exist anywhere in
     this codebase today (the only prior art called "audit" is C1's
     *accounting* journal, `internal/journal/audit.go`, which is unrelated —
     it's the money ledger, not an operator-action log). Add an
     `ops_audit_log` table here: `timestamp, operator, action, entity, old_value,
     new_value, reason, result, request_id`. Every mutating call this service
     makes downstream writes one row, before or after the call, atomically
     with a clear failure story if the downstream call fails after the row is
     written.
   - **Cross-service aggregation reads** — "orders in each pipeline state" (a
     single count needs to hit C1's `GET /v1/orders?state=X` eight times and
     sum), "system health," and a joined order-detail view (C1 order + C2
     address + C3 hold + C4 reservation + C5 dispatch + S1 signing request, by
     `external_id`) all belong here, not scattered across seven separate
     frontend API clients. Cache short-lived (a few seconds) where it fans out
     to avoid hammering seven services on every dashboard refresh.
   - **The two genuinely missing read endpoints**: a paginated "all deposit
     addresses" listing on C2 (today only per-order lookup exists — add
     `GET /v1/addresses?status=&limit=&cursor=` following the exact keyset
     pattern C1's `GET /v1/orders` already uses) and a webhook-deliveries
     listing on C6 (`GET /v1/webhook-deliveries?customer_id=&status=`) so the
     UI isn't the reason these get built with worse discipline than everything
     around them.
   - **Customer CRUD on the gateway** — `POST /v1/customers` (create, returns
     the raw API key exactly once), `GET /v1/customers`, `GET
     /v1/customers/{id}`, `POST /v1/customers/{id}/suspend`, `POST
     /v1/customers/{id}/rotate-key`. None of this exists; it's the actual
     blocker for original §14 in its entirety. Build it on `gateway/` itself
     (it already owns the `customers` table), not on the new Ops service,
     and have Ops call it with its own service identity.
   - **Wiring `gateway` into `docker-compose.yml`** so there's a real backend
     to develop the UI against at all — copy the existing pattern (its own
     `gateway-db` service, `GATEWAY_DATABASE_URL`, `GATEWAY_API_TOKENS`,
     `GATEWAY_LISTEN_ADDR=:8087`) from any of the six services already there.

2. **Configuration Center — scope this down honestly for v1.** Every tunable
   parameter in this system today (`BROKER_ROUTING_WEIGHTS`,
   `DISPATCHER_SLOT_BALANCE_CEILING`, `PROOFRUN_FEE_BASIS_POINTS`,
   `WATCHER_CONTRACT_ADDRESS`, and every other `*_` env var in the
   `docker-compose.yml` blocks) is a **process-start environment variable**,
   not a database row. There is no config service, no config table, and no
   mechanism anywhere for a running process to pick up a changed value without
   a restart. Building the original §19/§20's live edit→validate→preview→
   confirm→apply→audit flow *as specified, for every parameter listed, with no
   deploy required* is a materially larger project than a UI addition — it
   means adding a config store and a config-reload mechanism to seven already-
   shipped services. Do not silently scope this down without saying so; instead:
   - **v1 (build now, matches this backend's real capability):** a read-only
     Configuration page showing every current value, pulled live from each
     service's own env (expose a small `GET /v1/system/config` on each service
     that echoes its own already-loaded config struct — a few hours of work per
     service, not a new subsystem), each parameter's source (env var name),
     and a clear "changing this requires a redeploy — edit `docker-compose.yml`
     / your deployment config and restart the service" note. This is honest,
     useful, and buildable today.
   - **v2 (explicitly out of scope here, name it as follow-up work):** picking
     one or two of the lowest-risk, highest-value parameters (a strong
     candidate: C4's `BROKER_ROUTING_WEIGHTS` or the fee basis points) and
     giving *those specifically* a real database-backed, hot-reloadable
     config row with the full edit/audit flow, as a proof of the pattern
     before generalizing it to everything.

3. **Wallets/keys honesty**: S1 exposes `GET /v1/slots/{id}/address` (a slot's
   derived TRON address from its KMS public key) but no listing of all slot
   keys, and no distinction in its API between "real KMS-backed" and
   "FakeKMSClient" — since today *every* key is the latter, add one field to
   whatever endpoint backs the Wallets page (`custody: "kms_fake"` today) so
   the UI can render the true custody state instead of assuming HSM-backed
   because the concept exists in the architecture doc.

Everything else this document asks for (§5 onward) is genuinely buildable
against what already exists, once the Ops BFF in item 1 is in place.

---

## 4. Frontend stack (none exists — this is a real decision, not a formality)

Given the backend is a set of typed, OpenAPI-documented Go services and the
product is dense operational tooling (not a marketing surface), use:

- **React + TypeScript + Vite.**
- **TanStack Query** for server state (polling, cache, retry — see §5's
  real-time section) and **TanStack Table** for the Orders/Customers/Audit
  tables (server-side pagination, sorting, column filters against the keyset-
  paginated list endpoints C1/C2 already use — mirror that pattern in the new
  Ops BFF's own list endpoints rather than inventing OFFSET pagination).
- **React Router**, with filter state serialized to the URL (original §8/§34
  correctly ask for this).
- Generate the typed API client from each service's real `openapi.yaml` (five
  of the seven already have one) rather than hand-writing fetch wrappers —
  keeps the UI honest to the actual contract as it evolves. The Ops BFF's own
  new endpoints get an openapi.yaml too, written the same way every other
  service's was, so the whole system stays consistent (original §6.8-style
  discipline C6 itself follows).
- Tailwind CSS for the dark-first, dense, Stripe/Linear/Fireblocks-adjacent
  visual language original §3 describes — that guidance is good and needs no
  correction, just execution.
- Component primitives: Radix UI (unstyled, accessible) under Tailwind, not a
  full pre-styled component library — keeps the "original visual system, don't
  clone any existing product" requirement honest.

---

## 5. The product spec (corrected)

Everything below assumes the Ops BFF from §3 exists and is what the frontend
talks to exclusively (never the seven downstream services directly from the
browser).

### 5.1 Application shell

Sidebar: **Overview · Orders · Customers · Deposits · Payouts · Energy ·
Screening · Providers · Wallets · Configuration · Audit Log · System Health.**
(Screening gets its own item, promoted out of "Providers" — it's a large enough
domain with its own hold queue, per original §11, and doesn't fit the generic
"infrastructure provider" shape §16 describes.)

Top bar: global search (§5.7), environment indicator (which `docker-compose`
stack / deployment this Ops BFF is pointed at — real and configurable, never
hardcoded), aggregate system health pill (computed from the per-service reads
in §0's health table), operator identity + role (from the new auth in §3),
manual refresh, and a visible polling-interval control (§5.9).

### 5.2 Overview / Command Center

Global status banner computed from: any service's `/readyz` failing, C1's
`GET /v1/system/halt` being true, C1's `GET /v1/trial-balance` showing nonzero
drift on any asset, or `open_manual_fallback_events > 0` from C4.

Per-service health grid: the nine real service rows from §0 (ledger, watcher,
screening, broker, s1, dispatcher, gateway, BSC RPC pool via watcher's
`/v1/system/providers`, TRON — flag TRON explicitly as "no health endpoint
exists yet" rather than fabricating one), each showing status/latency (measure
it — a simple timed `/healthz` call from the BFF, since none of these services
report their own latency) /last success/last error.

KPIs, sourced exactly as named: orders today/processing/completed/failed
(`GET /v1/orders?state=X` counts, summed and time-bucketed by the BFF —
`created_at`/`updated_at` are the only timestamps to bucket on, there is no
separate events table), settlement volume (`sum(amount_in)` over settled
orders in range), gross fees (`sum(fee_units)`), network costs
(`sum(network_fee_units)`), energy costs (`sum(cost_trx)` from confirmed
reservations, converted at... there is no stored TRX/USD rate anywhere in this
system today — either accept TRX-denominated energy cost as the honest unit, or
add the smallest possible price lookup; don't invent a fabricated USD figure),
success rate, average settlement/detection/payout time (computed from real
timestamp deltas: `quoted_at`→`settled` transition time needs the Ops BFF to
read C1's own transition history, which today is only reconstructable from
journal entries' `occurred_at`, not a dedicated events log — note this as a
real limitation if C1 doesn't expose per-transition timestamps beyond
`updated_at`, which only reflects the *latest* transition).

Live pipeline: the real eight states from §0 — `quoted, funded, screened,
dispatching, settled, held, refunded, expired` — each a live count from
`GET /v1/orders?state=X`, clicking through to Orders filtered on that state.

Needs Attention: orphaned deposits (C2), open manual fallback events (C4),
open screening holds (C3), `stuck_pending_reconciliation` (C5's own
invariants field, already named exactly this), RPC providers unhealthy
(C2 `/v1/system/providers`), non-zero trial-balance drift (C1) — every one of
these is a real field on a real endpoint, nothing invented.

### 5.3 Orders

Columns, using the real Order shape (§0): External ID, Customer, Tier
(DIRECT/STANDARD/SWEEP), State, Amount In (USDT_BEP20), Amount Out
(USDT_TRC20), Fee, Network Fee, Recipient Address, Sender Address (nullable),
Created, Updated. **Deposit TX / Payout TX / processing-time columns require
the BFF's join** described in §3 — they are not on the base Order object.

Filtering/search: everything original §8 asks for is realistic, backed by the
BFF's aggregation, with one caveat — "amount filter" and "transaction hash
search" need the join to exist first (§3). Keyset pagination end-to-end,
matching C1's own `updated_after`/`next_cursor` convention rather than OFFSET.

### 5.4 Order detail

Build exactly the joined view from §0's "order lifecycle" discussion: the
C1 order plus, where they exist, the linked C2 address, C3 hold (if any), C4
reservation, C5 dispatch + attempt history, S1 signing request. Render `held`
orders with their real hold reason (`holds.reason_code`) and, if the operator
tries to reject one, show the actual `501 refund_entry_not_implemented`
response honestly rather than a generic error — this is a known, named backend
gap, not a transient failure.

### 5.5 Deposits

Per C2's real fields (§0): address, status (WATCHING/FUNDED/RETIRED), assigned/
retired timestamps. The ingestion chain diagram from original §10 is
directionally right but note explicitly in the UI that **no per-deposit
confirmation counter is exposed by any C2 endpoint today** — finality is
policy-driven (BEP-126 finality-tag based) and binary in what's exposed, not a
live "12/15 confirmations" progress value; don't fabricate one. Surface
orphaned deposits from `GET /v1/orphaned-deposits` with their resolve action.

### 5.6 Screening

Hold queue from `GET /v1/holds`, release action requiring a real `reviewer`
field, the `AlwaysCleanProvider` banner from §2, re-screen flags from
`GET /v1/rescreen-flags`, queue depth from `GET /v1/system/queue`.

### 5.7 Energy

Buffer levels (`GET /v1/buffer`), reservations (`GET /v1/reservations/{id}` /
by-order lookup via the BFF join), vendor price/health
(`GET /v1/system/prices`), manual fallback events with resolve action,
invariants (`buffer_available` vs `buffer_target`, `fast_path_ratio`,
reconciliation lag). Real vendors are Tronsell / Netts / CatFee — show them by
name, not "Provider A/B/C."

### 5.8 Payouts

From C5: dispatch status (DISPATCHING/SETTLED/HELD), attempt sub-status
(BUILT/SIGNED/BROADCAST/CONFIRMED/FAILED), `tron_txid`, slot used. Clearly show
Sweep-tier orders as blocked on the undeployed multisend contract (§2) rather
than "stuck."

### 5.9 Customers

Entirely blocked on §3's new gateway customer CRUD. Once built: list/detail
exactly per original §14, minus anything implying API-secret display (correct
as originally written — never show `api_key_hash` or a raw key outside the
one-time creation response).

### 5.10 Wallets

Merge C5's `GET /v1/slots` (payout side, with real ACTIVE/RETIRING/RETIRED
status and the real $50k/5,000-tx caps) with a new paginated C2 address listing
(§3). Label every key's custody honestly per §3 item 3 — today that's
`kms_fake` everywhere, not "cold" or "hot" in the HSM sense original §15
assumes exists.

### 5.11 Providers

BSC RPC pool health is real (C2's `/v1/system/providers`) and is exactly where
the "detect two configured providers that are actually the same underlying
provider" check from original §16 belongs — build that check for real (compare
resolved chain ID + a cheap on-chain fact like current block hash across
configured endpoints; flag a match). Energy vendor health comes from C4's
`/v1/system/prices`. There is no TRON node provider-health endpoint — say so
rather than fabricating one; if it matters enough, the smallest addition is a
`/v1/system/tron-health` on the dispatcher wrapping its existing gRPC client
with a block-height check.

### 5.12 System Health

Assembles every real endpoint from §0's health table into one page. Where a
service has no equivalent (S1, gateway, TRON), show it as "not instrumented
yet," not a fabricated green check.

### 5.13 Needs Attention

As specified in original §18 — every example item maps to a real field already
identified in §5.2's Needs Attention list. Don't add invented categories.

### 5.14 Configuration

Per §3 item 2 — v1 is read-only with source attribution and a "requires
redeploy" note; do not promise live editing the backend cannot yet do.

### 5.15 Audit Log

Reads the new `ops_audit_log` table from §3 item 1. Every mutating action this
UI performs (hold release, slot retire, fallback-event resolve, customer
suspend, order-detail actions) must write here — this is the actual mechanism,
not a UI-only feature.

### 5.16 Global search / blockchain UX / real-time / error UX / loading states

Original §23–§28 are sound as written and need no correction — build them
against the real entities and endpoints named throughout this document.
Real-time updates: **no WebSocket/SSE exists on any service today** — use
polling only, with the configurable interval original §25 asks for, and keep
it conservative (this hits seven backend services plus, transitively, BSC/TRON
RPC providers with real rate limits — the routing-weight/ceiling machinery in
C4 exists specifically because vendor rate limits are a real operational
constraint in this system).

---

## 6. Routes

```
/                      Overview
/orders                Orders (filters in URL)
/orders/:externalId    Order detail
/customers             Customers
/customers/:id         Customer detail
/deposits               Deposits
/payouts               Payouts
/energy                Energy
/screening              Screening
/providers              Providers
/wallets                Wallets
/configuration           Configuration (read-only v1)
/audit                  Audit Log
/health                  System Health
/login                  Operator login (new — see §3)
```

---

## 7. Security

- The frontend never holds a service-to-service bearer token or a customer
  API key. All of those live only in the new Ops BFF's own environment,
  exactly like every existing service's own tokens.
- Operator auth + RBAC is enforced in the Ops BFF (§3), not the frontend. The
  frontend reflects the operator's role (hide/disable actions) purely as a UX
  convenience; every mutating BFF route re-checks the role server-side
  regardless of what the UI sent.
- Every mutating action writes an audit row (§5.15) before returning success
  to the UI.

---

## 8. Definition of done (phased, matching what's real)

**Phase 1 — backend prerequisites (§3):** Ops BFF with real operator auth/RBAC
and audit log stood up; gateway wired into `docker-compose.yml`; gateway
customer CRUD built; C2 paginated address listing and C6 webhook-deliveries
listing built; each service's `/v1/system/config` read added.

**Phase 2 — UI, against the real backend above:**
1. App starts and authenticates a real operator.
2. Overview shows live per-service health and real pipeline counts — no
   service faked, no state invented.
3. Orders can be searched, filtered, paginated server-side, and opened to a
   correctly-joined detail view.
4. Deposits, Payouts, Energy, Screening pages show only fields that exist on
   the real APIs in §0, with the documented gaps (no confirmation counter, no
   TRON health, Sweep blocked on multisend) shown as such, not hidden.
5. Customers can be created, listed, suspended, and key-rotated once Phase 1's
   gateway CRUD exists.
6. Every mutating action (hold release, slot retire, event resolve, customer
   suspend) is role-gated server-side and produces a real audit row visible on
   `/audit`.
7. Configuration page is honest: read-only, sourced, with a clear path to
   change (redeploy), not a fake live-edit flow.
8. No fabricated data anywhere — including no assumed order id like
   `realrun2-3`: no such id is recorded anywhere in this repository's docs,
   commits, or code as of this write-up. If you want to demo against a real
   settled order, create one for real through the actual gateway
   `POST /v1/quotes` → `POST /v1/orders` flow (or the `proofrun` driver's
   `POST /v1/payouts`, which is scaffolding but does create real C1/C2 state),
   and use whatever `external_id` that run actually produces.
