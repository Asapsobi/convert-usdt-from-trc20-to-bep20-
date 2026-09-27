# MVP proof run — decisions, deferred scope, and the next-stage prompt

> **Background (history).** Written during the design and build phase and kept as a
> record. You don't need it to run or change today's product: start at
> [docs/README.md](../README.md).

_Written 8 Sep 2026, from a cloud session working alongside the local session that's been building C1–C5/S1. Captures a scoping conversation that happened outside a commit: the "final MVP" end-to-end target (`c6-api-gateway-build-prompts.md`'s own eventual consumer, and the customer-facing flow `product-operations-architecture.md` describes) is real, but proving the pipeline works at all doesn't require building it in full first. This document is that narrower plan, so the next person picking this up doesn't have to reconstruct why the scope is smaller than the original MVP target — see "Read this first" below for the one thing that made mainnet non-optional for it, which took a real code check to find, not a spec read.**This document has not yet been reviewed against any code by a session other than the one that wrote it — treat the audit steps in §2 as required, not a formality.**

---

## Why this exists

Six components are real and built (C1, C2, C4, C5, S1's crypto, most of C3), but nothing has ever executed one real settlement end to end — no order has ever been created, funded, screened, energy-reserved, paid out, and settled through the actual live system. Before finishing C6 (the full customer-facing gateway: auth, pricing tiers, HMAC webhooks, sandbox) and before hardening S1's KMS and C3's screening for production, the higher-value next step is proving the pipeline itself works, with the smallest amount of new code that makes that true. This document is that scope, and the reasoning behind cutting it the way it's cut.

---

## Decisions taken

### 1. Real cloud KMS custody — deferred, not because it's unimportant, but because it isn't blocking anything

`s1/internal/kmssign/mock.go`'s `FakeKMSClient` is not a stub that fakes cryptography — it generates a real, in-process secp256k1 keypair and produces genuinely valid, independently verifiable signatures over the real transaction digest (see the type's own doc comment: *"every Sign call produces a REAL, independently verifiable DER signature over the real requested digest -- never a placeholder"*). A TRON payout signed through it is a real, broadcastable, confirmable transaction. The only thing it lacks versus a production KMS is that the private key lives in process memory instead of an HSM/cloud boundary — a custody property, not a correctness one. Proving the pipeline works doesn't need custody hardening; it needs a real signature, which this already produces. **Decision: run this proof stage against `FakeKMSClient`, unchanged.**

### 2. A real AML vendor — deferred, but a placeholder still has to be wired, because the state machine doesn't allow skipping the step

C1's own transition table requires `funded -> screened` before `dispatching` — this isn't optional or bypassable without touching the ledger's transition table, which is explicitly out of scope to modify. What's deferred is *which vendor* (Chainalysis/TRM/Elliptic, per `component-map.md`) — that's a real compliance and contracting decision, independent of whether the pipeline's automation works. **Decision: wire a trivial, explicitly-labeled `AlwaysCleanProvider` into C3's already-built pipeline in place of a real vendor call, so `funded` orders advance automatically. This is not a compliance decision — it changes nothing about which vendor gets chosen later, it only unblocks the state machine for a proof run with no real customers on the other end of it.**

### 3. The full C6 (API gateway) — deferred in favor of a minimal order-origination driver

`c6-api-gateway-build-prompts.md` (already written, not yet built) specs customer auth, rate limiting, a full bp-tier pricing engine, persisted quotes, HMAC-signed webhooks with 8 retries, and an isolated sandbox with four deterministic failure triggers — real requirements for a customer-facing product, none of them required to prove that C1→C2→C3→C4→C5→S1 can move one order through automatically. Building the full gateway before the pipeline it fronts is proven working risks polishing an interface on top of something unverified. **Decision: build a minimal internal driver instead — create an order against C1, get an address from C2, poll status — with no auth, no pricing model, no webhooks. This is scaffolding for this proof run, not a step toward C6; C6 still needs to be built in full, separately, against a now-proven backend.**

### 4. This proof run happens on TRON mainnet, not testnet — this is the one decision that came from reading code, not from a spec

Tronsell, Netts, and CatFee are real commercial marketplaces selling TRX energy for money. Nothing in their documentation, and nothing referenced anywhere in this repository, indicates testnet support — and there's no commercial reason they'd offer it, since testnet energy is free via faucet. **This means the energy-rental leg of this flow cannot be proven on testnet at all.** Combined with C2 already being verified against real BSC mainnet, the whole run happens on mainnet, with small real amounts. This is real money, in small quantities, by necessity, not by choice — flagged explicitly so it isn't discovered mid-run.

---

## What's deferred, and why it still has to be built before this can take real customer traffic

None of the following are "not needed" — they're needed before production, just not before this proof run.

| Deferred | Why it's deferred here | Why it still has to be built |
|---|---|---|
| Real cloud KMS adapter + key-generation ceremony | `FakeKMSClient` already produces real, correct signatures | An in-process key has no custody story — no split custody, no HSM, no recovery procedure, no protection against the process host being compromised. Fine for one proof-run payout, not for holding real customer funds at any real volume. `s1-key-custody-architecture.md`'s own "Key generation and bootstrapping" section is the real procedure this eventually needs. |
| Real AML vendor (Chainalysis/TRM/Elliptic) | `AlwaysCleanProvider` unblocks the state machine without needing a signed vendor contract | It approves every order unconditionally — that's a real compliance gap, not a simplification, the moment real customer volume exists. Choosing and contracting a vendor is a business decision this document deliberately doesn't make. |
| Full C6 (customer auth, pricing engine, webhooks, sandbox) | None of it is required to prove the pipeline | Customers can't self-serve against an internal driver with no auth. The sandbox is a named production gate in `product-operations-architecture.md` decision 5 ("no production sign-off without exercising all four" triggers) — this proof run doesn't touch that gate at all. |
| C5's Sweep-tier multisend contract | Direct/Standard tier alone is enough to prove one settlement | No smart contract is designed, audited, or deployed for it yet — Sweep's own margin claim (decision 6) has nothing real under it until this exists. |
| C3's pipeline/re-screen production wiring, beyond the placeholder | `AlwaysCleanProvider` is sufficient for one supervised run | It's a real compliance placeholder, not a real screening decision — see the AML row above. |

---

## The next-stage prompt

This is the prompt to hand to whichever session builds this proof run — self-contained, written so it doesn't require this conversation's context to execute correctly.

```
MVP Proof Run — Automated BSC → TRC20 Settlement
(minimal custody, minimal compliance — everything else real)

You are working on the existing `usdt-settlement-corridor` repository.

GOAL
Prove, with one real automated settlement, that the following pipeline works
end to end with no manual database intervention:

  give a recipient TRON address + amount
    -> customer gets a BSC deposit address
    -> customer sends real USDT on BSC
    -> deposit is detected and confirmed for real
    -> TRON energy is rented from a REAL vendor (Tronsell/Netts/CatFee)
    -> a real TRC20 payout is signed, broadcast, and confirmed
    -> the ledger reflects the whole thing correctly
    -> the order reaches a terminal COMPLETED state on its own

This is a narrower goal than a production launch. Two things are
DELIBERATELY out of scope for this run, and you must not spend time
hardening either of them:

1. Real cloud KMS custody. S1 already ships `kmssign.FakeKMSClient`
   (s1/internal/kmssign/mock.go) -- read it before assuming it's a
   placeholder that needs replacing. It generates a real, in-process
   secp256k1 keypair and produces genuinely valid, independently
   verifiable signatures over the real transaction digest. The only
   thing "fake" about it is that the key lives in process memory instead
   of a cloud HSM. For this run, that is fine -- the resulting TRON
   transaction is real and will confirm on-chain like any other. Run
   `s1d` with the fake client. Do not build a cloud KMS adapter for this
   task.

2. Real AML/compliance screening (Chainalysis/TRM/Elliptic). You still
   cannot skip screening AS A STATE TRANSITION -- C1's own transition
   table hard-requires `funded -> screened` before `dispatching`, and
   that table is not something this task should touch. What you build
   instead is a trivial, explicitly-labeled `ScreeningProvider`
   implementation that always returns a clean verdict, wired into C3's
   already-built pipeline in place of a real vendor call. Name it and
   document it unambiguously as a non-production placeholder (e.g.
   `AlwaysCleanProvider`, with a doc comment that says exactly what it
   is and is not) so nobody mistakes it for a real compliance decision
   later.

Everything else in this flow must be real. In particular: do not fake
BSC deposit detection, do not fake the energy rental call, do not fake
the TRON signature, broadcast, or confirmation. Those are already real
in the existing code (C2, C4, C5, S1) -- your job is wiring and the two
items above, not rebuilding anything that already works.

1. AUDIT FIRST
Before writing anything:
- Read component-map.md, the repository's own README (build-status
  section is kept current and accurate as of this writing), and
  c1-scenario-catalog.md.
- Read C1's real order/entry contract (ledger/docs/openapi.yaml).
- Read C2's real address-assignment contract (POST /v1/addresses,
  C2.9 in c2-deposit-watcher-build-prompts.md).
- Read C4's real reservation contract as C5 actually calls it
  (dispatcher/internal/energy/client.go -- POST/GET /v1/reservations).
- Read C5's real signing contract as it actually calls S1
  (dispatcher/internal/signing/client.go).
- Read C3's existing pipeline code (screening/) to find exactly where a
  real ScreeningProvider is wired in today and is not.
- Identify anything in this prompt that conflicts with what the code
  actually does, and flag it before building rather than guessing.

Do not rebuild C1, C2, C4, or C5. They are done and real. Do not modify
C1's transition table. Do not build a second wallet, address, or
accounting system.

2. WHAT TO BUILD

(a) C3: AlwaysCleanProvider
    A minimal ScreeningProvider implementing whatever interface C3's
    real pipeline already expects, always returning a clean verdict
    immediately. Wire it into the production pipeline/discovery loop so
    `funded` orders advance to `screened` automatically. Label it
    unmistakably as a placeholder in code and in this task's final
    report.

(b) A minimal order-origination driver (NOT the full C6 spec)
    This is intentionally much smaller than c6-api-gateway-build-prompts.md
    (no customer auth, no rate limiting, no persisted quotes, no pricing
    bp-tier engine, no HMAC webhooks, no sandbox). Build only:
      - one entrypoint that takes {amount, recipient_tron_address,
        external_id}, computes a straightforward fee (a fixed
        percentage or flat number is fine for this run -- do not build
        the full tiered pricing model), calls C1's POST /v1/orders with
        the computed amounts, then calls C2's POST /v1/addresses with
        the resulting order_id, and returns the deposit address to the
        caller.
      - one status entrypoint that reads C1's GET /v1/orders/{external_id}
        (plus whatever's needed from C2/C4/C5 to report deposit_tx_hash,
        energy_reservation_id, payout_tx_hash) and returns it.
    A CLI tool or a bare HTTP service is fine -- whichever is faster to
    get running correctly. This is scaffolding to prove the pipeline,
    not a component to harden.

(c) Wiring
    Stand up C1, C2, C3 (with the AlwaysCleanProvider wired in), C4, C5,
    and S1 (with FakeKMSClient) together, each against its own real
    Postgres, pointed at each other with real bearer tokens -- same
    posture every component already uses internally. No new docker-compose
    spanning all of them exists yet; write one if it helps you run this,
    scoped to this task, not a production deployment artifact.

3. ENVIRONMENT -- READ THIS BEFORE ASSUMING TESTNET

Tronsell, Netts, and CatFee are real commercial marketplaces selling TRX
energy for real money. Nothing in their docs or this repo suggests
testnet support, and there's no commercial reason they'd offer it (TRON
testnet energy is free via faucet). This means the energy-rental leg of
this flow CANNOT be proven on testnet -- it requires TRON MAINNET, with
real (small) TRX.

Given that, run the whole flow on mainnet, not testnet, with small real
amounts -- this avoids the added complexity of mixing testnet BSC with
mainnet TRON for one proof run, and C2 is already verified against real
BSC mainnet anyway. This is real money. Use the smallest amounts that
still clear each vendor's minimum order size.

OPERATOR-SUPPLIED PREREQUISITES (yours to provide, not to fabricate):
- A funded TRON address for the S1-derived signing key -- enough real
  TRX for broadcast fees until the first energy rental lands.
- An account and API key with at least one real vendor -- start with
  Tronsell (60% of default routing weight), funded with a small real
  balance.
- A TronGrid API key (free tier is fine).
- A BSC wallet holding a small amount of real USDT, to act as the test
  customer's deposit.
If any of these aren't available to you, stop and name exactly which one
is missing rather than substituting a fake for it.

4. NON-NEGOTIABLES (carried over from the existing architecture, not new)
- No manual SQL to advance an order. If something doesn't advance
  automatically, that's a bug to fix, not a step to do by hand.
- Every write idempotent, matching the convention every component here
  already uses.
- No fabricated transaction hashes, reservation IDs, or provider
  responses in the final report -- if a step didn't happen for real,
  say so plainly instead.
- A single external_id/order_id must let you reconstruct the whole
  lifecycle: deposit_tx_hash, energy_reservation_id, payout_tx_hash.

5. DEFINITION OF DONE
One real order, created through the driver from (b), reaches COMPLETED
by itself: real BSC deposit detected and confirmed, real screening
verdict (from the labeled placeholder) applied, real energy reservation
confirmed against a real vendor, real TRC20 payout signed (via
FakeKMSClient) broadcast and confirmed on TRON mainnet, real ledger
entries posted for the deposit, the payout, the fee, and the energy
cost -- with nothing edited by hand at any point.

6. FINAL REPORT
- What you reused vs. what you built (should be small: the two items in
  §2, plus wiring).
- The exact order lifecycle: external_id, deposit_tx_hash,
  energy_reservation_id (provider name + real cost), payout_tx_hash,
  final ledger entries.
- Explicitly separate: what's real now vs. what still needs real KMS
  custody and real AML before this could take production traffic --
  don't let a working proof run read as production-ready.
```

---

## Cross-reference

- The full, deferred C6 spec this proof run's minimal driver stands in for: `c6-api-gateway-build-prompts.md`
- C1's real transition table this document declines to modify: `c1-ledger-build-prompts.md`, `ledger/docs/openapi.yaml`
- S1's real signing math and its one deferred piece (cloud KMS): `s1-key-management-build-prompts.md`, `s1/internal/kmssign/mock.go`
- C3's existing pipeline this document wires a placeholder into rather than a real vendor: `c3-screening-build-prompts.md`
- Current build/repo status as of the last write-up: `repository.md`
