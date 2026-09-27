# S1 — Key management / signing: custody architecture

> **Design reference.** Describes a part that exists today, as designed at the time;
> later changes may not be reflected here. For the current system see
> [how-it-works.md](../how-it-works.md).

_v1.0, 8 Sep 2026. Closes the gap `component-map.md` names but never designs
("S1 Keys — required before C5 touches mainnet") and the one
`c5-payout-dispatcher-build-prompts.md`'s "Read this first" section explicitly
declines to close: "This document does not build S1... Whoever picks up S1
next should treat \[the proposed `SigningService` interface\] as a starting
proposal, not a settled contract." This is that pickup. Builds on decision 4
in `product-operations-architecture.md` (segregated slots) and does not
re-open it._

**Read this first if you only read one section:** this document proposes a
**self-hosted, cloud-KMS-backed** custody model with a **hybrid
automatic/human-approval** signing path. Both were direct decisions, not
defaults — see "Why this, not a custodian" and "Why hybrid, not fully
automatic" below. Every number in this document (the approval threshold, the
approver count) is config, not policy — see "What's actually settled here"
before treating any of them as final.

---

## Why this, not a custodian

A third-party MPC/custody vendor (Fireblocks, Copper, Coinbase Prime — the
obvious alternative) trades engineering effort for ongoing cost and a vendor
dependency. At this business's own numbers — $255k float, 6 slots capped at
$50k each, ~100 payouts/day, an $18k/month infra budget held flat through
month 24 (decision 9) — a custodian's typical pricing (basis points on
custodied assets, or a flat enterprise fee that assumes far higher volume)
is disproportionate to what's actually being protected: never more than
$300k in hot signing capacity at once (6 × $50k), by construction.

Self-hosted cloud KMS (AWS KMS or GCP Cloud KMS; this document uses AWS KMS
in examples, the choice is not load-bearing) gets most of the real security
property a custodian sells — **the private key never leaves a hardware
security boundary, ever, not even to the process that requests a
signature** — for a cost that's noise against the $18k/month stack: six
asymmetric KMS keys run roughly $6/month in AWS, plus fractions of a cent
per signing operation at this volume. What it does NOT get is a vendor's
own incident-response team, insurance, or audited operational controls —
see "Threat model" for what that actually costs this design, honestly.

## Why hybrid, not fully automatic

C5's own invariant 1 requires exactly-once, unattended broadcast to hit the
product's ~4m20s settlement SLA (decision 1) — pure human-in-the-loop
signing (every payout waits on a person) cannot meet that at any real
volume, and a small team can't staff a 24/7 approval queue for the common
case. But an always-on automated signer with no per-transaction check is
also the single highest-blast-radius shape in this entire system: component-
map rates S1 "High (terminal)" for exactly this reason — a compromised
automated signing path can drain a slot with nothing to stop it, unlike
every other "High" risk in this project (C1's invariants, C2's reorg
handling), which are correctness failures, not fund-loss failures.

The hybrid model bounds that blast radius without sacrificing the SLA on
the common case: a per-signing-request USD threshold (config — see below)
splits every request into two paths. **Under threshold: signs immediately,
same shape C5 already assumed.** **At or above threshold: blocks on a
recorded human approval before signing at all.** A Sweep-tier batch, being
several orders' value in one transaction, crosses the threshold more often
by construction — no special-casing by tier is needed, the dollar check
already captures it.

## What's actually settled here

Everything numbered below is this document's own proposal, config, or
recommendation — none of it is a business decision only the account owner
can make, and none of it should be read as more final than decision 4's own
$50k/slot cap was before it was chosen. Treat these as starting values to
confirm before S1.3 ships, not as settled the way C1's HTTP surface is:

- **Approval threshold:** starts at **$10,000** per signing request (config
  `S1_APPROVAL_THRESHOLD_USD`) — roughly 3x the modelled average ticket
  ($3,000), so the overwhelming majority of Direct/Standard payouts sign
  automatically, and any batch aggregating more than ~3 average orders
  needs a human. Revisit once real ticket-size distribution is observed
  (the same "assumption vs. observed" discipline `product-operations-
  architecture.md`'s own "what must be tested, and by when" table already
  applies everywhere else).
- **Approver count:** **2-of-N** for anything crossing the threshold, not
  1-of-N — a single approver on the one automated path with no other check
  in this system is a weaker bar than the segregated-slot design already
  applies to fund exposure elsewhere; two humans is achievable even for a
  2-person founding team (each approves the other's queue) and closes off
  "one compromised or coerced approver drains a slot" as a single point of
  failure. Revisit if this becomes an operational bottleneck at higher
  approval volume.
- **N (the approver roster):** config, mapped the same `token:actor` way
  `BROKER_API_TOKENS`/`SCREENING_LEDGER_TOKEN` already are elsewhere in
  this project — not designed further here.

## Key topology

**Six independent TRON slot keys, never a shared master.** Each of the 6
payout slots (decision 4) gets its own KMS asymmetric signing key
(`ECC_SECG_P256K1`, the curve TRON — like Bitcoin and Ethereum — uses),
generated inside KMS with no export capability, ever. This is deliberately
**not** an HD tree deriving all 6 from one seed: an HD tree means one
compromised parent secret exposes every slot at once, which is exactly the
concentration-risk decision 4 already rejected once for wallet structure
("a pooled freeze breaks the settlement guarantee") — applying the same
reasoning one layer down, to keys, is not a new decision, it's the same one
applied consistently. The cost of independence over derivation is
operational (6 keys to create, track, and eventually rotate instead of one
seed) and is negligible at this scale.

**The BSC HD seed stays cold, entirely offline, and out of S1's live scope
at MVP.** This is not a gap — it's already correct by construction: C2's
own `internal/addresses` package (`depositwatcher/internal/addresses/
bip32.go`, `derive.go`) was deliberately built to work from an **extended
PUBLIC key (xpub) alone** — `WATCHER_XPUB` — and is structurally incapable
of parsing or holding an extended private key (`ErrPrivateKeyMaterial`,
enforced by its own `no_signing_test.go`). Nothing in the currently-built
system ever needs to sign a BSC transaction: deposit addresses only ever
receive, and sweeping them (moving accumulated BSC-side value out) is S2's
job, explicitly deferred to Phase 2 ("Treasury rebalancing as a service...
a runbook plus a balance dashboard is correct until the 4-hour loop is
proven sustainable," `component-map.md`). **S1's only obligation for the
BSC seed at MVP is custody at rest, not signing capability**: generate it
once, offline, on an air-gapped machine; extract the xpub for `C2`'s
`WATCHER_XPUB`; put the seed itself in cold storage (e.g. a sealed,
duplicated physical backup, or a KMS key with no `Sign` grant to anything
— just `kms:Encrypt`/`Decrypt` under a break-glass process) that nothing
in the running system can reach. Bringing BSC signing online is S2's own,
separate, later decision — not proposed here.

## Signing mechanics: TRON over KMS

TRON's transaction signature is a 65-byte `(r, s, v)` triple over
`secp256k1` — the same shape Ethereum uses (TRON forked much of its
account/signature model from it), computed over the **SHA256** hash of the
transaction's serialized protobuf `raw_data` (TRON uses SHA256 here, not
Keccak, despite the EVM-compatible smart-contract layer using Keccak
elsewhere — verify this against the TRON protocol docs current at
implementation time, the same "verify before trusting a remembered spec"
discipline this project applies everywhere else, e.g. C2.2's "verify
go-ethereum's ethclient is still the right choice").

Cloud KMS's asymmetric `Sign` operation returns a **DER-encoded `(r, s)`**
signature and, critically, **no recovery id** — TRON (like Ethereum) needs
one to make the signature self-contained (so a node can recover the
signer's public key from `(hash, r, s, v)` without a separate "here's who
signed this" field). The signing wrapper (S1.1 below) must:

1. Call KMS `Sign` with the pre-computed SHA256 digest (`MessageType:
   DIGEST`, never let KMS hash arbitrary application data — see "Threat
   model" for why).
2. Parse the DER signature into raw `(r, s)`.
3. Normalize `s` to the curve's lower half if needed (both `s` and
   `curve_order - s` are valid for the same signature; TRON, like
   Ethereum post-EIP-2, expects the canonical low-`s` form — a
   non-normalized signature is either rejected by the network or, worse,
   creates transaction-malleability the exactly-once broadcast logic in
   C5 doesn't expect).
4. Recover the public key for both candidate recovery ids (0 and 1)
   against `(hash, r, s)` and compare each to the slot's own known public
   key (fetched once at key-creation time, see S1.2) to determine which
   one is correct — this "try both, keep the one that matches" step is
   the standard, well-established pattern for recovering `v` from a KMS
   or HSM signature that doesn't provide it natively; it is not a novel
   or fragile technique.
5. Serialize `r || s || v` as the 65-byte signature TRON's broadcast API
   expects.

None of this ever constructs, holds, or logs a private key or anything
derived from one — the wrapper's only inputs are a digest and a key
identifier (a KMS ARN), and its only output is a signature. This matches
`SigningService`'s own existing doc comment in the C5 build doc exactly
("It never returns key material... C5 holds no more capability after this
call than it did before it") and extends the same guarantee one level
down, into S1 itself.

## The `SigningService` interface needs one real change: it can't stay synchronous

The interface C5's own doc proposes:

```go
type SigningService interface {
    Sign(ctx context.Context, slotID int, unsignedTx []byte) (signedTx []byte, err error)
    SlotAddress(ctx context.Context, slotID int) (string, error)
}
```

...assumes `Sign` always completes within one call's own context deadline.
That's true for the auto-sign path (a KMS round-trip, tens of
milliseconds) and **false** for anything crossing the approval threshold —
a human approval can legitimately take minutes to hours, and blocking a
synchronous call (or a short `ctx` timeout) on that is either a broadcast
attempted against a half-finished flow or a spuriously failed dispatch
attempt for an order that would have gone through fine five minutes later.

This project has already solved exactly this shape of problem once —
C4's reservation fast path/slow path split (`PENDING` → `CONFIRMED` |
`FAILED`, polled via `GET /v1/reservations/{id}`) exists for precisely
this reason: some completions are fast and synchronous, some aren't, and
the caller needs a way to ask "is it done yet" without either blocking
forever or inventing new state. S1 reuses that shape rather than a new
one:

```go
type SigningService interface {
    // RequestSignature starts a signing attempt. If unsignedTx's own
    // estimated USD value is under the configured threshold, it may
    // already be SIGNED by the time this call returns. Otherwise it
    // returns PENDING, and the caller polls GetSignature.
    RequestSignature(ctx context.Context, slotID int, unsignedTx []byte, idempotencyKey string) (SigningRequest, error)

    // GetSignature polls one request's own current status. SIGNED is a
    // terminal, cacheable result (idempotent on requestID -- a repeated
    // poll after SIGNED returns the same signedTx every time, never
    // re-signs). REJECTED is also terminal (an approver declined it) --
    // distinct from an error, the same way C4's own FAILED reservation
    // status is distinct from a transport error.
    GetSignature(ctx context.Context, requestID int64) (SigningRequest, error)

    SlotAddress(ctx context.Context, slotID int) (string, error)
}

type SigningRequest struct {
    ID        int64
    Status    string // "PENDING" | "SIGNED" | "REJECTED"
    SignedTx  []byte // nil until SIGNED
    CreatedAt time.Time
}
```

C5's own dispatch state machine (C5.5, "broadcast and exactly-once retry
semantics") needs a small, honest addition to account for this: a dispatch
attempt waiting on approval is a real, visible state — not folded silently
into whatever "in flight" already meant before this document. This is a
genuine, small correction to the C5 build doc's own §0 and C5.5, flagged
here rather than silently worked around, the same "flag it, don't
silently absorb it" posture C4's own reservation design used when it found
Redelegate didn't match any real vendor's capability.

## The approval queue

Mirrors C3's own `holds` package almost exactly — same shape of problem
(something needs a human to look at it before the system proceeds), same
solution shape already proven in this codebase:

```
TABLE signing_requests
  id                bigserial primary key
  idempotency_key   text not null unique
  slot_id           smallint not null
  unsigned_tx       bytea not null
  estimated_usd     numeric not null
  status            text not null  -- PENDING, SIGNED, REJECTED
  signed_tx         bytea          -- null until SIGNED
  created_at        timestamptz not null default now()

TABLE signing_approvals
  id                  bigserial primary key
  signing_request_id  bigint not null references signing_requests(id)
  approver             text not null   -- actor, bearer-token-resolved, same as everywhere else
  decision             text not null   -- APPROVE, REJECT
  decided_at            timestamptz not null default now()
  UNIQUE (signing_request_id, approver)  -- one decision per approver per request
```

`RequestSignature` inserts one `signing_requests` row; under threshold, it
signs synchronously within the same call and writes `status = 'SIGNED'`
directly (no approval row ever created — the common case stays a single
round trip, not a queue detour). At or above threshold, it leaves `status
= 'PENDING'`. A human-facing `POST /v1/signing-requests/{id}/approve` (or
`/reject`) records one `signing_approvals` row per distinct approver
(idempotent — a repeated approve from the same actor is a no-op, not a
second vote); once 2 distinct `APPROVE` decisions exist, the request
signs for real (the actual KMS `Sign` call happens here, on the second
approval crossing the threshold, not before) and flips to `SIGNED`. One
`REJECT` from any configured approver flips it straight to `REJECTED` —
rejecting is a veto, not a vote that needs matching.

## Threat model

Honest about what this design does and does not defend against, the same
"Status of the numbers" discipline the rest of this repo applies to its
own assumptions:

**Defends against:**
- A compromised application server or a bug in the signing service's own
  code exfiltrating a private key — there is no key to exfiltrate; only
  KMS holds it, and KMS's own `Sign` API surface never returns key
  material under any call shape.
- A compromised database (this service's own Postgres) leaking keys — the
  DB never stores anything but public addresses, unsigned/signed
  transaction bytes, and approval metadata.
- An engineer with production deploy access but no KMS IAM grant signing
  arbitrary transactions — IAM policy on each key is scoped to exactly the
  signing service's own execution role, nothing broader (verify this is
  actually enforced as an S1.1 acceptance criterion, not just a stated
  intent).
- A single compromised or coerced approver moving a large payout alone —
  2-of-N closes this off for anything crossing the threshold.

**Does NOT defend against:**
- Compromise of the cloud account's own root/IAM-admin credentials — that
  identity can always re-grant itself `kms:Sign` on any key, or exfiltrate
  key material if the provider's own control plane is compromised (a
  different, larger trust boundary than this document's scope). Standard
  mitigation (hardware MFA on root, no standing root sessions, a break-
  glass process with its own audit trail) is an operational control this
  document recommends but does not build.
- Every approver colluding together, or all approver credentials
  compromised at once — 2-of-N raises the bar, it doesn't remove the
  possibility.
- A malicious or compromised transaction-construction step in C5 itself
  presenting a legitimate-looking unsigned tx that actually pays an
  attacker — S1 signs whatever valid, well-formed transaction it's asked
  to sign for a given slot; it has no way to independently verify the
  payout recipient matches the order C5 believes it's dispatching for.
  This is exactly why step 1 of the signing wrapper insists on signing a
  pre-computed digest, never letting KMS (or S1) construct or interpret
  transaction semantics — that boundary is deliberate, but it also means
  S1 is not, and cannot be, a check on C5's own correctness. C5's own
  invariants (screened orders only, amounts frozen from quote time) are
  what actually bound this risk, not S1.
- Physical or supply-chain compromise of the HSM hardware backing KMS
  itself — out of scope for a self-hosted-on-a-major-cloud design by
  definition; this is the exact tradeoff named in "Why this, not a
  custodian" above, made explicitly rather than left implicit.
- No independent security audit of this design or its implementation is
  proposed here. Recommended as a gate before this handles real mainnet
  value at any meaningful volume — flagged, not built, the same posture
  `c1-scenario-catalog.md` already takes toward several of its own
  irreducible-risk items.

## Key generation and bootstrapping (operational, not code)

This is a procedure a human runs once per key, with real cloud
credentials this document has no access to and should not attempt to
automate away entirely — the moment of key creation is exactly where a
scripting mistake is least recoverable:

1. Create each of the 6 KMS keys via infrastructure-as-code (not the
   console, so the creation is reviewable and repeatable) — asymmetric,
   `ECC_SECG_P256K1`, `SIGN_VERIFY` usage, key rotation disabled (KMS's
   automatic rotation doesn't apply to asymmetric keys and isn't wanted
   here regardless — a TRON slot address is tied to one specific public
   key permanently; "rotating" a slot means retiring it and activating a
   new one, per `component-map.md`'s own rotation concept, not rotating
   the key underneath a fixed address).
2. Fetch each key's public key via KMS `GetPublicKey` (a read of public
   material only — this call never touches the private side).
3. Derive the TRON address from that public key (Keccak256 of the
   uncompressed point, last 20 bytes, TRON's own `0x41` address-version
   prefix, base58check-encoded) — mirrors what `c5...`'s own slot registry
   (§B) already expects: `TABLE slots (id, tron_address, ...)`.
4. Register the (slot id, KMS key ARN, TRON address) mapping in S1's own
   store — the only place this mapping lives; C5's slot registry stores
   the address only, never the key reference, keeping the "no signing
   capability outside S1's own package" boundary real across module
   lines, not just within one.
5. Fund each new slot address with a small amount of TRX/energy and send
   a real, small testnet-then-mainnet transaction through the full S1 →
   C5 path before it ever carries production float — the same "prove it
   against something real before trusting it" discipline `docs/03-build/
   c2-deposit-watcher-build-prompts.md`'s own "verify before coding"
   callouts already establish for this project.

## What's deliberately not built here

- BSC sweep signing (S2, Phase 2 — see "Key topology" above).
- True HSM/on-prem key management, MPC/TSS, or a third-party custodian —
  see "Why this, not a custodian."
- A formal security audit or penetration test of this design or its
  implementation.
- Automatic key rotation on a schedule — slot retirement (already a
  `component-map.md` concept) is the rotation mechanism; a fixed-schedule
  rotation independent of slot lifecycle is not proposed.
- Multi-region KMS replication / disaster recovery for the signing
  service itself — a real operational gap at scale, explicitly out of
  scope for this document, same "not solved, not pretended to be solved"
  posture as C5's own dust-stranding note.

See `docs/03-build/s1-key-management-build-prompts.md` for the sequenced
build chunks this document is written to support.
