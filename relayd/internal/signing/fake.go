package signing

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/crypto"
	tronaddress "github.com/fbsobreira/gotron-sdk/pkg/address"
)

// FakeSigningService is a deterministic, in-memory stand-in for a real
// S1 -- mirrors dispatcher/internal/signing's own FakeSigningService,
// used by relayd's own state-machine/orchestrate tests.
//
// It signs for real, with fixed test keys (one per slot, one per deposit
// index), so orchestrate's pre-broadcast signer check runs in these
// tests exactly as in production. A test's slot and deposit addresses
// must therefore be the ones these keys control: FakeSlotEVMAddress,
// FakeBSCDepositAddress, and friends.
type FakeSigningService struct {
	mu                                    sync.Mutex
	seq                                   int64
	byID                                  map[int64]*SigningRequest
	byIdemKey                             map[string]int64
	addresses                             map[int]string
	evmAddresses                          map[int]string
	forceErr                              map[string]error
	forceDuplicateOf                      map[string]string
	requestSignatureCalls                 int64
	requestDepositSweepSignatureCalls     int64
	requestTronDepositSweepSignatureCalls int64
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

// ForceError makes every signing request for idempotencyKey fail with
// err -- including relayd's digest-scoped keys built on it
// ("<idempotencyKey>:<digest>"), so a forced failure keeps failing for a
// transaction that gets rebuilt.
func (f *FakeSigningService) ForceError(idempotencyKey string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceErr[idempotencyKey] = err
}

// forcedError returns the error ForceError set for idempotencyKey, or for
// the key it was scoped from. Callers hold f.mu.
func (f *FakeSigningService) forcedError(idempotencyKey string) (error, bool) {
	if err, ok := f.forceErr[idempotencyKey]; ok {
		return err, true
	}
	if i := strings.LastIndex(idempotencyKey, ":"); i > 0 {
		if err, ok := f.forceErr[idempotencyKey[:i]]; ok {
			return err, true
		}
	}
	return nil, false
}

// ForceDuplicateSignature makes idempotencyKey's own signed_tx come out
// byte-identical to reuseIdempotencyKey's -- simulating a real vendor
// integrity failure.
func (f *FakeSigningService) ForceDuplicateSignature(idempotencyKey, reuseIdempotencyKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceDuplicateOf[idempotencyKey] = reuseIdempotencyKey
}

// fakeKey is the fixed test key for one (kind, n) -- a slot or a deposit
// index. Never used outside tests.
func fakeKey(kind string, n uint64) *ecdsa.PrivateKey {
	seed := sha256.Sum256([]byte(fmt.Sprintf("relayd-fake-signing-key:%s:%d", kind, n)))
	key, err := crypto.ToECDSA(seed[:])
	if err != nil {
		panic(fmt.Sprintf("signing: deriving fake %s key %d: %v", kind, n, err))
	}
	return key
}

func slotKey(slotID int) *ecdsa.PrivateKey          { return fakeKey("slot", uint64(slotID)) }
func bscDepositKey(index uint32) *ecdsa.PrivateKey  { return fakeKey("bsc-deposit", uint64(index)) }
func tronDepositKey(index uint32) *ecdsa.PrivateKey { return fakeKey("tron-deposit", uint64(index)) }
func evmAddress(key *ecdsa.PrivateKey) string       { return crypto.PubkeyToAddress(key.PublicKey).Hex() }
func tronAddress(key *ecdsa.PrivateKey) string {
	return tronaddress.PubkeyToAddress(key.PublicKey).String()
}

// FakeSlotEVMAddress is the BSC address slotID's fake key signs as.
func FakeSlotEVMAddress(slotID int) string { return evmAddress(slotKey(slotID)) }

// FakeSlotTronAddress is the TRON address slotID's fake key signs as.
func FakeSlotTronAddress(slotID int) string { return tronAddress(slotKey(slotID)) }

// FakeBSCDepositAddress is the BSC deposit address index's fake key
// controls.
func FakeBSCDepositAddress(index uint32) string { return evmAddress(bscDepositKey(index)) }

// FakeTronDepositAddress is the TRON deposit address index's fake key
// controls.
func FakeTronDepositAddress(index uint32) string { return tronAddress(tronDepositKey(index)) }

// signWith produces a real 65-byte r||s||v signature (v a raw 0/1
// recovery byte), the same shape the real S1 returns.
func signWith(key *ecdsa.PrivateKey, digest [32]byte) [65]byte {
	sig, err := crypto.Sign(digest[:], key)
	if err != nil {
		panic(fmt.Sprintf("signing: fake signature: %v", err))
	}
	var out [65]byte
	copy(out[:], sig)
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
	if err, ok := f.forcedError(idempotencyKey); ok {
		return SigningRequest{}, err
	}

	f.seq++
	id := f.seq
	signedTx := signWith(slotKey(slotID), digest)
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

// RequestDepositSweepSignature implements the same shape as
// signing.Client's own method -- shares byID/byIdemKey/forceErr/
// forceDuplicateOf with RequestSignature, mirroring the real S1's own
// dedup-by-idempotency-key behavior regardless of slot-vs-deposit ref
// (s1/internal/requests/store.go), with its own separate call counter.
// Signs with index's fixed BSC deposit key (FakeBSCDepositAddress).
func (f *FakeSigningService) RequestDepositSweepSignature(ctx context.Context, index uint32, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	if err := ctx.Err(); err != nil {
		return SigningRequest{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestDepositSweepSignatureCalls++

	if id, ok := f.byIdemKey[idempotencyKey]; ok {
		return *f.byID[id], nil
	}
	if err, ok := f.forcedError(idempotencyKey); ok {
		return SigningRequest{}, err
	}

	f.seq++
	id := f.seq
	signedTx := signWith(bscDepositKey(index), digest)
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

// RequestTronDepositSweepSignature implements the same shape as
// signing.Client's own method -- RequestDepositSweepSignature's own
// TRON-deposit counterpart, sharing byID/byIdemKey/forceErr/
// forceDuplicateOf, with its own separate call counter. Signs with
// index's fixed TRON deposit key (FakeTronDepositAddress).
func (f *FakeSigningService) RequestTronDepositSweepSignature(ctx context.Context, index uint32, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	if err := ctx.Err(); err != nil {
		return SigningRequest{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestTronDepositSweepSignatureCalls++

	if id, ok := f.byIdemKey[idempotencyKey]; ok {
		return *f.byID[id], nil
	}
	if err, ok := f.forcedError(idempotencyKey); ok {
		return SigningRequest{}, err
	}

	f.seq++
	id := f.seq
	signedTx := signWith(tronDepositKey(index), digest)
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

// RequestTronDepositSweepSignatureCallCount returns how many times
// RequestTronDepositSweepSignature has been called (regardless of
// idempotent-replay outcome) -- the counter TestFullHappyPath_TRC20ToBEP20
// now asserts against, since that leg type no longer calls
// RequestSignature at all.
func (f *FakeSigningService) RequestTronDepositSweepSignatureCallCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestTronDepositSweepSignatureCalls
}

// RequestDepositSweepSignatureCallCount returns how many times
// RequestDepositSweepSignature has been called (regardless of
// idempotent-replay outcome) -- the counter
// TestFullHappyPath_BEP20ToTRC20 now asserts against, since that leg
// type no longer calls RequestSignature at all.
func (f *FakeSigningService) RequestDepositSweepSignatureCallCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestDepositSweepSignatureCalls
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
