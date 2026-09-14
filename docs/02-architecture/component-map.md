# Component map — build decomposition and dimensions

_v1.0, 31 Aug 2026. Decomposition of decision 1 in `docs/02-architecture/product-operations-architecture.md` ("six services, one ledger, no smart contracts"). This is the build-order and scoping document; it does not re-open any architecture decision._

## The six services + three supporting pieces

| # | Component | One-line purpose | Eng-weeks | Risk | Phase |
|---|---|---|---|---|---|
| C1 | **Ledger core** | Single source of truth for money and order state | 1.5 | High (invariants) | MVP — first |
| C2 | **Deposit watcher (BSC)** | Turn inbound BEP20 USDT into confirmed, attributed credits | 1.5–2 | High (reorg correctness) | MVP |
| C3 | **Screening** | Release / hold decision on deposit provenance | 1.0 | Low–med (vendor) | MVP |
| C4 | **Energy broker** | Acquire TRON energy at the 25.7 sun blend before every payout | 1.5 | Med (3 vendors, timing race) | MVP |
| C5 | **Payout dispatcher (TRON)** | Deliver USDT to recipient and confirm to SR finality | 2.0 | High (money out) | MVP |
| C6 | **API gateway** | Customer contract: quote → order → status → webhook | 1.5 + 0.5 sandbox | Med | MVP |
| S1 | Key management / signing | BSC HD seed, 6 TRON slot keys, split custody | 0.5–1.0 | High (terminal) | MVP, cross-cutting |
| S2 | Treasury rebalancer | 4-hour loop, 65/35 target | 0 (runbook + dashboard) | Med | Service in Phase 2 |
| S3 | Status page | P50/P95/P99 by tier — the primary sales asset | 1.0 | Low | After first real traffic |

MVP total ≈ 9–10 eng-weeks ≈ two engineers × 5 weeks, inside the 6–8 week envelope in the findings doc.

## Dependency graph and build order

```
C1 Ledger ──┬─→ C2 Deposit watcher ──→ C3 Screening ──┐
            │                                          ├─→ C5 Payout dispatcher ──→ S3 Status page
            ├─→ C4 Energy broker ─────────────────────┘
            └─→ C6 API gateway
                 S1 Keys — required before C5 touches mainnet
```

C6 and C4 can be built in parallel with C2 once C1's journal-entry contract is frozen. Nothing may be built before C1, because C1's entry shape is every other component's interface.

## Per-component dimensions

### C1 — Ledger core
- **Owns:** account tree (customer liability · per-slot TRON asset · per-deposit-address BSC asset · custody suspense · energy expense · fee revenue), append-only balanced journal, order state machine, reconciler, halt signal.
- **Does not own:** chain access, keys, pricing, customer auth.
- **Interface:** idempotent `POST /entries`, `GET /balances`, `POST /orders/{id}/transition`, halt broadcast.
- **State machine:** `quoted → funded → screened → dispatching → settled | refunded | held`.
- **Gate:** 10k-order replay including duplicate idempotency keys, reorg reversals, partial batch failures — balances tie, halt fires on injected drift.

### C2 — Deposit watcher (BSC)
- **Owns:** HD address derivation + assignment/retirement, block cursor, rolling block-hash window for reorg detection, Transfer-log parsing for the BEP20 USDT contract only, amount classification (exact / under / over / dust), 15-confirmation finality declaration.
- **Does not own:** sweeping deposits, screening, payout, pricing, address funding with BNB.
- **Emits:** `deposit.detected` (0-conf, advisory only) · `deposit.final` (15-conf, credits the ledger) · `deposit.reorged` (reversing entry).
- **Scale dimension:** at 100 payouts/day ≈ 100 deposits/day, ~4/hour peak; one address per order, ~36k addresses/year derived. Trivial throughput — this component is about *correctness*, not scale.
- **Hard parts:** reorg deeper than the confirmation window; RPC provider divergence (needs ≥2 providers with hash agreement before finality); duplicate log delivery; deposits to retired addresses; wrong-token and zero-value transfers; deposits arriving after quote expiry.
- **Verify before coding:** current BSC block time and therefore the wall-clock cost of 15 confirmations — this is a direct input to the tier SLAs.
- **Proven live, 9 Sep 2026** — the MVP proof run (`docs/03-build/mvp-proof-run-plan.md`) detected two real BSC deposits end to end. Found and fixed two real bugs: `ReportDepositFinal` built the customer's own liability account code missing the `:USDT_BEP20` suffix every other component's identical account uses, and never called `EnsureAccount` for either account it references (C1 does not auto-vivify one on first journal reference); and the block-scanning cursor had no chunking, so it stalls permanently against a real high-traffic contract on a real RPC provider's own result cap (a failed tick never advances the cursor). Both fixed — see `depositwatcher/`. **Second proof run, 10 Sep 2026** — found the chunking cap from the first fix still too wide against a free-tier provider's own hard per-call block-range limit (10 blocks, independent of the result-count cap it was originally sized against); lowered accordingly.

### C3 — Screening
- **Owns:** provider call (Chainalysis / TRM / Elliptic), result caching by sender address, hold queue, manual review path, reason codes.
- **Does not own:** the release action itself — it returns a verdict; the ledger transitions the order.
- **Dimension:** ~100 calls/day at MVP volume. Cost per call is a real line item at scale; cache by sender.
- **Built** — `docs/03-build/c3-screening-build-prompts.md` (C3.0–C3.9), see `screening/`. The discovery loop runs for real against a real, running C1 (no vendor dependency). The pipeline and re-screen loops have no real `provider.ScreeningProvider` wired — vendor choice was deliberately left unmade — but as of 9 Sep 2026 `cmd/screend` can start both against `provider.AlwaysCleanProvider`, an explicitly-labeled non-production placeholder, for `docs/03-build/mvp-proof-run-plan.md`'s own supervised proof run only.

### C4 — Energy broker
- **Owns:** routing 60/35/5 across Tronsell / Netts / CatFee, price polling with a ceiling, pre-order lead time, delegation confirmation, fallback ladder, per-payout cost attribution to the ledger.
- **Does not own:** broadcasting the USDT transfer.
- **Hard parts:** delegation must land *before* broadcast or the payout burns TRX at market rate; provider outage mid-flight; price spike above the ceiling (must be able to hold rather than overpay).
- **Blocked on:** the week-2 wholesale pricing calls. Retail-quote pricing invalidates the routing weights.
- **Built** — `docs/03-build/c4-energy-broker-build-prompts.md` (C4.0–C4.9), see `energybroker/`. `cmd/brokerd` is fully wired: real Tronsell/Netts/CatFee HTTP clients, a real TronGrid on-chain reader. **Proven against a real vendor account, 9 Sep 2026** — real CatFee credentials, two real energy purchases actually delegated on TRON mainnet. Found and fixed one real bug: `VerifyOnChain` required the on-chain delegation to be `>=` the *exact* requested units; a real CatFee order for 65,000 landed on-chain as 64,999 (harmless vendor-side rounding), rejected outright by the exact check. Fixed with a small (0.1%) tolerance. **Second proof run, 10 Sep 2026** — found a second real bug in the same check: `VerifyOnChain` called immediately after a real `Delegate()` response saw 0 available energy and failed a genuinely-successful purchase (confirmed independently seconds later at 64,999/65,000 units) — checked before the on-chain state had time to propagate. Fixed with a bounded 3-attempt/9s retry; closed the resulting stuck-FAILED-reservation gap with a new `cmd/reconcile-reservation` ops tool.

### C5 — Payout dispatcher (TRON)
- **Owns:** slot selection under the $50k / 5,000-tx caps, batch window for the Sweep tier, multisend construction, signing, broadcast, SR-finality confirmation, retry with exactly-once semantics.
- **Does not own:** energy acquisition, screening.
- **Hard parts:** energy exhausted at broadcast; slot frozen mid-flight; duplicate broadcast on retry; partial batch success in multisend; slot rotation without stranding balance.
- **Testnet dependency:** the week-6 batched-multisend energy measurement (35,000/recipient assumption) is measured here — this component's own energy-per-recipient config, not yet independently confirmed.
- **Built** — `docs/03-build/c5-payout-dispatcher-build-prompts.md` (C5.0–C5.11), see `dispatcher/`. Signs against S1's real request/poll `SigningService` contract but an in-process fake standing in for S1 itself (no real cloud KMS behind S1 yet, so nothing in this component can touch mainnet). Sweep-tier batching is built against a *proposed* multisend contract interface (no smart contract exists on either chain per decision 1 — see the doc's own C5.8 discussion) and a fake standing in for it. Required a small addition to C1 (`POST /v1/accounts`, idempotent-on-code) so this component can ensure the per-customer/per-slot ledger accounts it references actually exist — C1 had no way to create an account over HTTP before.
- **Wired, 9 Sep 2026** — `internal/orchestrate`, started by `cmd/dispatchd`: the background loop that carries a screened Direct/Standard order from `EnterDispatching` through energy reservation, signing, broadcast, and finality confirmation with no manual step, closing the gap `cmd/dispatchd`'s own doc comment previously flagged as not yet built. Found and fixed one real bug along the way: `ConfirmFinality` never updated `dispatch_state` to `SETTLED` the way every other settlement/failure path in this package did. Sweep-tier orchestration is still undriven (needs the multisend contract above). Also added `cmd/seed-slot` — a one-time, idempotent CLI to register a real payout slot, since `internal/httpapi` has no `POST /v1/slots` route (mirrors `ledger/cmd/seed-console`'s own precedent, not a new production endpoint).
- **Proven on TRON mainnet, 9 Sep 2026** — two real settlements, real signatures, real broadcasts, independently verified on Tronscan. Found and fixed one more real bug getting there: `GrpcBroadcastClient` never passed transport credentials, so it couldn't connect to a live node at all. Confirmed live against `grpc.trongrid.io:50051` that plaintext (not TLS) is the correct transport — a TLS handshake against that same endpoint fails immediately, plaintext succeeds.

### C6 — API gateway
- **Owns:** auth, rate limits, quote issuance with the 90s lock, idempotency keys, HMAC-SHA256 webhook signing with 8 retries, sandbox with the four deterministic failure triggers (reorg, screening hold, energy exhaustion, retry storm).
- **Does not own:** money movement; calls a pricing library rather than embedding tiers.
- **Rule:** quote-then-order, never quote-inside-order.
- **Built** — `docs/03-build/c6-api-gateway-build-prompts.md` (C6.0–C6.9), see `gateway/`. `proofrun/`'s own minimal stand-in (no auth, no persisted quotes, no webhooks, no sandbox) is superseded, not extended — C6 is a separate, from-scratch build against the real C1/C2 HTTP contracts. Resolved one real internal contradiction in its own build-prompts doc along the way: the doc's prose describes fee brackets as applied "marginally across bracket boundaries," but its own golden test ($3,000/STANDARD → exactly the 25bp/$7.50 flat rate) and `findings-and-recommendation.md`'s own cited text both require a flat rate per bracket — the golden test and the primary source were trusted over the doc's own prose.
- **C6.4** (reconciliation), **C6.6** (webhook delivery, HMAC-SHA256, exponential backoff), and **C6.7** (sandbox, fully separate `sandbox_orders` table and `sk_test_`-keyed customers, mechanically enforced by an import-boundary test) are all real, not stubs. **C6.9's own replay harness** (`gateway/internal/replay`, `gateway/cmd/replay`) is the ship gate: builds and runs REAL `ledgerd` and `watcherd` binaries as subprocesses (mirroring C3's/C4's own `internal/testledger` convention) and drives all seven SCENARIO MIX rows and all five FINAL ASSERTIONS against them — no C1/C2 mocks anywhere in this component's own test suite. C3/C4/C5/S1 are never started; an order's own progress through screened/dispatching/settled is produced by a harness-only `ledgerFixture` posting the exact same C1 transition calls those real components would, the same posture screening's/dispatcher's own replay harnesses already take toward whichever upstream isn't part of their local test. Run 9 Sep 2026: `ALL PASSED`, twice in a row, against real subprocesses.

### S1 — Key management / signing
- **Owns:** custody of the six TRON slot keys and the BSC HD seed, the only signing capability anywhere in this system.
- **Does not own:** transaction construction, slot selection/caps/rotation (C5's own slot-identity registry), broadcast.
- **Design:** `docs/02-architecture/s1-key-custody-architecture.md` — self-hosted cloud KMS, six independent slot keys, a hybrid auto/2-of-N-human-approval threshold. **Built** — `docs/03-build/s1-key-management-build-prompts.md` (S1.0–S1.6), see `s1/`. No real cloud KMS adapter yet; signs against an in-process fake standing in for one. Same slot-registration gap as C5's own `cmd/seed-slot` — no `POST /v1/slots` route in `internal/httpapi` either — closed with `cmd/seed-slot-key`, found registering a real slot key for the 9 Sep 2026 mainnet proof run.

## What is deliberately not built at MVP

- Treasury rebalancing as a service — a runbook plus a balance dashboard is correct until the 4-hour loop is proven sustainable (month 3 test).
- Any smart contract on either chain.
- ~~A public swap page.~~ **Update, 14 Sep 2026: reversed for Model D** — see `findings-and-recommendation.md`'s own "Update, 14 Sep 2026" section and `docs/01-strategy/model-d-model-f-product-separation.md`. Still not built (no frontend code exists anywhere in this repo yet), but no longer excluded by policy. Model F's own identical constraint (`model-f-relay-findings.md`) is unaffected by this.
- Reverse flow (TRC20 → BEP20) — it arrives with the capital ladder, not before.
