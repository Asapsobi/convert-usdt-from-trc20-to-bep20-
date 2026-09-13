# Model F — zero-float relay: build prompts

_13 Sep 2026. Turns `docs/02-architecture/model-f-relay-architecture.md`'s decisions
into sequenced chunks (R0 → R6), written to be handed to an AI coding agent one chunk
at a time — same convention as `c1-ledger-build-prompts.md` etc. Each chunk assumes the
architecture doc is settled and does not re-open it._

## Happy flow this build has to satisfy — TRC20 → BEP20

1. Customer requests a quote: amount, direction, and their own BEP20 receiving
   address.
2. Gateway calls `upstream.Quote()`, applies the commission-or-margin from
   `model-f-relay-findings.md`, issues a locked quote (architecture §6).
3. Customer accepts → order created, `AWAITING_DEPOSIT`. C2′ hands back a dedicated
   TRC20 deposit address.
4. Customer sends TRC20 USDT to that address.
5. C2′ detects and finalizes → `DEPOSIT_DETECTED`; C1 credits
   `asset:relay:leg:<order_id>`.
6. C3 screens the sender → `SCREENED` (or `HELD`).
7. `relayd` calls `upstream.CreateOrder(TRC20→BEP20, amount, destination = customer's
   own BEP20 address)` → gets the upstream platform's own TRC20 deposit address + its
   order id. The destination is set once, here, never touched again.
8. C4 buys the TRON energy the forward transfer needs; S1 signs; `relayd` broadcasts a
   TRC20 transfer of `(received − take)` to the upstream deposit address →
   `FORWARDED`.
9. `relayd` polls (or receives a webhook from) the upstream order until `complete`,
   `failed`, or `expired`.
10. **`complete`** → `SETTLED`. C1 books the commission/margin, closes the suspense
    account, fires the customer's webhook.
11. **`failed` / `expired`** → refund to the customer's *sending* address if the funds
    never left this system's own address; if they already left (post-step-8), this is
    the `UNRECOVERABLE` path — see "Open items" below.

BEP20 → TRC20 is the mirror: C2 (BSC) is the front door, the forward leg needs BNB gas
instead of TRON energy (no C4 involvement), and the upstream order's destination is
the customer's own TRC20 address.

## Build chunks

### R0 — `internal/upstream` interface
- **Scope:** the interface, types, and `PlaceholderProvider` exactly as specified in
  `model-f-relay-architecture.md` §3.
- **Acceptance criteria:**
  - `dependency_test.go` mechanically fails the build if any package outside
    `internal/upstream` imports a named vendor SDK — same mechanism as
    `energybroker/internal/provider/dependency_test.go`.
  - `PlaceholderProvider` returns `ErrNoVendorConfigured` on every call, never a
    fabricated quote or order.
  - Unit tests cover `Quote`/`CreateOrder`/`GetOrder` against the placeholder and
    against a `mock.go` fake (mirroring `energybroker/internal/provider/mock.go`)
    usable by every later chunk's tests.

### R1 — C2′: TRON deposit watcher
- **Scope:** re-run `docs/03-build/c2-deposit-watcher-build-prompts.md`'s own
  chunk sequence (C2.0 → C2.10) against TRON/TRC20 instead of BSC/BEP20 — same HD
  address derivation, same rolling block-hash reorg window, same
  detected/final/reorged event split, same finality-confirmation-count discipline
  (confirm TRON's own equivalent confirmation count independently rather than
  assuming BSC's 15 carries over).
- **Acceptance criteria:** identical in kind to C2's own — see that document. The one
  addition: this watcher's `deposit.final` event must resolve to the
  `asset:relay:leg:<order_id>` account (architecture §5), not a treasury account.

### R2 — pricing decision + upstream platform selection (gate, not code)
- **Scope:** before writing `relayd`, get from at least one real candidate upstream
  platform: (a) documented quote/create-order/status API responses (not marketing
  copy), (b) confirmation it supports an arbitrary destination address distinct from
  any account of this system's own, (c) if pursuing mechanism 1
  (`model-f-relay-findings.md`), an actual partner/affiliate terms sheet — not just a
  retail UI, (d) current terms of service confirming automated commercial resale or
  white-label integration is permitted.
- **Acceptance criteria:** a short decision record (append to
  `model-f-relay-findings.md`, don't silently supersede it) naming the chosen vendor,
  which pricing mechanism it supports, and a link/reference to the terms confirming
  (d). No `relayd` code should be written against vendor-specific assumptions before
  this exists — this is the same posture C4's own "week-2 wholesale pricing calls"
  gate took before its routing weights were trusted.

### R3 — `relayd`: relay forwarder service
- **Scope:** the state machine in `model-f-relay-architecture.md` §4, a
  `relay_legs` table (own migration, own Postgres — this service does not share a
  database with C1/C2′/C3/C4/S1, same convention as every other service here), and
  HTTP clients into C1/C3/C4/S1 modeled directly on `dispatcher/internal/ledgerclient`,
  `dispatcher/internal/energy`, and `dispatcher/internal/signing` — reuse those
  clients' shape, not their Sweep-tier-specific logic.
- **Acceptance criteria:**
  - Full happy-flow (above) passes against real `ledgerd`, the R1 watcher, `brokerd`,
    and an in-process fake `SigningService`, mirroring the posture C5's own build took
    before S1 had a real KMS adapter.
  - No code path constructs a multisend/batch transaction — Sweep tier does not exist
    in this service; a single relay order is always exactly one forward transfer.
  - `relay_legs.provider_order_id` is unique per `(provider_name, provider_order_id)`
    — a duplicate `CreateOrder` response for the same logical order must be rejected,
    not silently overwritten.

### R4 — real vendor wired in
- **Scope:** implement `upstream.SwapProvider` against the vendor chosen in R2.
- **Acceptance criteria:**
  - Gated behind an explicit `UPSTREAM_PROVIDER=<name>` env var with no default — same
    posture as `SCREENING_PROVIDER=always_clean` requiring `SCREENING_ALLOW_ALWAYS_CLEAN=true`
    alongside it, and as S1's `S1_KMS_CLIENT` refusing to start unset.
  - At least one real, small, real-money order run against production endpoints
    (mirroring the proof-run discipline `mvp-proof-run-plan.md` used for Model D) before
    this chunk is called done — a vendor's documented API and its real behavior have
    diverged before in this codebase (see C4's CatFee rounding and propagation-delay
    bugs, both found only by a real proof run).

### R5 — refund path
- **Scope:** `HELD`→`REFUNDED`, `EXPIRED`→`REFUND_PENDING`→`REFUNDED`, and the
  `UNRECOVERABLE` terminal state for a post-`FORWARDED` failure with no upstream-side
  refund mechanism.
- **Acceptance criteria:**
  - A refund only ever returns funds to the address that originally sent the deposit
    — never to an address supplied after the fact, closing the obvious
    social-engineering vector on a refund flow.
  - Reaching `UNRECOVERABLE` fires an alert, not a silent log line — this state means
    a human decision is now required (see architecture §4 and "Open items" below).
  - Replay scenarios cover all three refund-triggering states plus the ordinary
    settle path.
- **Shipped (13 Sep 2026), first slice — recorded here rather than silently closed:**
  a leg stuck unable to broadcast its own forward transfer past
  `RELAYD_FORWARDING_TIMEOUT` is refunded automatically (reuses C1's *existing*
  `Dispatching→Held→Refunded` transitions, zero `ledger/` changes), and a leg whose
  forward transfer already confirmed but whose upstream swap then failed lands
  `UNRECOVERABLE` and fires a real alert (`internal/alert`). **Still open:** the manual
  `HELD→REFUNDED` path via C3's own hold-review queue (cross-module — C3's own
  `RefundEntryBuilder` is still `StubRefundEntryBuilder` repo-wide, not
  Model-F-specific; needs a new relayd-facing endpoint C3's `Reject` can call), a leg
  stuck `AWAITING_DEPOSIT` because `upstream.CreateOrder` itself never succeeds (needs
  a new C1 `{Screened, Refunded}` transition plus a per-state timestamp neither exists
  today), and real per-vendor `EXPIRED` semantics (gated on R2/R4 — see
  `internal/orchestrate/settle.go`'s own doc comment on why `EXPIRED` is treated
  identically to `FAILED` post-`FORWARDED` for now).

### R6 — replay ship-gate harness
- **Scope:** `relayd`'s own `internal/replay` + `cmd/replay`, mirroring every sibling
  component's own identical convention (confirmed by reading dispatcher's/gateway's own
  `cmd/replay`, not assumed from this doc's own earlier wording below): connects to a
  real, already-running `ledgerd` over HTTP plus a raw connection to its database for
  fixture/assertion use, exactly like every other `cmd/replay` in this repo — starting
  that `ledgerd` is the run's own caller's job (an operator or CI script), not this
  binary's. No component's own replay harness spawns its upstream dependency's process
  itself, and R1 (`tronwatcher`) is no exception: this harness fixtures the
  funded→screened step directly via HTTP+DB (`internal/replay/ledgerfixture.go`,
  mirroring `internal/testledger`'s own identical fixture), the same way dispatcher's
  own replay harness never spins up a real `depositwatcher` either.
- **Acceptance criteria, as shipped (13 Sep 2026) — deviates from this doc's own
  original wording above, recorded here rather than silently:** the original scenario
  mix ("a screening hold that resolves to refund," "an upstream `failed` before the
  forward transfer") assumed R5 shipped in full. Since R5 shipped only a first slice
  (see R5's own "Shipped" note above), the actual scenario mix is: a full successful
  relay in **both** directions (TRC20→BEP20 and BEP20→TRC20 — stronger than the
  original "a full successful relay," singular), a leg stuck unable to broadcast its
  forward transfer → automatically refunded (this is what R5 actually built, and
  stands in for both of the original doc's refund-triggering scenarios — relayd has no
  distinct code path for "upstream failed before forward" today: it only ever learns an
  upstream order's own status by polling it *after* its own forward transfer confirms,
  so a pre-forward vendor failure is indistinguishable from any other reason the
  forward attempt never completes), and an upstream `failed` *after* the forward
  transfer (`UNRECOVERABLE`, a real alert fires exactly once). All four pass against a
  real `ledgerd` — no C1 mocks, same rule C6.9 already enforces — confirmed by two real
  runs (`go run ./cmd/replay`, seeds `1789317414438614000` and `424242`), both `ALL
  PASSED`. One real bug this run itself found: the first FINAL ASSERTION draft asserted
  every relay-leg suspense account closes to zero, which is *wrong* for an
  `UNRECOVERABLE` leg — its own `asset:relay:leg:forwarding:<id>` account is supposed
  to sit at the full forwarded amount forever (nothing ever posts a closing entry
  against it, by design); fixed by excluding `UNRECOVERABLE` legs from that assertion
  and adding a positive counterpart confirming the stranded balance is exactly right,
  not just non-zero.

## Open items

- **Upstream platform selection (R2)** is the gate on everything after it — see R2's
  own acceptance criteria.
- **The `UNRECOVERABLE` case** needs an operational answer before go-live: a
  support-ticket runbook with the chosen vendor, a small compensation reserve, or a
  minimum order size below which the probability-weighted loss is acceptable to carry
  unfunded. This is an operational procedure, not something R5's code alone can close
  — the same posture `component-map.md` takes toward S1's real KMS key-generation
  ceremony ("an operational procedure with real cloud credentials, not something built
  here").
- **AML/KYT vendor** — same open decision C3 already carries in Model D; unchanged and
  no lower-priority here.
- **Re-run the economic gate from `model-f-relay-findings.md`** once R2's real numbers
  exist: at whatever commission or margin the chosen vendor actually supports, does
  the per-order economics clear fixed costs at a demand level reachable without a
  public page? If mechanism 1 (commission) turns out to be unavailable from every real
  candidate, that's the signal to revisit before R3, not after.
