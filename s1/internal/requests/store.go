package requests

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"s1/internal/db"
)

// Signer is the one call this package needs from internal/kmssign --
// kmssign.Wrapper's real implementation, or a fake for testing. Defined
// here (the consumer), matching this project's established convention.
type Signer interface {
	Sign(ctx context.Context, keyID string, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error)

	// SignBSCDeposit is Sign's own counterpart for a per-order BSC
	// deposit address at child index -- see
	// internal/kmssign/bscdeposit.go's own doc comment.
	SignBSCDeposit(ctx context.Context, index uint32, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error)

	// SignTronDeposit is SignBSCDeposit's own TRON-deposit counterpart --
	// see internal/kmssign/trondeposit.go's own doc comment.
	SignTronDeposit(ctx context.Context, index uint32, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error)
}

// DepositKeyGetter is the one call this package needs to resolve a BSC
// deposit-address child index to its own public key -- kmssign.Wrapper's
// own PublicKeyForDeposit, or a fake for testing.
type DepositKeyGetter interface {
	PublicKeyForDeposit(ctx context.Context, index uint32) ([33]byte, error)
}

// TronDepositKeyGetter is DepositKeyGetter's own TRON-deposit
// counterpart -- kmssign.Wrapper's own PublicKeyForTronDeposit, or a
// fake for testing.
type TronDepositKeyGetter interface {
	PublicKeyForTronDeposit(ctx context.Context, index uint32) ([33]byte, error)
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
	pool                   *db.Pool
	slots                  SlotKeyGetter
	depositKeys            DepositKeyGetter       // nil disables RequestDepositSweepSignature entirely
	tronDepositKeys        TronDepositKeyGetter   // nil disables RequestTronDepositSweepSignature entirely
	tronDepositProvisioner TronDepositProvisioner // nil disables ProvisionTronDepositKey entirely -- see provisioning.go
	bscDepositProvisioner  BSCDepositProvisioner  // nil disables ProvisionBSCDepositKey entirely -- see provisioning.go
	signer                 Signer
	cfg                    Config

	// requestLocks serializes calls for one idempotency key, so a replay
	// never signs while the first call's own signing is still under way:
	// exactly one signer call per request, each with its audit row.
	requestLocks keyedMutex
}

// keyedMutex is one mutex per key, created on demand and dropped once
// nobody holds or waits for it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sync.Mutex
	users int
}

// lock blocks until key is free and returns its unlock.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.users++
	k.mu.Unlock()

	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// NewStore wires a Store. depositKeys/tronDepositKeys/
// tronDepositProvisioner/bscDepositProvisioner may each independently be
// nil -- a deployment that never configures BSC (or TRON) deposit-sweep
// signing (S1_BSC_DEPOSIT_XPRV/XPUB, or S1_TRON_DEPOSIT_XPRV/XPUB,
// unset) gets ErrDepositSigningNotConfigured (or
// ErrTronDepositSigningNotConfigured) from the corresponding
// RequestDepositSweepSignature/RequestTronDepositSweepSignature call
// instead of a nil-pointer panic; a deployment that never configures
// Privy-backed TRON (or BSC) deposit-key provisioning (PRIVY_APP_ID/
// PRIVY_APP_SECRET unset) gets ErrTronDepositProvisioningNotConfigured
// (or ErrBSCDepositProvisioningNotConfigured) from
// ProvisionTronDepositKey (or ProvisionBSCDepositKey) the same way.
func NewStore(pool *db.Pool, slots SlotKeyGetter, depositKeys DepositKeyGetter, tronDepositKeys TronDepositKeyGetter,
	tronDepositProvisioner TronDepositProvisioner, bscDepositProvisioner BSCDepositProvisioner,
	signer Signer, cfg Config) *Store {
	return &Store{
		pool: pool, slots: slots, depositKeys: depositKeys, tronDepositKeys: tronDepositKeys,
		tronDepositProvisioner: tronDepositProvisioner, bscDepositProvisioner: bscDepositProvisioner,
		signer: signer, cfg: cfg,
	}
}

// ErrDepositSigningNotConfigured is RequestDepositSweepSignature's own
// result on a Store built with a nil DepositKeyGetter.
var ErrDepositSigningNotConfigured = errors.New("requests: BSC deposit-sweep signing is not configured on this S1 deployment")

// ErrTronDepositSigningNotConfigured is ErrDepositSigningNotConfigured's
// own TRON-deposit counterpart.
var ErrTronDepositSigningNotConfigured = errors.New("requests: TRON deposit-sweep signing is not configured on this S1 deployment")

// RequestSignature implements SigningService. See this package's own doc
// comment (requests.go) for the request/poll shape, and
// s1-key-management-build-prompts.md's own S1.3 for this method's exact
// acceptance criteria.
func (s *Store) RequestSignature(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	defer s.requestLocks.lock(idempotencyKey)()
	ref := keyRef{slotID: &slotID}
	req, created, err := s.insertPending(ctx, ref, digest, estimatedUSD, idempotencyKey)
	if err != nil {
		return SigningRequest{}, err
	}
	if !created {
		// Idempotent replay -- someone (possibly this exact call, racing
		// a concurrent duplicate) already owns this idempotency key.
		// Return its current state, finishing an auto-sign that failed
		// (resumeAutoSign); a recorded signature is never replaced
		// (invariant 2).
		return s.resumeAutoSign(ctx, req, ref, digest)
	}

	if estimatedUSD >= s.cfg.ApprovalThresholdUSD {
		return req, nil // PENDING -- S1.4's approval flow resolves it
	}

	signed, err := s.signAndRecord(ctx, req.ID, ref, digest, nil)
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

// RequestDepositSweepSignature implements SigningService. Identical
// shape to RequestSignature -- same idempotency, same approval-threshold
// auto-sign cutoff, same PENDING/SIGNED semantics -- routed to
// kmssign's own BSC-deposit derivation+signing instead of a fixed slot.
func (s *Store) RequestDepositSweepSignature(ctx context.Context, index uint32, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	defer s.requestLocks.lock(idempotencyKey)()
	if s.depositKeys == nil {
		return SigningRequest{}, ErrDepositSigningNotConfigured
	}
	ref := keyRef{bscDepositIndex: &index}
	req, created, err := s.insertPending(ctx, ref, digest, estimatedUSD, idempotencyKey)
	if err != nil {
		return SigningRequest{}, err
	}
	if !created {
		return s.resumeAutoSign(ctx, req, ref, digest)
	}

	if estimatedUSD >= s.cfg.ApprovalThresholdUSD {
		return req, nil
	}

	signed, err := s.signAndRecord(ctx, req.ID, ref, digest, nil)
	if err != nil {
		return SigningRequest{}, fmt.Errorf("requests: auto-sign for request %d: %w", req.ID, err)
	}
	return signed, nil
}

// RequestTronDepositSweepSignature implements SigningService.
// RequestDepositSweepSignature's own TRON-deposit counterpart -- identical
// shape, routed to kmssign's own TRON-deposit derivation+signing instead
// of the BSC one.
func (s *Store) RequestTronDepositSweepSignature(ctx context.Context, index uint32, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	defer s.requestLocks.lock(idempotencyKey)()
	if s.tronDepositKeys == nil {
		return SigningRequest{}, ErrTronDepositSigningNotConfigured
	}
	ref := keyRef{tronDepositIndex: &index}
	req, created, err := s.insertPending(ctx, ref, digest, estimatedUSD, idempotencyKey)
	if err != nil {
		return SigningRequest{}, err
	}
	if !created {
		return s.resumeAutoSign(ctx, req, ref, digest)
	}

	if estimatedUSD >= s.cfg.ApprovalThresholdUSD {
		return req, nil
	}

	signed, err := s.signAndRecord(ctx, req.ID, ref, digest, nil)
	if err != nil {
		return SigningRequest{}, fmt.Errorf("requests: auto-sign for request %d: %w", req.ID, err)
	}
	return signed, nil
}

// resumeAutoSign finishes an idempotent replay. A request below the
// approval threshold is signed the moment it is created -- but when that
// signing call fails (the signer briefly unreachable), the request stays
// PENDING, and with no approver ever asked to act on it, only a replay
// can sign it: the replay does, using the request's own stored digest
// and key. Signing the same digest again can only ever produce the same
// transaction, never a new one. A request waiting for approvers, or
// already SIGNED or REJECTED, is returned as it is.
func (s *Store) resumeAutoSign(ctx context.Context, req SigningRequest, ref keyRef, digest [32]byte) (SigningRequest, error) {
	if req.Status != StatusPending {
		return req, nil
	}
	detail, err := s.getPendingDetail(ctx, req.ID)
	if err != nil {
		return SigningRequest{}, err
	}
	if detail.status != StatusPending || detail.estimatedUSD >= s.cfg.ApprovalThresholdUSD {
		return req, nil
	}
	if detail.digest != digest || !sameKeyRef(detail.ref, ref) {
		return SigningRequest{}, fmt.Errorf("requests: idempotency key of request %d was reused for a different digest or key", req.ID)
	}
	signed, err := s.signAndRecord(ctx, req.ID, detail.ref, detail.digest, nil)
	if err != nil {
		return SigningRequest{}, fmt.Errorf("requests: retrying auto-sign for request %d: %w", req.ID, err)
	}
	return signed, nil
}

func sameKeyRef(a, b keyRef) bool {
	eqInt := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqIdx := func(x, y *uint32) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eqInt(a.slotID, b.slotID) && eqIdx(a.bscDepositIndex, b.bscDepositIndex) && eqIdx(a.tronDepositIndex, b.tronDepositIndex)
}

// keyRef names exactly one of a slot id, a BSC deposit child index, or a
// TRON deposit child index -- mirrors signing_requests' own CHECK
// constraint (migration 0006's num_nonnulls(...) = 1, superseding
// migration 0005's original two-way XOR) in Go, so insertPending/
// signAndRecord never have to juggle three separate parameter lists for
// what is otherwise identical logic.
type keyRef struct {
	slotID           *int
	bscDepositIndex  *uint32
	tronDepositIndex *uint32
}

// insertPending idempotently inserts a new PENDING row, or -- on a
// conflict, meaning this idempotencyKey already has a row, whether from
// an earlier call or a concurrent one that won the race -- fetches and
// returns the existing row instead. created is false in the latter case.
func (s *Store) insertPending(ctx context.Context, ref keyRef, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO signing_requests (idempotency_key, slot_id, bsc_deposit_index, tron_deposit_index, digest, estimated_usd, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, status, signed_tx, created_at
	`, idempotencyKey, ref.slotID, ref.bscDepositIndex, ref.tronDepositIndex, digest[:], estimatedUSD, string(StatusPending))

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
func (s *Store) signAndRecord(ctx context.Context, requestID int64, ref keyRef, digest [32]byte, approvers []string) (SigningRequest, error) {
	sig, err := s.signWith(ctx, ref, digest)
	if err != nil {
		return SigningRequest{}, err
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
			INSERT INTO signing_audit_log (signing_request_id, slot_id, bsc_deposit_index, tron_deposit_index, digest, approvers)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, requestID, ref.slotID, ref.bscDepositIndex, ref.tronDepositIndex, digest[:], approvers); err != nil {
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

// signWith resolves ref to a real signature over digest -- the slot path
// (KMS-mediated) or the BSC-deposit path (kmssign's own in-process
// derivation), depending on which half of ref is set.
func (s *Store) signWith(ctx context.Context, ref keyRef, digest [32]byte) ([65]byte, error) {
	switch {
	case ref.slotID != nil:
		key, err := s.slots.Get(ctx, *ref.slotID)
		if err != nil {
			return [65]byte{}, fmt.Errorf("looking up slot %d: %w", *ref.slotID, err)
		}
		sig, err := s.signer.Sign(ctx, key.KMSKeyID, digest, key.PublicKey)
		if err != nil {
			return [65]byte{}, fmt.Errorf("signing: %w", err)
		}
		return sig, nil
	case ref.bscDepositIndex != nil:
		if s.depositKeys == nil {
			return [65]byte{}, ErrDepositSigningNotConfigured
		}
		pub, err := s.depositKeys.PublicKeyForDeposit(ctx, *ref.bscDepositIndex)
		if err != nil {
			return [65]byte{}, fmt.Errorf("looking up public key for BSC deposit index %d: %w", *ref.bscDepositIndex, err)
		}
		sig, err := s.signer.SignBSCDeposit(ctx, *ref.bscDepositIndex, digest, pub)
		if err != nil {
			return [65]byte{}, fmt.Errorf("signing: %w", err)
		}
		return sig, nil
	case ref.tronDepositIndex != nil:
		if s.tronDepositKeys == nil {
			return [65]byte{}, ErrTronDepositSigningNotConfigured
		}
		pub, err := s.tronDepositKeys.PublicKeyForTronDeposit(ctx, *ref.tronDepositIndex)
		if err != nil {
			return [65]byte{}, fmt.Errorf("looking up public key for TRON deposit index %d: %w", *ref.tronDepositIndex, err)
		}
		sig, err := s.signer.SignTronDeposit(ctx, *ref.tronDepositIndex, digest, pub)
		if err != nil {
			return [65]byte{}, fmt.Errorf("signing: %w", err)
		}
		return sig, nil
	default:
		return [65]byte{}, fmt.Errorf("requests: signing request has neither a slot id nor a deposit index -- data corruption")
	}
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
		SELECT id, slot_id, bsc_deposit_index, tron_deposit_index, estimated_usd, created_at FROM signing_requests
		WHERE status = $1 ORDER BY created_at ASC
	`, string(StatusPending))
	if err != nil {
		return nil, fmt.Errorf("requests: listing pending: %w", err)
	}
	defer rows.Close()

	var out []PendingSummary
	for rows.Next() {
		var p PendingSummary
		var depositIndex, tronDepositIndex *int64
		if err := rows.Scan(&p.ID, &p.SlotID, &depositIndex, &tronDepositIndex, &p.EstimatedUSD, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("requests: scanning pending row: %w", err)
		}
		if depositIndex != nil {
			idx := uint32(*depositIndex)
			p.BSCDepositIndex = &idx
		}
		if tronDepositIndex != nil {
			idx := uint32(*tronDepositIndex)
			p.TronDepositIndex = &idx
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
	ref          keyRef
	digest       [32]byte
	status       Status
	estimatedUSD float64
}

func (s *Store) getPendingDetail(ctx context.Context, requestID int64) (pendingRequestDetail, error) {
	var d pendingRequestDetail
	var slotID *int
	var depositIndex, tronDepositIndex *int64
	var digest []byte
	var status string
	err := s.pool.QueryRow(ctx, `
		SELECT slot_id, bsc_deposit_index, tron_deposit_index, digest, status, estimated_usd FROM signing_requests WHERE id = $1
	`, requestID).Scan(&slotID, &depositIndex, &tronDepositIndex, &digest, &status, &d.estimatedUSD)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pendingRequestDetail{}, ErrRequestNotFound
		}
		return pendingRequestDetail{}, fmt.Errorf("requests: fetching request %d: %w", requestID, err)
	}
	d.ref.slotID = slotID
	if depositIndex != nil {
		idx := uint32(*depositIndex)
		d.ref.bscDepositIndex = &idx
	}
	if tronDepositIndex != nil {
		idx := uint32(*tronDepositIndex)
		d.ref.tronDepositIndex = &idx
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

	return s.signAndRecord(ctx, requestID, detail.ref, detail.digest, approvers)
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
