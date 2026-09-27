# C2 — Deposit watcher (BSC): sequenced build prompts

> **Background (history).** Written during the design and build phase and kept as a
> record. You don't need it to run or change today's product: start at
> [docs/README.md](../README.md).

**Target:** Go + PostgreSQL, matching C1's stack. **Consumer:** an AI coding agent (Claude Code or equivalent), same usage pattern as `c1-ledger-build-prompts.md`.

**How to use this file.** Paste §0 once at the start of the session — standing context the agent must hold for every chunk. Then paste chunks C2.0 → C2.10 one at a time, in order. Do not move to the next chunk until the current chunk's acceptance criteria pass against a real (or forked/simulated) chain, not a mock of your own assumptions about one.

**Before you start:** read the two call-outs immediately below. The first is four interface gaps found between this spec and the C1 that's already built — one (#1, the reorg endpoint) has since been closed by C1.11 and is marked as such; the other three are still open and none of them are this document's to close unilaterally. The second is new information about BSC itself that changes a number already published in `product-operations-architecture.md`.

**Update, 2 Sep 2026:** gap #1 below is closed — C1.11 added `POST /v1/orders/{external_id}/reorg` and `POST /v1/entries/{id}/reversal`, committed to the `ledger/` repo at `41c4bd0`. §A and C2.6 are updated to the actual shipped shape, which differs from this document's original guess in a couple of small but load-bearing ways (no `actor` or `occurred_at` in the reorg body; the transition example was missing `asset` on every line and had two fields — `idempotency_key`, `actor` — that don't exist on the real DTO and would be rejected outright). Gaps #2 and #3 are still open.

---

## Read this first — four gaps between C2 and the C1 that already exists

C1 is built (`c1-ledger-build-prompts.md`, all of C1.0–C1.10, verified against its own test suite as of the 1 Sep 2026 audit — see `c1-scenario-catalog.md`). That means C2's interface to it isn't a design choice anymore, it's a contract to read carefully — and reading it carefully surfaces four things C1's own spec describes but C1's own HTTP surface (§C1.8) doesn't actually expose a way to do. Each of these blocks a specific C2 chunk below; treat them as prerequisites, not as C2's problem to route around.

1. ~~There is no reorg-report endpoint.~~ **Closed — C1.11, commit `41c4bd0`.** This gap was real when first written: `HandleDepositReorg(ctx, orderID, originalEntryKey, actor)` was an internal Go function on C1's `internal/orders` package with no HTTP path to it, and C1.8's endpoint list had nothing that mapped to scenario B (which isn't a transition — it posts a loss entry and halts the system out-of-band, not something the generic transitions endpoint could be stretched to cover). C1.11 added `POST /v1/orders/{external_id}/reorg` as the fix, plus `POST /v1/entries/{id}/reversal` (C2.6 needs both — see §A). **The actual shape differs from the guess this document originally made below**, in two ways worth knowing before writing C2.6 against it:
   - Request body is `{"original_entry_key": "<idempotency key>"}` only. No `actor` field — every C1 write derives the actor from the bearer token, never a request body field, matching every other endpoint. No `occurred_at` — the handler timestamps the reversal itself.
   - Response is the `Order`, not a scenario tag — C1 still decides A vs B internally from the order's own state and returns whatever it left the order as; the caller was never going to be trusted to name the scenario itself, that was always the point of routing through `HandleDepositReorg`, so nothing here actually changed except that it's reachable now.

2. **Nothing owns sweeping the per-order deposit account, and no entry type exists for it.** Walk §B's worked example in the C1 doc: E1 credits `asset:bsc:deposit:1042` with the full $3,000. E2 (the conversion) never touches that account again — it moves `liability:customer:acme` and `position:corridor` instead. The $3,000 of actual on-chain BEP20 sitting at the physical deposit address is never shown leaving it. Physically, those tokens have to move to wherever the treasury actually holds BEP20 before E5's `asset:cex:<venue>` rebalance leg makes sense — and `component-map.md` explicitly lists "sweeping deposits" under what C2 does **not** own, without naming who does. S1 (key management) is the only component with signing authority over deposit addresses, so it's the most likely owner, but nothing currently specifies the entry type (candidate: `deposit_swept`, `DR asset:bsc:treasury CR asset:bsc:deposit:<order_id>` — `asset:bsc:treasury` doesn't exist in §A's chart of accounts yet either) or who calls C1 to post it. **This needs an owner and a chart-of-accounts addition before C2 (or S1) is complete**, even though the sweep operation itself is out of C2's scope.

3. **Deposit-after-expiry has no product decision behind it, only an engineering placeholder.** `c1-scenario-catalog.md` §1.5 and the component map both flag this as a hard part, but "hard part to handle" and "policy for what handling means" are different things. If a deposit reaches finality after `quote_expires_at` has already moved the order to `expired`, C2's transition call to `funded` gets `ErrIllegalTransition` — correctly, per C1.5's table. What happens to the customer's money at that point is a business decision (auto-refund at the original quote? re-quote at current price and require confirmation? hold for manual review?), not something C2.8 below can resolve by itself. The chunk builds the mechanism to detect and safely park this case; it cannot build the policy for what happens next.

4. **The BSC finality assumption behind the published tier SLAs is stale.** See the next section — this one C2 can and should resolve itself, because it's a technical input, not a business decision.

---

## Read this second — BSC's block time and finality have materially changed

`component-map.md` flagged this honestly: *"Verify before coding: current BSC block time and therefore the wall-clock cost of 15 confirmations — this is a direct input to the tier SLAs."* It was never verified. Here's what's true as of this write-up (1 Sep 2026), checked against current sources rather than assumed from whenever the original 15-confirmation figure was chosen:

BNB Chain's **Fermi hard fork went live 14 January 2026**, cutting block time to **~0.45 seconds** and bringing chain-level finality to **~1 second**, via BSC's fast-finality consensus mechanism (BEP-126) — the same style of BFT finality gadget Ethereum uses post-Merge, exposed over standard JSON-RPC as a `"finalized"` block tag, not something bespoke to this product. Fermi followed the earlier Maxwell hard fork (0.75s blocks), and a further Pasteur upgrade (activated ~25 Aug 2026, throughput-focused) landed after that without changing block time again as far as public reporting shows.

What this means concretely: **15 raw confirmations, at current block time, is roughly 6.75 seconds of block production** — not the tens of seconds a 15-confirmation policy would have implied under BSC's pre-2026 ~3-second blocks, which is almost certainly the assumption baked into the *"4m 20s median custody window on Standard"* figure in `product-operations-architecture.md`'s decision 1. That number should be re-measured once C2 exists, not trusted as-is — it's very likely to come down, which is good for the tier SLA story but needs a real measurement against your actual RPC providers before it's republished anywhere customer-facing.

There are now two legitimate ways to build the finality decision in C2.5, and the choice matters enough that it's called out as §B below rather than buried in a chunk:

- **(a) Depth-based, as originally planned** — track a fixed confirmation count (still 15, or re-derived) via a rolling block-hash window, independent of anything the chain claims about its own finality.
- **(b) Finality-tag-based** — poll `eth_getBlockByNumber("finalized")` and treat a log as final once its block height is at or below the chain-reported finalized height, leaning on BSC's own BEP-126 consensus finality instead of an arbitrary depth choice.

C2.5 below builds (b) as the primary path with (a) retained underneath it for defense-in-depth on the pre-final `deposit.detected` advisory event — see §B for the reasoning and the risk in trusting (b) blindly.

---

## §0 — Standing context (paste once)

```
You are building C2, the deposit watcher of a USDT cross-network settlement system
(BEP20 -> TRC20 payouts). C1, the ledger core, is already built and is the only
system of record C2 writes to — C2 has no ledger of its own beyond the local state
it needs to do its own job (address book, block cursor, dedupe, reorg window).

STACK
- Go 1.22+, PostgreSQL 16 — C2's own database, separate from C1's. C2 is a
  standalone service; it calls C1 over HTTP like every other component does.
- go-ethereum's ethclient for BSC RPC (BSC is EVM-compatible; this is the de facto
  standard client, not a bespoke choice).
- HD derivation from an extended PUBLIC key only (BIP32/44) to generate watch-only
  BEP20 addresses. No private key material of any kind is ever held, generated,
  or handled by this service — that is entirely S1's domain.
- pgx/v5, goose migrations, chi routing, log/slog, testify, testcontainers-go.
- An HTTP client for calling C1's API, built against the exact contract in §A —
  not against assumptions about what C1 "probably" exposes.

WHAT C2 IS
The component that turns an inbound BEP20 USDT transfer into a confirmed, correctly
attributed credit in C1. It derives and assigns deposit addresses, ingests BSC
blocks, parses and classifies Transfer logs against the USDT BEP20 contract, tracks
confirmations to finality, and reports both routine pre-dispatch reorgs and (once
C1 exposes the endpoint from the gap list above) post-settlement reorgs.

WHAT C2 IS NOT — do not build any of this, do not import libraries for it
- No private keys, no signing, no wallets. Address derivation is public-key-only.
- No sweeping of deposited funds. C2 observes; it never constructs or broadcasts
  a transaction of any kind.
- No screening (C3), no payout (C5), no pricing or quoting (C6).
- No funding deposit addresses with BNB for gas — that's an operational concern
  belonging to whoever manages the HD wallet's gas float, out of scope here.
- No ledger of money. C2's own database holds operational state (addresses,
  cursor, dedupe, reorg window) — never a balance, never something that could be
  mistaken for a second source of truth about what anyone is owed.
If a chunk seems to require any of the above, you have misread it. Stop and say so.

NON-NEGOTIABLE INVARIANTS
1. Read-only chain access. C2 never holds a private key for any address it watches.
2. Every credit reported to C1 is idempotent on tx_hash:log_index — never a
   generated id — matching C1.3's own documented key convention exactly:
   "watcher:deposit_final:<tx_hash>:<log_index>".
3. C2 never invents an entry shape, entry_type, or account code C1's chart of
   accounts (in c1-ledger-build-prompts.md §A) doesn't already define. C2 fills in
   transaction-specific values into shapes C1 already understands.
4. Finality, once asserted for a given (tx_hash, log_index), is monotonic. It is
   never silently un-asserted — only reversed through an explicit, auditable event
   that becomes a halt on C1's side (scenario B territory). If C2 is ever tempted
   to "just re-check and quietly correct" a past finality call, that is a bug.
5. No single RPC provider is trusted alone for a finality decision. Every
   deposit.final requires independent agreement from at least two providers, both
   on the log content and on the block hash / finalized-tag height involved.
6. Every address is assigned to exactly one order for its entire lifetime and is
   never reassigned to a different order after retirement, even if the original
   order never funded. Reuse turns every classification edge case (wrong-token,
   zero-value, late deposit) into a live attribution bug instead of a filtered one.
7. All timestamps recorded against a deposit are the block timestamp at the
   relevant chain height, in UTC — never wall-clock time at the moment C2 happened
   to observe it, which can lag the real event by however far behind the tip C2's
   cursor is.

STYLE
- Small packages, explicit boundaries: internal/chain (RPC + parsing),
  internal/addresses, internal/finality (confirmation/reorg policy),
  internal/ledgerclient (the C1 HTTP contract, isolated so nothing else in C2
  constructs a C1 request by hand), internal/httpapi.
- Errors are typed, wrapped with %w, and every error reaching the HTTP layer maps
  to one stable error code — same discipline C1 already established.
- Tests are the deliverable. A chunk involving chain interaction is not done until
  it's proven against a forked/simulated chain capable of producing a real reorg,
  not merely against hand-constructed fixtures that assume the happy path.
- Do not add features, endpoints, or abstractions a chunk did not ask for.
```

---

## §A — Interface contract with C1

Everything C2 sends to C1 must match this exactly. This is derived from C1's own published contract (`c1-ledger-build-prompts.md` §A/§B/C1.5/C1.8), not re-derived — if anything here looks inconsistent with that document, that document wins and this one is wrong.

### Accounts C2's entries touch (from C1's chart of accounts)

| Code | Type | Asset | C2's relationship to it |
|---|---|---|---|
| `asset:bsc:deposit:<order_id>` | ASSET | USDT_BEP20 | Debited by E1 when C2 reports finality. One per order, created implicitly by C1 on first use (C1.1: account creation is idempotent on code). |
| `liability:customer:<customer_id>` | LIABILITY | USDT_BEP20 | Credited by the same E1 entry. C2 does not choose this code — it comes from the order record C2 already has (see order lookup below). |

### The one call C2 makes to credit a deposit

A single HTTP call, not two — C1.5 is explicit that a transition requiring an entry commits the entry and the state change atomically, and C1.8's transition endpoint takes the entry inline:

```
POST /v1/orders/{external_id}/transitions
Idempotency-Key: watcher:deposit_final:<tx_hash>:<log_index>

{
  "to_state": "funded",
  "expected_version": <order's current version, from a prior GET>,
  "reason": "bep20_deposit_final",
  "occurred_at": "<block timestamp at finality, RFC3339 UTC>",
  "entry": {
    "entry_type": "deposit_final",
    "occurred_at": "<block timestamp at finality, RFC3339 UTC>",
    "lines": [
      {"account_code": "asset:bsc:deposit:<order_id>", "asset": "USDT_BEP20", "amount": "<decimal string>"},
      {"account_code": "liability:customer:<customer_id>", "asset": "USDT_BEP20", "amount": "-<decimal string>"}
    ]
  }
}
```

Two things this example gets right that are easy to get wrong by analogy with other APIs: **there is no `idempotency_key` or `actor` field anywhere in this body.** C1's JSON decoder rejects unknown fields outright (`DisallowUnknownFields`), so adding either would fail the whole request with `400 invalid_request`, not silently ignore it. The single `Idempotency-Key` header becomes the identity of both the transition attempt and the journal entry it posts — one header, two things it's the key for, by construction. Actor is derived server-side from whichever bearer token authenticated the call, on every C1 write, no exceptions — it's not a value any caller ever supplies. And note `occurred_at` appears **twice**, once for the transition record and once for the entry it posts — they'll be the same value here (the block timestamp) but they're genuinely two separate fields on two separate DTOs, both required.

Amounts are decimal strings, never JSON numbers — C1.8 rejects a JSON number outright, and it's called out as "the most likely way this system loses money." C2's own money type should therefore be the same `int64` minor-units representation C1 uses internally, formatted through the same rules (6 decimals for USDT_BEP20), so a value never round-trips through a float at any point between the chain log and the HTTP body.

### Discovering what to watch for

C1 doesn't push order data to C2 — nothing in C1.8 is a webhook or event stream, and building one is explicitly C6's job (decision 5), not C1's or C2's. So the direction of discovery has to run the other way: **whoever creates the order (C6, once it exists) must call C2**, not the reverse. That means C2 needs an inbound endpoint of its own — see C2.9 — where the caller hands over `order_id`, `external_id`, `customer_id`, `quoted_at`, `quote_expires_at`, and gets back an assigned deposit address. C2 then needs to independently learn when that order leaves a state it cares about (funded, or terminal), which — absent a push mechanism — means C2 polls `GET /v1/orders/{external_id}` on whatever addresses it's still actively watching. This polling dependency is a real cost (nothing here makes it free or instant) and should be treated as a placeholder for a proper event mechanism once C6 exists, not a permanent design.

### Reporting a reorg

C1.11 added the endpoint (see gap #1). C2.6 calls it with only the original deposit's idempotency key:

```
POST /v1/orders/{external_id}/reorg
Idempotency-Key: watcher:reorg_report:<tx_hash>:<log_index>

{
  "original_entry_key": "watcher:deposit_final:<tx_hash>:<log_index>"
}
```

No `actor` (from the bearer token, as everywhere else), no `occurred_at`, no scenario flag — C1 decides internally whether the order's current state makes this scenario A or scenario B from `HandleDepositReorg`, and returns the order however it left it (back to `quoted` for A; unchanged, and now halted, for B). C2 never gets to assert which scenario applies; that was always the reason for routing through a single ledger-side function instead of two separate calls.

---

## §B — The confirmation and finality model (crux decision — read the "Read this second" section above first)

Two mechanisms, used together, not as alternatives:

**Primary — finality-tag-based.** Poll `eth_getBlockByNumber("finalized", false)` on each configured RPC provider. A log is eligible for `deposit.final` once its block's height is ≤ every polled provider's reported finalized height, and at least two providers agree on the block hash at that height. This piggybacks on BSC's own BEP-126 fast-finality consensus (~1 second under current conditions) rather than a hand-chosen depth, and — because it's the chain's own consensus committing to that block — nothing shy of a >1/3 validator equivocation can undo it, a fundamentally different (and much stronger) guarantee than "N confirmations have gone by."

**Defense in depth — depth-based, for the pre-final advisory only.** `deposit.detected` (the 0-conf, advisory-only event mentioned in the component map) fires the moment a Transfer log is seen in any block, before any finality claim. Between detection and the finalized tag catching up, C2 still tracks a rolling block-hash window exactly as originally planned, purely to detect and log routine pre-final reorgs for observability — this is expected, routine behavior per the scenario catalog ("reorg before dispatch... must be routine"), not an error condition, and it never by itself triggers a report to C1.

**Why not finality-tag alone:** not every RPC provider correctly or promptly implements BEP-126's finalized tag — older or lagging nodes can under-report it. This is exactly why invariant 5 (≥2 provider agreement) applies to the finalized-height claim itself, not just to raw block hashes. A provider that never advances its finalized tag, or advances it inconsistently with its peers, is a provider to eject from the agreement set and alert on — not a provider to trust because it's one of only two configured.

**Why not depth-based alone:** it would ignore new, verifiable information about how BSC's own consensus already works, and it's what produced the stale "tens of seconds" assumption behind the currently-published custody-window figure in the first place.

---

## C2.0 — Scaffold and the address derivation primitive

```
Build the repository skeleton and watch-only address derivation. Nothing else.

1. Go module `depositwatcher`. Layout mirrors C1's:
   cmd/watcherd/main.go
   internal/addresses/
   internal/db/
   migrations/
   docker-compose.yml (Postgres 16 only — no chain node; tests run against a
     real testnet RPC or a local fork, configured via env, not bundled)
   Makefile (build, test, test-integration, migrate-up, migrate-down, lint)

2. internal/addresses:
   - DeriveAddress(xpub string, index uint32) (Address, error) — BIP32 public
     derivation only. Reject any input that looks like it might be a private key
     or a seed phrase (a deliberate guard, not just an unused code path — this
     service must be structurally incapable of ever handling one).
   - Deterministic: same (xpub, index) always yields the same address.
   - Property test: 10,000 random indices, every derived address is a valid
     checksum EVM address (EIP-55), no collisions.

3. internal/db: pgx pool setup, config from env, Tx helper — same pattern as C1.

4. Migration 0001: role split mirroring C1's philosophy — a `watcher_writer` role
   for the service, no destructive-DML testing needed here since this database
   isn't append-only by design (address assignment state legitimately updates:
   an address's status field, watched -> funded -> retired).

5. cmd/watcherd: starts, connects, serves GET /healthz, shuts down on SIGTERM.

ACCEPTANCE
- `make test` green.
- Derivation property test as above.
- Feeding DeriveAddress something that looks like a WIF private key or a BIP39
  seed phrase is rejected before any derivation is attempted.
- No file in internal/addresses imports anything that could sign a transaction
  (no private-key-capable library in that package's dependency tree at all —
  enforce with a dependency-graph check in CI, not just code review).

DO NOT
- Do not add any chain-RPC code yet. This chunk is offline and deterministic.
- Do not add a private key type anywhere in this module, even unused, even for
  "future use." If S1 ever needs to share code with this service, that's a
  separate, deliberate decision — not a default.
```

---

## C2.1 — Address lifecycle and assignment store

```
Build the address book: assignment, status, retirement. No chain watching yet.

TABLE watched_addresses
  id              bigserial primary key
  address         text not null unique          -- EIP-55 checksummed
  derivation_index bigint not null unique
  order_id        bigint not null                -- C1's internal order id
  external_id     text not null                  -- for correlating with C1's API
  customer_id     text not null
  status          address_status not null        -- WATCHING, FUNDED, RETIRED
  quoted_at       timestamptz not null
  quote_expires_at timestamptz not null
  assigned_at     timestamptz not null default now()
  retired_at      timestamptz null
  retired_reason  text null                       -- settled | refunded | expired | superseded

RULES
- One row per order, enforced by a unique constraint on order_id — an order is
  never assigned a second address.
- derivation_index increments monotonically and is never reused, even for a
  retired address's slot — reuse is exactly the collision invariant 6 exists to
  prevent, and the cheapest way to guarantee it is to never recycle the index.
- Retirement is explicit (a status transition with a reason), never implicit
  from simply no longer polling an address.
- WATCHING -> FUNDED -> RETIRED is the only legal path; WATCHING -> RETIRED
  directly is legal (order expired with nothing ever detected); FUNDED -> back to
  WATCHING is legal (the scenario-A reorg case, once C2.6 exists to drive it).

BUILD internal/addresses (extend)
  Assign(ctx, order_id, external_id, customer_id, quoted_at, quote_expires_at)
    (Address, error) — idempotent on order_id: calling twice for the same order
    returns the same address, does not derive a second one.
  MarkFunded(ctx, order_id) error
  Retire(ctx, order_id, reason) error
  ListActive(ctx) ([]WatchedAddress, error) — everything not RETIRED, this is
    what the block-ingestion loop (C2.3) filters logs against.

ACCEPTANCE
- Assign called twice with the same order_id returns the identical address both
  times and derivation_index only advances once.
- Concurrent Assign calls for 100 distinct orders: 100 distinct addresses, no
  duplicate derivation_index, no gaps that matter (gaps from a failed attempt are
  fine — reuse is the invariant, not density).
- Illegal status transitions (e.g. RETIRED -> WATCHING) rejected at the DB level
  via a CHECK or trigger, not just in Go — same defense-in-depth posture C1 used
  for its own invariants.
```

---

## C2.2 — RPC client abstraction and multi-provider agreement

```
Build the multi-provider layer before anything reads a single log. Getting this
wrong is invisible until the day one provider quietly diverges from the others.

BUILD internal/chain
  type Provider struct { Name string; Client *ethclient.Client }
  type Pool struct { providers []Provider; minAgreement int }

  Pool.LatestFinalized(ctx) (height uint64, agreedHash Hash, error)
    - Calls eth_getBlockByNumber("finalized", false) on every configured
      provider concurrently.
    - Requires at least minAgreement (config, default 2) providers to report the
      SAME height and hash before returning success.
    - A provider that times out, errors, or disagrees is excluded from this
      round's agreement count and logged — it is not treated as agreeing by
      default, and a round with fewer than minAgreement live, agreeing providers
      is a hard error, not a degraded-but-successful result.

  Pool.LogsAt(ctx, fromBlock, toBlock, contractAddress, topics) ([]Log, error)
    - Fetches from a primary provider; cross-checks a sample (or all, at MVP
      volume — ~4 deposits/hour peak per component-map, this is cheap) against a
      second provider before returning. A mismatch is a hard error, surfaced
      loudly, never silently resolved by "trust the primary."

CONFIG
- At least 2 providers required to start; refuse to boot with fewer than 2 — this
  isn't a runtime-degradable feature, it's invariant 5.
- Provider health tracked continuously (rolling error rate, rolling agreement
  rate), exposed on /system/providers for ops visibility — modeled after C1.7's
  GET /system/invariants, same "make the health of the thing visible" instinct.

ACCEPTANCE
- Against a local multi-node test setup (or mocked providers with deliberately
  injected divergence): two providers agreeing succeeds; providers disagreeing on
  hash at the same height is a hard error, not a silent pick-the-first; a
  provider timing out is excluded and the round still succeeds if the remaining
  providers still meet minAgreement, fails loudly if they don't.
- A provider that has disagreed or failed on the last N consecutive rounds is
  flagged unhealthy on /system/providers without operator action.
```

---

## C2.3 — Block ingestion loop and cursor

```
Build the loop that walks the chain forward. No log parsing yet — just cursor
correctness.

TABLE ingestion_cursor
  id              smallint primary key default 1 check (id = 1)
  last_scanned    bigint not null default 0
  updated_at      timestamptz not null default now()

TABLE seen_blocks           -- rolling window, for the pre-final advisory reorg
                             -- check described in §B, NOT the finality decision
  height          bigint not null
  hash            text not null
  parent_hash     text not null
  observed_at     timestamptz not null default now()
  primary key (height)

BUILD internal/chain (extend)
  RunIngestionLoop(ctx, pool *Pool, interval time.Duration)
    - Each tick: fetch current chain tip from the provider pool, scan
      [last_scanned+1, tip], for each block store height/hash/parent_hash in
      seen_blocks, detect a parent-hash mismatch against the previously stored
      block at height-1 (a pre-final reorg, advisory only per §B), advance
      last_scanned, prune seen_blocks entries older than the configured window
      (e.g. 200 blocks — generous at 0.45s/block, still under 2 minutes of
      history, cheap to keep).
    - Idempotent and resumable: a restart picks up from last_scanned, re-scanning
      nothing already committed, never skipping a block.

ACCEPTANCE
- Kill the process mid-scan (real SIGKILL in the integration test, not a
  graceful shutdown) and restart: no block is skipped, no block is double-
  processed in a way that has an externally visible effect (log parsing doesn't
  exist yet in this chunk, so "double-processed" here just means cursor
  correctness).
- Inject a parent-hash mismatch at a height inside the seen_blocks window:
  logged as a pre-final reorg event, loop continues, does not crash or stall.
- seen_blocks never grows unbounded — prune is verified under a long-running
  soak test, not just asserted in code review.
```

---

## C2.4 — Transfer-log parsing and classification

```
Parse the chain's Transfer events against the USDT BEP20 contract and classify
each one against what C2 is watching for. No finality logic yet — this chunk
turns raw logs into typed, classified candidates.

CONFIG
- USDT_BEP20_CONTRACT = 0x55d398326f99059fF775485246999027B3197955 (Binance-Peg
  BSC-USD). Verify this against BscScan yourself before hardcoding it — this
  document cites it from a live BscScan lookup as of 1 Sep 2026, but a value this
  load-bearing deserves your own confirmation, not a copy-paste from a spec.
- Transfer event topic: keccak256("Transfer(address,address,uint256)").

BUILD internal/chain (extend)
  type Classification int
  const (
    Exact Classification = iota   // matches order's quoted amount_in exactly
    Overpay                       // more than quoted
    Underpay                      // less than quoted, above dust floor
    Dust                          // below a configured floor, e.g. < $1 equiv
    WrongToken                    // topic/contract mismatch that slipped the
                                   // filter somehow — should be near-impossible
                                   // given the filter, but classify defensively
    ZeroValue                     // Transfer event with amount 0 (legal on-chain,
                                   // meaningless here)
  )

  ParseTransferLog(log) (from, to Address, amount money.Amount, error)
  ClassifyAgainstOrder(amount money.Amount, order WatchedAddress) Classification

RULES
- Only logs where `to` matches a currently-WATCHING or FUNDED address in C2's own
  address book are candidates at all — everything else is filtered before
  classification, not classified and then discarded, to keep the hot path cheap
  at scale (irrelevant given ~100 deposits/day, but the filter-first order is
  also what makes "wrong-token" and "zero-value" genuinely rare paths rather than
  the common case).
- A log to a RETIRED address is a distinct, logged case (late deposit) — handed
  to C2.8, not silently dropped and not silently credited.
- Classification never decides what ledger entry to post — that's still C1's
  business once C2 reports; classification only decides what C2 does next
  (proceed to finality tracking for Exact/Overpay/Underpay, still track Dust for
  visibility but don't necessarily rush it to finality, flag WrongToken/ZeroValue
  as anomalies for operator visibility, never auto-credit them).

ACCEPTANCE
- A synthetic contract deployed on a local fork emitting Transfer events: exact,
  over, under, dust, and zero-value transfers each classify correctly.
- A Transfer log from a different ERC20-style contract (not the USDT BEP20
  address) never reaches classification, filtered upstream.
- A log to an address not in the address book at all (never assigned) is ignored
  entirely, not logged as an anomaly — this is expected noise on a shared chain,
  not a signal.
```

---

## C2.5 — Confirmation and finality policy

```
Implement §B: the finality-tag primary path plus the depth-based advisory path,
together. This is the component's own crux chunk — do not simplify it to "just
count 15 confirmations," that was the pre-Fermi plan and it's demonstrably stale.

BUILD internal/finality
  OnLogObserved(ctx, log, classification) error
    - Fires deposit.detected (internal event, see C2.7 for what "fires" means —
      this chunk defines when, not how it's transmitted) immediately, 0-conf,
      advisory only. Never touches C1.

  CheckFinality(ctx, pool *Pool) error  — called on a ticker, e.g. every 2-3s
    - Pool.LatestFinalized() to get the agreed finalized height.
    - For every tracked candidate log at height <= finalized height, with the
      required provider agreement (invariant 5) also holding at that height:
      mark it final, hand off to C2.7 for reporting to C1.
    - A candidate that has been pending finality for longer than a generous
      configured ceiling (e.g. 5 minutes — orders of magnitude beyond the ~1s
      current expectation) is not force-finalized; it's alerted on. A finality
      delay that large means something is wrong with the provider pool or the
      chain itself, not that the wait should be skipped.

ACCEPTANCE
- Against a forked/simulated chain capable of advancing a finalized tag on
  command: a log becomes eligible for reporting exactly when the finalized
  height passes it, not one block early, not one block late.
- A log whose block gets reorged out BEFORE the finalized tag ever reaches it:
  never reported to C1 at all — this is the routine, expected case, and the test
  asserts silence, not a reorg report (reorg reports are for AFTER finality,
  C2.6's job).
- Provider disagreement at the moment a candidate would otherwise finalize:
  finalization withheld, not granted on a majority-of-one basis.
- The stale-pending alert fires in a test that deliberately stalls one provider's
  finalized tag past the ceiling.
```

---

## C2.6 — Reorg handling

```
C1.11 closed the endpoint gap this chunk originally depended on (see gap #1) —
POST /v1/orders/{external_id}/reorg exists, is tested, and is committed at
c93dfb9..41c4bd0 in the ledger repo. This chunk is no longer blocked; build it
whole, including the live call, against a real running C1 instance.

SCOPE
- Detecting a post-final reorg: CheckFinality's own finalized-height tracking is
  the source of truth here too — if a block C2 previously reported as final is
  ever contradicted by a later finalized-height read from the provider pool
  (i.e., the chain's own consensus finality was itself violated), that is an
  extraordinary event, categorically different from the routine pre-final case
  in C2.3. Log it as a distinct, loud event type from the moment it's detected.
- ReportReorg(ctx, orderExternalID, originalEntryKey) error — POSTs to
  /v1/orders/{external_id}/reorg with body {"original_entry_key": <the exact
  idempotency key C2.7 used for that deposit's deposit_final entry>}. No actor
  field (the bearer token supplies it), no occurred_at, no scenario flag — see
  §A's "Reporting a reorg" for the full shape and why it's this narrow. Behind
  an interface (ReorgReporter) anyway, not for the reason it used to be (an
  unreachable endpoint) but so the detection/bookkeeping logic upstream of the
  call can still be tested against a fake without needing a live C1 for every
  run.
- Map C1's response: order back in `quoted` is scenario A (routine, log at
  info); order unchanged and C1 now reports halted (poll GET /v1/system/halt
  or watch for 423 on the next unrelated call) is scenario B (a real loss,
  alert loudly — this is exactly the "impossible to handle quietly" case
  c1-ledger-build-prompts.md's C1.6 was built to guarantee).

ACCEPTANCE
- A finalized-height contradiction is detected and classified within one
  CheckFinality tick of occurring, against a forked chain that can be made to
  violate its own prior finality (this requires deliberately misconfiguring the
  simulated finality gadget — document how, since it's not a normal chain
  behavior to reproduce).
- ReportReorg is called exactly once per contradicted candidate, idempotently —
  a retry of the surrounding process does not call it twice, and calling it
  twice on purpose (simulating a retry) hits C1's own already_reversed path
  cleanly rather than erroring in some C2-side way.
- Against a real running C1 instance (testcontainers, same pattern C1 itself
  uses): scenario A (order still funded/pre-dispatch) returns the order to
  `quoted` with a zero net effect on C1's trial balance; scenario B (order
  already dispatching/settled) results in expense:loss:reorg holding exactly
  the reported amount_out and C1 reporting halted — confirming C1's actual
  response matches c1-ledger-build-prompts.md's C1.6 acceptance criteria, not
  a mock of it.
```

---

## C2.7 — Emission to C1

```
The only chunk that actually writes to C1. Everything upstream produces a
finalized, classified candidate; this chunk turns that into the one HTTP call
defined in §A.

BUILD internal/ledgerclient
  ReportDepositFinal(ctx, candidate) error
    - Builds exactly the request in §A. Idempotency-Key header set to the same
      key as the entry's own idempotency_key — belt and suspenders, matching
      C1.8's rule that every write handler requires the header.
    - On 409 illegal_transition: this means the order was no longer in `quoted`
      by the time C2 got here — hand off to C2.8, do not retry blindly and do
      not swallow the error.
    - On 409 version_conflict: re-fetch the order's current version and retry
      once with the fresh version — this is a legitimate race (something else
      touched the order between C2's read and write), not a fatal condition.
    - On 423 system_halted: back off and retry on a longer interval — halted
      means C1 is deliberately refusing money-moving transitions right now, not
      that anything about this specific report is wrong.
    - On 409 idempotency_conflict: this should be structurally impossible if
      C2's own key construction is correct (same tx_hash:log_index always
      produces the same payload) — treat it as a P1 bug alert, not a retry case.

ACCEPTANCE
- Against a real running C1 instance (testcontainers, same pattern C1 itself
  uses): a finalized candidate results in the order transitioning to `funded`
  and the exact E1 entry from §A landing in C1's journal.
- Replaying the same candidate (simulating a C2 restart that re-processes a
  block it already handled) hits C1's idempotency path and does not double-
  credit — verified by checking C1's own balance, not just C2's local state.
- A 409 illegal_transition (order already expired) is routed to C2.8 and does
  not retry in a loop.
- A 423 system_halted is retried on backoff and eventually succeeds once the
  test harness clears the halt.
```

---

## C2.8 — Deposit-after-expiry and retired-address handling

```
Builds the mechanism from gap #3 above. Does NOT decide the business policy —
implements whatever policy is decided, behind an interface, with a safe default
of "do nothing automatically, alert a human" until that decision is made.

TABLE orphaned_deposits
  id                bigserial primary key
  order_id          bigint not null
  external_id       text not null
  tx_hash           text not null
  log_index         int not null
  amount            numeric(38,0) not null
  detected_at       timestamptz not null
  order_state_at_detection text not null   -- whatever C1 reported when C2 tried
  resolution        text null              -- null until a human or a policy acts
  resolved_at       timestamptz null

BUILD internal/finality (extend)
  HandleUnreportable(ctx, candidate, c1Error) error
    - Called from C2.7's 409 illegal_transition branch.
    - Records an orphaned_deposits row. Does not retry the transition. Does not
      guess. Exposes the row via C2.9's HTTP surface for operator visibility and
      eventual manual resolution.
    - Fires an alert (structured log at minimum; hook for a real alerting
      integration later) — this represents real customer money that arrived for
      an order C1 no longer considers open, and it should never be silently
      absorbed into normal operational noise.

ACCEPTANCE
- A deposit finalizing after its order's quote_expires_at has passed (and C1 has
  independently moved it to `expired`) is recorded in orphaned_deposits, not
  retried, not dropped.
- The orphaned-deposits list is visible via GET (C2.9) and each row carries
  enough to manually reconcile: order, tx, amount, when, and what C1 said.
- No code path in this chunk posts anything to C1 on its own initiative — it is
  strictly a capture-and-surface mechanism until the product decision from gap #3
  exists to drive it.
```

---

## C2.9 — HTTP boundary

```
Expose C2 as a service, mirroring C1.8's shape and discipline.

ENDPOINTS (all under /v1)
  POST /addresses                      body: order_id, external_id, customer_id,
                                        quoted_at, quote_expires_at
                                        -> assigns and returns a deposit address.
                                        Idempotent on order_id (C2.1).
  POST /addresses/{order_id}/retire    body: reason
  GET  /addresses/{order_id}
  GET  /orphaned-deposits              ?resolved=false filter
  POST /orphaned-deposits/{id}/resolve body: resolution, actor
  GET  /system/providers               per-provider health from C2.2
  GET  /system/invariants              cursor lag, pending-finality count,
                                        oldest pending candidate age
  GET  /healthz  GET /readyz  GET /metrics

AUTH
- Service-to-service bearer token, same posture as C1.8 — this is the C6-facing
  boundary once C6 exists, and the same "mTLS can replace this without touching
  handlers" design goal applies.

RULES
- Same decimal-string-not-JSON-number discipline as C1 wherever an amount
  appears in a response.
- Every write requires an idempotency key, same as C1, for the same reason.

ACCEPTANCE
- OpenAPI spec generated/maintained, same as C1.8's requirement, with a test that
  every route is present in it.
- POST /addresses called twice with the same order_id: 200 both times, same
  address, not 201 then 200 — matches C1's own Replayed-vs-Created convention in
  spirit even though this isn't a journal entry.
- Metrics exposed: candidates_detected_total, candidates_finalized_total,
  reports_to_ledger_total by result code, orphaned_deposits_total,
  provider_agreement_failures_total, cursor_lag_blocks gauge.
```

---

## C2.10 — Replay harness against a simulated chain

```
C1.9 hammers the ledger with generated orders because a real Postgres is cheap to
spin up and drive at will. C2 cannot do the equivalent against real BSC — you do
not get to command mainnet to reorg on schedule. This chunk instead builds a
harness against a controllable simulated/forked chain (a local dev node capable
of manual block production and, ideally, deliberate chain-history rewrites) and
is the acceptance gate for C2 as a whole.

cmd/replay/main.go, runnable as a Go test in CI, same posture as C1.9: seeded,
deterministic, prints the seed on failure.

SCENARIO MIX (mirrors c1-scenario-catalog.md Part 1/2's C2 rows directly —
if you add a scenario here, add the matching row there too, and vice versa)
  Clean deposits at exact, over, under, and dust amounts, reaching finality
    via the finalized-tag path
  Deposits to a retired address (order already terminal)
  Deposits landing after quote_expires_at, racing the order's own expiry
  Pre-final reorg at various depths inside the advisory window — asserted as
    silent (no report to C1) as long as the affected block never crosses the
    finalized tag
  Provider disagreement injected mid-run — asserted as a withheld finalization,
    not a majority-of-one grant
  A provider going dark entirely for an extended period — asserted as a health
    alert, and (if it drops below minAgreement) a hard ingestion stall rather
    than a silent single-provider fallback
  Duplicate log delivery from a provider (the same event replayed) — asserted as
    a no-op against C1, not a double report
  Wrong-token and zero-value transfers to a watched address — asserted as
    filtered before classification, never reaching C1 at all
  A post-final reorg (once C2.6's real endpoint integration exists) — asserted
    against C1's actual scenario A/B behavior, not a mock of it

FINAL ASSERTIONS
1. Every finalized candidate that should have reached C1 did, exactly once.
2. No candidate that should have been filtered (wrong-token, zero-value, dust
   below floor) ever reached C1.
3. Cursor never skips a block and never double-processes one with an externally
   visible effect, across every injected failure and restart.
4. Provider disagreement never results in a finalization grant.
5. Every orphaned-deposit case is captured, none silently dropped, none
   auto-resolved without the policy from gap #3 explicitly wired in.
6. Zero deadlocks, zero panics, zero unexplained errors.

PERFORMANCE TARGET: given the real scale here — ~100 deposits/day, ~4/hour peak,
per component-map.md — this harness's job is correctness under adversarial
conditions, not throughput. A slow run is acceptable; a run that passes by
accident under low concurrency is not — inject real concurrency in the parts of
this that can race (the ingestion loop's own restart-mid-scan behavior, multiple
candidates finalizing in the same tick).
```

---

## Suggested sequencing

| Chunk | Days | Notes |
|---|---|---|
| C2.0 Scaffold + address derivation | 0.5 | — |
| C2.1 Address lifecycle store | 0.5 | — |
| C2.2 Multi-provider RPC pool | 1.5 | Higher risk than it looks — provider disagreement handling is the whole point of invariant 5 |
| C2.3 Block ingestion loop | 1.0 | — |
| C2.4 Log parsing and classification | 1.0 | Verify the USDT BEP20 contract address yourself before this chunk starts |
| C2.5 Finality policy (§B) | 1.5 | The crux chunk — do not compress this to save time |
| C2.6 Reorg handling | 1.0 | No longer blocked — C1.11 closed the endpoint gap. Build whole, against a real C1. |
| C2.7 Emission to C1 | 1.0 | Needs a running C1 instance to test against for real |
| C2.8 Orphaned-deposit handling | 0.5 | Mechanism only — policy is a separate, non-engineering decision |
| C2.9 HTTP boundary | 1.0 | Can run in parallel with C2.7/C2.8 |
| C2.10 Replay harness | 1.5 | Needs a controllable forked/simulated chain set up first — budget time for that tooling, it's not free |

≈10.5–11 days, consistent with the 1.5–2 eng-week estimate in `component-map.md`. The estimate originally assumed gap #1 would be closed early rather than discovered mid-build — it now is closed (C1.11), so the full sequence C2.0→C2.10 can run without an integration pause at C2.6.

**Before starting C2.0:** gap #1 is closed. Gaps #2 (sweep entry type/owner) and #3 (deposit-after-expiry policy) remain open but don't block C2 itself — C2 never sweeps and C2.8 only needs to capture the orphaned case, not resolve it. Gap #4 (BSC finality timing) is addressed above with sourced figures, but **verify it against your own chosen RPC providers before anything customer-facing** — pricing pages, SLA copy, the status page (S3) — gets built assuming those numbers hold in practice.

---

## Cross-reference

- C1's full build spec and the exact contract this document is written against: `c1-ledger-build-prompts.md`
- Where these scenarios live in the corridor-wide risk view, and what's still open in Part 2's C2 section: `c1-scenario-catalog.md`
- Component ownership boundaries and the original hard-parts list this spec expands: `component-map.md`
- The custody-window and tier-SLA numbers that need re-verification once C2's finality measurement is real: `product-operations-architecture.md`

Sources for the BSC block-time and finality figures cited above:
- [BNB Chain: Fermi Hard Fork Accelerates BSC to 0.45-Second Block Times](https://www.bnbchain.org/en/blog/fermi-hard-fork-accelerates-bsc-to-0-45-second-block-times)
- [BNB Chain: Maxwell Hardfork — BSC Moves to 0.75-Second Block Times](https://www.bnbchain.org/en/blog/bnb-chain-announces-maxwell-hardfork-bsc-moves-to-0-75-second-block-times)
- [CoinMarketCap Academy: BNB Chain Cuts Block Time to 0.45 Seconds With Fermi](https://coinmarketcap.com/academy/article/bnb-chain-cuts-block-time-to-045-seconds-with-fermi)
- [BEP-126: Introduce Fast Finality Mechanism](https://github.com/bnb-chain/BEPs/pull/126)
- [BscScan: Binance-Peg BSC-USD (BEP-20) contract](https://bscscan.com/token/0x55d398326f99059ff775485246999027b3197955)
