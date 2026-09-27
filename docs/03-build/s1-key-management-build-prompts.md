# S1 — Key management / signing: sequenced build prompts

> **Background (history).** Written during the design and build phase and kept as a
> record. You don't need it to run or change today's product: start at
> [docs/README.md](../README.md).

**Target:** Go + PostgreSQL, matching C1–C5's stack. **Consumer:** an AI coding
agent (Claude Code or equivalent), same usage pattern as every prior
build-prompts doc in this project.

**How to use this file.** Paste §0 once at the start of the session. Then
paste chunks S1.0 → S1.6 one at a time, in order. Do not move to the next
chunk until the current chunk's acceptance criteria pass.

**Before you start:** read `docs/02-architecture/s1-key-custody-architecture.md`
in full — it is the decision record this document turns into chunks, and it
explains *why* each design choice below was made, not just what to build.
In particular: the custody model (self-hosted cloud KMS, not a third-party
custodian), the hybrid auto/human-approval threshold, and the reason
`SigningService` is proposed here as request/poll rather than the single
synchronous `Sign` call `c5-payout-dispatcher-build-prompts.md` originally
proposed. **This document supersedes that interface** — whoever wires C5
against a real S1 needs to update C5's own client accordingly (a small,
contained change: C5.2's energy-reservation polling pattern already exists
in that codebase as a direct template for how to poll `GetSignature`).

---

## §0 — Standing context (paste once)

```
You are building S1, key management and signing, for a USDT cross-network
settlement system. C1 (ledger), C2 (deposit watcher), C3 (screening), and C4
(energy broker) are built. C5 (payout dispatcher) has a build-prompts doc but
no code yet, and is written against a PROPOSED SigningService interface --
this document's own §A supersedes that proposal with an async-capable
version; see docs/02-architecture/s1-key-custody-architecture.md for why.

STACK
- Go 1.22+, PostgreSQL 16 -- S1's own database, separate from every other
  component's, port 5437 (distinct from all five prior services).
- pgx/v5, goose migrations, chi routing, log/slog, testify.
- AWS SDK v2's KMS client (github.com/aws/aws-sdk-go-v2/service/kms) for the
  real signing wrapper -- verify this is still the current SDK major version
  at implementation time, the same "verify before trusting a remembered
  choice" discipline every prior component applied to its own dependencies.
- decred/dcrd/dcrec/secp256k1/v4 for signature parsing/normalization and
  public-key recovery -- already a proven, actively-maintained dependency in
  this monorepo (depositwatcher/internal/addresses/bip32.go uses it for the
  same curve, for a different purpose).

WHAT S1 IS
The only thing in this repository that can produce a valid signature over a
TRON transaction. Holds no private key material itself -- every key lives
inside cloud KMS, never exported, never reconstructible from anything S1's
own database or logs contain. Splits every signing request by an estimated-
USD threshold: under threshold signs immediately: at or above, blocks on a
recorded 2-of-N human approval before ever calling KMS Sign.

WHAT S1 IS NOT -- do not build any of this, do not import libraries for it
- No transaction construction or interpretation. S1 receives an
  already-serialized unsigned transaction's digest and a slot id; it has no
  opinion on what the transaction does, per the architecture doc's own
  threat-model note that this is a deliberate boundary, not an oversight.
- No BSC/deposit-address signing capability. The BSC HD seed is cold,
  offline, out of scope -- see the architecture doc's "Key topology."
- No broadcast. S1 returns a signed transaction; getting it onto the TRON
  network is C5's job entirely.
- No slot selection, no caps, no rotation policy -- C5 owns slot IDENTITY
  (which address is slot 3); S1 owns slot CUSTODY (what can sign for it).
  These are deliberately different tables in different services -- see the
  architecture doc's own key-generation step 4.
- No customer-facing anything. Every S1 caller is a service (C5) or an
  authenticated human approver.
If a chunk seems to require any of the above, you have misread it. Stop and
say so.

NON-NEGOTIABLE INVARIANTS
1. No private key, seed, or anything key-derived may ever appear in this
   module's own memory beyond a single KMS SDK call's own stack frame, in a
   log line, in the database, or in an error message. Enforced mechanically
   (S1.0) the same way depositwatcher/internal/addresses/no_signing_test.go
   enforces the equivalent guarantee for BSC addresses -- adapted, not
   copied, since S1's own forbidden vocabulary is different (KMS SDK types
   and raw curve-math constructors, not bip32/bip39).
2. RequestSignature is idempotent on its own caller-supplied idempotencyKey
   -- a retried request for the same key returns the existing
   SigningRequest, at whatever status it has actually reached, never creates
   a second row or signs twice. Matches this project's universal
   idempotency convention.
3. A request at or above the configured USD threshold NEVER signs on fewer
   than 2 distinct APPROVE decisions. This is checked at the moment of the
   second approval, inside one transaction with the KMS call itself failing
   the whole approval if Sign errors -- an approval is not "spent" on a
   failed sign attempt.
4. Every KMS Sign call is logged (request id, slot id, digest hash,
   timestamp, and for an approved request, which two approvers) to an
   append-only audit table -- never updated, never deleted, mirroring C1's
   own journal discipline for the same reason: this is the record an
   incident review depends on existing and being trustworthy.
5. GetSignature never returns a different signedTx for the same requestID
   twice -- SIGNED is terminal and cached, matching C4's own Reservation
   status semantics exactly (PENDING -> CONFIRMED|FAILED, never
   re-resolved).

STYLE
- Small packages: internal/kmssign (the ONLY package that ever imports the
  KMS SDK or does curve math -- the no-signing-boundary test's own
  enforcement point), internal/requests (the signing_requests/
  signing_approvals queue), internal/slots (S1's own key registry --
  distinct from, and never imported by, C5's own slot-identity registry),
  internal/httpapi.
- Errors typed, wrapped with %w, one stable code per condition reaching
  HTTP -- same discipline as every prior component.
- Tests are the deliverable. internal/kmssign is tested against a fake KMS
  endpoint (a local interface implementation returning controlled DER
  signatures, never a real AWS call in CI) capable of returning a
  malformed DER blob, a timeout, and a signature for the WRONG digest (to
  prove the recovery-id-matching step in S1.1 actually verifies against
  the expected public key rather than trusting whatever comes back).
- Do not add features, endpoints, or abstractions a chunk did not ask for.
```

---

## §A — Interface contracts

### `SigningService` (what C5 calls)

```go
type SigningService interface {
    RequestSignature(ctx context.Context, slotID int, unsignedTxDigest [32]byte, idempotencyKey string) (SigningRequest, error)
    GetSignature(ctx context.Context, requestID int64) (SigningRequest, error)
    SlotAddress(ctx context.Context, slotID int) (string, error)
}

type SigningRequest struct {
    ID        int64
    Status    string // "PENDING" | "SIGNED" | "REJECTED"
    SignedTx  []byte // the 65-byte r||s||v signature, nil until SIGNED
    CreatedAt time.Time
}
```

Note `unsignedTxDigest` is a `[32]byte`, not the raw unsigned transaction
bytes: S1 signs a digest, never constructs or parses transaction semantics
(see §0's own "WHAT S1 IS NOT"). The caller (C5) computes the SHA256 digest
of the serialized `raw_data` itself and hands S1 only that — this keeps
S1's own attack surface to "sign this 32 bytes for this slot," nothing
richer.

### `KMSClient` (what `internal/kmssign` calls — the real AWS boundary)

```go
type KMSClient interface {
    GetPublicKey(ctx context.Context, keyID string) (derPublicKey []byte, err error)
    Sign(ctx context.Context, keyID string, digest [32]byte) (derSignature []byte, err error)
}
```

A thin interface over the AWS SDK's own KMS client, defined here (the
consumer) rather than depended on directly everywhere, matching this
project's established convention (e.g. C4's `PriceSource`,
`reservations.BufferReserver`) — lets S1.1's own tests run against a fake
without a real AWS account, and keeps the real SDK import confined to one
small adapter file.

---

## S1.0 — Scaffold, interfaces, and fakes

```
Build the repository skeleton and every interface above, with fakes for
both -- nothing that touches real KMS or a real chain yet.

1. Go module `s1`. Layout mirrors C1-C5:
   cmd/s1d/main.go
   internal/kmssign/    -- KMSClient interface + FakeKMSClient
   internal/requests/   -- SigningService's own implementation (queue, approvals)
   internal/slots/      -- S1's own key registry (distinct from C5's)
   internal/db/
   migrations/
   docker-compose.yml (Postgres 16, port 5437)
   Makefile (build, test, test-integration, migrate-up, migrate-down, lint)

2. internal/kmssign:
   - KMSClient interface (see §A).
   - FakeKMSClient: deterministic on a seeded PRNG, generates its own
     in-memory secp256k1 keypair per keyID on first use (never persisted,
     never exported by any method this fake exposes), returns real,
     valid DER signatures over the real digest -- so downstream recovery-
     id-matching logic (S1.1) can be tested against genuinely correct
     signatures, not placeholder bytes. Configurable to return a
     malformed DER blob, a timeout, or (for a negative test) a
     syntactically valid signature over a DIFFERENT digest than the one
     requested.
   - The no-signing-material dependency test (invariant 1): scans every
     .go file outside internal/kmssign for AWS KMS SDK signing-adjacent
     imports and raw secp256k1 private-key constructors, same pattern as
     C2's/C3's/C4's own dependency tests, adapted to this package's own
     forbidden-identifier list (see §0's own note on why the list
     differs from depositwatcher's).

3. internal/requests: SigningService interface (see §A) as a Go
   interface only -- no implementation yet, just the type and a
   FakeSigningService (in-memory map, deterministic, immediately SIGNED
   for any request under a configurable fake threshold, PENDING
   otherwise) for C5's own future tests to build against, matching the
   posture c5-payout-dispatcher-build-prompts.md already established
   toward its OWN fake.

4. internal/db, cmd/s1d: same pattern as every prior component.

ACCEPTANCE
- `make test` green.
- FakeKMSClient: same (seed, keyID) always yields the same keypair; Sign
  produces a signature that independently verifies (using the same
  secp256k1 library, NOT by trusting the fake's own internals) against
  the digest and the public key GetPublicKey returns for that keyID.
- The no-signing-material dependency test passes and is specific enough
  to fail if someone later adds a real AWS KMS Sign call or a raw
  private-key constructor to a package outside internal/kmssign by
  accident -- prove this by temporarily adding one in a throwaway commit
  during development and confirming the test catches it, then revert.
- FakeSigningService: RequestSignature is idempotent on idempotencyKey
  (invariant 2), proven by a concurrent-call test, same discipline as
  C1.3's own idempotency proof.
```

---

## S1.1 — The real KMS signing wrapper

```
Build the actual TRON-over-KMS signing mechanics from the architecture
doc's own "Signing mechanics" section -- against FakeKMSClient, proving
correctness independent of any real AWS account.

BUILD internal/kmssign (extend)
  type Wrapper struct { client KMSClient }
  Sign(ctx, keyID string, digest [32]byte, expectedPubKey []byte) (sig [65]byte, err error)
    1. client.Sign(ctx, keyID, digest) -> DER bytes.
    2. Parse DER into (r, s).
    3. Normalize s to the curve's lower half if it isn't already.
    4. For v in {0, 1}: recover a candidate public key from (digest, r, s, v);
       compare (compressed-point-equal) against expectedPubKey; keep the v
       that matches.
    5. Neither v matches -> ErrSignatureDoesNotMatchKey -- a real, serious
       condition (S1.0's own hostile-fake test proves this path is
       reachable and correctly rejected, not just theoretically possible).
    6. Return r || s || v as the 65-byte result.
  GetPublicKey(ctx, keyID string) (compressed [33]byte, err error)
    - Thin wrapper, parses KMS's own DER-encoded SubjectPublicKeyInfo into
      the compressed point form the rest of this module works with.

ACCEPTANCE
- Against FakeKMSClient: Sign's own output independently verifies (a
  standard secp256k1 ecrecover-style check, not the wrapper's own
  internals) against the original digest and the expected public key, for
  many (digest, keyID) pairs including ones exercising both v=0 and v=1 in
  practice, not just by construction.
- FakeKMSClient configured to return a signature over the WRONG digest:
  Sign returns ErrSignatureDoesNotMatchKey, never a signature that looks
  valid but isn't for what was asked.
- A non-canonical (high-s) DER signature from the fake is normalized
  before returning -- test asserts the returned s is always <= curve
  order / 2.
- Malformed DER from the fake: a typed, wrapped parse error, never a
  panic.
```

---

## S1.2 — Slot key registry

```
BUILD internal/slots
  TABLE s1_slot_keys
    slot_id       smallint primary key       -- 1-6, matches C5's own slot ids
    kms_key_id    text not null unique
    tron_address  text not null unique
    public_key    bytea not null              -- compressed, cached from GetPublicKey
    status        text not null               -- ACTIVE, RETIRED
    created_at    timestamptz not null default now()
    retired_at    timestamptz

  Register(ctx, slotID int, kmsKeyID string) (SlotKey, error)
    - Calls kmssign.GetPublicKey, derives the TRON address from it
      (Keccak256 of the uncompressed point, TRON's 0x41 address-version
      byte, base58check) -- this is the ONE place in this module a TRON
      address is computed from a public key; C5's own slot registry gets
      told the resulting address, never derives it independently, so the
      two registries cannot silently disagree.
    - Rejects a duplicate slot_id or kms_key_id -- one key per slot,
      permanently, for as long as that row is ACTIVE (see the
      architecture doc's own "rotating a slot means retiring it," not
      swapping the key under a live address).
  Retire(ctx, slotID int) error
    - One-way ACTIVE -> RETIRED, enforced at the DB level, same
      defense-in-depth instinct as C2.1's address lifecycle and C5's own
      proposed slot-status transition.
  Get(ctx, slotID int) (SlotKey, error)

ACCEPTANCE
- Register against FakeKMSClient: the derived tron_address is
  deterministic for a given (seed, keyID) and matches an independently
  computed value in the test (computed via a different code path than
  Register's own, e.g. a small test-only reference implementation, so
  the test isn't just checking Register agrees with itself).
- Duplicate slot_id or kms_key_id: rejected, no row written.
- Retire is one-way -- a second Retire on an already-RETIRED slot is a
  no-op or a clear error (pick one, document it, test it), never
  silently re-activates anything.
```

---

## S1.3 — Auto-sign path

```
Wire RequestSignature's own under-threshold path for real: this is the
common case (per the architecture doc's own $10,000 default threshold
against a $3,000 average ticket) and must be a single round trip.

BUILD internal/requests (extend)
  Config { ApprovalThresholdUSD float64 }  -- config, not a constant, per
    the architecture doc's own "What's actually settled here"
  RequestSignature(ctx, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error)
    - Idempotent insert into signing_requests (invariant 2) -- a
      duplicate idempotencyKey returns the existing row's current state,
      never re-signs, never errors.
    - estimatedUSD < cfg.ApprovalThresholdUSD: calls slots.Get for the
      slot's own kms_key_id/public_key, then kmssign.Sign, records the
      audit row (invariant 4) and the signed_tx, flips status to SIGNED,
      all inside one transaction with the KMS call -- a failed Sign
      leaves the request PENDING for a caller-driven retry (RequestSignature
      is safe to call again with the same idempotencyKey), never silently
      drops it.
    - estimatedUSD >= threshold: inserts PENDING, does not call KMS at
      all (S1.4's job).
  GetSignature(ctx, requestID int64) (SigningRequest, error)
    - Plain read -- SIGNED/REJECTED are terminal and returned as-is
      (invariant 5); PENDING is returned as-is too, no side effects.

ACCEPTANCE
- Under threshold: RequestSignature returns SIGNED synchronously, with a
  signedTx that independently verifies against the slot's own known
  public key.
- At/above threshold: returns PENDING, zero calls made to KMSClient.Sign
  (assert via a call-counting FakeKMSClient, same "assert by call count,
  not just outcome" discipline C4's own MockProvider tests use).
- Idempotent replay of an already-SIGNED request's own idempotencyKey:
  returns the same signed_tx, zero additional KMS calls.
- A concurrent-call test (two goroutines, same idempotencyKey, under
  threshold): exactly one KMS Sign call happens, both callers see the
  same result -- mirrors C1.3's own concurrent-idempotency proof.
```

---

## S1.4 — The approval queue

```
BUILD internal/requests (extend)
  TABLE signing_approvals
    id                  bigserial primary key
    signing_request_id  bigint not null references signing_requests(id)
    approver            text not null
    decision            text not null  -- APPROVE, REJECT
    decided_at          timestamptz not null default now()
    UNIQUE (signing_request_id, approver)

  Approve(ctx, requestID int64, approver string) (SigningRequest, error)
    - Records one APPROVE decision (idempotent per (request, approver) --
      the UNIQUE constraint makes a repeated approve from the same actor
      a no-op read, not a second vote, not an error).
    - On the row reaching 2 distinct APPROVE decisions (invariant 3):
      inside one transaction, calls slots.Get + kmssign.Sign, records the
      audit row naming both approvers, flips to SIGNED. A KMS failure
      here rolls the whole transaction back -- the two approvals still
      stand (querying signing_approvals shows them), but status stays
      PENDING for a retry, matching S1.3's own "a failed Sign never spends
      the state that led to it" rule.
  Reject(ctx, requestID int64, approver string) (SigningRequest, error)
    - One REJECT from any configured approver -> REJECTED immediately,
      no vote-counting -- a veto, not a ballot (see the architecture
      doc's own note on why).
    - Idempotent: rejecting an already-REJECTED request is a no-op, not
      an error. Approving or rejecting an already-SIGNED or already-
      REJECTED request is a typed ErrRequestAlreadyResolved -- decisions
      after resolution are a caller bug worth surfacing loudly, not
      silently absorbing.

ACCEPTANCE
- One APPROVE: stays PENDING, zero KMS calls.
- Two distinct approvers' APPROVE: signs, exactly one KMS call (not one
  per approval), audit row names both approvers by their real actor
  strings.
- The same approver approving twice: still PENDING, still zero KMS
  calls -- the UNIQUE constraint is proven to actually prevent a
  double-count, not just assumed to.
- One REJECT after zero or one APPROVE: REJECTED, zero KMS calls ever,
  and a subsequent APPROVE attempt on the same request fails with
  ErrRequestAlreadyResolved.
- KMS configured to fail on the second approval: transaction rolls back,
  status stays PENDING, both approval rows still readable, a later
  successful retry (e.g. re-calling Approve with an already-recorded
  approver, or a third approver) completes the sign without needing a
  third real vote if 2 valid ones already exist and only the sign step
  itself needed retrying -- exact retry semantics here are this chunk's
  own design choice; whichever is picked must be tested explicitly, not
  left implicit.
```

---

## S1.5 — HTTP boundary

```
Expose SigningService to C5 (service-to-service, bearer auth) and the
approval actions to human approvers (bearer auth, actor-resolved from
token exactly like every other reviewer-facing endpoint in this project --
C3's holds release/reject is the direct precedent).

ROUTES
  POST /v1/signing-requests
    { "slot_id": int, "digest": "<hex, 32 bytes>", "estimated_usd": float,
      "idempotency_key": "..." }
    -> SigningRequest, 201 (or 200 if idempotent replay of an existing one)

  GET  /v1/signing-requests/{id}
    -> SigningRequest

  POST /v1/signing-requests/{id}/approve
  POST /v1/signing-requests/{id}/reject
    -- actor from bearer token, per-route auth scope distinct from the
       service-to-service token C5 uses (an approver token must not also
       be a valid C5-calling token, and vice versa -- two separate
       AuthConfig token sets, not one shared one, since these are
       different trust levels).

  GET  /v1/slots/{id}/address
    -> { "tron_address": "..." }  -- SlotAddress, over HTTP

  GET  /healthz, /readyz, /metrics -- same posture as every prior
    component: unauthenticated, operational.

ACCEPTANCE
- Full round trip against a real Postgres (testcontainers): an
  under-threshold request signs and is readable via GET immediately; an
  over-threshold request stays PENDING until two real HTTP approve calls
  from two distinct bearer tokens complete it.
- A C5-scoped token cannot call /approve or /reject (403, not a silent
  no-op) -- proves the two token sets are actually enforced separately,
  not just declared separately.
- OpenAPI spec covering every route above, matching every prior
  component's own "every route must appear in the spec" discipline.
```

---

## S1.6 — Replay / ship-gate harness

```
Mirrors C1.9/C2.10/C3.9/C4.9's own pattern: a scenario suite proving this
component's real invariants against a real Postgres, run before this is
trusted with anything resembling production configuration (even though
"production configuration" here still means FakeKMSClient until a real
cloud account is wired in -- see the architecture doc's own "Key
generation and bootstrapping," which stays a manual, human-run procedure
even after this chunk ships).

SCENARIOS (minimum set; extend if a real gap is found while building,
same posture every prior ship-gate harness took)
- Clean under-threshold majority: N requests, all auto-sign, all verify.
- Over-threshold, 2 real approvers, signs correctly.
- Over-threshold, 1 approver then a reject: REJECTED, never signs.
- Concurrent duplicate idempotency-key requests under threshold: exactly
  one KMS call, matching S1.3's own unit-level proof but now against a
  real Postgres and real HTTP, not just the package's own test harness.
- A KMS failure mid-approval (FakeKMSClient forced to error once): the
  request recovers on retry without losing either recorded approval.
- FINAL ASSERTIONS (mirroring C4.9's own shape): every SIGNED request has
  an audit row; every over-threshold SIGNED request's audit row names AT
  LEAST 2 real, distinct approvers, every one of them backed by an actual
  matching APPROVE decision (not exactly 2 -- a request that needed a
  retry after a failed sign attempt, per the KMS-failure scenario above,
  legitimately accumulates a 3rd distinct approver by the time it signs,
  and the audit log correctly names all real contributors rather than an
  artificially truncated 2 -- this was found by actually running the
  harness, not designed in up front); no signing_requests or audit row
  ever contains anything that looks like a private key or seed (a
  mechanical grep-style check against every text/bytea column, not just a
  code-level dependency
  test) -- the OUTPUT of this system getting invariant 1 right, checked
  independently of whether the code enforcing it is itself correct.

ACCEPTANCE
- All scenarios pass against real Postgres.
- Every final assertion passes.
- A deliberately-broken invariant (temporarily skip the 2-approver check
  in a throwaway commit) causes the relevant scenario to fail loudly --
  proves the harness would actually catch a real regression, not just
  pass by construction. Revert after confirming.
```
