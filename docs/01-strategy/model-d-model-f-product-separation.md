# Model D / Model F product separation — permanent decision record

_14 Sep 2026. Written after an explicit request to make this boundary permanent and
stop treating a decision made for one model as automatically binding the other. This
doc is the single place that boundary is stated; `findings-and-recommendation.md`,
`component-map.md`, and `model-f-relay-findings.md` each carry a short pointer back
here rather than restating it._

## The decision

**Model D and Model F are separate products built on shared infrastructure, not one
product with two faces.**

- **Model D** is Tether TRC20↔BSC conversion/settlement **infrastructure**, distributed
  through multiple channels: a B2B partner API (`gateway`/C6, built), a public B2C
  website (planned, not yet built — see "What changed" below), and potentially future
  channels (Telegram, bots, embedded widgets, none yet designed).
- **Model F** is a separate zero-float TRC20↔BEP20 relay product, sold only as embedded
  infrastructure behind a partner's own product — no public page, own customers, own
  pricing (commission), own roadmap.

Shared technical infrastructure (`ledger` C1, `screening` C3, `energybroker` C4, `s1`
key management, the BSC-side deposit watcher C2, `opsconsole`) serves both without
being forked or duplicated. `dispatcher`(C5)/`gateway`(C6) are Model D-only;
`relayd`/`tronwatcher`(C2′) are Model F-only. This split already existed correctly at
the code level before this doc was written — confirmed by direct inspection, not
assumed — see "What was actually found" below.

**A decision, constraint, or roadmap item belonging to one model is never assumed to
apply to the other. Any such borrowing must be named explicitly, in writing, the way
this doc and its two source docs now do.**

## What changed, and what didn't

Model D's original MVP scope (`findings-and-recommendation.md`, Aug 2026) banned a
public swap page, reasoning that a public conversion page competes on visible spread
against a $1.00 CEX reference price, while Model D's real margin comes from a
structural cost advantage (wholesale TRON energy, batch multisend) that doesn't depend
on being cheaper on the visible fee. Model F's own findings doc (13 Sep 2026) later
borrowed that same ban for itself, citing it as "the same move Model D already makes."

As of 14 Sep 2026:

- **Model D's ban is reversed**, deliberately — a channel decision, not a reversal of
  the underlying pricing economics. The cost advantage the original analysis describes
  applies to volume from any channel; nothing about it requires the customer to be a
  business rather than an individual. Model D is now understood as infrastructure
  distributed through B2B, B2C, and potentially other channels, per the diagram and
  reasoning the user supplied when making this decision.
- **Model F's ban is unchanged, but now stands on its own reasoning**, not on
  inheritance from Model D's (since Model D's version no longer exists). If anything
  Model F's case for staying non-public is *stronger* than Model D's original one:
  Model F's margin (in the "quote your own rate" pricing mechanism) is a spread on top
  of a third-party vendor's own rate, not a structural cost advantage — it has less
  room than Model D ever had to survive a customer shopping the visible price.

Neither model's shared infrastructure changes. Neither model's other constraints
(AML/screening still applies to both regardless of capital model; Tether freeze risk
applies to every address either model ever holds funds in) are affected by this.

## What was actually found (14 Sep 2026 audit)

An explicit request to find every place D and F's product strategies might be silently
conflated turned up no silent conflation. Specifically confirmed by reading the code
and docs directly, not assumed:

- No frontend code exists anywhere in this repo for either model — `gateway`(C6) is a
  pure B2B partner API (bearer-token API keys, no end-user session model); `relayd`'s
  own driver is explicitly documented as a placeholder standing in for a future real
  gateway integration, not a step toward one; `opsconsole` is an internal,
  cookie-authenticated operator dashboard, not customer-facing in any sense.
- Every place Model F borrows a Model D constraint is explicit and self-labeled (e.g.
  `model-f-relay-findings.md`'s own "Carried over from Model D unchanged" section) —
  never implicit or silent.
- `orders.tier` in C1 cleanly includes `RELAY` alongside `DIRECT`/`STANDARD`/`SWEEP` —
  shared ledger, no fork.
- One real inaccuracy was found and fixed: `model-f-relay-findings.md` cited
  `product-operations-architecture.md` for the literal "public swap page" ban text,
  which actually lives in `findings-and-recommendation.md` and `component-map.md`.

## What a future B2C frontend for Model D would need (not yet built)

C6's existing data shape already covers what a frontend mechanically needs (quote,
order creation returning a deposit address, status polling). What it does not have is
an end-user auth/session model — its API-key auth is shaped for named partner
accounts, not individual customers. Building the frontend needs, at minimum: a new,
additive auth layer for end-user sessions (partner API keys stay as they are,
unchanged), and an actual frontend application (none exists in this repo today for
anything customer-facing). This is deliberately not scoped further here — see
`docs/03-build/` for where a real build-prompts doc for this should eventually live,
once designed.
