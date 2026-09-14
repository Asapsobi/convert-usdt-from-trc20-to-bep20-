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
   defensible if paired with the same move Model D already makes: sell it as embedded
   settlement infra behind someone else's product, not a public "convert my USDT"
   page — `docs/02-architecture/product-operations-architecture.md`'s existing ban on a
   public swap page for Model D applies with equal force here.

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
