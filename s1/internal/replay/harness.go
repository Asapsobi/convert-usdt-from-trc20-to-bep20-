package replay

import (
	"context"
	"fmt"
	"math/rand"
	"sync"

	"s1/internal/db"
	"s1/internal/kmssign"
	"s1/internal/requests"
	"s1/internal/slots"
)

// harness bundles everything every scenario needs: a real Postgres-
// backed slots.Store and requests.Store around ONE shared
// kmssign.Wrapper/FakeKMSClient -- sharing is safe here (unlike C4's own
// harness, which gives every scenario a fresh MockProvider) because
// FakeKMSClient's every forceable behavior is keyed by keyID, and every
// scenario below gets its own freshly-registered slot with its own
// unique KMS key id, so no scenario's forced timeout/malformed/wrong-
// digest state can ever leak into another's.
type harness struct {
	ctx    context.Context
	pool   *db.Pool
	rng    *rand.Rand
	fake   *kmssign.FakeKMSClient
	signer *kmssign.Wrapper
	slots  *slots.Store

	mu      sync.Mutex
	slotSeq int
}

func newHarness(ctx context.Context, cfg Config, pool *db.Pool) *harness {
	fake := kmssign.NewFakeKMSClient(cfg.Seed)
	signer := kmssign.NewWrapper(fake)
	return &harness{
		ctx: ctx, pool: pool, rng: rand.New(rand.NewSource(cfg.Seed)),
		fake: fake, signer: signer, slots: slots.NewStore(pool, signer),
	}
}

// newSlot registers a fresh slot (a unique id and KMS key per call, so
// concurrent/adjacent scenarios never collide) and returns its id.
func (h *harness) newSlot(namePrefix string) (int, error) {
	h.mu.Lock()
	h.slotSeq++
	slotID := h.slotSeq
	h.mu.Unlock()

	kmsKeyID := fmt.Sprintf("%s-%d", namePrefix, slotID)
	if _, err := h.slots.Register(h.ctx, slotID, kmsKeyID); err != nil {
		return 0, fmt.Errorf("replay: registering slot for %s: %w", namePrefix, err)
	}
	return slotID, nil
}

// newStore builds a fresh requests.Store at thresholdUSD -- cheap
// (no owned state beyond its dependencies), so every scenario gets its
// own, letting each pick whatever threshold its own test needs without
// the others interfering.
func (h *harness) newStore(thresholdUSD float64) *requests.Store {
	return requests.NewStore(h.pool, slotKeyGetterAdapter{h.slots}, nil, h.signer, requests.Config{ApprovalThresholdUSD: thresholdUSD})
}

type slotKeyGetterAdapter struct{ store *slots.Store }

func (a slotKeyGetterAdapter) Get(ctx context.Context, slotID int) (requests.SlotKeyInfo, error) {
	key, err := a.store.Get(ctx, slotID)
	if err != nil {
		return requests.SlotKeyInfo{}, err
	}
	return requests.SlotKeyInfo{KMSKeyID: key.KMSKeyID, PublicKey: key.PublicKey, TronAddress: key.TronAddress}, nil
}

func (h *harness) nextDigest() [32]byte {
	var d [32]byte
	h.mu.Lock()
	_, _ = h.rng.Read(d[:])
	h.mu.Unlock()
	return d
}

func (h *harness) nextIdemKey(prefix string) string {
	h.mu.Lock()
	h.slotSeq++
	seq := h.slotSeq
	h.mu.Unlock()
	return fmt.Sprintf("replay-%s-%d", prefix, seq)
}
