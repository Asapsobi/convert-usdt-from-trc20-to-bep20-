# USDT Settlement Corridor

A specialised USDT cross-network settlement layer — BEP20 → TRC20 — sold not as a
swap service but as **TRC20 payout infrastructure for businesses**.

> *"Reliable TRC20 payouts, funded from any chain."*

This repository is the working record of the evaluation, the architecture decisions
that came out of it, and the build specifications derived from those decisions —
and, as of 9 Sep 2026, every MVP service is built: the ledger core, the deposit
watcher, screening, the energy broker, key management, the payout dispatcher,
and now the API gateway (C6). Separately, the pipeline C1→C2→C3→C4→C5/S1 was
also *wired together* and has **executed for real**: five real,
independently-verified settlements on TRON mainnet across two separate proof
runs, USDT actually moved BEP20→TRC20 end to end with no manual database
step — see `docs/03-build/mvp-proof-run-plan.md` and `proofrun/`. Running it
for real (not against fakes) found and fixed seven real bugs no existing
test caught — a TRON gRPC client that had never actually connected to a live
node, a ledger account C2 never created before referencing it, an on-chain
verification check too strict for a real vendor's own rounding, a
block-scanning cursor with no chunking that stalls permanently against a
real high-traffic contract, a missing slot-registration path in S1, a free
RPC provider's own per-call block-range cap tighter than the result cap the
scanning chunk size was originally sized against, and a real vendor purchase
discarded because on-chain verification checked before the delegation had
time to propagate — all documented below, by component. C6's own ship gate
(its C6.9 replay harness) separately ran against real `ledgerd`/`watcherd`
subprocesses three times across both sessions, `ALL PASSED` every time —
see the API gateway entry below.

## Where things stand

| | |
|---|---|
| Market verdict | Not viable as "network conversion"; viable as payout infrastructure |
| Recommended model | **Model D** — conversion/payout API now; Model E (netting) year 2–3 |
| Margin engine | Wholesale TRON energy (25.7 sun blend vs 41 sun market) + batch multisend |
| Contribution margin | 87.1% at a $3,000 ticket |
| MVP scope | 6 services, one ledger, no smart contracts · ≈9–10 eng-weeks |
| Build status | **C1 (ledger core), C2 (deposit watcher), C3 (screening), C4 (energy broker), S1 (key management), and C5 (payout dispatcher) are built and tested.** C1: all chunks C1.0–C1.11 shipped, C1.9 replay gate passing at 10,000 orders / 32 workers, scenario catalog audited row-by-row against the real test suite (3 coverage gaps found and closed, one real HTTP-boundary bug found and fixed). C1.11 added the reversal/reorg HTTP surface (`POST /v1/entries/{id}/reversal`, `POST /v1/orders/{id}/reorg`) that C2 needs. C2: all chunks C2.0–C2.10 shipped, including the replay harness and HTTP boundary, wired to a live chain-watching engine and verified against real BSC. Running it against a real deposit on mainnet (9 Sep 2026) found and fixed two real bugs no fake-backed test caught: `ReportDepositFinal` built the customer's own liability account code without the `:USDT_BEP20` suffix every other component's identical account uses (silently a different account than the one C5's own E2 entry nets against) and never called `EnsureAccount` for either account it references before posting the entry — C1 does not auto-vivify one on first reference; and the block-scanning cursor had no chunking, so a single `eth_getLogs` call covering however many blocks had accumulated since the last successful tick eventually exceeds a real public RPC provider's own result cap against a high-traffic contract like USDT_BEP20 itself — and since a failed tick never advances the cursor, the range only ever grew, a permanent stall rather than a transient error. Both fixed; scanning is now capped at 100 blocks/tick. A second real proof run (10 Sep 2026) found that cap still too wide against a free-tier RPC provider's own hard per-call block-range ceiling (10 blocks on Alchemy's free tier, independent of the result-count cap the 100 figure was sized against) — lowered to 10, with the finding recorded directly on `maxBlocksPerTick`'s own doc comment rather than superseding it silently. C3: all chunks C3.0–C3.9 shipped, including its own replay ship-gate harness; `cmd/screend` now also runs the discovery background loop for real (against a real, running C1 — no fake, no vendor dependency). The pipeline and re-screen loops still have no real `provider.ScreeningProvider` wired — vendor choice among the Chainalysis/TRM/Elliptic candidates `component-map.md` names was deliberately left unmade — but as of 9 Sep 2026 `cmd/screend` can start both loops against `provider.AlwaysCleanProvider`, an explicitly-labeled, non-production placeholder, gated behind `SCREENING_PROVIDER=always_clean` **and** `SCREENING_ALLOW_ALWAYS_CLEAN=true` so it can't be reached by one mistyped env var. This exists only for `docs/03-build/mvp-proof-run-plan.md`'s own supervised proof run — wiring a real vendor in for production traffic is still the open item. C4: all chunks C4.0–C4.9 shipped, including its own replay ship-gate harness (9/9 scenarios, 6/6 final assertions passing against a real ledger and real Postgres); `cmd/brokerd` is now fully wired to production, including real Tronsell/Netts/CatFee HTTP integrations and a real TronGrid on-chain reader, added after discovering none of the three vendors' real APIs support the delegation-retargeting the original buffer design assumed — see the "Read this fourth" addendum in `docs/03-build/c4-energy-broker-build-prompts.md`. Proven against a real vendor account on 9 Sep 2026 — CatFee, real credentials, real TRX balance, two real energy purchases actually delegated on TRON mainnet — which found one real bug: `VerifyOnChain` required the on-chain delegation to be `>=` the *exact* requested units, and a real CatFee order for 65,000 landed on-chain as 64,999 (harmless vendor-side rounding, not a partial delegation), rejected outright by the exact check. Fixed with a small (0.1%) tolerance. A second real proof run (10 Sep 2026, three more real CatFee purchases and settlements) found a second real bug in the same check: the very first `VerifyOnChain` call right after a real `Delegate()` response saw 0 available energy and failed the reservation outright, discarding a delegation that (confirmed independently seconds later) had landed correctly at 64,999/65,000 units — a real purchase wasted by checking before the on-chain state had time to propagate, not a real shortfall. Fixed with a bounded 3-attempt/9s retry in the slow path before failing; a delegation genuinely absent or partial after that still fails exactly as before. The reservation from this run that was wrongly marked FAILED before the fix landed had no automatic path back (`Create`'s own idempotency-key replay never re-attempts a terminal row) — closed with a new `cmd/reconcile-reservation` ops tool, given a delegation the operator has independently verified real, matching this project's own `cmd/seed-slot`/`cmd/seed-slot-key` convention for gaps with no HTTP route; used live to reconcile the actual affected reservation before the fix shipped. Going live still needs the real payout slot addresses and a confirmed Tronsell base URL (its own docs never publish one) if that vendor is used instead of or alongside CatFee — all operator-supplied, none fabricated. C5: all chunks C5.0–C5.11 shipped, including its own replay ship-gate harness (11/11 scenarios, 6/6 final assertions passing against a real ledger and real Postgres) — see `dispatcher/`. Signs against S1's real, shipped request/poll `SigningService` contract, but S1 itself is still an in-process fake in every test and in the ship gate (no real cloud KMS exists behind S1 yet — see S1's own entry below), so nothing in this component has touched mainnet. Sweep-tier batching (`internal/txbuild.BuildMultisend`, `CutBatch`/`BroadcastBatch`/`HandlePartialSettlement`) is built against a *proposed* multisend contract interface (modeled on disperse.app's own real, audited contract shape) plus a fake standing in for it — no smart contract exists on either chain per decision 1, and none is deployed here. Building C5 required one small, contained addition to already-shipped C1: `POST /v1/accounts` (idempotent on code), since nothing let a remote caller create the per-customer/per-slot ledger accounts C5 references before referencing them. Testing against a real ledgerd (not just fakes) caught two real, previously-invisible bugs: `PostReversal` never sending the `Idempotency-Key` header C1's own middleware requires on every write route, and `EnterDispatching` computing a fresh `occurred_at` on every retry instead of a caller-supplied stable one, which broke C1's own idempotent-replay check on a legitimate crash-recovery retry — both fixed. As of 9 Sep 2026, `cmd/dispatchd` also runs `internal/orchestrate`'s own background loop — carrying a screened Direct/Standard order through energy reservation, signing, broadcast, and finality confirmation with no manual step, the piece this file's own doc comment previously named as deliberately not started. Building it surfaced and fixed one real, previously-invisible bug: `ConfirmFinality` never called `Store.MarkSettled` the way every other settlement/failure path in this package did, so `dispatch_state` stayed `DISPATCHING` forever after a real settlement. Proving it against real TRON mainnet (9 Sep 2026, two settlements) found one more real bug: `GrpcBroadcastClient` never passed transport credentials, so it couldn't connect to a live node at all — grpc-go refuses to dial with none set. Confirmed live against `grpc.trongrid.io:50051` that the correct choice is plaintext, not TLS (a TLS handshake against that same endpoint fails immediately; plaintext succeeds and returns real chain data) — fixed. Sweep-tier orchestration (`CutBatch`/`BroadcastBatch`) is still not driven by any loop — it needs the real multisend contract below, which doesn't exist. Going live needs everything S1 needs (below) plus the real multisend contract designed, audited, and deployed, and the 35,000-per-recipient energy estimate independently measured against a real testnet payout, not assumed. S1: all chunks S1.0–S1.6 shipped, including its own replay ship-gate harness (5/5 scenarios, 4/4 final assertions passing against real Postgres) — see `s1/`. Signs against a real, independently-verified secp256k1/TRON signing path (DER↔compact conversion, recovery-id resolution against KMS's own missing `v` value) and a real 2-of-N human-approval queue, both tested against an in-process `FakeKMSClient` standing in for real cloud KMS — there is no real AWS/GCP adapter yet, and `cmd/s1d` refuses to start without `S1_KMS_CLIENT` set (no default, so a misconfigured deployment fails loud rather than signing real payouts against a fake key). The replay ship gate itself caught one real spec bug during the build (a "names exactly 2 approvers" assertion that a legitimate KMS-failure-retry scenario violates) — fixed in both the code and the build doc. Same gap as C5's own `cmd/seed-slot`: S1 had no HTTP route or CLI to register a slot's own signing key, only internal Go code (tests) — closed with `cmd/seed-slot-key`, found while actually registering a real slot key for the mainnet proof run. Going live needs the real KMS key-generation ceremony (`docs/02-architecture/s1-key-custody-architecture.md`'s own "Key generation and bootstrapping" — an operational procedure with real cloud credentials, not something built here) and a real KMS adapter behind `kmssign.KMSClient`. See `docs/03-build/c1-scenario-catalog.md`, `depositwatcher/`, `screening/`, and `energybroker/`. C6: all chunks C6.0–C6.9 shipped, see `gateway/`. Auth (`sk_live_`/`sk_test_` API keys, never interchangeable), rate limiting, quote issuance (90s lock), the C1+C2 order-creation choreography with its own C6.4 reconciliation loop for the address-assignment partial-failure window, the C6.5 pull-based status backstop, C6.6 webhook delivery (HMAC-SHA256, exponential backoff, 8 retries), and C6.7's sandbox (a fully separate `sandbox_orders` table and `sk_test_`-keyed customer namespace, mechanically enforced by an import-boundary test) are all real, not stubs. Resolved one real contradiction in its own build-prompts doc: the doc's prose describes the fee schedule as applied "marginally across bracket boundaries," but its own golden test and `findings-and-recommendation.md`'s own cited text both require a flat rate per bracket — the golden test and the primary source were trusted over the doc's own prose. C6.9's own replay harness (`gateway/internal/replay`, `gateway/cmd/replay`) builds and runs real `ledgerd` and `watcherd` binaries as subprocesses (mirroring C3's/C4's own `internal/testledger` pattern) and drives all seven SCENARIO MIX rows (including a real concurrent-order-creation race and a real system-halt refusal) and all five FINAL ASSERTIONS against them — no C1/C2 mocks anywhere in this component's own test suite, and no C3/C4/C5/S1 process is ever started (an order's own progress to settled is produced by a harness-only fixture posting the same C1 transition calls those real components would). Run 9 Sep 2026: `ALL PASSED`, twice in a row; re-run 10 Sep 2026 against that day's own real `ledgerd`/`watcherd` (mid a second live proof run, not a fresh environment): `ALL PASSED` again. Going live needs a real webhook-endpoint verification story for customers (documented HMAC-verification example) and the same production prerequisites named below (S1's real KMS, a real AML vendor) before real orders can reach `settled` through this gateway. **Ops console:** built separately (not one of C1–C6/S1, no architecture doc preceded it) after the two mainnet proof runs above showed operators needed direct, safe read/operate access to C1–C5/S1 instead of raw `psql` — see `opsconsole/` and `docs/03-build/ops-console-build-prompts.md`; wired into the root `docker-compose.yml` in this pass. |
| Next action | Every MVP service is now built, including C6. Three gaps stand between that and production traffic — not one linear next step. **S1 needs a real cloud KMS adapter and the actual key-generation ceremony** before C5 can sign anything for real — the code and every service's own ship gate is done, the cloud-account work isn't (and can't be done from here). **The proposed multisend contract C5's own Sweep tier depends on needs to be designed, audited, and deployed** (or the tier's own margin claim — decision 6's "batching halves per-recipient energy" — needs to be re-examined without it), and its real per-recipient energy cost measured against testnet rather than assumed at 35,000. **A real AML vendor needs to be chosen and contracted** (Chainalysis/TRM/Elliptic, per `component-map.md`) before C3's pipeline and re-screen loops can run against anything but the explicitly-labeled `AlwaysCleanProvider` placeholder — discovery is already wired for real, since it needed no vendor. On the business side, the week-2 wholesale pricing calls to Tronsell/Netts flagged in the findings doc are still not confirmed — C4 now polls real vendor prices live, which removes the code-correctness risk, but not the open question of whether retail-tier pricing still clears the modeled margin. Separately: the actual MVP proof run (`docs/03-build/mvp-proof-run-plan.md`) has now executed for real across two separate sessions (9 Sep and 10 Sep 2026), five settlements total, on TRON mainnet — see the intro above and each affected component's own entry for exactly what that found and fixed. The gaps that run deliberately left open (S1's real KMS custody, a real AML vendor) are unchanged by it and are still what stands between this and production traffic. |

## Model F — zero-float relay (second product line, happy flow built)

_Added 13 Sep 2026. Does not change anything above — Model D stands as built. This is
a second, parallel line: instead of pre-funding both sides of the corridor, relay each
order through an upstream instant-exchange platform's own liquidity, so the business
never holds a net position and needs no capital ladder._

| | |
|---|---|
| Verdict | Structurally close to Model A (rejected above for margin compression) — decision made to build anyway because it needs zero balance-sheet exposure. See the strategy note for the full reasoning. |
| Pricing mechanism | **Commission** (pass the upstream rate through, earn a referral fee — no visible markup to the customer). Decided; `RELAYD_FEE_BASIS_POINTS`. |
| Reuses from Model D | C1 (ledger, new `RELAY` tier + relay-leg suspense accounts), C3 (screening, unchanged — discovers `RELAY` orders via its own existing tier-agnostic polling loop), C4 (energy broker, TRC20-direction forward leg only), S1 (key management — same secp256k1 slot key, both a TRON and an EVM address derived from it) |
| New | `tronwatcher/` (C2′ — TRON-side deposit watcher, mirrors C2 against TRC20 via TronGrid's REST API instead of `eth_getLogs`), `relayd/` (the front door + driving state machine for **both** relay directions), `internal/upstream` (a vendor-agnostic swap-provider interface), `internal/evmtx`/`internal/evmbroadcast` (relayd's own BEP20 forward-leg construction/broadcast — genuinely new code, no service in this repo had ever sent a BEP20 transaction before) |
| Build status | **Everything self-contained is now built and tested. R2 is decided (FixedFloat, commission mechanism — see `docs/01-strategy/model-f-relay-findings.md`'s own decision record) and R4 wires it in (`relayd/internal/upstream/fixedfloat.go`), but that wiring is unit-tested against a fake server only — no real order has been placed against FixedFloat's production API yet, and its two currency-code env vars still need verifying against a live `GET /api/v2/ccies` response. Everything below still defaults to `UPSTREAM_PROVIDER=placeholder` until that proof run happens.** `tronwatcher` (C2′): full service, unit + integration tested against real Postgres. `relayd`: full state machine (`AWAITING_DEPOSIT → FORWARDING → FORWARDED → SETTLED`) for TRC20→BEP20 *and* BEP20→TRC20, driven end-to-end against a real `ledgerd` + real Postgres in both directions, real double-entry ledger postings verified to net to zero on every suspense account. **R5 (refund path) — complete, all three triggers:** a leg stuck unable to broadcast its own forward transfer, or whose `upstream.CreateOrder` call never once succeeds, is refunded automatically past `RELAYD_FORWARDING_TIMEOUT` (the FORWARDING case reuses C1's *existing* `Dispatching→Held→Refunded` transitions; the AWAITING_DEPOSIT case needed one new, additive C1 transition, `{Screened, Refunded}`, and a new `relay_legs.forward_attempt_started_at` timestamp — see `relayd/`'s own entry below); a held order a human manually rejects via C3's own hold-review queue is *also* refunded for real (`screening/internal/holds.RelayAwareRefundEntryBuilder` asks relayd for the entry, submits it through C3's existing `Reject` flow unchanged); a leg whose forward transfer already confirmed but whose upstream swap then failed lands `UNRECOVERABLE` with a real alert (`internal/alert`). **Ops console:** a read-only relay-leg listing page plus home-page cards for both new services. **Stale-relay-leg reconciliation alarm** (architecture doc §5): `internal/orchestrate/reconcile.go` fires a `SeverityWarning` alert, once per leg, for any leg legitimately still in flight (`FORWARDING`/`FORWARDED`/`REFUND_PENDING`) but sitting there past `RELAYD_STALE_LEG_ALERT_AFTER` (opt-in, no default) — purely observational; `AWAITING_DEPOSIT` gets an *active* recovery mechanism instead (R5's own refund-by-timeout), not a passive alarm. **R6 (replay ship gate):** `relayd/internal/replay` + `cmd/replay`, connecting to a real, already-running `ledgerd` (the convention every sibling `cmd/replay` in this repo already uses). Seven scenarios — both directions' full happy path, both refund-by-timeout triggers, the manually-rejected-hold refund, post-forward `UNRECOVERABLE`-with-alert, and the stale-leg alarm — all pass against a real `ledgerd`, confirmed across many real runs (`go run ./cmd/replay`, different seeds spanning two days), all `ALL PASSED`. Found one real bug along the way, in its own first-draft assertion, not in relayd itself: asserting every relay-leg suspense account closes to zero is *wrong* for an `UNRECOVERABLE` leg (its own forwarding account is supposed to stay stranded at the full forwarded amount forever, by design) — fixed. Runs against `upstream.PlaceholderProvider` (`UPSTREAM_PROVIDER=placeholder`) — every quote/swap call returns `ErrNoVendorConfigured` unless `UPSTREAM_ALLOW_PLACEHOLDER=true` is also set, the same double-gate convention `screening` uses for its own non-production placeholder. Wired into the root `docker-compose.yml`. R2/R4: vendor chosen (FixedFloat) and wired, but not yet proven against a real order (see `docs/03-build/model-f-relay-build-prompts.md`'s own R4 entry). |
| Next action | R2 in the build-prompts doc: get a real quote/create-order/status API and a partner/affiliate terms sheet from a candidate upstream platform, then R4 to wire it in behind `internal/upstream`'s own `SwapProvider` interface (no `relayd` code elsewhere needs to change — see that package's own `dependency_test.go`). |

Documents, in reading order: `docs/01-strategy/model-f-relay-findings.md` →
`docs/02-architecture/model-f-relay-architecture.md` →
`docs/03-build/model-f-relay-build-prompts.md`. See `tronwatcher/` and `relayd/`
below for the built services themselves.

## Documents

### `docs/01-strategy/`

- **[findings-and-recommendation.md](docs/01-strategy/findings-and-recommendation.md)** —
  the market assessment. Verdict, the five findings that drive everything, structural
  market facts, pricing recommendation, expected 24-month outcome, and the single gate
  question that decides whether the business continues. Includes two addenda: the
  "competitors do it free" objection checked against real pricing, and a survey of
  TRON energy rental partners.

### `docs/02-architecture/`

- **[product-operations-architecture.md](docs/02-architecture/product-operations-architecture.md)** —
  eleven architecture and operations decisions with the numbers behind each: rent vs
  stake energy, segregated payout slots vs a pooled treasury, the three service tiers,
  treasury rebalancing, the capital ladder, and why the status page is the primary
  sales asset. Includes the economics summary, GTM sequence, and the assumption test
  schedule.
- **[component-map.md](docs/02-architecture/component-map.md)** —
  decomposition into six services (C1–C6) plus three supporting pieces, with the
  dependency graph, per-component ownership boundaries, hard parts, and what is
  deliberately *not* built at MVP.
- **[s1-key-custody-architecture.md](docs/02-architecture/s1-key-custody-architecture.md)** —
  the custody decision record component-map only ever named, never designed:
  self-hosted cloud KMS over a third-party custodian, six independent TRON
  slot keys (never a shared HD seed, for the same isolation reason decision 4
  rejected a pooled treasury), a hybrid threshold splitting signing requests
  into auto-sign versus 2-of-N human approval, the actual TRON-over-KMS
  signing mechanics (DER→compact, recovery-id resolution), and an explicit
  threat model naming what this design does and doesn't defend against.

### `docs/03-build/`

- **[c1-ledger-build-prompts.md](docs/03-build/c1-ledger-build-prompts.md)** —
  the ledger core, specified as eleven sequenced build chunks (C1.0 → C1.10) with
  acceptance criteria for each, written to be handed to an AI coding agent one chunk
  at a time. Includes the chart of accounts (§A) and the worked double-entry
  conversion example (§B) that the whole design rests on. **Built, plus C1.11** —
  the reversal/reorg HTTP surface C2 needs, added after C2's own spec surfaced that
  C1.8 hadn't exposed it — see `ledger/` and `ledger/docs/errors.md`.
- **[c1-scenario-catalog.md](docs/03-build/c1-scenario-catalog.md)** —
  the full scenario / risk catalog for the corridor: what's engineered and gated in
  C1 today, what cross-component failure modes are still open (C3–C6, not yet built),
  and what's irreducible risk that has to be priced or insured rather than fixed.
  Audited against the real test suite on 1 Sep 2026.
- **[c2-deposit-watcher-build-prompts.md](docs/03-build/c2-deposit-watcher-build-prompts.md)** —
  the deposit watcher, specified the same way C1 was: sequenced build chunks
  (C2.0 → C2.10) with acceptance criteria, written for an AI coding agent. Opened
  with four prerequisite gaps against the already-built C1; the endpoint gap is
  now closed (C1.11) and the spec updated to match the shipped shape. **Built** —
  all chunks C2.0–C2.10 shipped, wired to a live chain-watching engine and
  verified against real BSC — see `depositwatcher/`.
- **[c3-screening-build-prompts.md](docs/03-build/c3-screening-build-prompts.md)** —
  screening, specified the same way as C1/C2: sequenced build chunks (C3.0 → C3.9)
  with acceptance criteria. **Built, discovery loop wired** — all chunks shipped,
  including C3.9's own replay ship-gate harness — see `screening/`. `cmd/screend`
  serves the manual-review/audit HTTP surface and now runs the discovery loop for
  real. The pipeline and re-screen loops stay unwired until a real AML vendor is
  chosen and contracted — vendor choice was deliberately left unmade, and wiring a
  mock into a production compliance path would be worse than the honest gap.
- **[c4-energy-broker-build-prompts.md](docs/03-build/c4-energy-broker-build-prompts.md)** —
  the energy broker, specified the same way as C1–C3: sequenced build chunks
  (C4.0 → C4.9) with acceptance criteria. **Built and production-wired** — all
  chunks shipped, including C4.9's own replay ship-gate harness, and `cmd/brokerd`
  is fully wired, including real Tronsell/Netts/CatFee HTTP clients and a real
  TronGrid on-chain reader. The doc's own "Read this fourth" addendum records why
  the original buffer design changed after those real vendor integrations were
  built — none of the three vendors' APIs support retargeting an existing
  delegation, which the first design assumed — see `energybroker/`.
- **[c5-payout-dispatcher-build-prompts.md](docs/03-build/c5-payout-dispatcher-build-prompts.md)** —
  the payout dispatcher, specified the same way as C1–C4: sequenced build chunks
  (C5.0 → C5.11). **Built** — all chunks shipped, including C5.11's own replay
  ship-gate harness (11/11 scenarios, 6/6 final assertions passing against a
  real ledger and real Postgres) — see `dispatcher/`. Originally written against
  a *proposed* synchronous `SigningService` interface plus a fake, since S1 did
  not exist yet even as a design; that proposal was **superseded** by
  s1-key-management-build-prompts.md's async, request/poll version before any
  C5 code was written, so the shipped component signs against S1's real,
  shipped contract (S1 itself is still a fake everywhere, pending a real KMS
  adapter). Also proposed a fix for a real atomicity gap this document found in
  C1's dispatching→held path (`POST /v1/orders/{id}/dispatch-failure`) — not yet
  built into C1; C5 ships against the interim two-call sequence the doc itself
  names as the fallback. C5.8's own Sweep-batching chunk surfaced a second real
  contradiction — decision 1 ("no smart contracts on either chain") versus
  decision 6's batching-margin claim, which needs one to exist — resolved the
  same way: build against a proposed contract interface and a fake, real
  deployment left as an explicit open item.
- **[s1-key-management-build-prompts.md](docs/03-build/s1-key-management-build-prompts.md)** —
  key management, specified the same way as C1–C5: sequenced build chunks
  (S1.0 → S1.6) turning `s1-key-custody-architecture.md`'s decisions into code.
  **Built** — all chunks shipped, including S1.6's own replay ship-gate harness,
  which caught one real spec bug (an over-strict "exactly 2 approvers" final
  assertion a legitimate retry scenario violates) during the build itself — see
  `s1/`. Replaces C5's original synchronous `Sign` proposal with a request/poll
  `SigningService` (mirroring C4's own PENDING→CONFIRMED reservation shape)
  since a human-approval path can legitimately take minutes to hours, which a
  synchronous call can't wait on safely. No real cloud KMS adapter yet — signs
  against `kmssign.KMSClient`, real for everything except the actual AWS/GCP
  call.
- **[c6-api-gateway-build-prompts.md](docs/03-build/c6-api-gateway-build-prompts.md)** —
  the customer-facing API, specified the same way as C1–C5/S1: sequenced
  build chunks (C6.0 → C6.9) turning decision 5's own "quote-then-order"
  contract into code. **Built** — all chunks shipped, including C6.9's own
  replay ship-gate harness, which runs real `ledgerd`/`watcherd` subprocesses
  (not mocks) through all seven SCENARIO MIX rows and all five FINAL
  ASSERTIONS, `ALL PASSED` — see `gateway/`. Resolved one real contradiction
  in the doc's own prose (marginal vs. flat-rate fee brackets) by trusting
  the doc's own golden test and its cited source over its prose. No real
  webhook-endpoint-verification documentation for customers written yet;
  everything else in decision 5's own list (90s quote lock, idempotency keys,
  HMAC-SHA256 webhooks with 8 retries, the four-trigger sandbox) is real.
- **[mvp-proof-run-plan.md](docs/03-build/mvp-proof-run-plan.md)** — written
  8 Sep 2026 by a cloud session working alongside the one building C1–C5/S1:
  scopes the smallest real end-to-end proof (not the full C6 gateway) that
  proves the pipeline works, and names exactly what stays fake (S1's
  `FakeKMSClient` — already real signatures, just not HSM custody) versus what
  must be real (BSC deposit detection, TRON energy rental, signing, broadcast,
  confirmation). **Built, and executed for real** — C5's own orchestration
  loop (`internal/orchestrate`), C3's `AlwaysCleanProvider` wiring, the
  minimal order-origination driver (`proofrun/`), and a docker-compose stack
  spanning C1–C5/S1 all exist, per this doc's own §2, and on 9 Sep 2026 the
  run itself happened: real BSC USDT deposits detected, real TRON energy
  rented from a live CatFee account, real signatures, two real TRC20 payouts
  broadcast and confirmed on TRON mainnet — independently verified on
  Tronscan, not just this system's own say-so. Running it for real (against
  live vendors and a live chain, not fakes) is what found the five bugs
  itemized in each affected component's own entry above — every one of them
  invisible to a fake-backed test by construction. See `proofrun/` and the
  root `docker-compose.yml`/`.env.proofrun.example` for how to reproduce it;
  the operator-supplied prerequisites it lists (real TRX, a real
  energy-vendor account, a TronGrid key, a real BSC USDT wallet) are exactly
  what this run actually used.
- **[ops-console-build-prompts.md](docs/03-build/ops-console-build-prompts.md)** —
  the ops console: a small internal web dashboard in front of C1–C5/S1, closing
  three real operator gaps (a C2 cursor-inspection route, a C4 reservation
  listing, and a general read/operate surface over the corridor) surfaced while
  running the MVP proof run by hand. **Built** — see `opsconsole/` below. Holds
  no database of its own; session state is a signed cookie, not a
  server-side table.
- **[operations-control-center-build-prompts.md](docs/03-build/operations-control-center-build-prompts.md)** —
  a separate, much larger design for the same "operator dashboard" concern
  (RBAC roles, a new Ops BFF service, a React/TS/Vite frontend, customer CRUD,
  an audit log) written independently on `origin/main` while the ops console
  above was being built on this branch. **Not built** — nothing in this doc
  exists in code. The merge that brought both branches together (`3aed5fd`)
  deliberately kept both docs rather than picking one, and explicitly left
  reconciling them as "a separate decision" that has still not been made — treat
  this doc as an unbuilt, unreconciled alternative/extension proposal, not as
  describing anything currently running.

### `ledger/`

The built C1 service — Go + PostgreSQL, no ORM, hand-written SQL, `pgx/v5`, `chi`
routing, `goose` migrations. `cmd/ledgerd` (the service), `cmd/migrate`, `cmd/replay`
(the C1.9 ship-gate harness), `cmd/seed-console` (seeds accounts for the manual test
console at `docs/console.html`). Packages under `internal/`: `money`, `accounts`,
`journal`, `orders`, `recon`, `halt`, `httpapi`, `replay`. Operational docs live at
`ledger/docs/`: `runbook.md`, `accounts.md`, `errors.md`, `openapi.yaml`.

### `depositwatcher/`

The built C2 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose` migrations.
`cmd/watcherd` (the live chain-watching engine), `cmd/migrate`, `cmd/replay`.
Packages under `internal/`: `addresses` (HD derivation), `chain` (ingestion,
finality, reorg detection), `candidates`, `orphaned`, `finality`, `ledgerclient`,
`httpapi`, `replay`, `money`. Operational docs at `depositwatcher/docs/openapi.yaml`.

### `screening/`

The built C3 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose`
migrations. `cmd/screend` (the HTTP boundary plus the real discovery loop —
see above; also starts the pipeline/re-screen loops when `SCREENING_PROVIDER=
always_clean` and `SCREENING_ALLOW_ALWAYS_CLEAN=true` are both set, against
`provider.AlwaysCleanProvider` — a labeled non-production placeholder for
`docs/03-build/mvp-proof-run-plan.md`'s own proof run, never a real
compliance decision), `cmd/migrate`, `cmd/replay` (C3.9's own ship-gate
harness). Packages under `internal/`: `provider` (the vendor-agnostic AML
interface, plus `AlwaysCleanProvider`), `cache`, `verdict`, `discovery`,
`pipeline`, `holds`, `rescreen`, `ledgerclient`, `httpapi`, `replay`.
`internal/holds`' own `RefundEntryBuilder` (the journal entry a manually
rejected hold's `held→refunded` transition needs) has two implementations
now: `StubRefundEntryBuilder` (every non-RELAY tier, `ErrRefundEntryNotImplemented`
— unchanged, still no owner for the physical BEP20 refund) and, added
14 Sep 2026 for Model F, `RelayAwareRefundEntryBuilder` (`internal/holds/relay_refund.go`):
asks relayd for the entry via a new narrow client, `internal/relaydclient`,
falling back to the stub for any external_id relayd reports it has no
relay leg for. `cmd/screend` wires the real one only if
`SCREENING_RELAYD_BASE_URL`/`SCREENING_RELAYD_TOKEN` are both set —
optional, same "Model F is a separate product line" posture
`opsconsole`'s own Relayd/Tronwatcher clients already take.

### `energybroker/`

The built, production-wired C4 service — Go + PostgreSQL, `pgx/v5`, `chi`
routing, `goose` migrations. `cmd/brokerd` (the full production server: pricing,
routing, the buffer, reservations, all wired), `cmd/migrate`, `cmd/replay`
(C4.9's own ship-gate harness). Packages under `internal/`: `provider` (the
vendor-agnostic interface plus real Tronsell/Netts/CatFee HTTP clients),
`pricing`, `routing`, `buffer` (including the real `TronGridReader` on-chain
verifier), `reservations`, `ledgerclient`, `httpapi`, `replay`.

### `s1/`

The built S1 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose`
migrations. `cmd/s1d` (the HTTP boundary — two separate bearer-auth scopes,
one for C5's own signing calls, one for human approvers; refuses to start
without `S1_KMS_CLIENT` set, no default), `cmd/migrate`, `cmd/replay` (S1.6's
own ship-gate harness), `cmd/seed-slot-key` (a one-time, idempotent CLI to
register a slot's own signing key — `internal/httpapi` has no `POST /v1/slots`
route, a real gap the MVP proof run surfaced, mirroring `dispatcher/cmd/seed-slot`'s
own precedent). Packages under `internal/`: `kmssign` (the *only*
package that may hold or produce signing-capable code — `KMSClient` interface,
`FakeKMSClient`, and `Wrapper`, the real TRON-over-KMS signing mechanics —
enforced by its own dependency test), `slots` (the key registry, deriving each
slot's real TRON address from its KMS public key), `requests` (the
`SigningService` queue: auto-sign under threshold, 2-of-N human approval at or
above it), `httpapi`.

### `dispatcher/`

The built C5 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose`
migrations. `cmd/dispatchd` (the HTTP boundary — `POST /v1/dispatch` still
performs only the synchronous half of a dispatch, slot selection plus the E2
conversion entry — plus, as of 9 Sep 2026, `internal/orchestrate`'s own
background loop, carrying a screened Direct/Standard order the rest of the
way: energy reservation, signing, broadcast, and finality confirmation, no
manual step. Sweep-tier orchestration is still not driven by any loop — it
needs the real multisend contract this repo doesn't have yet), `cmd/migrate`,
`cmd/replay` (C5.11's own ship-gate harness), `cmd/seed-slot` (a one-time,
idempotent CLI to register a real payout slot — `internal/httpapi` has no
`POST /v1/slots` route, a real gap the proof-run docker-compose stack
surfaced, mirroring `ledger/cmd/seed-console`'s own precedent rather than a
new production HTTP endpoint or manual SQL). Packages under `internal/`:
`slots` (the slot-identity registry, cap-checked selection, and real
Tether-blacklist freeze detection — verified live against USDT-TRC20's own
`isBlackListed(address)`), `txbuild` (offline TRC20 transfer and
proposed-multisend construction — both verified, where a real contract
exists, against a live TRON node, plus `GrpcBroadcastClient`'s own
`CurrentBlockReference` for real block-reference resolution), `signing` (the
S1 client), `energy` (the C4 client), `dispatch` (the state machine: entering
dispatching, broadcast with proven exactly-once semantics, finality
confirmation, non-retryable-failure handling and reconciliation, Sweep
batching, freeze handling), `orchestrate` (the background loop above),
`ledgerclient`, `httpapi`, `replay`.

### `gateway/`

The built C6 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose`
migrations, `prometheus/client_golang`. `cmd/gatewayd` (the HTTP boundary,
plus three background loops: C6.4's reconciliation loop, C6.6's webhook
trigger and delivery loops), `cmd/migrate`, `cmd/replay` (C6.9's own
ship-gate harness — builds and runs real `ledgerd`/`watcherd` subprocesses,
see below). Packages under `internal/`: `pricing` (the pure fee-schedule
library — flat rate per bracket, not marginal, see the gateway entry in
`component-map.md` for why), `customers` (API keys — `sk_live_`/`sk_test_`,
SHA-256 hashed at rest, never a slow password hash, since GenerateAPIKey's
own 256 bits of randomness makes that the wrong tool — plus each customer's
own webhook secret and URL), `ratelimit` (per-customer token bucket),
`quotes`, `orders` (`gateway_orders`, the row that exists precisely to
close the C1-order-created/C2-address-assigned partial-failure window),
`reconcile` (C6.4), `webhooks` (C6.6 — trigger and delivery are separate
loops, deliberately: the trigger polls C1 every 5s, tighter than any
sibling component's own background interval, because it is the one loop in
this whole system with a customer-facing SLA sitting on top of it),
`sandbox` (C6.7 — a fully separate `sandbox_orders` table and `sk_test_`
customer namespace; `TestSandboxNotImportedOutsideAllowlist` mechanically
enforces that no production handler ever imports this package),
`c1client`/`c2client` (this component's own two upstream HTTP clients),
`httpapi`, `testledger`/`testwatcher` (build and run real `ledgerd`/
`watcherd` subprocesses for integration tests — deliberately duplicated
from screening's/dispatcher's own identical packages, not shared, matching
this whole project's per-module convention), `replay` (C6.9 — drives the
real router against those real subprocesses through all seven SCENARIO MIX
rows, including a real concurrent-order-creation race and a real
system-halt refusal, and all five FINAL ASSERTIONS; C3/C4/C5/S1 are never
started — an order's own path to `settled` is produced by a harness-only
fixture posting the same C1 transition calls those real components would).

### `proofrun/`

The MVP proof run's own minimal order-origination driver
(`docs/03-build/mvp-proof-run-plan.md`'s §2(b)) — deliberately not
`c6-api-gateway-build-prompts.md`'s full spec: no customer auth, no rate
limiting, no persisted quotes, no tiered pricing engine, no HMAC webhooks, no
sandbox. `cmd/proofrund` serves two routes: `POST /v1/payouts` (computes a
straightforward 25bp fee plus a flat placeholder network fee — see
`internal/driver`'s own doc comment — creates the order against C1, gets a
deposit address from C2) and `GET /v1/payouts/{external_id}` (reconstructs
lifecycle status across C1/C2/C5, degrading gracefully rather than failing
outright if one of them is unreachable). Packages under `internal/`:
`upstream` (three narrow HTTP clients to C1/C2/C5), `driver` (the fee math
and orchestration logic above), `httpapi`, `money`. This was scaffolding to
prove the pipeline works, not a step toward C6 — C6 (`gateway/`) has since
been built in full, separately, against this now-proven backend, and
supersedes this driver as the real customer-facing API.

### `opsconsole/`

The built ops console — Go, `chi` routing, no database of its own (session
state is a signed cookie, per `docs/03-build/ops-console-build-prompts.md`;
see that doc for what "no database" does and doesn't mean here). `cmd/opsconsoled`
serves the dashboard: an operator login (`internal/session`), read/operate
views over each upstream component (`internal/httpapi/{home,ledger,watcher,
broker,s1,screening_dispatcher}.go`), and an append-only, write-before-call
JSON-lines audit log (`internal/auditlog`) independent of each upstream's own
audit trail. `internal/opclient` holds one narrow HTTP client per upstream
(`ledger`, `watcher`, `screening`, `broker`, `dispatcher`, `s1`). Talks to
C1–C5/S1 over the same internal HTTP surface those components already
exposed for each other, plus the operator-facing routes added to C2/C4/S1
(`96ecd02`) specifically because this console needed them. Landed via a
same-named-file merge conflict against `origin/main`'s own, unbuilt
"Operations Control Center" doc — kept as `ops-console-build-prompts.md`
above; the origin-side doc was renamed to
`operations-control-center-build-prompts.md` rather than discarded. Not
wired into the root `docker-compose.yml` at merge time; now wired as its own
`opsconsole` service with an `opsconsole_audit` volume for its log file.

**Model F visibility (added 13 Sep 2026):** a read-only relay-leg listing
page (`internal/httpapi/relayd.go`, `/relayd/legs?status=`) plus home-page
cards for `tronwatcher` and `relayd`, calling two new narrow clients
(`internal/opclient/{relayd,tronwatcher}.go`) against a new
`GET /v1/relay-legs?status=` relayd added for exactly this (relayd's own
local rows only, no per-row cross-service fetch to C1 — see that
handler's own doc comment). Deliberately read-only, unlike the
holds/slots pages: R5's own automatic refund path already handles the
one leg-level failure an operator could safely act on from here, and an
`UNRECOVERABLE` leg's resolution is an off-system operational decision
(a vendor support ticket, a compensation reserve), not a button this
console could correctly offer — its job is to make that state visible
(a highlighted row, a home-page count), not to act on it. Unlike every
other upstream client here, `Relayd`/`Tronwatcher` are allowed to be
`nil` — Model F is a separate, optional product line, so an unset
`OC_RELAYD_BASE_URL`/`OC_TRONWATCHER_BASE_URL` degrades that one
card/page rather than refusing to start (`internal/httpapi.Server`'s own
doc comment explains why this one pair breaks the "every dependency
required" posture every other upstream client in this package takes).

### `tronwatcher/`

Model F's own C2′: the TRON-side deposit watcher, a near-mirror of
`depositwatcher/`'s own structure retargeted to TRC20. `internal/addresses`
reuses `depositwatcher/internal/addresses/bip32.go`'s own CKDpub math
unchanged, encoded as a TRON base58check address instead of an EIP-55 hex
one. `internal/chain` scans TronGrid's REST API
(`/v1/accounts/{address}/transactions/trc20`) per watched address — TRON has
no `eth_getLogs` equivalent, so this watches per-address rather than
per-contract-then-filter, unlike C2. `internal/finality` confirms via TRON's
own per-transaction solidity endpoint
(`/walletsolidity/gettransactioninfobyid`), not a "latest finalized height"
model. A deposit finalizes into a relay leg's own suspense account
(`asset:relay:leg:<order_id>`), not a treasury account. Own Postgres
(`tronwatcher-db`), own migrations, `cmd/tronwatcherd`. Unit- and
integration-tested (real Postgres); not yet proven against a real TronGrid
account or TRON mainnet.

### `relayd/`

Model F's own front door and driving loop, both relay directions. New Go
module, own Postgres (`relayd-db`), no shared `internal` package with any
other module (this repo's own convention throughout). `internal/relay` is
the leg state machine (`AWAITING_DEPOSIT → FORWARDING → FORWARDED →
SETTLED`/`FAILED`); `internal/orchestrate` is the driving loop, mirroring
`dispatcher/internal/orchestrate`'s own two-phase-per-tick shape. `internal/upstream`
is the vendor-agnostic `SwapProvider` interface (`PlaceholderProvider` today,
gated behind `UPSTREAM_PROVIDER=placeholder` + `UPSTREAM_ALLOW_PLACEHOLDER=true`,
mirroring `screening`'s own double-gate convention for a non-production
default). The TRC20-direction forward leg reuses `dispatcher/internal/txbuild`'s
and `tronchain.go`'s own transfer-construction/broadcast code near-verbatim
(`internal/txbuild`, `internal/tronbroadcast`); the BEP20-direction forward
leg needed genuinely new code — no service in this repo had ever built or
broadcast a BEP20 transaction before — added as `internal/evmtx` (ERC20
`transfer(address,uint256)` construction against `go-ethereum`) and
`internal/evmbroadcast` (a real BSC node client: nonce/gas-price resolution,
broadcast, BEP-126 "finalized"-tag finality, mirroring `depositwatcher`'s own
identical finality approach). Both directions sign through the *same* S1
slot key — one secp256k1 key, two address encodings (TRON base58check and
EIP-55 hex), added to S1 as a small, additive `EVMAddress` method/endpoint
(`s1/internal/slots/evm.go`) — S1's own signing internals needed no change,
since `kmssign.Wrapper.Sign`'s raw 0/1-recovery-byte output already matches
what `go-ethereum`'s own `EIP155Signer` expects. `internal/driver` is the
customer-facing front door (`POST /v1/relay-legs`, `GET
/v1/relay-legs/{external_id}`), mirroring `proofrun/internal/driver`'s own
shape. Both directions' happy flow is tested end-to-end against a real
`ledgerd` + real Postgres (`internal/orchestrate/orchestrate_integration_test.go`):
real double-entry postings for `relay_forward_start` and `relay_settle`,
every suspense account verified to close to exactly zero, commission booked
into a real backing `asset:relay:commission_wallet:<asset>` account (not
just left as an unbacked credit). `internal/orchestrate/refund.go` is R5's own home, now complete across all three
triggers: a leg stuck unable to broadcast its own forward transfer past
`RELAYD_FORWARDING_TIMEOUT` is refunded automatically, on-chain, back to
whoever actually sent the deposit — reusing C1's *existing*
`Dispatching→Held→Refunded` transitions, not a new one; a leg whose
forward transfer already confirmed but whose upstream swap then failed lands
`UNRECOVERABLE` and fires a real alert (`internal/alert`). The manual
`HELD→REFUNDED` path is also wired (added 14 Sep 2026): `GET /v1/relay-legs/{id}/refund-entry`
(`internal/httpapi/relay_legs_handlers.go` → `driver.BuildRefundEntry`) computes the
same entry shape for a real held order that `screening/internal/holds.RelayAwareRefundEntryBuilder`
calls and submits through C3's own existing `Reject` flow, unchanged;
`internal/orchestrate/refund.go`'s own new `startExternallyRefundedLegs` phase notices
the order reached `refunded` this way and drives the on-chain broadcast through the
*same* `advanceRefundPendingLegs` machinery the timeout-triggered path already uses —
no new broadcast code, only a new trigger. The last self-contained gap, a leg stuck
`AWAITING_DEPOSIT` because `upstream.CreateOrder` itself never succeeds, shipped 14 Sep
2026 too: `refundStuckAwaitingDepositLegs` watches `relay_legs.forward_attempt_started_at`
(a new idempotent first-write-wins timestamp, set by `startOne` the first time it ever
sees a leg reach `screened`, distinguishing "still waiting on a deposit" from "a deposit
arrived and forwarding is genuinely stuck") against the same `RELAYD_FORWARDING_TIMEOUT`,
and refunds through one new, additive C1 transition, `{Screened, Refunded}` — no other
change to `ledger/`'s own transition table logic. `internal/orchestrate/reconcile.go` is the
stale-relay-leg reconciliation alarm (architecture doc §5): any leg still
`FORWARDING`/`FORWARDED`/`REFUND_PENDING` past `RELAYD_STALE_LEG_ALERT_AFTER` (opt-in,
no default) fires a `SeverityWarning` alert once, checked against `relay_legs.updated_at`
rather than querying C1's journal directly — `AWAITING_DEPOSIT` gets the active refund
mechanism above instead of this passive alarm. `internal/replay` + `cmd/replay` is R6's own
ship gate, connecting to a real,
already-running `ledgerd` (the same convention every sibling `cmd/replay` in this repo
already uses) — seven scenarios (both directions' happy path, both refund-by-timeout
triggers, the manually-rejected-hold refund, `UNRECOVERABLE`-with-alert, and the stale-leg
alarm), run for real across many seeds spanning two days, all `ALL PASSED` (still
against `upstream.PlaceholderProvider` — R6 hasn't been re-run against the real
FixedFloat integration below). `internal/upstream/fixedfloat.go` is R4: a real
`SwapProvider` against FixedFloat's v2 API (`/price`, `/create`, `/order`, HMAC-SHA256
request signing), gated behind `UPSTREAM_PROVIDER=fixedfloat` with no default for any
of its own credential/currency-code env vars (`cmd/relayd/upstream_provider.go`).
Unit-tested against a fake HTTP server only — no real order has been placed against
FixedFloat's production API, and its `FIXEDFLOAT_CCY_USDT_TRC20`/`_BEP20` values are
still unverified working assumptions, not confirmed against a live `GET
/api/v2/ccies` response (see `docs/01-strategy/model-f-relay-findings.md`'s own R2
decision record for exactly what is and isn't independently confirmed here). Not yet
built: real per-vendor `EXPIRED` semantics (no longer gated on R2/R4, now gated on
reading FixedFloat's own `emergency.choice` field from a live order — see
`docs/03-build/model-f-relay-build-prompts.md`'s own R5 entry).
`cmd/relayd` is the binary — see its own
`buildDriverAndOrchestrator` for the full list of required env vars, no
hardcoded defaults for anything real-money-shaped.

## Reading order

If you are new to this: **findings → architecture decisions → component map → C1 build
prompts → scenario catalog → `ledger/` → C2 build prompts → `depositwatcher/` → C3
build prompts → `screening/` → C4 build prompts → `energybroker/` → S1 architecture
→ S1 build prompts → `s1/` → C5 build prompts → `dispatcher/` → MVP proof-run plan
→ `proofrun/`.** Each document assumes the previous one is settled and does not
re-open it.

**Ops console:** a side branch off C5/S1, not a gate on the sequence above —
`ops-console-build-prompts.md` → `opsconsole/`. Read it whenever the internal
operator dashboard is the thing in question; nothing downstream of C5/S1
depends on it.

**Model F** (the zero-float relay, a separate product line, not a continuation of the
sequence above) has its own reading order, given in full in the "Model F" section
near the top of this file.

## Repository conventions

- Decisions live in documents, not in commit messages or chat history. If a decision
  changed, the document changes and the diff is the record.
- Numbers carry their baseline. Anything derived from TRX at $0.34, 74,750 blended
  energy per payout, or $255k of working float says so.
- A document does not re-litigate a decision made upstream of it.
- Now that code exists, **the repository is the canonical source of build status.**
  The project workspace this repo mirrors from can go stale between visits — it only
  updates when someone explicitly checks git — so when the two disagree on what's
  built, trust this repo.

## Status of the numbers

Every figure here is either observed, modelled, or assumed — and the documents mark
which. The assumptions carrying the most weight, and the dates by which they must be
tested, are tabulated in the architecture decisions record under *"What must be
tested, and by when."* Treat untested assumptions as untested.

---

Private working repository. Not an offer, not investment advice, and not a
description of a live service.
