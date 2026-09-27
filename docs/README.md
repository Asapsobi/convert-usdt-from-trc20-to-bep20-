# Documentation

Start with the first two. Everything under "Background" is history: it explains
how the project got here, but you do not need it to run or change the product.

## Current

| Document | What it tells you |
|---|---|
| [00-product-goals.md](00-product-goals.md) | What the product must do. The source of truth: where another document disagrees, this one wins. |
| [how-it-works.md](how-it-works.md) | How the running system works: services, an order step by step, wallets, keys, safety checks. |
| [local-setup.md](local-setup.md) | Run the whole product on your own computer, and what changes for real money. |
| [03-build/model-f-production-mvp-roadmap.md](03-build/model-f-production-mvp-roadmap.md) | What is still missing before a public launch. |
| [01-strategy/model-d-model-f-product-separation.md](01-strategy/model-d-model-f-product-separation.md) | Decision record: why there are two designs, and why Model F is the public product (update of 26 Sep 2026). |

## Design references

Detailed designs of parts that exist today. They were written before later
changes, so where they disagree with [how-it-works.md](how-it-works.md), trust
that and the code.

| Document | Covers |
|---|---|
| [02-architecture/model-f-relay-architecture.md](02-architecture/model-f-relay-architecture.md) | relayd and the relay flow, as designed on 13 Sep 2026 |
| [02-architecture/s1-key-custody-architecture.md](02-architecture/s1-key-custody-architecture.md) | The signing service. Written for a cloud KMS; production now keeps keys in Privy. |
| [03-build/ops-console-build-prompts.md](03-build/ops-console-build-prompts.md) | The admin panel as specified |
| [03-build/c1-scenario-catalog.md](03-build/c1-scenario-catalog.md) | The ledger's test scenarios |

## Background (history)

| Document | What it was |
|---|---|
| [history/build-log-2026-09.md](history/build-log-2026-09.md) | The previous README: a log of how every service was built and proven, Aug–Sep 2026 |
| [01-strategy/findings-and-recommendation.md](01-strategy/findings-and-recommendation.md) | The first market study, which recommended Model D |
| [01-strategy/model-f-relay-findings.md](01-strategy/model-f-relay-findings.md) | Why the relay design (Model F) was built |
| [02-architecture/component-map.md](02-architecture/component-map.md) | Model D's architecture (C1–C6) |
| [02-architecture/product-operations-architecture.md](02-architecture/product-operations-architecture.md) | Model D's business and operations decisions |
| [03-build/mvp-proof-run-plan.md](03-build/mvp-proof-run-plan.md) | Model D's mainnet test, Sep 2026 |
| `03-build/*-build-prompts.md` | Step-by-step instructions each service was built from: [ledger](03-build/c1-ledger-build-prompts.md), [BSC watcher](03-build/c2-deposit-watcher-build-prompts.md), [screening](03-build/c3-screening-build-prompts.md), [energy broker](03-build/c4-energy-broker-build-prompts.md), [dispatcher](03-build/c5-payout-dispatcher-build-prompts.md), [gateway](03-build/c6-api-gateway-build-prompts.md), [signing](03-build/s1-key-management-build-prompts.md), [relay](03-build/model-f-relay-build-prompts.md) |
| [03-build/operations-control-center-build-prompts.md](03-build/operations-control-center-build-prompts.md) | A larger admin-panel design that was never built |

## Glossary

**Model F** — the product that runs today. Each order is relayed through an
exchange partner (FixedFloat), so we never hold our own stock of USDT. In code
this is the "relay".

**Model D** — an earlier design: we would hold USDT on both networks and pay
customers straight from our own balance. Built and tested in Sep 2026, not
running. Its services are `dispatcher/`, `energybroker/`, `gateway/`,
`storefront/` and `proofrun/`.

**Relay leg** — one customer order in relayd, from quote to payout.

**Direction** — `BEP20_TO_TRC20` (send on BSC, receive on TRON) or `TRC20_TO_BEP20`.

**C1–C6, S1** — build codes from the design phase: C1 ledger, C2 BSC deposit
watcher, C2′ TRON deposit watcher, C3 screening, C4 energy broker, C5 payout
dispatcher, C6 API gateway, S1 key management and signing.

**Deposit-wallet pool** — the small set of reusable wallets customers pay
into, one pool per network. A wallet serves one order at a time.

**Treasury** — our wallet that pays network fees for deposit wallets (BNB on
BSC, TRX on TRON) and receives swept profit. One key gives it an address on
both networks.

**Slot** — a signing key registered in S1. The treasury is a slot (`RELAYD_SLOT_ID`).

**Sweep** — moving collected profit from idle deposit wallets to the treasury.

**Energy / bandwidth** — TRON's transaction resources. Sending USDT on TRON
needs energy, which relayd rents (CatFee) instead of burning TRX.

**Activation** — a TRON address has to receive TRX once before it can send;
that costs about 1.1 TRX, paid by the treasury.

**Tier** — the ledger's order type: `RELAY` for Model F, `DIRECT`/`STANDARD` for Model D.

**Privy** — the service that holds the production private keys and signs
what S1 asks it to sign.
