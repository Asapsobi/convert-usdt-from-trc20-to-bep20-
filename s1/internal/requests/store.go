package requests

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"s1/internal/db"
)

// Signer is the one call this package needs from internal/kmssign --
// kmssign.Wrapper's real implementation, or a fake for testing. Defined
// here (the consumer), matching this project's established convention.
type Signer interface {
	Sign(ctx context.Context, keyID string, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error)
}

// SlotKeyInfo is the subset of internal/slots.SlotKey this package needs
// to sign for one slot and to answer SlotAddress -- a local type, not a
// direct dependency on the slots package's own struct, so this package's
// own interface (SlotKeyGetter) stays satisfiable by a simple adapter
// rather than coupling the two packages' internal shapes together.
type SlotKeyInfo struct {
	KMSKeyID    string
	PublicKey   [33]byte
	TronAddress string
	// EVMAddress is the SAME key's EVM-format (BSC included) address --
	// see s1/internal/slots/evm.go's own doc comment. Added for Model
	// F's own BEP20->TRC20 relay direction.
	EVMAddress string
}

// SlotKeyGetter is the one call this package needs from internal/slots.
type SlotKeyGetter interface {
	Get(ctx context.Context, slotID int) (SlotKeyInfo, error)
}

// Config scopes this Store's own auto-sign/approval split.
type Config struct {
	// ApprovalThresholdUSD is RequestSignature's own auto-sign cutoff --
	// see s1-key-custody-architecture.md's own "What's actually settled
	// here" for why this is config, not a constant, and its own starting
	// value ($10,000).
	ApprovalThresholdUSD float64
}

// Store is this package's own real SigningService implementation.
type Store struct {
	pool   *db.Pool
	slots  SlotKeyGetter
	signer Signer
	cfg    Config
}

// NewStore wires a Store.
func NewStore(pool *db.Pool, slots SlotKeyGetter, signer Signer, cfg Config) *Store {
	return &Store{pool: pool, slots: slots, signer: signer, cfg: cfg}
}

// RequestSignature implements SigningService. See this package's own doc
// comment (requests.go) for the request/poll shape, and
// s1-key-management-build-prompts.md's own S1.3 for this method's exact
// acceptance criteria.
func (s *Store) RequestSignature(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	req, created, err := s.insertPending(ctx, slotID, digest, estimatedUSD, idempotencyKey)
	if err != nil {
		return SigningRequest{}, err
	}
	if !created {
		// Idempotent replay -- someone (possibly this exact call, racing
		// a concurrent duplicate) already owns this idempotency key.
		// Return its current state as-is; never sign a second time for
		// it (invariant 2).
		return req, nil
	}

	if estimatedUSD >= s.cfg.ApprovalThresholdUSD {
		return req, nil // PENDING -- S1.4's approval flow resolves it
	}

	signed, err := s.signAndRecord(ctx, req.ID, slotID, digest, nil)
	if err != nil {
		// A failed Sign leaves the request PENDING for a caller-driven
		// retry (a repeat RequestSignature call with the same
		// idempotencyKey) -- it never silently drops the request, and
		// never marks it SIGNED on anything less than a real,
		// independently-verified signature.
		return SigningRequest{}, fmt.Errorf("requests: auto-sign for request %d: %w", req.ID, err)
	}
	return signed, nil
}

// insertPending idempotently inserts a new PENDING row, or -- on a
// conflict, meaning this idempotencyKey already has a row, whether from
// an earlier call or a concurrent one that won the race -- fetches and
// returns the existing row instead. created is false in the latter case.
func (s *Store) insertPending(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO signing_requests (idempotency_key, slot_id, digest, estimated_usd, status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, status, signed_tx, created_at
	`, idempotencyKey, slotID, digest[:], estimatedUSD, string(StatusPending))

	req, err := scanRequest(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := s.getByIdempotencyKey(ctx, idempotencyKey)
			if getErr != nil {
				return SigningRequest{}, false, getErr
			}
			return existing, false, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			existing, getErr := s.getByIdempotencyKey(ctx, idempotencyKey)
			if getErr != nil {
				return SigningRequest{}, false, getErr
			}
			return existing, false, nil
		}
		return SigningRequest{}, false, fmt.Errorf("requests: inserting signing request: %w", err)
	}
	return req, true, nil
}

func (s *Store) getByIdempotencyKey(ctx context.Context, idempotencyKey string) (SigningRequest, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, status, signed_tx, created_at FROM signing_requests WHERE idempotency_key = $1
	`, idempotencyKey)
	return scanRequest(row)
}

// signAndRecord calls the real signer for one request and, only on
// success, atomically flips it to SIGNED and writes its audit row --
// the KMS call itself happens OUTSIDE any open database transaction
// (an external network call has no business holding a DB lock), so a
// slow or hung Sign call never blocks unrelated readers/writers; only
// the two writes that follow a SUCCESSFUL Sign are transactional
// together, per invariant 4's own "every KMS Sign call is logged"
// requirement -- a signed_tx with no matching audit row (or vice versa)
// must never be possible to observe.
func (s *Store) signAndRecord(ctx context.Context, requestID int64, slotID int, digest [32]byte, approvers []string) (SigningRequest, error) {
	key, err := s.slots.Get(ctx, slotID)
	if err != nil {
		return SigningRequest{}, fmt.Errorf("looking up slot %d: %w", slotID, err)
	}

	sig, err := s.signer.Sign(ctx, key.KMSKeyID, digest, key.PublicKey)
	if err != nil {
		return SigningRequest{}, fmt.Errorf("signing: %w", err)
	}

	var alreadyDone bool
	err = db.Tx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		// Guarded on status = PENDING so two concurrent callers that both
		// reached "this can sign now" (S1.4's own two-approvals race,
		// concretely) never both write a SIGNED row or both insert an
		// audit entry -- whichever commits first wins; the loser sees
		// zero rows affected and simply reports the winner's already-
		// SIGNED result instead of erroring or double-recording.
		tag, err := tx.Exec(ctx, `
			UPDATE signing_requests SET status = $1, signed_tx = $2 WHERE id = $3 AND status = $4
		`, string(StatusSigned), sig[:], requestID, string(StatusPending))
		if err != nil {
			return fmt.Errorf("marking request %d signed: %w", requestID, err)
		}
		if tag.RowsAffected() == 0 {
			alreadyDone = true
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO signing_audit_log (signing_request_id, slot_id, digest, approvers)
			VALUES ($1, $2, $3, $4)
		`, requestID, slotID, digest[:], approvers); err != nil {
			return fmt.Errorf("recording audit log for request %d: %w", requestID, err)
		}
		return nil
	})
	if err != nil {
		return SigningRequest{}, err
	}
	if alreadyDone {
		return s.GetSignature(ctx, requestID)
	}

	return SigningRequest{ID: requestID, Status: StatusSigned, SignedTx: sig}, nil
}

// GetSignature implements SigningService.
// ListPending lists every PENDING signing request, oldest first -- the
// one query GetSignature (get-by-id-only) never let an approver make: an
// approver has no way to discover WHICH requests are awaiting them short
// of already knowing the id, unless the id came from somewhere else
// entirely (dispatcher logs/DB). Oldest first, not newest, because the
// operational question this answers is "what's been waiting longest,"
// not "what just came in."
func (s *Store) ListPending(ctx context.Context) ([]PendingSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, slot_id, estimated_usd, created_at FROM signing_requests
		WHERE status = $1 ORDER BY created_at ASC
	`, string(StatusPending))
	if err != nil {
		return nil, fmt.Errorf("requests: listing pending: %w", err)
	}
	defer rows.Close()

	var out []PendingSummary
	for rows.Next() {
		var p PendingSummary
		if err := rows.Scan(&p.ID, &p.SlotID, &p.EstimatedUSD, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("requests: scanning pending row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("requests: listing pending: %w", err)
	}
	return out, nil
}

func (s *Store) GetSignature(ctx context.Context, id int64) (SigningRequest, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, status, signed_tx, created_at FROM signing_requests WHERE id = $1
	`, id)
	req, err := scanRequest(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SigningRequest{}, ErrRequestNotFound
		}
		return SigningRequest{}, fmt.Errorf("requests: fetching request %d: %w", id, err)
	}
	return req, nil
}

// SlotAddress implements SigningService.
func (s *Store) SlotAddress(ctx context.Context, slotID int) (string, error) {
	key, err := s.slots.Get(ctx, slotID)
	if err != nil {
		return "", fmt.Errorf("requests: resolving address for slot %d: %w", slotID, err)
	}
	return key.TronAddress, nil
}

// EVMAddress implements SigningService.
func (s *Store) EVMAddress(ctx context.Context, slotID int) (string, error) {
	key, err := s.slots.Get(ctx, slotID)
	if err != nil {
		return "", fmt.Errorf("requests: resolving EVM address for slot %d: %w", slotID, err)
	}
	return key.EVMAddress, nil
}

// requiredApprovals is the 2-of-N default s1-key-custody-architecture.md
// recommends -- config in the sense that a future chunk could expose it,
// but not exposed as one yet since nothing in this project has asked for
// a different value.
const requiredApprovals = 2

type pendingRequestDetail struct {
	slotID int
	digest [32]byte
	status Status
}

func (s *Store) getPendingDetail(ctx context.Context, requestID int64) (pendingRequestDetail, error) {
	var d pendingRequestDetail
	var digest []byte
	var status string
	err := s.pool.QueryRow(ctx, `
		SELECT slot_id, digest, status FROM signing_requests WHERE id = $1
	`, requestID).Scan(&d.slotID, &digest, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pendingRequestDetail{}, ErrRequestNotFound
		}
		return pendingRequestDetail{}, fmt.Errorf("requests: fetching request %d: %w", requestID, err)
	}
	d.status = Status(status)
	copy(d.digest[:], digest)
	return d, nil
}

// Approve records one APPROVE decision from approver for requestID --
// idempotent per (requestID, approver): a repeated approve from the same
// actor is a no-op read, never a second vote (signing_approvals' own
// UNIQUE constraint). Once requiredApprovals distinct approvers have
// approved, this call itself performs the real sign (invariant 3: never
// on fewer than 2 distinct decisions, checked at the moment of the
// approval that crosses the threshold).
func (s *Store) Approve(ctx context.Context, requestID int64, approver string) (SigningRequest, error) {
	detail, err := s.getPendingDetail(ctx, requestID)
	if err != nil {
		return SigningRequest{}, err
	}
	if detail.status != StatusPending {
		return SigningRequest{}, ErrRequestAlreadyResolved
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO signing_approvals (signing_request_id, approver, decision)
		VALUES ($1, $2, 'APPROVE')
		ON CONFLICT (signing_request_id, approver) DO NOTHING
	`, requestID, approver); err != nil {
		return SigningRequest{}, fmt.Errorf("requests: recording approval for request %d: %w", requestID, err)
	}

	approvers, err := s.approversFor(ctx, requestID, "APPROVE")
	if err != nil {
		return SigningRequest{}, err
	}
	if len(approvers) < requiredApprovals {
		return s.GetSignature(ctx, requestID)
	}

	return s.signAndRecord(ctx, requestID, detail.slotID, detail.digest, approvers)
}

// Reject records a REJECT decision from approver -- a veto, not a vote:
// one REJECT from any configured approver resolves the request
// immediately, regardless of how many APPROVE decisions already exist.
// Idempotent: rejecting an already-REJECTED request is a no-op, not an
// error. Rejecting an already-SIGNED request is ErrRequestAlreadyResolved
// -- a decision after a real signature already exists is a caller bug
// worth surfacing loudly.
func (s *Store) Reject(ctx context.Context, requestID int64, approver string) (SigningRequest, error) {
	detail, err := s.getPendingDetail(ctx, requestID)
	if err != nil {
		return SigningRequest{}, err
	}
	if detail.status == StatusRejected {
		return s.GetSignature(ctx, requestID)
	}
	if detail.status != StatusPending {
		return SigningRequest{}, ErrRequestAlreadyResolved
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO signing_approvals (signing_request_id, approver, decision)
		VALUES ($1, $2, 'REJECT')
		ON CONFLICT (signing_request_id, approver) DO NOTHING
	`, requestID, approver); err != nil {
		return SigningRequest{}, fmt.Errorf("requests: recording rejection for request %d: %w", requestID, err)
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE signing_requests SET status = $1 WHERE id = $2 AND status = $3
	`, string(StatusRejected), requestID, string(StatusPending)); err != nil {
		return SigningRequest{}, fmt.Errorf("requests: rejecting request %d: %w", requestID, err)
	}
	return s.GetSignature(ctx, requestID)
}

func (s *Store) approversFor(ctx context.Context, requestID int64, decision string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT approver FROM signing_approvals WHERE signing_request_id = $1 AND decision = $2 ORDER BY approver
	`, requestID, decision)
	if err != nil {
		return nil, fmt.Errorf("requests: listing %s decisions for request %d: %w", decision, requestID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type scannable interface {
	Scan(dest ...any) error
}

func scanRequest(row scannable) (SigningRequest, error) {
	var req SigningRequest
	var status string
	var signedTx []byte
	if err := row.Scan(&req.ID, &status, &signedTx, &req.CreatedAt); err != nil {
		return SigningRequest{}, err
	}
	req.Status = Status(status)
	copy(req.SignedTx[:], signedTx)
	return req, nil
}
