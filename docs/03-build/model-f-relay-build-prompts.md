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
- **Shipped (14 Sep 2026):** FixedFloat (ff.io) chosen, commission mechanism — full
  decision record appended to `model-f-relay-findings.md`'s own "R2 decision record"
  section, including which criteria were independently confirmed vs. inferred.

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
- **Shipped (14 Sep 2026), the code — the proof run is still open:**
  `relayd/internal/upstream/fixedfloat.go` implements `SwapProvider` against
  FixedFloat's real v2 API (`POST /api/v2/price`/`/create`/`/order`, HMAC-SHA256
  request signing per their own docs), gated behind `UPSTREAM_PROVIDER=fixedfloat`
  with `FIXEDFLOAT_API_KEY`/`FIXEDFLOAT_API_SECRET`/`FIXEDFLOAT_CCY_USDT_TRC20`/
  `FIXEDFLOAT_CCY_USDT_BEP20` all required, no defaults (`cmd/relayd/upstream_provider.go`).
  One real API-shape wrinkle: FixedFloat's own status check needs both an order id and a
  separate security token, but `SwapProvider.GetOrder` takes one opaque string — worked
  around by packing `"id|token"` into the single `ProviderOrderID` `CreateOrder` returns
  rather than changing the interface (see that file's own top-of-file doc comment).
  Unit-tested against a fake HTTP server (request signing, currency-code mapping,
  status mapping, the id/token packing round-trip) — **this acceptance criterion's own
  real-money proof run has NOT happened**: nobody has yet placed one real order against
  FixedFloat's production API and confirmed the response actually matches what this
  code assumes. The two currency-code env vars also still need verifying against a live
  `GET /api/v2/ccies` response (see `model-f-relay-findings.md`'s own R2 decision
  record) before that proof run can mean anything. Until both happen, treat this as
  code-complete but unproven, the same distinction R6's own replay-vs-real-run posture
  already draws elsewhere in this doc.
- **Shipped (14 Sep 2026), a second vendor + best-rate routing:** a real order against
  FixedFloat surfaced a real problem (still being root-caused), and R2's own findings
  doc records the decision not to stay committed to one vendor while that's worked
  out — `relayd/internal/upstream/changenow.go` implements `SwapProvider` against
  ChangeNOW's real v1 API (no request signing needed, simpler than FixedFloat's own
  HMAC scheme — the API key travels as a path segment), gated behind
  `UPSTREAM_PROVIDER=changenow` with `CHANGENOW_API_KEY`/`CHANGENOW_CCY_USDT_TRC20`/
  `CHANGENOW_CCY_USDT_BEP20` all required, no defaults. `relayd/internal/upstream/router.go`'s
  `MultiProvider` then wraps 2+ configured vendors behind the *same* `SwapProvider`
  interface, so nothing else in `relayd` needs to know routing happens at all:
  `Quote` asks every vendor in parallel and returns whichever offers the best rate;
  `CreateOrder` **re-quotes fresh** rather than reusing an earlier `Quote`'s winner
  (folded into architecture doc §6's own existing re-quote-at-forward-time step, since
  a leg's deposit-wait gap already made an early quote unreliable regardless of
  routing); `GetOrder` routes back to the originating vendor via a
  `"<vendor>:<real id>"` prefix `CreateOrder` adds, which composes cleanly with
  FixedFloat's own `"id|token"` packing (proven by a dedicated test, not just asserted).
  A single vendor's error never blocks a call — only every configured vendor failing
  does, mirroring `energybroker`'s own fallback-ladder posture. Gated behind
  `UPSTREAM_PROVIDER=best_rate` plus `UPSTREAM_BEST_RATE_PROVIDERS` (comma-separated
  vendor names, no default). Unit-tested thoroughly (best-rate selection, single-vendor
  fallback, all-fail case, re-quote-not-stale-quote, order-id prefix round-tripping
  including the FixedFloat nested-separator case) — **not yet proven against either
  vendor's live API or a real R6 replay run**, the same "wired, not shipped" distinction
  FixedFloat's own entry above already draws.

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
- **Shipped (13 Sep 2026), first slice:** a leg stuck unable to broadcast its own
  forward transfer past `RELAYD_FORWARDING_TIMEOUT` is refunded automatically (reuses
  C1's *existing* `Dispatching→Held→Refunded` transitions, zero `ledger/` changes), and
  a leg whose forward transfer already confirmed but whose upstream swap then failed
  lands `UNRECOVERABLE` and fires a real alert (`internal/alert`).
- **Shipped (14 Sep 2026), the manual path:** the `HELD→REFUNDED` path via C3's own
  hold-review queue is now wired end to end.
  `screening/internal/holds.RelayAwareRefundEntryBuilder` calls a new relayd endpoint,
  `GET /v1/relay-legs/{external_id}/refund-entry` (`driver.BuildRefundEntry`), which
  computes the same entry shape `internal/orchestrate/refund.go`'s own timeout-triggered
  path already posts; screening's existing `holds.Reject` submits it to C1 exactly as it
  already does for every other tier (no change to `Reject` itself, only which
  `RefundEntryBuilder` `cmd/screend` wires in — `SCREENING_RELAYD_BASE_URL`/`TOKEN`,
  both optional, matching every other Model F cross-service dependency's own
  "optional, degrades cleanly if unset" posture). relayd's own
  `internal/orchestrate/refund.go`'s new `startExternallyRefundedLegs` phase notices a
  RELAY order reached `refunded` by this path and carries it through the *same*
  `advanceRefundPendingLegs` machinery the timeout-triggered refund already uses — no
  new on-chain-broadcast code was needed, only a new trigger. For any non-RELAY order,
  `RelayAwareRefundEntryBuilder` falls back to `StubRefundEntryBuilder`'s own
  `ErrRefundEntryNotImplemented`, unchanged from before this landed. Restores the
  build-prompts doc's own original R6 scenario, `ManuallyRejectedHoldGetsRefunded` (see
  R6's own entry below).
- **Shipped (14 Sep 2026), the last self-contained gap:** a leg stuck `AWAITING_DEPOSIT`
  because `upstream.CreateOrder` itself never succeeds is now also refunded
  automatically, directly from `screened` (no reversal step first — the deposit is
  still sitting untouched in `asset:relay:leg:<id>`, `relay_forward_start` never ran).
  Needed two real, additive changes neither of which existed before this: a new C1
  transition, `{Screened, Refunded}` (`ledger/internal/orders/transitions.go` — not
  tier-gated, C1 reuses its own existing state machine rather than forking it, per §5
  above), and a new `relay_legs.forward_attempt_started_at` timestamp
  (`relayd`'s own migration 0005), set the first time `runloop.go`'s own `startOne`
  ever notices a leg needs forwarding, whether or not that first `CreateOrder` attempt
  succeeds — the timestamp `refundStuckAwaitingDepositLegs` needed and that neither
  `relay_legs.CreatedAt` (quote time, legitimately old while genuinely waiting on a
  customer deposit) nor `UpdatedAt` (never touched while still `AWAITING_DEPOSIT`)
  could provide. Reuses `RELAYD_FORWARDING_TIMEOUT` rather than a new config value —
  the same underlying question ("how long will relayd wait on itself before giving up
  and refunding"), just checked against a different clock. Restores the R6 scenario
  mix's own missing case, `StuckAwaitingDepositLegAutomaticallyRefunded` (see R6's own
  entry below).
- **Still open:** real per-vendor `EXPIRED` semantics. R2/R4 are no longer the blocker
  (FixedFloat is chosen and wired, and its own `EMERGENCY` status already maps to
  `SwapStatus` -- see `statusFromFixedFloat` in `internal/upstream/fixedfloat.go`), but
  `internal/orchestrate/settle.go` still treats `StatusExpired` identically to
  `StatusFailed` post-`FORWARDED` -- a real implementation needs to read FixedFloat's
  own `emergency.choice` field (`NONE`/`EXCHANGE`/`REFUND`) from a live `GET
  /api/v2/order` response to know whether the vendor already auto-refunded its own
  receipt, something no unit test against a fake server can confirm on its own. This is
  genuinely separate, not-yet-started work, left for its own chunk rather than folded
  into R4's own vendor-wiring pass.

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
- **Acceptance criteria, as shipped:** the scenario mix originally deviated from this
  doc's own wording above (13 Sep 2026, when R5 had shipped only a first slice), and
  has grown a scenario each time a real R5 gap closed since, restoring this doc's own
  original cases for real rather than leaving them permanently stood in for. Current
  mix, all seven scenarios, each named after the real code path it exercises: a full
  successful relay in **both** directions (`FullHappyPath_TRC20ToBEP20`,
  `FullHappyPath_BEP20ToTRC20` — stronger than the doc's own original "a full
  successful relay," singular); a leg stuck unable to broadcast its forward transfer
  → automatically refunded (`StuckForwardingLegAutomaticallyRefunded` — stands in for
  "an upstream `failed` before the forward transfer": relayd has no distinct code path
  for that, it only ever learns an upstream order's own status by polling it *after*
  its own forward transfer confirms); a leg whose own `upstream.CreateOrder` call never
  succeeds → automatically refunded directly from screened
  (`StuckAwaitingDepositLegAutomaticallyRefunded`); a held order manually rejected by a
  human via C3 → refunded (`ManuallyRejectedHoldGetsRefunded`, this doc's own original
  "a screening hold that resolves to refund"); an upstream `failed` *after* the forward
  transfer → `UNRECOVERABLE` with a real alert
  (`PostForwardUpstreamFailureLandsUnrecoverableAndAlerts`); and a leg legitimately
  still in flight but sitting past the reconciliation threshold → a `SeverityWarning`
  alert, once (`StaleLegAlarmFiresOnceForLegLeftForwardedTooLong`, architecture doc §5's
  own reconciliation alarm, not originally part of R6's own scope but added alongside
  it once that alarm was built the same day). All seven pass against a real `ledgerd` —
  no C1 mocks, same rule C6.9 already enforces — confirmed across many real runs
  (`go run ./cmd/replay`, several different seeds spanning 13-14 Sep 2026), all
  `ALL PASSED`. One real bug the very first of those runs found: the first FINAL
  ASSERTION draft asserted every relay-leg suspense account closes to zero, which is
  *wrong* for an `UNRECOVERABLE` leg — its own `asset:relay:leg:forwarding:<id>`
  account is supposed to sit at the full forwarded amount forever (nothing ever posts a
  closing entry against it, by design); fixed by excluding `UNRECOVERABLE` legs from
  that assertion and adding a positive counterpart confirming the stranded balance is
  exactly right, not just non-zero.

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
