# Product & operations architecture — decisions record

> **Background (history).** Written during the design and build phase and kept as a
> record. You don't need it to run or change today's product: start at
> [docs/README.md](../README.md).

_v1.0, 30 Aug 2026. Full document: interactive artifact "TRC20 Payout Engine" (claude.ai artifact gallery). Builds on `docs/01-strategy/findings-and-recommendation.md` — market viability is settled, not re-litigated here._

Baseline carried through every number: TRX $0.34 · 74,750 blended energy per payout (85/15 existing/fresh at 65k/130k) · $255k working float.

## The eleven decisions

1. **Architecture** — six services, one ledger, no smart contracts on either chain. Our custody window is only from BSC 15-conf finality to TRON SR finality: 4m 20s median on Standard. Everything else is the customer's funds or the recipient's.
2. **Energy: rent, never stake.** Staking for 100 payouts/day locks $264,740 of TRX to save $38,034/yr — 14.4% on capital that earns 246% as float. Route 60/35/5 across Tronsell (24 sun) / Netts (28) / CatFee (30) = 25.7 sun blend = $0.6532/payout, against the market's $1.0420 at 41 sun. That $0.389 gap is ticket-invariant and is the margin engine.
3. **Bandwidth: stake in month 13, not month 9.** The 55% return is real but constant at any volume; the honest crossover is when float stops being the binding constraint, not a payout-per-day threshold.
4. **Wallets: segregated rotating payout slots, not a pooled treasury.** Six slots capped at $50k / 5,000 tx each. Caps a Tether freeze at $50k and 17% of throughput instead of $255k and 100%. Expected-loss math is close ($3,374 vs $662/yr); the decider is that a pooled freeze breaks the settlement guarantee the product is priced on.
5. **API: quote-then-order, never quote-inside-order.** 90s price lock, idempotency keys, HMAC-SHA256 webhooks with 8 retries, sandbox with four deterministic failure triggers (reorg, screening hold, energy exhaustion, retry storm). No production sign-off without exercising all four.
6. **Three tiers — Direct / Standard / Sweep** at $2.54 / $1.80 / $1.00 network fee, costing $0.928 / $0.829 / $0.406. **Sweep is both the cheapest tier and the highest-margin one** because batched multisend halves per-recipient energy. That inversion is the competitive argument: only an aggregator can price the floor and still earn.
7. **Treasury: $255k split 65/35 toward TRON**, 4-hour loop, ~$161k blocks via CEX network switch (0.06 bp realised against a 1.5 bp planning assumption). Ceiling $28.7M/month, binds month 9.
8. **Break the ceiling with someone else's balance sheet first** — customer prefunding, then reverse-flow customers, then OTC partner float, then a credit line (month 18 at the earliest), then Model E netting. Outside capital is the fifth rung, not the first.
9. **Stay on the $18k/month stack through month 24.** The $123k Phase-3 stack breaks even at $42.8M/month — 149% of the float ceiling. Headcount cannot outrun the capital ladder.
10. **The 3.5 bp tier is a margin trap.** At a $5M ticket, rebalancing at 1.5 bp is 42.8% of revenue and margin falls to 57%. Quote it case-by-case, do not publish it, until the OTC leg or netting exists.
11. **The status page is the primary sales asset**, not the docs. Real P50/P95/P99 by tier, hourly, 90 days back, including the misses — made survivable by auto-refunding the tier premium on every SEV-3 breach. Same data as `GET /v1/settlement-stats` so a prospect's engineer can diff them.

## Economics summary

| | Value |
|---|---|
| Contribution margin at $3,000 ticket | 87.1% ($8.45 on $9.70 revenue) |
| Effective take at $3,000 | 32.3 bp (25 bp fee + $2.20 network fee) |
| Break-even, $18k stack | 72 payouts/day = $6.50M/month |
| Volume ceiling on $255k float | $28.7M/month |
| Monthly break-even | Month 5 · cumulative month 6 |
| 24-month cumulative net | $1,238,773 on $300k deployed (413% ROIC) |
| Modelled residual risk exposure | $98k, worst single year |

Reconciles to the assessment's $1.32M within 6%; the gap is the capital ladder's timing, which is the model's most sensitive input.

## GTM sequence

OTC desks and wallets first (days to integrate, they supply the volume that makes batching work) → payment providers second (high switching cost, high volume, they make Sweep economics real) → exchanges last (longest cycle, and by then the status page has 90 days of history).

Winning metric per segment: exchange = all-in cost per withdrawal (pitch is releasing their staked TRX, not a fee saving); wallet = % of sends completed without the user acquiring TRX; OTC desk = P95 settlement time; payment provider = all-in cost per payout at their ticket size (32 bp against their 50–100 bp).

## What must be tested, and by when

| Input | Value used | Why it matters | By |
|---|---|---|---|
| Wholesale partner pricing (retail quotes only so far) | 25.7 sun | ±4 sun = ±$13.6k/yr at Y2 volume. **Highest-value call to make this week.** | Week 2 |
| Batched multisend energy | 35,000/recipient | At 50,000, Sweep margin falls 59%→46% and the GTM inversion disappears. One testnet afternoon. | Week 6 |
| Rebalance all-in cost | 1.5 bp | At 3 bp the top tier is loss-making. 20 real blocks across both CEX routes and one OTC leg. | Month 3 |
| 4-hour cycle sustainable with 3 people | 3.75 turns/day | At 2 turns/day the ceiling drops to $15.3M/mo and the ladder is needed in month 6. | Month 3 |
| Average ticket / 85-15 recipient split | $3,000 / 74,750 e | Observed, not experimented. At 70/30 blended energy rises 13%. | Month 4 |
| Price elasticity | −1.6 | Sets the ramp and therefore the ladder timing. Re-run the 12 bp gate against **non-promotional** competitor pricing. | Month 5 |

## Still genuinely open

- Does a free-at-point-of-use competitor — even promotional — shift the elasticity assumption or the 12 bp gate threshold?
- Is the wholesale energy pipeline the bigger business? Every USDT processor pays the same $0.71 energy cost with no solved sourcing layer. If the month-5 gate fails, that is not a fallback — it is the answer.
