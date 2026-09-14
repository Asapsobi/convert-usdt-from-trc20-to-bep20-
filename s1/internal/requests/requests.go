// Package requests is S1's own external contract: SigningService, the
// interface C5 calls. It supersedes the synchronous Sign() call
// c5-payout-dispatcher-build-prompts.md originally proposed -- see
// docs/02-architecture/s1-key-custody-architecture.md's own "The
// SigningService interface needs one real change: it can't stay
// synchronous" for why. A human-approval decision can take minutes to
// hours; blocking a single call on that is either a broadcast attempted
// against a half-finished flow or a spuriously failed dispatch attempt
// for an order that would have gone through fine five minutes later.
// This package reuses the shape energybroker's own reservations.Service
// already proved for exactly this problem: PENDING, polled until a
// terminal state.
package requests

import (
	"context"
	"errors"
	"time"
)

// Status is one SigningRequest's own lifecycle state.
type Status string

const (
	StatusPending  Status = "PENDING"
	StatusSigned   Status = "SIGNED"
	StatusRejected Status = "REJECTED"
)

// SigningRequest is RequestSignature's own result, and GetSignature's
// own read. SignedTx is nil until Status is SIGNED; once SIGNED or
// REJECTED, a request is terminal -- GetSignature never returns a
// different SignedTx for the same ID twice (invariant 5 in
// s1-key-management-build-prompts.md), the same "PENDING ->
// CONFIRMED|FAILED, never re-resolved" guarantee C4's own
// reservations.Reservation makes.
type SigningRequest struct {
	ID        int64
	Status    Status
	SignedTx  [65]byte // valid only when Status == StatusSigned
	CreatedAt time.Time
}

// PendingSummary is one row of ListPending's own result -- the ops
// console's own OC.7 (see docs/03-build/ops-console-build-prompts.md):
// enough for an approver to see which requests are waiting on them
// without already knowing the id, never the digest or anything
// signature-related, matching GetSignature's own posture of returning
// SignedTx only once SIGNED. Exactly one of SlotID/BSCDepositIndex is
// set, mirroring signing_requests' own CHECK constraint (migration
// 0005) -- a slot-key request or a BSC-deposit-sweep request.
type PendingSummary struct {
	ID              int64
	SlotID          *int
	BSCDepositIndex *uint32
	EstimatedUSD    float64
	CreatedAt       time.Time
}

// ErrRequestAlreadyResolved guards Approve/Reject against acting on a
// request that already reached a terminal state -- a caller bug worth
// surfacing loudly (a decision after resolution means something upstream
// is confused about what already happened), never silently absorbed.
var ErrRequestAlreadyResolved = errors.New("requests: signing request is already SIGNED or REJECTED")

// ErrRequestNotFound means no signing request exists with the given ID.
var ErrRequestNotFound = errors.New("requests: no such signing request")

// SigningService is the one thing C5 calls to get a transaction signed.
// See this package's own doc comment for why it's request/poll rather
// than a single synchronous call.
type SigningService interface {
	// RequestSignature starts a signing attempt for slotID over digest
	// (the SHA256 hash of the transaction's own serialized raw_data --
	// S1 never constructs or interprets transaction semantics, see
	// s1-key-management-build-prompts.md's own §0 "WHAT S1 IS NOT").
	// idempotencyKey makes a retried call safe: a repeated call with the
	// same key returns the existing request's current state, never
	// creates a second one, never re-signs. If estimatedUSD is under the
	// configured approval threshold, the returned SigningRequest may
	// already be SIGNED. Otherwise it is PENDING, and the caller polls
	// GetSignature.
	RequestSignature(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error)

	// RequestDepositSweepSignature is RequestSignature's own counterpart
	// for a per-order BSC deposit address at child index, rather than one
	// of the 6 fixed slots -- see internal/kmssign/bscdeposit.go's own
	// doc comment for why this needed a genuinely new signing capability,
	// not just a new caller of the existing one. Same idempotency,
	// approval-threshold, and PENDING/SIGNED semantics as RequestSignature
	// throughout; the only difference is which key signs.
	RequestDepositSweepSignature(ctx context.Context, index uint32, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error)

	// GetSignature polls one request's own current status by id.
	GetSignature(ctx context.Context, id int64) (SigningRequest, error)

	// SlotAddress resolves a slot id (1-6) to its real TRON address --
	// public information, not a private detail, so it's part of this
	// interface rather than something C5 has to hardcode or duplicate
	// from wherever S1 keeps it.
	SlotAddress(ctx context.Context, slotID int) (string, error)

	// EVMAddress resolves a slot id to its EVM-format (BSC included)
	// address -- the SAME key SlotAddress derives a TRON address from,
	// added for Model F's own BEP20->TRC20 relay direction (see
	// slots/evm.go's own doc comment). Public information, same posture
	// as SlotAddress.
	EVMAddress(ctx context.Context, slotID int) (string, error)
}
