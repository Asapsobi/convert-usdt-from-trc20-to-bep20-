# Ops Console — build prompts

> **This is the built one.** A separate, much larger, unbuilt design for the
> same "operator dashboard" concern exists at
> `operations-control-center-build-prompts.md` — written independently on
> `origin/main` while this doc/service was being built on another branch, and
> kept as a distinct file (rather than one overwriting the other) by merge
> commit `3aed5fd`. Nothing in that doc has been built; reconciling the two
> designs was explicitly deferred and is still open. If you're looking for
> what actually runs today, see `opsconsole/` and this doc, not that one.

**Target:** Go, no dedicated database of its own. **Consumer:** an AI coding
agent (Claude Code or equivalent), same usage pattern as every prior
build-prompts doc in this project — paste §0 once, then work chunk by chunk.

**Context.** This is not one of the six MVP services decision documents
scoped in advance — it doesn't appear in `component-map.md`'s C1–C6 list, and
there's no architecture doc behind it the way S1 or C4's buffer redesign had
one. It exists because running two real end-to-end proof runs (9 Sep and
10 Sep 2026, see `mvp-proof-run-plan.md` and the README's own account of what
those found) required an operator — a human, by hand, over `psql` and a
handful of one-off CLI tools (`cmd/reconcile-reservation`, `cmd/seed-slot`,
`cmd/seed-slot-key`) — to directly inspect and correct state in three
different services' databases mid-run: a stuck deposit-watcher cursor, a
wrongly-FAILED energy reservation that a real vendor purchase had actually
succeeded for, a payout slot that needed registering before dispatch could
use it. Those tools work, but they're scattered, require shell access to
whichever machine is running the stack, and know nothing about each other —
an operator checking "is anything stuck right now" has to run five different
`curl`s and read three services' logs by hand, which is what actually
happened. This gives that operator one web page.

**What already exists to build on.** Every one of C1–C5/S1 already exposes
a `GET /v1/system/invariants`-shaped route reporting exactly the kind of
"is anything wrong" signal an ops console's home page wants (halt state,
cursor lag, queue depth, buffer-vs-target, stuck-dispatching count — see
each service's own `system_handlers.go` or `system.go`). C1 already has a
real halt/unhalt route. C3 already has real hold release/reject routes. C5
already has a real slot list and a real manual slot-retire override. None of
that needs rebuilding — the console is a thin client against it. Three real
gaps exist and get closed as part of this build, each mirroring a manual
action this session actually had to do by hand:

1. **C2 has no route to read or adjust `ingestion_cursor`.** The only
   existing signal is a derived lag metric. The 10 Sep proof run needed the
   cursor's `last_scanned`/`last_candidate_scanned` split and fast-forwarded
   by hand, directly against Postgres, twice. OC.4 adds
   `GET /v1/system/cursor` and `POST /v1/system/cursor` to depositwatcher.
2. **C4 has no route to list reservations by status.** `GET
   /v1/reservations/{id}` is get-by-id only; finding the stuck FAILED/PENDING
   row this session needed required a direct `SELECT` against
   `broker_dev`. OC.5 adds `GET /v1/reservations?status=` to energybroker.
3. **S1 has no route to list pending signing-approval requests.** An
   approver has no way to discover *which* requests are awaiting them short
   of already knowing the id. OC.7 adds `GET
   /v1/signing-requests?status=pending` to s1.

Each of these is a small, additive, backward-compatible route on an
already-shipped service — not a redesign of anything C1–C5/S1's own build
docs already settled.

**A genuine, disclosed limitation this design does not solve:** S1's
`POST /v1/signing-requests/{id}/approve` and `/reject` record the *bearer
token's own actor identity* as the approver — that's the whole reason S1
has a separate `S1_APPROVER_API_TOKENS` scope in the first place (§0 of
`s1-key-management-build-prompts.md`, invariant 3: never fewer than 2
*distinct* approvers). If the ops console holds one shared token and proxies
every operator's click through it, every approval would record the same
actor, silently defeating the 2-distinct-approver invariant. OC.2 (auth)
therefore does **not** let the console hold S1's approver token as a shared
service credential — each operator's own individual S1 approver token is
entered once at login and held only in their own signed session cookie
(§OC.2), never in the console's own config, so an approval genuinely
attributes to the human who clicked it, exactly as S1 already requires. This
is the one place this design had to bend the "console holds service tokens,
operators hold console tokens" shape everywhere else uses — called out here
because it's the one integration invariant-sensitive enough to get wrong
silently.

---

## §0 — Standing context (paste once)

```
You are building the Ops Console, a small internal web dashboard for a
USDT cross-network settlement system. C1 (ledger), C2 (deposit watcher), C3
(screening), C4 (energy broker), C5 (payout dispatcher), S1 (key
management), and C6 (API gateway) are all built and running as separate Go
services, each with its own Postgres database and its own service-to-service
bearer-token auth (`token:actor` pairs, one shared env var per service). The
ops console is a NEW, eighth service with no database of its own for
business data (see OC.2 for the one narrow exception: signed session
cookies, not a database).

STACK
- Go 1.22+, no ORM. chi routing, log/slog, testify -- same as every other
  component in this monorepo, for the same reason: consistency, not
  novelty.
- Server-rendered HTML via html/template, one small vanilla-JS file for
  interactivity (form submission via fetch, auto-refreshing status panels).
  No React/Vue/frontend build step -- this project has none anywhere else
  and one operator-facing dashboard is not the place to introduce one.
- No new database. Session state is a signed cookie (HMAC-SHA256 over a
  server-side secret, OC_SESSION_SECRET) carrying the operator's identity
  and their own per-service bearer tokens (see OC.2) -- nothing server-side
  to look up, nothing to migrate, nothing to leak from a database dump.

WHAT THE OPS CONSOLE IS
A thin HTTP client and HTML renderer in front of C1-C5/S1's own existing
`/v1/system/*` and resource routes, plus three new routes this build adds to
those services (OC.4, OC.5, OC.7) to close real gaps found running real
proof runs. Every read is a live call to the owning service; the console
caches nothing across requests.

WHAT THE OPS CONSOLE IS NOT -- do not build any of this, do not import
libraries for it
- Not a database client. Every piece of state the console shows or changes
  goes through the owning service's own HTTP API -- never a direct
  Postgres connection to C1-C5/S1's own databases. (Contrast this
  deliberately with gateway's own C6.9 replay harness, which DOES hold a
  raw LEDGER_DATABASE_URL connection for one narrow, already-justified
  reason -- a test fixture, not a live operator action. The ops console has
  no equivalent justification and must not acquire one.)
- Not a replacement for any of the six services' own background loops --
  it never runs a reconciliation loop, a poller, or a retry itself. It
  shows what those loops are doing and lets an operator take the one-off
  actions those loops can't (see each OC chunk's own "why this needs a
  human" framing).
- Not multi-tenant, not customer-facing. There is no equivalent of C6's
  customer API keys here -- every user of this console is an internal
  operator.
- No new business logic. If a chunk seems to require deciding something
  the owning service doesn't already decide (e.g. what counts as "stuck"),
  that's that service's own /v1/system/invariants definition to reuse, not
  a new rule to invent here.
If a chunk seems to require any of the above, you have misread it. Stop and
say so.

NON-NEGOTIABLE INVARIANTS
1. The console never holds a human approver's own S1 approval authority as
   a shared credential -- see this doc's own "genuine, disclosed
   limitation" section above. Each operator's own S1 approver token lives
   only in their own session cookie, entered at login, never in server
   config.
2. Every write action the console performs against another service passes
   through that service's own existing authorization and idempotency
   rules unmodified -- the console adds a UI, never a bypass. A write
   route the console calls always sends the same Idempotency-Key header,
   the same required body fields, and gets the same validation any other
   caller of that route would.
3. Every write action is logged, server-side, before the downstream call
   is made -- operator identity (from session), action, target, timestamp
   -- to a single append-only local file (OC_AUDIT_LOG_PATH), independent
   of whatever the downstream service's own audit trail records. This is
   belt-and-suspenders: if the downstream call fails, the attempt is still
   on record.
4. Read routes render even if one or more downstream services are
   unreachable -- one dead service's timeout degrades that one panel to an
   error state, never the whole page. Matches proofrun's own driver's
   "degrading gracefully rather than failing outright if one of them is
   unreachable" convention exactly.
5. No secret (a downstream service's own bearer token, the session HMAC
   key) is ever rendered into an HTML page, a log line, or a URL query
   string. Session cookies carry them HMAC-signed and this process's own
   env holds the console's own per-service tokens (OC.1) -- neither ever
   appears in template output.

STYLE
- Package layout: internal/opclient (the six typed HTTP clients to
  C1-C5/S1, one file each, matching gateway's own c1client/c2client shape
  rather than inventing a new client convention), internal/session (cookie
  signing/parsing), internal/auditlog, internal/httpapi (routes + html/
  template rendering), cmd/opsconsoled.
- Errors typed, wrapped with %w, one stable code per condition reaching an
  HTML error panel -- same discipline as every prior component, adapted:
  the "response" here is a rendered panel, not a JSON envelope, but the
  underlying error taxonomy is built the same way.
- Tests are the deliverable. internal/opclient against httptest.Server
  fakes standing in for each downstream service (never a real running
  C1-C5/S1 in unit tests). internal/session has table-driven tests
  proving a tampered cookie is rejected. An integration suite
  (`-tags=integration`) runs the real console against real, disposable
  ledgerd/watcherd/brokerd/dispatchd/s1d/screend subprocesses, the same
  `internal/testledger`/`internal/testwatcher` pattern C3/C4/gateway
  already established -- extended here to the other three services rather
  than reinvented.
- Do not add features, endpoints, or abstractions a chunk did not ask for.
```

---

## OC.0 — Skeleton, health, config

**Build:**
- `cmd/opsconsoled/main.go`: reads config from env (below), builds a chi
  router, serves `/healthz` (always 200 once the process is up — this
  service holds no DB connection to be unready against) and `/metrics`
  (`prometheus/client_golang`, matching every sibling service).
- Config, one `BaseURL`/`Token` pair per downstream service, all required,
  no default (same posture every other component takes on required config):
  `OC_LEDGER_BASE_URL`/`OC_LEDGER_TOKEN`, `OC_WATCHER_BASE_URL`/
  `OC_WATCHER_TOKEN`, `OC_SCREENING_BASE_URL`/`OC_SCREENING_TOKEN`,
  `OC_BROKER_BASE_URL`/`OC_BROKER_TOKEN`, `OC_DISPATCHER_BASE_URL`/
  `OC_DISPATCHER_TOKEN`, `OC_S1_BASE_URL`/`OC_S1_C5_TOKEN` (S1's own C5-scope
  token, used only for read routes S1.2/S1.3's scope already covers — never
  for approve/reject, see OC.2). Plus `OC_SESSION_SECRET` (≥32 bytes, fails
  loud if shorter — this is the one secret whose weakness would actually
  matter), `OC_AUDIT_LOG_PATH`, `OC_LISTEN_ADDR` (default `:8091`, the next
  free port after C6's 8090).
- `internal/opclient`: one typed client per service, each wrapping
  `net/http` with the console's own bearer token, a bounded timeout (5s,
  matching this project's other cross-service clients), and typed
  request/response structs for exactly the routes later chunks need — not
  a generic client, six narrow ones, matching gateway's own
  `c1client`/`c2client` shape.

**Acceptance:** `go build ./...`, `go vet ./...` clean. `GET /healthz`
returns 200 with no downstream services running (this service has nothing
to be unready against). Missing any required env var fails startup with a
clear message naming which one, same posture as `LEDGER_DATABASE_URL`.

## OC.1 — Auth: operator login, session cookie

**Why this shape.** No user database — see OC.2's own note on why S1's
approver token specifically can't be a shared server credential; the same
reasoning extends more loosely to every other per-service token, since an
audit log entry that says "the console did X" is weaker than one that says
"operator Sobhan did X." Operators are configured via one env var,
`OC_OPERATORS`, formatted `username:bcrypt_hash:display_name,...` (bcrypt,
not a bearer token — humans type a password into a login form, they don't
paste a service token, this is the one place in the whole project a real
password hash is the right tool, unlike C6's `customers.GenerateAPIKey`
which deliberately avoided one for a machine-held 256-bit key).

**Build:**
- `GET /login`: a plain HTML form (username, password, and — S1 approver
  token, optional, only needed to approve/reject signing requests later).
- `POST /login`: checks username/password against `OC_OPERATORS` via
  `bcrypt.CompareHashAndPassword`; on success, sets a signed cookie
  (`internal/session`) carrying `{username, display_name,
  s1_approver_token, issued_at}`, HMAC-SHA256 over `OC_SESSION_SECRET`,
  `HttpOnly`, `SameSite=Strict`, expiring after 12h.
- `POST /logout`: clears the cookie.
- Middleware: every route under `/` except `/login`, `/healthz`,
  `/metrics`, and static assets requires a valid, unexpired session; an
  invalid/missing one redirects to `/login`.
- `internal/session`: `Sign(Session) (cookieValue string)`,
  `Verify(cookieValue string) (Session, error)` — table-driven tests
  proving a tampered value, an expired one, and a value signed with a
  different secret are all rejected.

**Acceptance:** unit tests for `internal/session` cover tamper/expiry/
wrong-secret rejection. An end-to-end test posts valid credentials, gets a
cookie, and confirms a protected route 200s with it and 302s (to `/login`)
without it.

## OC.2 — Home: service health grid, system invariants, halt banner

**Why a human needs this, not just each service's own /healthz.** No
existing view aggregates "is anything wrong" across all six services in one
place — that's exactly the gap this session's own "check every service's
healthz and system/invariants by hand, five separate curls" pattern is
standing in for today.

**Build:**
- `GET /`: calls `GET /healthz` and `GET /v1/system/invariants` (or the
  closest equivalent — C3's `/v1/system/queue`, C4's `/v1/system/prices`
  plus `/v1/system/invariants`) on all six downstream services,
  concurrently, each bounded by its own 5s timeout (invariant 4 — one
  slow/dead service never blocks the others). Renders one card per
  service: green/red health dot, and that service's own invariants fields
  verbatim (cursor lag for C2, queue depth for C3, buffer-vs-target for
  C4, stuck-dispatching count for C5) — the console does not reinterpret
  what "healthy" means, it displays what each service already decided.
- A halt banner across the top of every page (not just `/`): if C1's `GET
  /v1/system/halt` reports halted, a persistent red banner with the reason
  and a link to OC.3 — this is the one piece of state urgent enough to
  surface everywhere, not just the home page.
- Auto-refresh: the vanilla-JS file polls `/` (or a `/partial/home` variant
  returning just the cards) every 10s — no websockets, a plain interval
  fetch, matching this project's preference for the simplest tool that
  works.

**Acceptance:** integration test against real subprocess services proves
the home page renders all six cards; killing one subprocess mid-test
proves that card alone degrades to an error state while the others still
render (invariant 4, directly tested, not just asserted in prose).

## OC.3 — Ledger halt control

**Build:**
- `GET /ledger/halt`: renders current state (`GET /v1/system/halt`) and,
  if not halted, a form to set one (`reason`, `detail`, `note` — matching
  C1's own request body); if halted, a form to clear it.
- `POST /ledger/halt/set` / `POST /ledger/halt/clear`: audit-logs first
  (invariant 3), then calls C1's own route with the operator's
  `display_name` threaded into `note` (C1's own halt route takes actor
  from the bearer token identity, which here is the console's own
  `OC_LEDGER_TOKEN` — the operator's name goes in `note` precisely because
  C1 has no per-human actor concept to attribute to, the same limitation
  this doc's own intro names for S1 but smaller in consequence, since halt
  is reversible and always visible on the banner, not an irreversible
  approval).

**Acceptance:** integration test sets a halt through the console, confirms
the banner (OC.2) shows it, clears it through the console, confirms the
banner clears.

## OC.4 — C2: ingestion cursor (new route + console page)

**New depositwatcher route** (`depositwatcher/internal/httpapi/`):
- `GET /v1/system/cursor`: returns the raw `ingestion_cursor` row
  (`last_scanned`, `last_candidate_scanned`, `updated_at`) — the first
  route to expose it directly, everything before this only derived a lag
  metric from it.
- `POST /v1/system/cursor`: body `{last_scanned, last_candidate_scanned,
  reason}` (reason required, logged), updates the row. Validates
  `last_candidate_scanned <= last_scanned` (candidates can never be ahead
  of ingestion — `internal/candidates/loop.go`'s own existing invariant,
  just enforced here too rather than trusting the caller) and both `<=`
  the chain's own current tip (read fresh via the configured RPC pool at
  request time — refuses to set the cursor into the future). Requires
  `Idempotency-Key` like every other write route in this service.
- Tests: a new `cursor_test.go`/`cursor_integration_test.go` covering the
  validation rejections and a successful round-trip.

**Console build:**
- `GET /watcher/cursor`: current row plus computed lag against a live
  chain-tip read; a form to set new values with a required reason.
- `POST /watcher/cursor`: audit-logs first, calls the new route.

**Acceptance:** depositwatcher's own `go test -tags=integration ./...`
covers the new route (regression-safe against C2's existing suite, not
just new-code-only). Console integration test performs a set through the
UI and confirms the read view reflects it.

## OC.5 — C4: reservations by status (new route + console page + reconcile form)

**New energybroker route** (`energybroker/internal/httpapi/`):
- `GET /v1/reservations?status=FAILED&status=PENDING&limit=`: list,
  filtered by one or more `status` values (validated against the existing
  `PENDING|CONFIRMED|FAILED` check constraint), newest first, cursor-
  paginated matching `GET /v1/orders`' own existing pagination shape in
  C1 rather than inventing a new one.

**Console build:**
- `GET /broker/reservations?status=FAILED`: table of matching
  reservations (id, external_id, target_address, energy_units, deadline,
  status).
- `GET /broker/reservations/{id}/reconcile`: a form replacing
  `cmd/reconcile-reservation`'s own CLI flags one-for-one (provider,
  delegation-id, target-address, energy-units, cost-trx) — the form's own
  help text says explicitly what that CLI tool's own doc comment already
  says: the operator is vouching for a delegation they independently
  verified on-chain, this is not automatic.
- `POST /broker/reservations/{id}/reconcile`: audit-logs first, calls
  `POST /v1/reservations` is NOT reused here (that's the create path) —
  this needs a new, narrow C4 route wrapping `Service.ManualConfirm`
  directly (`POST /v1/reservations/{id}/reconcile`, body matching the
  CLI's own flags), added in this same chunk, since `ManualConfirm` had no
  HTTP route at all before now, only the CLI. Keep `cmd/reconcile-
  reservation` itself — the console is an additional way to reach the same
  operation, not a replacement requiring shell access to be retired.
- `GET /broker/fallback-events`, `POST /broker/fallback-events/{id}/
  resolve`: thin wrap of the existing `GET /v1/manual-fallback-events` /
  `POST .../resolve` routes — no new C4 code needed here, already exists.

**Acceptance:** energybroker's own integration suite covers the two new
routes (list-by-status, reconcile-via-HTTP) including the same "delegation
never actually existed" rejection case `ManualConfirm`'s own doc comment
implies an operator could get wrong. Console integration test reconciles a
seeded FAILED reservation through the UI and confirms it reads back
CONFIRMED.

## OC.6 — C3: holds queue, C5: slots

**Console build (no new routes needed — both already exist):**
- `GET /screening/holds?status=open`: thin wrap of `GET /v1/holds`.
- `POST /screening/holds/{id}/release` / `.../reject`: audit-logs first,
  calls C3's own route with `reviewer` set to the operator's
  `display_name` (C3's own route takes reviewer from the request body,
  exactly the shape that lets this be attributed correctly, unlike C1's
  halt route — the design note in this doc's intro about C3 already
  expecting "the ops tool's own auth... establishes the reviewer identity"
  is this chunk, confirmed against the actual code before writing it).
- `GET /dispatcher/slots`: thin wrap of `GET /v1/slots`.
- `POST /dispatcher/slots/{id}/retire`: audit-logs first, calls C5's own
  route; body includes the `immediate` flag with the same warning C5's own
  doc comment gives it ("a manual override of the normal cap-triggered
  rotation") rendered directly into the confirmation UI, not softened.

**Acceptance:** integration tests release a seeded hold and retire a
seeded slot through the console UI, confirm both read back updated.

## OC.7 — S1: pending signing-approval requests (new route + console page)

**New s1 route** (`s1/internal/httpapi/`):
- `GET /v1/signing-requests?status=pending`: list, C5-scope-authenticated
  like every other S1 route an external caller uses for reads. Returns
  enough to render a queue (id, slot_id, estimated_usd, requested_at) —
  never the digest or anything signature-related beyond status, matching
  S1's existing posture of returning `signed_tx` only once SIGNED.

**Console build:**
- `GET /s1/approvals`: table of pending requests via the new route (using
  `OC_S1_C5_TOKEN`, a read — see invariant 1, this specific route is
  fine to read with a shared token, only approve/reject is not).
- `POST /s1/approvals/{id}/approve` / `.../reject`: **requires the
  operator's own S1 approver token from their session** (OC.1) — if their
  session has none (they didn't enter one at login), the button is
  disabled with an explanatory tooltip rather than silently using some
  other credential. Audit-logs first, calls S1's own route using that
  per-operator token, never `OC_S1_C5_TOKEN`. This is invariant 1, made
  concrete: the one action in this whole console that cannot go through a
  shared server-held credential, by construction, not by convention.

**Acceptance:** s1's own integration suite covers the new list route.
Console integration test: an operator session WITHOUT an S1 approver token
sees the approve/reject buttons disabled; a session WITH one successfully
approves a seeded pending request, and the request's own recorded approver
actor (read back via S1's existing `GET /v1/signing-requests/{id}`) matches
that operator's own token identity, not the console's.

## OC.8 — Audit log

**Build:**
- `internal/auditlog`: `Log(ctx, operator, action, target string, detail
  map[string]any)` appends one JSON line to `OC_AUDIT_LOG_PATH` (`log/
  slog` writing to a file handle, not a database — invariant 3 asks for
  independence from every downstream service's own audit trail, and this
  project has no shared logging infrastructure to plug into yet). Called
  by every OC.3–OC.7 write handler before the downstream call, per
  invariant 3 — this chunk wires the calls those chunks already reference,
  rather than leaving `Log` unused until now.
- `GET /audit`: a simple reverse-chronological read view of the same file
  (tail, bounded to the last 500 lines — this is an operator convenience
  page, not a queryable log store).

**Acceptance:** a write action anywhere in the console produces a
corresponding audit-log line even when the downstream call itself fails
(a fake returning 500 in the relevant `internal/opclient` test) — proving
invariant 3's "logged before the downstream call is made, independent of
whether it succeeds" is real, not just documented.

## OC.9 — Ship gate: integration suite against real subprocesses

**Build:** `cmd/replay` (or reuse `go test -tags=integration` directly,
whichever this project's own C3/C4/gateway precedent argues for once
OC.0–OC.8 exist to look at) driving the console's own router against real
`ledgerd`, `watcherd`, `screend`, `brokerd`, `dispatchd`, `s1d` subprocesses
— extending `internal/testledger`/`internal/testwatcher`'s own pattern
(already duplicated once per component, per this whole project's
established convention of not sharing that package across modules) to the
four services that don't have it yet. Exercises: login, the home page
against all six live services, one write action per OC.3–OC.7 chunk
end-to-end, and the OC.7 disabled-button-without-approver-token case
specifically, since that's the one invariant most likely to silently
regress into "console signs as itself" if a future change forgets it.

**Acceptance:** `ALL PASSED`, matching the exact report shape C6.9's own
replay harness already established — this is the ship gate before the
console is considered done, same bar every prior component held itself to.
