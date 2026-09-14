# C1 / settlement corridor — full scenario catalog

_Companion to `c1-ledger-build-prompts.md` and `component-map.md`. Answers one question: what is the complete list of things that can happen to money in this corridor, and which of them are already engineered away versus which are irreducible risk that has to be priced, capped, or insured rather than fixed._

## How to read this

There are three different kinds of "enough" and they get confused if this stays one flat list:

1. **Enough to ship C1.** Answered by the replay harness in C1.9 — a fixed scenario mix run 10,000 times. Part 1 below is that mix, organized and cross-referenced to the acceptance criteria that already gate it.
2. **Enough to sell a settlement guarantee to an exchange, OTC desk, or payment provider.** Requires the cross-component failure modes in Part 2 — most of these live in C2–C6, which are not built yet, so they're not gated by anything today.
3. **Enough to be actuarially insurable, or to self-insure with a sized reserve.** Requires Part 3 — the handful of scenarios that no amount of engineering removes, only bounds. This is where `findings-and-recommendation.md`'s Tether freeze numbers already live; the rest of Part 3 extends that same treatment to the other terminal risks the corridor carries.

The short answer to "is this enough for our product": Part 1 is enough to trust the ledger. Parts 1+2 are enough to make the settlement-guarantee claim in the GTM pitch true. Part 3 is what an insurer, a reinsurer, or your own board would ask for before signing off on the float — and right now only one line item in it (Tether freeze) has been sized.

---

## Part 1 — Ledger-core scenarios (C1, engineered and gated today)

These are the scenarios the C1.9 replay harness already injects, plus the acceptance-test scenarios from C1.2–C1.8 that aren't part of the harness mix but are equally load-bearing. Every row has a home in the codebase; none of these should be open questions by the time C1.8 is frozen.

**Update, 1 Sep 2026 — audited against the real test suite.** C1.0–C1.10 are now actually built (`ledger/` in the repository), so this part stopped being a spec claim and got checked against real code, row by row, commit `c93dfb9`. 31 of 34 rows checked out exactly as written — including the specific numbers (1,000 concurrent Posts, 200 goroutines, 50 concurrent transitions, 53 of 64 illegal pairs, 1 minor unit of drift) all matching real, accurately-named tests. Three rows had no test behind them despite being listed as proven, and now do — marked **[closed 1 Sep]** below, with the test that closed them. The audit also surfaced one thing this catalog had missed entirely: a real HTTP-boundary bug, found by building and driving a manual test console rather than by test code. It's now §1.8.

### 1.1 — Core money-movement path

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| Clean happy path, full lifecycle | C1.5 transition table, §B worked entries E1–E5 | Order reaches `settled`, trial balance 0 |
| Conversion entry (BEP20 in, TRC20 out, one entry, two assets) | C1.2, §B | Each asset's lines sum to zero independently |
| Fee and network-fee split at settlement | §B entry E2 | `revenue:fee` and `revenue:network_fee` credited exactly |
| Treasury rebalance closing the corridor position | §B entry E5 | **[closed 1 Sep]** Had zero test coverage until the audit. `TestTreasuryRebalanceClosesCorridorPosition` now posts §B's exact E2 then E5 entries and confirms both corridor accounts return to exactly zero |

### 1.2 — Idempotency and concurrency

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| Same idempotency key, same payload, retried | C1.3 | Second call returns original entry, marked Replayed, no second row |
| Same idempotency key, different payload | C1.3 | `ErrIdempotencyConflict`, nothing written |
| 1,000 concurrent identical Posts | C1.3 acceptance | Exactly one row, one Created, 999 Replayed |
| Line reordering on retry | C1.3 | Same hash — order must not matter |
| 200 goroutines posting overlapping account sets | C1.4 acceptance | Zero deadlocks (ascending account-id lock order) |
| 50 concurrent transitions on one order, same expected version | C1.5 acceptance | Exactly one succeeds, 49 get `ErrVersionConflict` |
| Duplicate idempotency keys, half concurrent half sequential | C1.9 mix, 6% | Harness final assertions hold |

### 1.3 — Reorg and reversal

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| Reorg before dispatch — deposit reorged out while `funded` | C1.6 scenario A | Reversed to zero, order back to `quoted`, no loss account touched |
| Reorg after settlement — TRC20 already paid, BEP20 deposit vanishes | C1.6 scenario B | `expense:loss:reorg` posted for full `amount_out`, system halts with `POST_SETTLEMENT_REORG`, order stays `settled` |
| Reorg landing exactly at quote expiry | C1.9 mix (quote expiry bucket) | Order routes to `expired` on the next touch, not stuck |
| Reversal attempted twice on the same entry | C1.6 acceptance | Second call errors; unique index on `reversal_of` holds |
| Reversal of a reversal | C1.6 acceptance | Rejected outright |
| `HandleDepositReorg` called on an order in an unexpected state (e.g. `expired`) | C1.6 acceptance | Named error, not a silent no-op or best-guess branch |

### 1.4 — Screening, holds, and refunds

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| Screening hold, then manual release | C1.5 (`held → screened`), C1.9 mix 5% | Requires actor on the transition |
| Screening hold, then reject and refund | C1.5 (`held → refunded`), C1.9 mix 2% | Halt-blocked transition, requires entry |
| Customer cancels pre-conversion (`funded → refunded`) | C1.5 | Halt-blocked, requires reversing entry |

### 1.5 — Amounts and edge quantities

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| Overpay, underpay, dust deposit | C1.9 mix, 3% | Classification doesn't crash the state machine; C2 owns detection, C1 owns recording whatever it's told |
| Amount with more decimal places than the asset allows | C1.0 / C1.8 | Rejected — parse error, never silently rounded |
| Amount sent as a JSON number instead of a decimal string | C1.8 acceptance | Rejected with `invalid_amount` — this is called out as the most likely way the system loses money silently |
| Zero-amount line in an entry | C1.2 acceptance | Entry rejected |
| Entry with one line, or none | C1.2 acceptance | Rejected |

### 1.6 — Batch and dispatch failure

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| Sweep batch where 40/50 recipients settle, 10 fail | C1.9 mix, 2% | Partial batch handled without corrupting the other 40 |
| Non-retryable dispatch failure → `held`, conversion reversed | C1.9 mix, 1.5%; C1.5 (`dispatching → held`) | Conversion entry reversed atomically with the transition |
| Illegal transition attempted (53 of the 64 possible pairs) | C1.5 acceptance, exhaustive cross-product test | `ErrIllegalTransition`, no row written |

### 1.7 — Integrity, reconciliation, and halt

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| 1 minor unit of drift injected on a USDT account | C1.7 acceptance | Halts within one cycle, account named in `halt_detail` |
| `account_balances` cache corrupted directly via SQL | C1.4 / C1.7 acceptance | `VerifyBalances` catches it, `CACHE_DIVERGENCE` halt |
| Unbalanced entry inserted via raw SQL, bypassing Go validation | C1.2 / C1.7, defence in depth | Postgres trigger refuses it; if it somehow lands, `TRIAL_BALANCE_BROKEN` halts within one cycle |
| `position:corridor` exceeds its configured ceiling | C1.7 | Alert, explicitly **not** a halt — a slow rebalance isn't an error |
| Halt clear attempted without an operator identity | C1.7 acceptance | Rejected — no auto-clear exists |
| Reconciliation snapshot with non-zero drift on a USDT asset | C1.7 | Halts — tolerance is zero for USDT by design, non-zero tolerance only ever considered for TRX rounding |
| All four halt-blocked pairs individually verified (`funded→refunded`, `held→refunded`, `screened→dispatching`, `dispatching→settled`), plus every non-blocked pair confirmed to keep working while halted | C1.5's transition table | **[closed 1 Sep]** Only `screened→dispatching` had ever been behaviorally checked. `TestHaltBlocksExactlyTheDocumentedPairs` now drives all 11 legal pairs through a live halt and checks each against the spec's own table |

### 1.8 — Input validation

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| Order creation rejected on each of its 10 invalid-input branches (empty actor, wrong asset, `quote_expires_at` before `quoted_at`, and so on) | `orders.CreateParams.validate()` | **[added 1 Sep]** Table-driven test added during the audit — previously every existing test only ever exercised the accept path |
| Order transition rejected on each of its 4 invalid-input branches | `orders.TransitionParams.validate()` | **[added 1 Sep]** Same audit, same pattern, mirroring `journal.Reverse`'s existing validation tests |

### 1.9 — HTTP boundary correctness under real client encoding

Not in the original catalog at all — found by building and driving `ledger/docs/console.html`, a manual test console, rather than by any written scenario. Worth calling out on its own because it's a class of bug neither the spec nor the automated harness would have caught: every existing integration test built its URLs by plain string concatenation and never encoded anything, so the gap was invisible until a real client (a browser, using `encodeURIComponent` like any well-behaved HTTP client) hit it.

| Scenario | Where it's handled | Proof it worked |
|---|---|---|
| Account code containing `:` (this system's separator throughout) sent through normal URL-encoding | chi's router matches the *raw* encoded path when the request has any escaping, so `chi.URLParam` returned the still-encoded segment on `GET /accounts/{code}/balance` and both order routes, producing false `404`/`order_not_found` | **[fixed 1 Sep]** A `urlParam()` decode helper at all three call sites, plus a regression test that deliberately encodes a real account code the way a browser does. Commit `7faa738` |

**Coverage note:** every row above now has a named test behind it, not just a spec reference — the 1 Sep audit is what makes that true rather than assumed. There is no scenario in this part that is merely "planned." If any of these ever goes red, that's the blocker, not a gap in this catalog.

---

## Part 2 — Cross-component corridor scenarios (built, per-component)

These come from the "hard parts" called out per component in `component-map.md`. C1 will receive whatever these components report and behave correctly *given* a correct report (Part 1 covers that). What's listed here is what can go wrong *before* it reaches C1.

**Update:** this was originally written when C2–C6 didn't exist yet, hence "not yet built, not yet gated" in this section's own original heading. All five now exist, and each has shipped its own replay ship-gate harness (`internal/replay`, per that component's own `cmd/replay`) covering most or all of the scenarios below — not one unified cross-component harness, but each component's own gate proves its own slice against a real upstream/downstream dependency, the same posture C1's own Part 1 harness uses. See each component's own `internal/replay/scenarios.go` for the exact, named scenario list and its own acceptance criteria — the bullets below are kept as the original hazard list, not rewritten line-by-line against that harness, so treat this section as "what to worry about," and each component's own replay package as "what's actually proven."

### C2 — Deposit watcher (BSC)

Covered by `depositwatcher/internal/replay/scenarios.go` (wired via that package's own `run.go`): reorg beyond the tracked window, RPC provider disagreement, a dark/unresponsive provider, duplicate log delivery, deposit to a retired address, wrong-token/zero-value log matching, and deposit-after-quote-expiry.

- Reorg deeper than the tracked block-hash window — the one case C1.6 scenario B exists for, but only if C2 correctly detects and reports it. An undetected deep reorg is a silent loss, not a halted one.
- RPC provider divergence — two providers disagree on finality. Needs ≥2-provider hash agreement before declaring `deposit.final`, or a false-final gets recorded and is indistinguishable from scenario B after the fact.
- Duplicate Transfer-log delivery from the RPC provider (same log replayed) — must not double-credit; this is exactly why idempotency keys are `tx hash + log index`, not a generated id.
- Deposit sent to a retired address (order already expired/refunded, deposit shows up anyway).
- Wrong-token transfer or zero-value log matching the filter incidentally.
- Deposit landing after quote expiry — timing race between chain confirmation and the 90s/order-level expiry clock.

### C3 — Screening

Covered by `screening/internal/replay/scenarios.go`: vendor timeout under both fail-closed and fail-open configurations, a re-screen after an order already passed once, and a cache-hit false-positive/negative case.

- Vendor (Chainalysis/TRM/Elliptic) outage or timeout at the moment a decision is needed — does the order sit in `funded` indefinitely, or is there a hold-and-retry policy? Not yet specified.
- Verdict changes between initial screen and eventual release — a sender address gets flagged *after* an order already passed. No re-screen path exists yet.
- Cache-by-sender false positive/negative — a shared cache means one wrong verdict can propagate across multiple orders from the same sender.

### C4 — Energy broker

Covered by `energybroker/internal/replay/scenarios.go`: a single provider going unhealthy (routing re-normalizes), all providers unhealthy (manual fallback), a price spike above ceiling across every provider, and a delegation that never lands on-chain.

- Energy delegation lands *after* broadcast instead of before — the payout burns TRX at market rate instead of the wholesale blend, silently destroying the margin on that one payout without triggering any ledger-level error.
- Provider outage mid-flight, mid-routing (60/35/5 across Tronsell/Netts/CatFee) — fallback ladder exists in principle, not yet tested under a live outage.
- Price spike above the configured ceiling — broker must be able to *hold* rather than overpay; the failure mode to test is whether "hold" here ever silently becomes "pay anyway."
- Two or three providers degraded simultaneously (see Part 3 — this tips into terminal risk if it happens for long enough).

### C5 — Payout dispatcher (TRON)

Covered by `dispatcher/internal/replay/scenarios.go`: partial settlement within a Sweep batch, a duplicate-broadcast retry storm, a slot freeze both pre- and post-broadcast, and an energy-reservation failure/timeout with zero ledger impact. Still genuinely open, not covered by any replay harness (an environment constraint, not a missed scenario): real on-chain timing/energy behavior has never been measured against an actual TRON node, and no component has signed/broadcast through a real (non-fake) S1 KMS adapter — see `c5-payout-dispatcher-build-prompts.md`'s own notes on both.

- Energy exhausted exactly at broadcast time (race with C4).
- A slot gets frozen (Tether action) mid-flight, between selection and confirmation — different from the freeze scenario in Part 3 in that this one is caught pre-settlement and should be recoverable without a loss entry.
- Duplicate broadcast on retry — exactly-once semantics under retry is listed as a hard part and needs its own proof, analogous to C1.3's concurrent-Post test but at the chain-broadcast layer.
- Partial multisend success in a Sweep batch (the batch-level version of the C1.9 "40/50 settle" scenario, but originating from real chain behavior, not injected).
- Slot rotation stranding a small balance below the dust threshold in a retiring slot.

### C6 — API gateway

Covered by `gateway/internal/replay/scenarios.go`: a quote expiring before order creation, webhook delivery exhausting all retries with the pull-based backstop confirmed to agree, and all four deterministic sandbox failure triggers exercised in isolation.

- Webhook delivery exhausts all 8 retries — customer never learns the order settled. Needs a pull-based reconciliation path (`GET /orders/{id}`) as the backstop, and a test that the backstop actually agrees with what the webhook would have said.
- Quote lock (90s) expires between quote and order creation, order created anyway with a stale price.
- The four deterministic sandbox failure triggers (reorg, screening hold, energy exhaustion, retry storm) — required to be exercised before any production sign-off per decision 5 in `product-operations-architecture.md`. Worth treating as its own gate, parallel to C1.9.

---

## Part 3 — Terminal / insurable risk (irreducible, must be priced not patched)

This is the part that actually answers "would this be enough for our product" in the insurance sense. Every scenario in Parts 1 and 2 has an engineering fix. These don't — the fix is a cap, a reserve, or a premium, not a code change. `findings-and-recommendation.md` already sizes one of these; the rest are named there but not yet sized, or not named at all.

| Scenario | Exposure driver | Existing mitigation | Sizing status |
|---|---|---|---|
| **Tether freezes a payout slot** | 11,085 freezes / $5.85B frozen industry-wide in 2026, 87% on Tron, 11.8% eventually released | Segregated rotating slots cap a single freeze at $50k / 17% of throughput instead of $255k / 100% (decision 4) | **Sized**: $3,374/yr expected loss with segregation vs $662/yr theoretical minimum — already in the architecture doc |
| **Post-settlement reorg** | BSC reorg deeper than 15 confirmations, landing after TRC20 already paid out | `expense:loss:reorg` + mandatory halt (C1.6 scenario B) | Not sized — no stated confirmation-depth-vs-probability model or expected annual frequency |
| **CEX counterparty risk on the rebalancing venue** | `asset:cex:<venue>` — up to ~$161k in a single rebalance block moves through an exchange the corridor doesn't control | None specified — no stated cap on in-flight CEX exposure per rebalance cycle | Not sized |
| **Key compromise on a TRON slot key or the BSC HD seed** (S1) | Custody window is short (4m20s median on Standard) but non-zero, and slot keys are long-lived | Split custody (S1), 6-slot segregation limits blast radius | Not sized — no stated probability model, only the qualitative "high risk, terminal" flag in the component map |
| **Simultaneous multi-provider energy market failure** | All three energy vendors (Tronsell/Netts/CatFee) degraded or price-spiked together, e.g. a TRON-wide energy shortage event | Fallback ladder (C4), but ladder assumes at least one provider is healthy | Not sized — no stated worst-case duration or fallback-to-JustLendDAO cost delta |
| **Demand shock exceeding the $255k float / 4-hour rebalance cycle** | A volume spike (e.g. a large customer's own bank-run event) that outpaces 3.75 turns/day | Capital ladder (decision 8): prefunding → OTC float → credit line → Model E netting | Partially sized — the $28.7M/month ceiling is known, but the *loss* if breached (refused orders vs. degraded settlement time) isn't modeled, only the ceiling itself |
| **Regulatory/jurisdictional action against a customer mid-flight** | Sanctions list update, or a jurisdiction blocking a specific counterparty after funds are already in `funded`/`dispatching` | Screening (C3) catches this pre-conversion in the common case; nothing catches a mid-flight designation | Not sized, not fully specified as a scenario in C3 |
| **Insider or human error bypassing the DB role grants** | The append-only/no-UPDATE/no-DELETE guarantee is enforced by Postgres roles (C1.0), which is only as strong as who holds superuser/table-owner credentials | Tested at the `ledger_writer` role level (C1.0 acceptance); not tested against a table-owner or superuser connection, which is the actual insider-risk surface | Not sized, and arguably not fully mitigated — worth a deliberate decision on who can hold owner-level Postgres credentials in production |

### What "enough" looks like for this part specifically

An insurer — or a sophisticated customer doing their own diligence before trusting a settlement guarantee — isn't going to ask whether C1's replay harness passes. They're going to ask for this table, fully sized, with a stated annual expected loss and a stated worst-case single-event loss per row, the same way the Tether-freeze row already is. Three concrete next steps would close most of the gap:

1. Extend the freeze-risk expected-loss method (already done) to the reorg, CEX-counterparty, and key-compromise rows — each has enough public data (BSC reorg history, exchange incident history, industry key-compromise incident rates) to get a first-pass number.
2. Turn the float-ceiling breach from a capacity number into a loss number: what actually happens to an order when the corridor is over-committed — refused, queued, or settled late against an SLA that then owes a refund per the status-page auto-refund policy in decision 11.
3. Decide, explicitly, who can hold Postgres owner/superuser credentials against the production ledger, and whether that decision itself needs to be in the pitch to a customer asking "who can edit your books."

---

## Cross-reference

- Full C1 build spec and acceptance criteria: `c1-ledger-build-prompts.md`
- Component ownership and hard-parts list Part 2 is drawn from: `component-map.md`
- Freeze-risk sizing and the eleven architecture decisions Part 3 is grounded in: `findings-and-recommendation.md`, `product-operations-architecture.md`
