# Model F — zero-float relay: strategy note

_13 Sep 2026. Second product line alongside Model D (`findings-and-recommendation.md`).
Does not re-open Model D — that verdict, and everything built against it, stands as is._

## Verdict

**Decision: build it.** Structurally this is closest to **Model A** — the public
conversion/swap model `findings-and-recommendation.md` scored 4/10 and rejected for
margin compression against a $1.00 CEX reference price. That parallel is real and is
recorded here for whoever reads this next, not to relitigate it — the decision is to
proceed anyway, on the basis that **zero balance-sheet exposure** is worth more right
now than the pricing edge Model D gets from owning wholesale energy. This is the same
trade-off the Addendum in `findings-and-recommendation.md` already named without
building it: *"the capital-light orchestration path ... is the only version that
reaches a venture outcome."*

## What's different from Model D

| | Model D (built) | Model F (this doc) |
|---|---|---|
| Capital required | ~$255k working float, $300k ceiling | None — never holds a net position |
| Where the margin comes from | Wholesale TRON energy (structural cost edge) | Upstream platform's rate — a spread or a commission, not a cost edge you own |
| Reverse flow (TRC20→BEP20) | Deferred — "arrives with the capital ladder" | Required from day one — no ladder to wait for |
| Customer segment | B2B treasury/payout infrastructure | Open — commodity resale (needs #2 below) or embedded infra (same positioning Model D uses) |

## Two open pricing mechanisms — pick one before building past the interface layer

1. **Commission.** Pass the upstream platform's own retail rate through unchanged;
   earn a referral/affiliate commission on the back end. No visible markup for a
   customer to shop against — margin comes from volume, not from being cheaper than
   the $1.00 reference.
2. **Margin.** Quote the customer your own rate (upstream rate plus your spread). Only
   defensible if paired with a public-page ban: sell it as embedded settlement infra
   behind someone else's product, not a public "convert my USDT" page.

**Update, 14 Sep 2026 — this ban now stands on Model F's own reasoning, not Model D's.**
Originally written as inheriting "the same move Model D already makes" (citing
`docs/02-architecture/product-operations-architecture.md`'s own ban on a public swap
page for Model D, "applies with equal force here") — that citation was also imprecise:
the literal ban lived in `findings-and-recommendation.md` and `component-map.md`, not
in the architecture doc. Both points are now moot regardless: Model D's own ban has
since been reversed (see `findings-and-recommendation.md`'s own "Update, 14 Sep 2026"),
deliberately, as a channel decision separate from its cost-advantage-driven pricing.
Model F's ban is **not** reversed by that — it holds on its own, arguably *stronger*
footing: Model F's margin (mechanism 2 above) is a spread on top of a third-party
vendor's own rate, not a structural cost advantage the way Model D's wholesale-energy/
batch-multisend margin is, so it has even less room than Model D's original analysis to
survive a customer shopping the visible price against a $1.00 reference. See
`docs/01-strategy/model-d-model-f-product-separation.md` for the full, permanent record
of why these two models' decisions don't propagate to each other.

Neither requires different engineering (`docs/02-architecture/model-f-relay-architecture.md`
§0/§6 cover both), so this doesn't block starting the build — it blocks going live with
a real vendor, which is R2 in `docs/03-build/model-f-relay-build-prompts.md`.

## Carried over from Model D unchanged

- The AML/screening requirement (C3) doesn't relax because there's no float — briefly
  holding a customer's deposit before relaying it is still custodial activity.
- Tether freeze risk (`findings-and-recommendation.md`'s "structural market facts")
  applies to every address this system ever holds funds in, including the short-lived
  relay-leg address, not just a pooled treasury.

## Key assumption to test, same discipline as Model D's own list

- That a real candidate upstream platform actually offers a partner/affiliate terms
  sheet (mechanism 1 above) at a size worth building for. If none does, mechanism 2 is
  the only option, and the public-page ban above becomes load-bearing rather than
  optional.

## R2 decision record (14 Sep 2026)

**Chosen vendor: FixedFloat (ff.io).** Pricing mechanism: **commission** (mechanism 1
above) — FixedFloat's own affiliate program pays a share of their margin per
qualifying order, rather than this system quoting its own marked-up rate.

Evaluated against three real candidates (ChangeNOW, SimpleSwap, StealthEX) on the four
criteria `docs/03-build/model-f-relay-build-prompts.md`'s own R2 section lists:
documented quote/create/status API, an arbitrary destination address distinct from any
account of this system's own, a real partner/affiliate terms sheet (not just a retail
UI), and current terms confirming automated commercial resale is permitted. FixedFloat
was the only one of the three with (d) independently confirmed from their own public
API Terms of Use rather than inferred: **API Terms of Use §6** names a dedicated "For
commercial use with the affiliate program" API key type, with its own application/
approval process — see https://ff.io/en/api-terms. (b)/(a) are confirmed directly
against their own v2 API (`POST /api/v2/price`, `/create`, `/order` — a required
`toAddress` param on order creation). (c): commission is paid automatically per
completed order via their affiliate program, integrated through the same API key.

Not independently re-verified here: the exact FixedFloat currency codes for USDT on
TRC20/BEP20 (used `USDTTRC`/`USDTBSC` as working assumptions in R4's own code, per
public FixedFloat pages — see `relayd/internal/upstream/fixedfloat.go`'s own doc
comment). Whoever operates this integration for real must confirm both against a live
`GET /api/v2/ccies` response before routing real orders — R4's own config
(`FIXEDFLOAT_CCY_USDT_TRC20`/`FIXEDFLOAT_CCY_USDT_BEP20`) has no default specifically
so this can't be skipped silently.

R4 (wiring): `relayd/internal/upstream/fixedfloat.go` implements `SwapProvider`
against FixedFloat's real v2 API, gated behind `UPSTREAM_PROVIDER=fixedfloat` (see
`cmd/relayd/upstream_provider.go`). Not yet proven against FixedFloat's own live API or
a real R6 replay run — this repo's own "prove it against something real, not just unit
tests" discipline still applies before this is considered shipped, not just wired.

**Update (14 Sep 2026): a real order placed against FixedFloat surfaced a real problem**
(reported by the operator, not yet root-caused) — the specific symptom hasn't been
captured in this doc yet, pending more detail. Until it's resolved, FixedFloat should
not be treated as a proven, production-ready vendor on its own.

## R2 addendum: second vendor + best-rate routing (14 Sep 2026)

**Decision: don't wait on debugging FixedFloat alone — add a second real vendor and
route each order to whichever offers the best rate at the moment it's created,** rather
than staying committed to a single vendor. This also directly serves the "best rate for
the customer" goal mechanism 1 (commission) doesn't otherwise optimize for on its own:
a single-vendor integration only ever offers that one vendor's own rate, whatever it is.

**Chosen second vendor: ChangeNOW (changenow.io).** Evaluated against the same four R2
criteria FixedFloat was: (a)/(b) confirmed directly against their real v1 API
(`GET /exchange-amount/...`, `POST /transactions/{api_key}`, `GET
/transactions/{id}/{api_key}` — arbitrary destination `address` param on transaction
creation, reconstructed from a real third-party Go client's own documented endpoint
list, since the official Postman docs are JS-rendered and could not be fetched directly
while building this — needs live verification before production, same caveat as (d)
being independently confirmed rather than inferred: **API Terms of Use §2.1/§3.3**
(https://changenow.io/terms-of-use/changenow-api) explicitly grant "a ... license to
use and make calls to our API solely in connection with developing, implementing, and
distributing your application that interoperates or integrates with the Service" —
exactly relayd's own model (integrate their API into this system, never resell API
access itself, which the same clause's own non-sublicensable restriction rules out
regardless). (c): 0.4% default commission via their own affiliate program, adjustable,
paid per completed order.

A third real candidate, Changelly, was also checked: real documented endpoints
(`createTransaction`/`getStatus`/`getExchangeAmount`) and both USDT TRC20/BEP20 pairs
confirmed (`usdtrx`/`usdtbsc`), and overwhelming circumstantial evidence of a mature
commercial API business (350+ white-label API partners) — but no equivalent dedicated
ToS clause could be found confirming criterion (d) the way FixedFloat's §6 and
ChangeNOW's §2.1/§3.3 both do; their own public terms only surfaced generic
content-IP boilerplate, and a real commercial integration appears to need a negotiated
quote (affiliate@changelly.com) rather than a self-service confirmation. Not chosen for
now on that basis — a real candidate to revisit if either FixedFloat or ChangeNOW
becomes untenable, but not preferred over ChangeNOW today.

**Not independently re-verified**, same posture as FixedFloat's own currency-code
caveat above: ChangeNOW's exact currency ticker codes for USDT TRC20/BEP20 (working
assumptions `usdttrc20`/`usdtbsc`, inferred from public ChangeNOW pages) and its exact
transaction-status string values (reconstructed from ChangeNOW's own public help-center
articles, not the API docs directly). R4's own config
(`CHANGENOW_CCY_USDT_TRC20`/`CHANGENOW_CCY_USDT_BEP20`) has no default, so this can't be
skipped silently, matching FixedFloat's own posture exactly.

**R4 (routing):** `relayd/internal/upstream/router.go`'s `MultiProvider` implements
`SwapProvider` by querying every configured vendor in parallel and routing to whichever
offers the best rate — `Quote` for the customer-facing preview, and a **fresh re-quote**
at `CreateOrder` time (folded into architecture doc §6's own existing
re-quote-at-forward-time step, not a separate mechanism) so the vendor actually used is
whichever is best *at the moment of commitment*, not whichever won a possibly-stale
earlier quote. A single vendor's error never blocks an order — only failing when every
configured vendor fails, mirroring `energybroker`'s own fallback-ladder posture.
`GetOrder` routes back to the originating vendor via a `"<vendor>:<real id>"` prefix
`CreateOrder` adds to whatever opaque id the winning vendor itself returned (composes
cleanly with FixedFloat's own `"id|token"` packing — proven by a dedicated test).
Gated behind `UPSTREAM_PROVIDER=best_rate` plus `UPSTREAM_BEST_RATE_PROVIDERS`
(comma-separated vendor names, no default). Unit-tested (best-rate selection, fallback
on a single vendor's error, all-fail case, re-quote-not-stale-quote at CreateOrder,
order-id prefix round-tripping) — **not yet proven against either vendor's live API or
a real R6 replay run**, same "wired, not yet shipped" distinction as FixedFloat's own
R4 entry above.
