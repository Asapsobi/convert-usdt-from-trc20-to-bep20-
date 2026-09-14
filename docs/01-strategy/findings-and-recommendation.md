# USDT BEP20 → TRC20 settlement layer — findings and recommendation

_Assessment completed August 2026. Full report: interactive artifact + `USDT-Settlement-Corridor.docx`._

## Verdict

**Not viable as specified; viable repositioned.** A service sold as "USDT network conversion" is a commodity with a $1.00 reference price (Binance: free BEP20 deposit + $1 TRC20 withdrawal) and BestChange desks already quoting 0–10 bp. The same engine sold as **TRC20 payout infrastructure for businesses** is a real product with 78–87% gross margins in its core band.

Scores: market attractiveness 4/10 (7/10 repositioned) · technical complexity 3/10 · business opportunity 5/10 · capital efficiency 8/10.

## The five findings that drive everything

1. **The margin is in TRON energy, not the spread.** Energy for a TRC20 payout costs **$0.71** at the best rental rate (28 sun) but is priced to users at $1.00–$2.54. That is 49–72% margin on a line item everyone else passes through as "network fee" — and it is invariant to ticket size, so it survives fee compression. At 1,000 payouts/day it is $250k–$390k a year.
2. **Staking TRX for energy is the wrong use of $300k.** 100 payouts/day requires **$264,740 of TRX locked** (7,786 TRX per daily slot at 9.6 energy/TRX/day) to save ~$38k/yr vs renting. Rent energy; stake only for *bandwidth* at Phase 2 (~$78k locked covers 1,000/day and returns 55%).
3. **All-in on-chain cost is $1.16 per conversion** ($1.042 energy at market 41 sun + $0.117 bandwidth + $0.003 BSC gas). At $10,000 that is 4.6% of a 25 bp fee — gas is not the cost driver, fixed cost per transaction and float velocity are.
4. **Capital is not the constraint; demand is.** Float turns 3.75×/day on a 4-hour rebalance cycle — a 246% gross annualised return at 18 bp. $255k of working float supports $19–29M of monthly volume. But the $300k ceiling still binds around month 9 in the base case.
5. **Scarce capital argues for a *higher* price.** Unconstrained profit-maximising take ≈ 14 bp; with a $255k float ceiling the funded optimum moves out to ≈ 51 bp, because the volume that 14 bp attracts cannot be funded.

## Structural market facts

- USDT on Tron **$87.9B** vs BNB Chain **~$8.9B** — 9.9× asymmetry, so net flow is permanently BEP20 → TRC20 and the treasury drifts monotonically. Rebalancing is the core loop.
- **No canonical route exists for this pair.** Stargate has no Tron support; USDT0 (Tether's LayerZero OFT) deliberately excludes *both* Tron and BNB Chain because Tether mints natively on each. CEX network-switching is the de-facto standard.
- Price dispersion on an identical outcome is **50–100×**: $1.00 (CEX) to $100 (ChangeNOW), sustained entirely by friction.
- **Tether freeze risk is the terminal one.** 11,085 freezes, $5.85B frozen, $1.43B destroyed; Tron carries 87% of 2026 events; 11.8% later released (high false-positive rate). Your pooled TRC20 treasury is the address that gets frozen.

## Recommendation

**Model D — conversion/payout API — now; Model E — settlement layer with netting — year 2–3. Never a bridge; LP only as a use for idle float.**

Positioning: *"Reliable TRC20 payouts, funded from any chain."* Lead with the payout, not the conversion. Defensible differentiators: wholesale energy procurement, batch multisend (halves per-recipient energy, only available to whoever aggregates), and an SLA no manual desk or exchange will give.

Pricing: **max(flat floor, declining bp tier)** — never a pure percentage. Floor never below $1.80. Tiers: 40 bp under $1k · 25 bp to $10k · 12 bp to $100k · 6 bp to $1M · 3.5 bp above.

MVP: 6–8 weeks, two engineers, ~$50k float. REST API (quote/order/status/webhook), HD deposit addresses on BSC, 15-confirmation finality with block-hash reorg tracking, double-entry ledger with continuous reconciliation and auto-halt on drift, energy rental routed across two providers, deposit screening before payout release. ~~No public swap page.~~ **Update, 14 Sep 2026 — superseded, see "B2C update" below: this MVP-scope line no longer holds.** A status page publishing real settlement-time percentiles was the sales asset at MVP; it remains valid but is no longer the *only* customer-facing surface.

## Update, 14 Sep 2026 — B2C reconsidered

**The "no public swap page" line above is reversed for Model D, deliberately, not as a correction of a mistake.** This was Model D's own original MVP-scope decision, made on the reasoning that a public conversion page competes on a visible spread against a $1.00 CEX reference price (the "Model A" positioning this doc's own Verdict rejected) rather than on the structural cost advantage (wholesale energy, batch multisend) that findings #1 and the Recommendation above actually describe. That reasoning is not wrong — it is still why Model D's *pricing* stays cost-advantage-driven rather than becoming a race-to-the-bottom spread. What changed is the *channel* decision, not the economics: Model D is now treated as conversion/settlement **infrastructure**, distributed through multiple channels — the existing B2B partner API (built), a new B2C public website (planned, not yet built), and potentially future channels (Telegram, bots, embedded widgets) — rather than assuming B2B is the only legitimate distribution path. The same wholesale-energy/batch-multisend cost advantage this doc identifies applies to volume from any channel; nothing about it depends on a customer being a business rather than an individual.

This does **not** reopen Model F's own identical ban (`model-f-relay-findings.md`) — Model F's version of this constraint now stands on its own reasoning, independent of Model D's, and is unaffected by this update. See `docs/01-strategy/model-d-model-f-product-separation.md` for the full, permanent decision record.

## Expected outcome

Run lean (3 people, $18k/mo fixed), the base demand curve produces **~$1.32M cumulative net profit over 24 months on $300k deployed** (438% cumulative ROIC), break-even around month 5. The escalating-headcount variants are all worse because the capital ceiling caps volume regardless. This is a good cash business and a poor venture case — the capital-light orchestration path ($10M/day partner volume at 8 bp ≈ $2.4M/yr net with zero balance sheet) is the only version that reaches a venture outcome.

## The one gate

Answerable by month five: **will a business pay more than 12 bp, above a $1.00 substitute, for a settlement guarantee?** If no, sell the engine — or the energy pipeline alone — to someone who already owns the demand.

## Key assumptions to test (not observations)

- Price elasticity −1.6 — run deliberate price experiments in months 3–5.
- $3,000 average ticket and the 85/15 split between existing and fresh recipient addresses.
- 1.5 bp all-in rebalancing cost, and the 4-hour cycle being operationally sustainable.
- Fixed-cost stack ($12.5k → $40k → $123k/month by phase).
- Batched multisend at ~35k energy per recipient.

## Addendum (Aug 30, 2026) — the "competitors do it free" objection, checked

Objection raised: a public API that charges users a fee they can shop against competitors is a dead-end because the market is too competitive and providers like NOWPayments already offer it free.

This is correct for **Model A** (public conversion/swap API, arbitrary spread) — the doc already rejected that model in the Verdict above, for the same reason. It is not an objection to **Model D**, the actual recommendation, which has no public swap page and is not priced as "whatever the operator wants."

Fact-check on the specific claim: NOWPayments' "$0 network fee on USDT TRC20" is a **two-month promotion for new partners**, not a standing price, and it only waives the network-fee line item. Their real revenue is a **0.5% service fee on a standard payment, ~1% when conversion is involved** — on this report's $3,000 average ticket that is $15–30, more than double the 25 bp ($7.50) this report proposes charging in the same volume tier. NOWPayments is also a merchant checkout gateway (buyer: a store accepting crypto payments), a different customer than an exchange, wallet, OTC desk, or payment provider moving treasury balances and buying settlement guarantees, batch payouts, and reconciliation. "Free conversion" from a checkout provider does not compete with payout infrastructure sold to a treasury desk.

Why Model D's margin survives price competition on the visible fee: it doesn't depend on the visible fee. It comes from wholesale TRON energy (49–72% margin on a cost line every competitor — including a "free" one — still pays somewhere) and batch multisend (only available to whoever aggregates volume). Both are structural cost advantages, not a spread that can be raced to zero.

New value angles for the user segments, beyond Model D/E as scoped:
- **Treasury-as-a-service for OTC desks / market makers**: guaranteed TRC20 liquidity on demand, without the counterparty holding TRX or running its own energy stack. Sells against "we need liquidity now," not "convert my USDT."
- **Wholesale energy pipeline as a B2B feed to other processors** (pulling the doc's existing exit option forward as a primary line, not just a fallback): every USDT payment processor, NOWPayments included, pays the same $0.71 energy cost with no solved wholesale-sourcing layer. Selling into their cost stack turns their price competition into a customer acquisition channel instead of a threat.

Open question raised by this check, not yet answered: does a genuinely free (even promotional) competitor change the elasticity assumption (−1.6) or the 12 bp gate threshold in the base case? Worth re-running the gate test with current non-promotional pricing from 3–5 TRC20 payout providers before month 5.

## Addendum 2 (Aug 30, 2026) — energy/bandwidth rental partner candidates

Surveyed the TRON energy rental market (18+ platforms, per TronGuides' July 2026 comparison) to identify who would actually supply the wholesale energy this business runs on. Two names stand out as primary candidates, and one is a validating data point for the model's own cost assumption:

- **Netts** — quoting **28 sun** for a 24-hour rental as of the survey, which is the exact rate this report's $0.71 wholesale energy cost is built on. Has a documented API ("automate your operations"), enterprise-tier routing for optimized pricing, and a business contact channel for partnership/volume terms (not self-serve published).
- **Tronsell.io** — currently top-ranked on price (24 sun 24-hour rate, even better than the report's assumption), with a documented API supporting sub-5-minute rental windows, per-call energy/time parameters, and up to 300 QPS — built for automated, high-frequency use rather than manual retail rental.
- **CatFee** — third-ranked (30 sun 24-hour), reasonable diversification candidate if a third routing option is wanted.
- **JustLendDAO** — TRON's own on-chain lending protocol; materially more expensive (68 sun) and not price-competitive, but non-custodial and the most trusted/brand-recognized option — worth holding as a backstop route for reliability rather than cost.

This lines up with the MVP spec's plan to route energy rental across two providers rather than one: Tronsell + Netts cover both the price leader and a documented enterprise/API tier, and diversification protects against a single provider losing liquidity mid-day (a real failure mode multiple reviews flag). Before committing, get actual wholesale/partner-tier pricing from Netts and Tronsell directly (both require contacting a business channel rather than publishing bulk rates) — the sun rates above are retail-facing quotes, not confirmed partner pricing.
