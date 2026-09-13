package signing

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
)

// FakeSigningService is a deterministic, in-memory stand-in for a real
// S1 -- mirrors dispatcher/internal/signing's own FakeSigningService,
// used by relayd's own state-machine/orchestrate tests that care about
// forward-leg logic, not S1's own cryptography.
type FakeSigningService struct {
	mu                    sync.Mutex
	seq                   int64
	byID                  map[int64]*SigningRequest
	byIdemKey             map[string]int64
	addresses             map[int]string
	evmAddresses          map[int]string
	forceErr              map[string]error
	forceDuplicateOf      map[string]string
	requestSignatureCalls int64
}

// NewFakeSigningService returns an empty FakeSigningService -- every
// request signs immediately.
func NewFakeSigningService() *FakeSigningService {
	return &FakeSigningService{
		byID:             make(map[int64]*SigningRequest),
		byIdemKey:        make(map[string]int64),
		addresses:        make(map[int]string),
		evmAddresses:     make(map[int]string),
		forceErr:         make(map[string]error),
		forceDuplicateOf: make(map[string]string),
	}
}

// SetSlotAddress configures slotID to resolve to address.
func (f *FakeSigningService) SetSlotAddress(slotID int, address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses[slotID] = address
}

// SetEVMAddress configures slotID to resolve to address for EVMAddress.
func (f *FakeSigningService) SetEVMAddress(slotID int, address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evmAddresses[slotID] = address
}

// ForceError makes the next RequestSignature call for idempotencyKey
// fail with err.
func (f *FakeSigningService) ForceError(idempotencyKey string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceErr[idempotencyKey] = err
}

// ForceDuplicateSignature makes idempotencyKey's own signed_tx come out
// byte-identical to reuseIdempotencyKey's -- simulating a real vendor
// integrity failure.
func (f *FakeSigningService) ForceDuplicateSignature(idempotencyKey, reuseIdempotencyKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceDuplicateOf[idempotencyKey] = reuseIdempotencyKey
}

func fakeSignedTx(idempotencyKey string, digest [32]byte) [65]byte {
	h := sha256.Sum256(append([]byte("fake-signed-tx:"+idempotencyKey+":"), digest[:]...))
	var out [65]byte
	copy(out[:32], h[:])
	copy(out[32:64], h[:])
	out[64] = byte(len(idempotencyKey))
	return out
}

// RequestSignature implements the same shape as signing.Client's own
// method.
func (f *FakeSigningService) RequestSignature(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	if err := ctx.Err(); err != nil {
		return SigningRequest{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestSignatureCalls++

	if id, ok := f.byIdemKey[idempotencyKey]; ok {
		return *f.byID[id], nil
	}
	if err, ok := f.forceErr[idempotencyKey]; ok {
		return SigningRequest{}, err
	}

	f.seq++
	id := f.seq
	signedTx := fakeSignedTx(idempotencyKey, digest)
	if reuseKey, ok := f.forceDuplicateOf[idempotencyKey]; ok {
		if reuseID, ok := f.byIdemKey[reuseKey]; ok {
			signedTx = f.byID[reuseID].SignedTx
		}
	}

	req := &SigningRequest{ID: id, Status: StatusSigned, SignedTx: signedTx}
	f.byID[id] = req
	f.byIdemKey[idempotencyKey] = id
	return *req, nil
}

// GetSignature implements the same shape as signing.Client's own
// method.
func (f *FakeSigningService) GetSignature(ctx context.Context, id int64) (SigningRequest, error) {
	if err := ctx.Err(); err != nil {
		return SigningRequest{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.byID[id]
	if !ok {
		return SigningRequest{}, fmt.Errorf("signing: no such request %d", id)
	}
	return *req, nil
}

// RequestSignatureCallCount returns how many times RequestSignature has
// been called (regardless of idempotent-replay outcome).
func (f *FakeSigningService) RequestSignatureCallCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestSignatureCalls
}

// SlotAddress implements the same shape as signing.Client's own method.
func (f *FakeSigningService) SlotAddress(ctx context.Context, slotID int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.addresses[slotID]
	if !ok {
		return "", fmt.Errorf("signing: no address configured for slot %d", slotID)
	}
	return addr, nil
}

// EVMAddress implements the same shape as signing.Client's own method.
func (f *FakeSigningService) EVMAddress(ctx context.Context, slotID int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.evmAddresses[slotID]
	if !ok {
		return "", fmt.Errorf("signing: no EVM address configured for slot %d", slotID)
	}
	return addr, nil
}
