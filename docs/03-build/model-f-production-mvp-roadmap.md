# Model F — roadmap to a production MVP

_26 Sep 2026. Measured against [`../00-product-goals.md`](../00-product-goals.md) (the
source of truth) and the public-product decision in
[`../01-strategy/model-d-model-f-product-separation.md`](../01-strategy/model-d-model-f-product-separation.md).
Written after reading the code on `main` at `99ae0fd`, not the README — several
README/build-doc claims were stale at that commit (see Phase 0). Model D is out of
scope here._

## Where this starts from

**The product flow is built. What's missing is proof, safety, and operations.** Commit
`9ada83a` ("relayd: full product flow") implemented nearly every requirement in the
product goals. What stands between that and paying public customers is: no real order
has settled end-to-end through Model F on mainnet, the AML screen is a placeholder, and
nothing about hosting, alerting, or legal exists yet.

| Goal (00-product-goals.md) | In code at `99ae0fd` | Where | Gap to production |
|---|---|---|---|
| §1 Pick direction, get a deposit address | ✅ | `relayd/internal/driver`, `relay-storefront/` | Proven only in tests and replay |
| §2 Amount → vendor fee + our profit → payout, shown first | ✅ | `internal/pricing` (admin-set, snapshotted per order), `POST /v1/quotes`, storefront breakdown | Profit floor has to cover real per-order costs — see Phase 2 |
| §3 Limited, centrally managed deposit wallets | ✅ | `tronwatcher` / `depositwatcher` `internal/addresses/pool.go`, leases, admin pool routes | — |
| §4 BEP-20: check BNB, top up from treasury | ✅ | `orchestrate/chain_bsc.go`, `resources.go` | Treasury needs real BNB; it alerts only once an order is already blocked |
| §5 TRC-20: check energy/bandwidth, rent the exact shortfall | ✅ | `orchestrate/chain_tron.go`, `internal/vendors/catfee.go` | CatFee prepaid balance isn't watched |
| §6 Forward net amount to vendor, payout to customer's wallet | ✅ code / ❌ proof | `internal/upstream` (FixedFloat, ChangeNOW, SideShift) | **Never settled for real.** FixedFloat's real-order problem was never root-caused; currency codes are unverified |
| §7 Profit sweeps to treasury | ✅ | `internal/sweeps`, `orchestrate/sweep.go` (off until an admin enables it) | Never run for real |
| §8 Multiple vendors, best-rate selection, failover | ✅ | `internal/vendors` — DB registry, best_rate/cheapest/priority, back-off 30s→10m, auto-return | Failover never exercised against a real outage |
| §9 Full per-transaction tracking | ✅ | Admin API `/v1/admin/legs/{id}`, `transfer_attempts`, opsconsole relay pages | — |
| Public-product requirement: real AML | ❌ | `screening` still runs `AlwaysCleanProvider` | **Blocker** |
| Public-product requirement: abuse limits | 🟡 | Per-IP rate limit on quotes and orders (`httpapi/ratelimit.go`), in memory | OK for one instance; no per-order or daily caps |
| Public-product requirement: production hosting | ❌ | `docker-compose.yml` only; no CI, no deploy config | **Blocker** |
| Public-product requirement: operator alerting | 🟡 | `internal/alert` writes loud log lines only | **Blocker**: an `UNRECOVERABLE` leg has to page a person |
| Key custody | 🟡 | `s1/internal/kmssign` has real Privy adapters (TRON + BSC) | Privy integration tests skip without credentials; custody never proven live |

## Phases

Each phase ends with an exit check. Don't start the next one until the check passes.
Phases 2–4 can overlap, because their long waits are outside the code (vendor
contracts, legal).

| Phase | What it delivers | Estimated time* | Blocked by |
|---|---|---|---|
| 0 | Repo docs that match the code | 1–2 days | — |
| 1 | Real mainnet settlements in both directions | ~1 week | Vendor accounts, funded wallets |
| 2 | Safe to open to the public | 1–3 weeks | AML vendor contract, legal advice |
| 3 | Running in production and watched | 1–2 weeks | Hosting choice |
| 4 | Storefront ready for customers | ~1 week (alongside 3) | Phase 2 decisions on limits and refunds |
| 5 | Wider ship gate, private beta, public launch with caps | 1–2 weeks of beta | Phases 1–4 |

_\*For one engineer working with AI coding agents. The calendar time mostly comes from
waiting on outside parties, not from writing code. Treat these as estimates._

---

### Phase 0 — make the repo tell the truth

Agents and people plan from the docs, and at `99ae0fd` the docs were wrong in ways that
send work in the wrong direction.

| Item | Fix |
|---|---|
| README, `model-f-relay-build-prompts.md` R4, and `docker-compose.yml` comments point to `relayd/internal/upstream/router.go` / `MultiProvider` | That file doesn't exist. Routing lives in `relayd/internal/vendors` (`ConversionRouter`, `EnergyRouter`), and every real vendor goes through it, even a single one. The setting is now `UPSTREAM_PROVIDER=router` + `UPSTREAM_VENDORS`; `best_rate` / `UPSTREAM_BEST_RATE_PROVIDERS` still work as aliases (`cmd/relayd/upstream_provider.go`). Update all three places |
| SideShift isn't mentioned in any doc | Add it to the R2 decision record. Its adapter is the only one checked against the live API (`upstream/sideshift.go` header, `cmd/sideshift-probe`) |
| The Privy custody adapters aren't mentioned in any doc | Record them in `s1-key-custody-architecture.md` and the README's S1 row |
| The README's Model F "Next action" still says "do R2" | Point it here |
| The FixedFloat real-order problem was "reported, not root-caused" on 14 Sep and nothing was written down after | Write down the symptom in `model-f-relay-findings.md` even if the cause is still unknown |

**Exit:** a fresh agent reading only the README and the Model F docs gets an accurate
picture of what's built.

### Phase 1 — prove it with real money

The same approach Model D used (`mvp-proof-run-plan.md`): small real orders, each checked
independently on the block explorer, before anything is called shipped. In Model D,
every real run found bugs that tests couldn't.

| # | Step | Detail |
|---|---|---|
| 1.1 | Verify currency codes against the live APIs | `FIXEDFLOAT_CCY_*`, `CHANGENOW_CCY_*`, SideShift's network ids. Use the probe CLIs (`relayd/cmd/fixedfloat-probe`, `relayd/cmd/sideshift-probe`). There's no ChangeNOW probe yet, so write one on the same pattern |
| 1.2 | Custody | Provision the deposit-pool wallets through S1's Privy deposit keys (admin `POST /v1/admin/pool/{chain}/wallets`, which calls the watchers' `ProvisionWallet`). Create the treasury keys in Privy and register them with `s1/cmd/seed-slot-key`. Sign one real transfer on each chain. Run the Privy integration tests with real credentials (they skip without `PRIVY_APP_ID`/`PRIVY_APP_SECRET`) |
| 1.3 | Fund operations | Treasury BNB, treasury TRX, CatFee prepaid balance. Small amounts only |
| 1.4 | First vendor, both directions | Start with **SideShift** (most verified). One small TRC20→BEP20 order and one BEP20→TRC20 order through `relay-storefront` |
| 1.5 | Root-cause FixedFloat | Reproduce with a small order and fix it, or disable FixedFloat in the vendor registry and record why |
| 1.6 | ChangeNOW | Same as 1.4 |
| 1.7 | Failover for real | With two vendors enabled, disable or break the best one mid-quote and confirm the order routes to the other and the first one comes back afterward |
| 1.8 | One real sweep | Enable sweeps with a low minimum and sweep one wallet's profit to treasury. Confirm the ledger booking |
| 1.9 | One real refund | Send a too-small deposit and confirm the full refund reaches the sender |

**Exit:** at least 3 settlements per direction across at least 2 vendors, each checked on
Tronscan/BscScan; every `asset:relay:leg:*` account nets to zero; one sweep and one
refund done; every bug found is fixed with a regression test. Record the results in
`model-f-relay-build-prompts.md`, the same way C2/C4/C5 recorded theirs.

### Phase 2 — safe to open to the public

| # | Item | Why | Notes |
|---|---|---|---|
| 2.1 | **Real AML/KYT vendor in C3** | The separation doc makes it mandatory for a public product. Tainted funds passing through your wallets are your legal problem, and they're also Tether-freeze exposure | Enterprise options: Chainalysis KYT, TRM, Elliptic. Mid-market options: AMLBot, Crystal, Scorechain. Choose by per-check cost at your volume. The provider interface already exists (`screening/internal/provider`), so only the adapter is new |
| 2.2 | **Refund address captured up front** | Refunds go only to the on-chain sender (`orchestrate/refund.go:411`). Customers who pay from an exchange withdrawal would get the refund sent to the exchange's hot wallet, which is often lost | Optional refund address collected when the order is created (before payment), validated with `addrcheck`, and never changeable afterward. Keeps the rule "no address supplied after the fact" |
| 2.3 | **Order limits** | Limits both the damage from an `UNRECOVERABLE` leg and AML exposure per customer | Per-order min/max plus a daily total cap, set by an admin like pricing. Start low for beta |
| 2.4 | **Profit floor that covers costs** | Every order spends real resources: TRON energy/bandwidth for the TRC20 leg, BNB gas, a share of each sweep, and the vendor fee | Minimum profit per order ≥ worst-case resource cost + a buffer. Baseline from `findings-and-recommendation.md`: about $1.16 all-in for one TRC20 transfer at TRX $0.34. Measure the real figure in Phase 1 and use that |
| 2.5 | **Tether blacklist check** | A blacklisted destination or deposit wallet strands funds | Reuse Model D's `isBlackListed` check (`dispatcher/internal/slots`) on relay deposit wallets before leasing and on customer payout addresses before quoting |
| 2.6 | **Legal and compliance** | A public exchange service is regulated in many jurisdictions | Get legal advice on licensing (VASP / money transmission) where the operating entity is based and where customers are. Confirm that entity's jurisdiction is eligible under each vendor's terms (FixedFloat, ChangeNOW, SideShift, Privy, CatFee, TronGrid, the AML vendor), since several exclude some countries. Publish Terms of Service and a privacy policy |

**Exit:** real AML screening decides every order; limits and the profit floor are set
from Phase 1's measured costs; vendor eligibility is confirmed in writing; legal
position is documented.

### Phase 3 — production operations

| # | Item | Detail |
|---|---|---|
| 3.1 | Hosting | A single-instance deployment is fine for the MVP (relayd already takes a single-instance DB lock). One VM or a small managed-container setup running the stack from `docker-compose.yml`: ledgerd, watcherd, tronwatcher, screend, s1d, relayd, opsconsole, relay-storefront, Postgres |
| 3.2 | Edge | TLS, a reverse proxy, and `TrustProxy` set so per-IP rate limits use the real client IP. opsconsole and `/v1/admin` must not be reachable from the internet (VPN or IP allowlist) |
| 3.3 | Secrets | Privy, vendor, CatFee, TronGrid/RPC and admin tokens kept in a secrets manager, not `.env` on disk |
| 3.4 | Data | Automated Postgres backups, **plus one restore drill** before launch. The ledger is the record of who is owed what |
| 3.5 | CI | `go test ./...` for every module, the frontend builds, image builds on every PR. Nothing like this exists today |
| 3.6 | **Real alerting** | Implement `alert.Alerter` against one channel you actually read (a Telegram bot is the cheapest). Alert on: `UNRECOVERABLE`, stale legs, the ledger halt signal, vendor all-down, and wallets where the books and the chain disagree |
| 3.7 | **Balance monitoring** | relayd already raises a critical alert when the treasury can't cover a top-up an order needs (`orchestrate/chain_bsc.go`), but by then that order is already stuck. Add a warning *before* that point: treasury BNB, treasury TRX, and the CatFee prepaid balance each below the level needed for N more orders |
| 3.8 | Runbooks | `UNRECOVERABLE` leg (vendor support-ticket steps), vendor outage, ledger halt, stuck lease, blacklisted wallet, restoring from backup |

**Exit:** the stack runs on production infrastructure; a deliberately triggered
`UNRECOVERABLE` alert reaches a phone; a restore from backup has been done once.

### Phase 4 — storefront ready for customers

`relay-storefront` already has the order-creation page with the payout breakdown and a
status page. For launch it also needs:

| Item | Detail |
|---|---|
| Refund address field | From 2.2 |
| Limits shown up front | Min/max per order, and a clear message when the amount is outside them |
| Quote countdown | The locked quote's remaining time, and a re-quote when it expires |
| Status page | Every stage (awaiting deposit → confirming → converting → paid), with explorer links for the deposit, forward, and payout transactions |
| Terms acceptance | A checkbox linking the ToS from 2.6 |
| Support | A contact channel plus the order's external id shown for quoting |
| Error states | Vendor down, pool exhausted, screening hold, refund in progress — each with plain wording for the customer |
| Mobile | Most crypto users will be on phones |

**Exit:** a person who didn't build it completes an order on a phone without help.

### Phase 5 — wider ship gate, beta, launch

| # | Step | Detail |
|---|---|---|
| 5.1 | Extend the R6 replay gate | Its 7 scenarios predate `9ada83a`. Add: pricing snapshot, too-small-deposit refund, lease expiry and release, BNB top-up, energy-shortfall rental, conversion failover, energy failover, sweep, refund to the up-front refund address |
| 5.2 | Private beta | Invited users only, low caps from 2.3, every order watched in opsconsole. Two weeks or 50 orders, whichever takes longer |
| 5.3 | Public launch | Same caps. Raise them in steps once there's a clean week at each level |

**Exit (MVP done):** public, with caps, real AML, alerting that reaches a person, and a
week of clean settlements.

## Decisions the product owner has to make

| Decision | Needed by | Options / notes |
|---|---|---|
| AML vendor | Phase 2 | See 2.1. This is the longest wait, so start contacting vendors now |
| Operating entity and jurisdiction | Phase 2 | Determines licensing and which vendors will accept you |
| Launch caps | Phase 2 | Per-order min/max and daily total |
| Profit margin and floor | Phase 2 | Set from Phase 1's measured costs |
| Hosting provider | Phase 3 | Any provider that runs containers and Postgres with backups |
| Alert channel | Phase 3 | Telegram / email / Slack |
| Launch vendor order | Phase 1 | Suggested: SideShift first, then ChangeNOW, FixedFloat only after it's root-caused |

## After the MVP (not in scope)

Running more than one instance (the rate limiter is in memory and relayd takes a
single-instance lock); more conversion vendors; a real partner-facing gateway for
Model F; Model D's retail storefront; publishing settlement-time percentiles (the
status page `component-map.md` calls the main sales asset).
