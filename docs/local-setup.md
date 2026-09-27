# Run it locally

One script sets up and runs the whole product on your computer: all seven
services, the storefront and the admin panel. It needs no accounts and no API
keys:

- **Keys** are generated on your machine (a local stand-in for Privy).
- **The exchange** is simulated: quotes and orders work, but nothing is sent to a
  real exchange.
- **Screening** approves every order.
- **The watchers** read the real BSC and TRON blockchains through free public
  nodes.

> **Never send real funds to an address from a local setup.** Its keys are plain
> files on your computer and its exchange is simulated, so money sent there
> would sit in a wallet only your local keys can move.

## 1. Install the tools

| Tool | Version | macOS | Debian/Ubuntu |
|---|---|---|---|
| Go | 1.27 or newer | <https://go.dev/dl/> | <https://go.dev/dl/> |
| Node.js | 20 or newer | <https://nodejs.org/> or `brew install node` | <https://nodejs.org/> |
| PostgreSQL | 16 | [Postgres.app](https://postgresapp.com/) or `brew install postgresql@16` | `sudo apt install postgresql` |
| curl, git | any | built in | `sudo apt install curl git` |

PostgreSQL must accept a login from your machine that may create databases
and roles. Postgres.app and Homebrew set this up already. On Linux, create a
superuser for yourself and give it a password:

```bash
sudo -u postgres createuser --superuser "$USER"
sudo -u postgres psql -c "alter user \"$USER\" password 'devpass'"
export DEV_PG_URL="postgres://$USER:devpass@localhost:5432"
```

## 2. Set up

```bash
git clone https://github.com/Asapsobi/convert-usdt-from-trc20-to-bep20-.git
cd convert-usdt-from-trc20-to-bep20-
scripts/dev.sh setup
```

This takes a few minutes the first time. It:

1. checks the tools and finds PostgreSQL;
2. builds every service;
3. creates six databases (`usdtconv_ledger`, `usdtconv_relayd`, …) and runs their migrations;
4. writes each service's config to `<service>/.env.local`, from its `.env.example`,
   with fresh random tokens, keys and an admin password;
5. registers the local treasury key in S1;
6. installs the storefront's packages.

Running it again is safe: it keeps existing databases and config.

## 3. Start

```bash
scripts/dev.sh start
```

It starts the services in order, waits until each one answers, and prints:

```
Storefront:    http://127.0.0.1:5181
Admin panel:   http://127.0.0.1:18190  (user: admin, password: …)
relayd API:    http://127.0.0.1:18187
```

## 4. Try it

**As a customer:** open the storefront, choose what to send and receive, enter
an amount and a destination wallet, and click *See what I'll receive*. Confirm,
and you get an order page with a deposit address from the local wallet pool.
(Don't pay into it; see the warning above.)

**As an operator:** log in to the admin panel. *Orders* lists orders, each
with its money trail; *Pricing* sets the margin per direction; *Vendors* shows
the simulated exchange; *BSC wallets* and *TRON wallets* show the pools and
which wallet serves which order; *Profit & sweeps* shows balances and sweep
settings.

**Through the API:**

```bash
# a quote
curl -s -X POST http://127.0.0.1:18187/v1/quotes \
  -H 'Content-Type: application/json' \
  -d '{"direction":"BEP20_TO_TRC20","amount_in":"100.000000"}'

# the admin API: your token is the one named "you" in relayd/.env.local
TOKEN=$(sed -n "s/^RELAYD_ADMIN_TOKENS='\([^:]*\):you.*/\1/p" relayd/.env.local)
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:18187/v1/admin/overview
```

## Everyday commands

| Command | Does |
|---|---|
| `scripts/dev.sh status` | Shows what is running, the URLs and the admin login |
| `scripts/dev.sh logs relayd` | Follows one service's log (`ledger`, `s1`, `screening`, `depositwatcher`, `tronwatcher`, `relayd`, `opsconsole`, `storefront`) |
| `scripts/dev.sh stop` | Stops everything |
| `scripts/dev.sh start` | Rebuilds anything you changed and starts what isn't running |
| `scripts/dev.sh reset` | Stops everything and deletes the local databases and config, to start over |

Logs, binaries and the admin password are kept in `tmp/dev/`, which git ignores.

## Settings

Set these before `setup`; `setup` remembers them in `tmp/dev/config.env`.

| Variable | Default | Use it when |
|---|---|---|
| `DEV_PG_URL` | tries `postgres://postgres@localhost:5432`, then `postgres://$USER@localhost:5432` | PostgreSQL is elsewhere or needs a password |
| `DEV_DB_PREFIX` | `usdtconv_` | you want different database names |
| `DEV_PORT_BASE` | `18180` | those ports are taken; services use base to base+10 |
| `DEV_STOREFRONT_PORT` | `5181` | that port is taken |

## Watching the blockchains

The watchers read real mainnet data through free public nodes, set in
`depositwatcher/.env.local` (`WATCHER_RPC_PROVIDERS`) and
`tronwatcher/.env.local` (`TRONWATCHER_PROVIDERS`).

- TronGrid limits requests without a key. If the TRON watcher logs "429" or
  "too many requests", get a free key at <https://www.trongrid.io/> and put it
  in `TRONWATCHER_API_KEY` and `RELAYD_TRONGRID_API_KEY`.
- To run without any chain access, empty both provider variables. Orders can
  still be created, but deposits are never seen, and unpaid orders don't
  expire, so the pools fill up after a few test orders (`reset` clears them).

After editing a `.env.local`, restart: `scripts/dev.sh stop && scripts/dev.sh start`.

## When something goes wrong

| Problem | Fix |
|---|---|
| `no PostgreSQL on localhost:5432 accepted a password-less login` | Start PostgreSQL, or set `DEV_PG_URL` (see step 1). |
| `must be able to create databases and roles` | Use a superuser login (see step 1). |
| `<service> exited while starting` | Read `tmp/dev/logs/<service>.log`; the error is near the end. |
| A port is already in use | Stop what uses it, or `scripts/dev.sh reset` and set up again with `DEV_PORT_BASE=28180`. |
| The storefront says it can't price the conversion | Check that relayd is running (`scripts/dev.sh status`) and read its log. |

## From local to real money

A local setup is for development. Running with real money changes these
parts; the [roadmap](03-build/model-f-production-mvp-roadmap.md) lists what
else a public launch needs (a server, alerts, backups, real screening, legal).

| Part | Local | Real |
|---|---|---|
| Keys (`s1`) | `S1_KMS_CLIENT='fake'` and generated XPRV/XPUB lines | `S1_KMS_CLIENT='privy'`, `PRIVY_APP_ID`, `PRIVY_APP_SECRET`; no XPRV/XPUB. Register the treasury with `s1/cmd/seed-slot-key` against Privy. |
| Deposit addresses (watchers) | derived from `WATCHER_XPUB` / `TRONWATCHER_XPUB` | created by S1 in Privy: `WATCHER_S1_BASE_URL` + `WATCHER_S1_PROVISIONING_TOKEN` (and the `TRONWATCHER_` pair); no XPUB |
| Exchange (`relayd`) | `UPSTREAM_PROVIDER='demo'` | `UPSTREAM_PROVIDER='fixedfloat'` with `FIXEDFLOAT_API_KEY`, `FIXEDFLOAT_API_SECRET`, `FIXEDFLOAT_CCY_USDT_TRC20='USDTTRC'`, `FIXEDFLOAT_CCY_USDT_BEP20='USDTBSC'` |
| TRON energy (`relayd`) | none | `RELAYD_CATFEE_API_KEY`, `RELAYD_CATFEE_API_SECRET` |
| TronGrid | no key | `TRONWATCHER_API_KEY`, `RELAYD_TRONGRID_API_KEY` |
| Treasury | empty | funded with a little BNB and TRX for network fees |
| Screening | approves everything | a real screening vendor (not built yet) |

Each `.env.example` explains its variables. Keep real config files out of git:
every `.env.*` file except `.env.example` is ignored.
