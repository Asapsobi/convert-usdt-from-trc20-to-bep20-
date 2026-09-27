# USDT Network Converter

Convert USDT between **TRON (TRC-20)** and **BNB Smart Chain (BEP-20)**.

A customer picks what to send and what to receive, sees exactly what they will
get, sends USDT to a deposit address, and receives USDT on the other network in
their own wallet. An exchange partner (FixedFloat) does the conversion itself.
This system handles everything around it: prices and fees, deposit wallets,
network fees, signing, payouts, profit, and a record of every step.

**New here?** Read these two first:

1. [docs/00-product-goals.md](docs/00-product-goals.md) — what the product must do (the source of truth)
2. [docs/how-it-works.md](docs/how-it-works.md) — how the system does it

## Status

- **Both directions work with real money.** Tested on mainnet on 26 Sep 2026
  with 2 USDT each way.
- **Not open to the public yet.** Still needed: a real anti-money-laundering
  (AML) screening vendor, server hosting with alerts and backups, and legal
  review. See the [roadmap](docs/03-build/model-f-production-mvp-roadmap.md).

## How an order works

1. **Quote.** The customer enters an amount and sees: amount − exchange fee − our fee = what they receive.
2. **Deposit.** They confirm and get a deposit address from a small pool of reusable wallets, then send USDT to it.
3. **Detect.** A watcher sees the deposit on-chain and waits until it is final.
4. **Screen.** The order is checked before any money moves.
5. **Prepare.** The deposit wallet gets what it needs to send: a little BNB on BSC, or activation and rented energy on TRON.
6. **Forward.** Our fee stays in the deposit wallet; the rest goes to the exchange, which pays the customer on the other network.
7. **Verify.** The system checks the payout on-chain before marking the order complete.
8. **Sweep.** Profit collected in deposit wallets is moved to the treasury from time to time.

## Run it locally

You need macOS or Linux with **Go 1.27+**, **Node.js 20+**, **PostgreSQL 16**
(a local superuser login), and **curl**. Then:

```bash
scripts/dev.sh setup   # once: databases, config, keys, packages
scripts/dev.sh start   # start everything
```

Open the storefront at <http://127.0.0.1:5181> and the admin panel at
<http://127.0.0.1:18190> (the login is printed by `start`). No accounts or API
keys are needed: a local setup uses keys generated on your machine and a
simulated exchange. Details and troubleshooting are in
[docs/local-setup.md](docs/local-setup.md).

> **Never send real funds to an address from a local setup.** Its keys are
> plain files on your computer, and its exchange is simulated.

## What's in this repository

**The running product**

| Folder | What it is |
|---|---|
| [`relayd/`](relayd/) | The core service: quotes, orders, and every money movement for an order |
| [`depositwatcher/`](depositwatcher/) | Watches BNB Smart Chain for deposits; owns the BSC deposit-wallet pool |
| [`tronwatcher/`](tronwatcher/) | Watches TRON for deposits; owns the TRON deposit-wallet pool |
| [`ledger/`](ledger/) | Double-entry ledger that records every movement of money |
| [`screening/`](screening/) | Checks orders before money moves (placeholder approves all until a vendor is chosen) |
| [`s1/`](s1/) | Signing service; in production every private key is held by Privy |
| [`opsconsole/`](opsconsole/) | Admin panel: orders, pricing, vendors, wallets, sweeps |
| [`relay-storefront/`](relay-storefront/) | The customer website |
| [`scripts/`](scripts/) | Local setup (`dev.sh`) |
| [`docs/`](docs/) | Documentation — index in [docs/README.md](docs/README.md) |

**An earlier design ("Model D"), kept for reference, not running**

| Folder | What it is |
|---|---|
| [`dispatcher/`](dispatcher/) | Paid customers from our own pre-funded wallets |
| [`energybroker/`](energybroker/) | Rented TRON energy for the dispatcher (relayd now rents it directly) |
| [`gateway/`](gateway/) | API for business customers |
| [`storefront/`](storefront/) | That design's customer website |
| [`proofrun/`](proofrun/) | Driver for its Sep 2026 mainnet test |
| `docker-compose.yml`, `Dockerfile`, `docker-entrypoint.sh`, `.env.proofrun.example` | Container setup for that test; never run with Docker. Use `scripts/dev.sh` instead. |

## Words you'll see

- **Model F** — the product that runs today: each order is relayed through an
  exchange partner, so we never hold stock of USDT. In code it is the "relay":
  `relayd`, and a **relay leg** is one customer order.
- **Model D** — the earlier design above: we would hold USDT on both networks
  and pay customers from our own balance.
- **C1–C6, S1** — build codes from the design phase: C1 ledger, C2 BSC
  watcher, C2′ TRON watcher, C3 screening, C4 energy broker, C5 dispatcher,
  C6 gateway, S1 signing.
- **Deposit-wallet pool** — the small set of reusable wallets customers pay into.
- **Treasury** — our wallet that pays network fees for deposit wallets and
  receives swept profit. In S1 it is a **slot**.
- **Sweep** — moving collected profit from deposit wallets to the treasury.

The full glossary is in [docs/README.md](docs/README.md#glossary).

## Working on the code

- Every service is its own Go module: `cd relayd && go test ./...`.
- Integration tests need a throwaway PostgreSQL database, named by
  `<SERVICE>_TEST_DATABASE_URL` (for example `RELAYD_TEST_DATABASE_URL`), and run
  with `go test -tags=integration ./...`. Never point them at a database you
  care about: they empty tables.
- Config lives in `.env.local` / `.env.*` files, which git ignores; only
  `.env.example` templates are committed. Never commit real keys.
