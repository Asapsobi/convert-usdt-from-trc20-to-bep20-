package requests

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// FakeSigningService is a deterministic, in-memory SigningService --
// C5's own future tests build against this, the same way this project's
// every other cross-component interface ships a fake alongside its real
// implementation (e.g. energybroker's own fake C4 HTTP server C5.2 is
// specified to use). Never used by S1's own tests, which exercise the
// real request/approval logic (internal requests.Service, S1.3/S1.4)
// directly.
type FakeSigningService struct {
	thresholdUSD float64

	mu           sync.Mutex
	seq          int64
	byID         map[int64]*SigningRequest
	byIdemKey    map[string]int64
	addresses    map[int]string
	evmAddresses map[int]string
}

// NewFakeSigningService returns a FakeSigningService that auto-signs any
// request with estimatedUSD strictly under thresholdUSD, and leaves
// everything else PENDING until ApproveFake/RejectFake is called.
func NewFakeSigningService(thresholdUSD float64) *FakeSigningService {
	return &FakeSigningService{
		thresholdUSD: thresholdUSD,
		byID:         make(map[int64]*SigningRequest),
		byIdemKey:    make(map[string]int64),
		addresses:    make(map[int]string),
		evmAddresses: make(map[int]string),
	}
}

// SetSlotAddress configures slotID to resolve to address -- a test fixture
// setter, since a real deployment's slot addresses come from S1.2's own
// registry, which this fake does not implement.
func (f *FakeSigningService) SetSlotAddress(slotID int, address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses[slotID] = address
}

// SetEVMAddress configures slotID to resolve to address for EVMAddress --
// mirrors SetSlotAddress's own identical test-fixture-setter role, for
// Model F's own BEP20->TRC20 relay direction.
func (f *FakeSigningService) SetEVMAddress(slotID int, address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evmAddresses[slotID] = address
}

func fakeSignedTx(slotID int, digest [32]byte) [65]byte {
	// Deterministic, distinguishable bytes -- NOT a real signature, and
	// never asserted to verify as one. This fake exists to let C5's own
	// code exercise the SigningService contract (idempotency, PENDING vs
	// SIGNED, polling), not to exercise S1's own cryptography -- that's
	// kmssign's own, separately-tested job.
	h := sha256.Sum256(append([]byte(fmt.Sprintf("fake-signed-tx:%d:", slotID)), digest[:]...))
	var out [65]byte
	copy(out[:32], h[:])
	copy(out[32:64], h[:])
	out[64] = byte(slotID)
	return out
}

// RequestSignature implements SigningService.
func (f *FakeSigningService) RequestSignature(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	if err := ctx.Err(); err != nil {
		return SigningRequest{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if id, ok := f.byIdemKey[idempotencyKey]; ok {
		return *f.byID[id], nil
	}

	f.seq++
	id := f.seq
	req := &SigningRequest{ID: id, Status: StatusPending, CreatedAt: time.Now().UTC()}
	if estimatedUSD < f.thresholdUSD {
		req.Status = StatusSigned
		req.SignedTx = fakeSignedTx(slotID, digest)
	}
	f.byID[id] = req
	f.byIdemKey[idempotencyKey] = id
	return *req, nil
}

// RequestDepositSweepSignature implements SigningService -- the fake's
// own counterpart to RequestSignature for a BSC deposit-address child
// index instead of a slot id, sharing the same id sequence and
// idempotency-key map (a real Store's own signing_requests table is
// similarly unified -- see migration 0005's own doc comment).
func (f *FakeSigningService) RequestDepositSweepSignature(ctx context.Context, index uint32, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	if err := ctx.Err(); err != nil {
		return SigningRequest{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if id, ok := f.byIdemKey[idempotencyKey]; ok {
		return *f.byID[id], nil
	}

	f.seq++
	id := f.seq
	req := &SigningRequest{ID: id, Status: StatusPending, CreatedAt: time.Now().UTC()}
	if estimatedUSD < f.thresholdUSD {
		req.Status = StatusSigned
		req.SignedTx = fakeSignedTx(int(index), digest)
	}
	f.byID[id] = req
	f.byIdemKey[idempotencyKey] = id
	return *req, nil
}

// GetSignature implements SigningService.
func (f *FakeSigningService) GetSignature(ctx context.Context, id int64) (SigningRequest, error) {
	if err := ctx.Err(); err != nil {
		return SigningRequest{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.byID[id]
	if !ok {
		return SigningRequest{}, ErrRequestNotFound
	}
	return *req, nil
}

// SlotAddress implements SigningService.
func (f *FakeSigningService) SlotAddress(ctx context.Context, slotID int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.addresses[slotID]
	if !ok {
		return "", fmt.Errorf("requests: no address configured for slot %d", slotID)
	}
	return addr, nil
}

// EVMAddress implements SigningService.
func (f *FakeSigningService) EVMAddress(ctx context.Context, slotID int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.evmAddresses[slotID]
	if !ok {
		return "", fmt.Errorf("requests: no EVM address configured for slot %d", slotID)
	}
	return addr, nil
}

// ApproveFake simulates a human approval completing a PENDING request --
// a test-only helper, not part of SigningService (a real caller approves
// through S1's own HTTP endpoint, S1.5, never through this interface).
func (f *FakeSigningService) ApproveFake(id int64, slotID int, digest [32]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.byID[id]
	if !ok {
		return ErrRequestNotFound
	}
	if req.Status != StatusPending {
		return ErrRequestAlreadyResolved
	}
	req.Status = StatusSigned
	req.SignedTx = fakeSignedTx(slotID, digest)
	return nil
}

// RejectFake simulates a human rejecting a PENDING request.
func (f *FakeSigningService) RejectFake(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.byID[id]
	if !ok {
		return ErrRequestNotFound
	}
	if req.Status != StatusPending {
		return ErrRequestAlreadyResolved
	}
	req.Status = StatusRejected
	return nil
}
