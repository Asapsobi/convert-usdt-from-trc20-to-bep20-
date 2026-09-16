package chain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// twoNodePool sets up two fake nodes, dials both with the real ethclient,
// and returns a ready Pool plus the fake nodes so a test can control what
// each one reports. Cleans up both HTTP servers via t.Cleanup.
func twoNodePool(t *testing.T, minAgreement int) (pool *Pool, a, b *fakeNode) {
	t.Helper()
	a, b = newFakeNode(), newFakeNode()
	clientA, srvA := a.client()
	clientB, srvB := b.client()
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)

	pool, err := NewPool([]Provider{
		{Name: "A", Client: clientA},
		{Name: "B", Client: clientB},
	}, Config{MinAgreement: minAgreement})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool, a, b
}

// threeNodePool mirrors twoNodePool exactly, for Step 1's own N-of-M
// quorum tests (a 3rd+ configured provider was previously silently
// ignored by LogsAt -- see logs.go's own doc comment on why that
// defeated the whole point of adding one).
func threeNodePool(t *testing.T, minAgreement int) (pool *Pool, a, b, c *fakeNode) {
	t.Helper()
	a, b, c = newFakeNode(), newFakeNode(), newFakeNode()
	clientA, srvA := a.client()
	clientB, srvB := b.client()
	clientC, srvC := c.client()
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)
	t.Cleanup(srvC.Close)

	pool, err := NewPool([]Provider{
		{Name: "A", Client: clientA},
		{Name: "B", Client: clientB},
		{Name: "C", Client: clientC},
	}, Config{MinAgreement: minAgreement})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool, a, b, c
}

func hash(b byte) common.Hash {
	var h common.Hash
	h[31] = b
	return h
}

// ---------------------------------------------------------------------
// NewPool
// ---------------------------------------------------------------------

func TestNewPool_RefusesFewerThanTwoProviders(t *testing.T) {
	node, srv := newFakeNode().client()
	defer srv.Close()

	_, err := NewPool([]Provider{{Name: "solo", Client: node}}, Config{})
	if !errors.Is(err, ErrTooFewProviders) {
		t.Fatalf("expected ErrTooFewProviders, got %v", err)
	}
	_, err = NewPool(nil, Config{})
	if !errors.Is(err, ErrTooFewProviders) {
		t.Fatalf("expected ErrTooFewProviders for nil providers, got %v", err)
	}
}

func TestNewPool_RejectsBadConfiguration(t *testing.T) {
	nodeA, srvA := newFakeNode().client()
	defer srvA.Close()
	nodeB, srvB := newFakeNode().client()
	defer srvB.Close()

	t.Run("empty name", func(t *testing.T) {
		_, err := NewPool([]Provider{{Name: "", Client: nodeA}, {Name: "B", Client: nodeB}}, Config{})
		if err == nil {
			t.Fatal("expected an error for an empty provider name")
		}
	})
	t.Run("nil client", func(t *testing.T) {
		_, err := NewPool([]Provider{{Name: "A", Client: nil}, {Name: "B", Client: nodeB}}, Config{})
		if err == nil {
			t.Fatal("expected an error for a nil client")
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		_, err := NewPool([]Provider{{Name: "same", Client: nodeA}, {Name: "same", Client: nodeB}}, Config{})
		if err == nil {
			t.Fatal("expected an error for duplicate provider names")
		}
	})
	t.Run("MinAgreement exceeds provider count", func(t *testing.T) {
		_, err := NewPool([]Provider{{Name: "A", Client: nodeA}, {Name: "B", Client: nodeB}}, Config{MinAgreement: 5})
		if err == nil {
			t.Fatal("expected an error when MinAgreement exceeds the number of providers")
		}
	})
}

// ---------------------------------------------------------------------
// LatestFinalized
// ---------------------------------------------------------------------

func TestLatestFinalized_TwoProvidersAgree_Succeeds(t *testing.T) {
	pool, a, b := twoNodePool(t, 2)
	a.setFinalized(1000, hash(1))
	b.setFinalized(1000, hash(1))

	height, agreedHash, err := pool.LatestFinalized(context.Background())
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if height != 1000 || agreedHash != hash(1) {
		t.Fatalf("got (%d, %s), want (1000, %s)", height, agreedHash, hash(1))
	}

	for _, h := range pool.ProviderHealthSnapshot() {
		if !h.Healthy || h.ConsecutiveFailures != 0 {
			t.Fatalf("provider %s should be healthy with a reset streak after agreement: %+v", h.Name, h)
		}
	}
}

func TestLatestFinalized_ProvidersDisagree_HardError(t *testing.T) {
	pool, a, b := twoNodePool(t, 2)
	a.setFinalized(1000, hash(1))
	b.setFinalized(1000, hash(2)) // same height, DIFFERENT hash

	_, _, err := pool.LatestFinalized(context.Background())
	if !errors.Is(err, ErrNoAgreement) {
		t.Fatalf("expected ErrNoAgreement (disagreement, not a silent pick), got %v", err)
	}

	// Both providers answered, but neither is in a group of >= 2 (they
	// disagree with each other), so both count as a failure this round.
	for _, h := range pool.ProviderHealthSnapshot() {
		if h.ConsecutiveFailures != 1 {
			t.Fatalf("provider %s: expected 1 consecutive failure after disagreement, got %d", h.Name, h.ConsecutiveFailures)
		}
	}
}

func TestLatestFinalized_ProviderErrors_ExcludedRoundFailsBelowMinAgreement(t *testing.T) {
	pool, a, b := twoNodePool(t, 2)
	a.setFinalized(1000, hash(1))
	b.setFinalizedError("boom")

	_, _, err := pool.LatestFinalized(context.Background())
	if !errors.Is(err, ErrNoAgreement) {
		t.Fatalf("expected ErrNoAgreement (only 1 of 2 required providers answered), got %v", err)
	}
}

func TestLatestFinalized_ProviderTimesOut_ExcludedRoundSucceedsIfEnoughRemain(t *testing.T) {
	// 3 providers, MinAgreement 2: one slow provider should be excluded by
	// the context deadline, and the round still succeeds on the other two.
	a, b, c := newFakeNode(), newFakeNode(), newFakeNode()
	clientA, srvA := a.client()
	clientB, srvB := b.client()
	clientC, srvC := c.client()
	defer srvA.Close()
	defer srvB.Close()
	defer srvC.Close()

	pool, err := NewPool([]Provider{
		{Name: "A", Client: clientA},
		{Name: "B", Client: clientB},
		{Name: "C", Client: clientC},
	}, Config{MinAgreement: 2})
	if err != nil {
		t.Fatal(err)
	}

	a.setFinalized(1000, hash(1))
	b.setFinalized(1000, hash(1))
	c.setFinalized(1000, hash(1))
	c.setFinalizedDelay(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	height, agreedHash, err := pool.LatestFinalized(ctx)
	if err != nil {
		t.Fatalf("expected success (2 of 3 is still >= MinAgreement), got %v", err)
	}
	if height != 1000 || agreedHash != hash(1) {
		t.Fatalf("got (%d, %s), want (1000, %s)", height, agreedHash, hash(1))
	}

	snap := healthByName(pool)
	if snap["C"].ConsecutiveFailures != 1 {
		t.Fatalf("expected C (the slow one) to have 1 consecutive failure, got %+v", snap["C"])
	}
	if snap["A"].ConsecutiveFailures != 0 || snap["B"].ConsecutiveFailures != 0 {
		t.Fatalf("expected A and B to be unaffected: %+v / %+v", snap["A"], snap["B"])
	}
}

func TestLatestFinalized_ProviderTimesOut_RoundFailsIfNotEnoughRemain(t *testing.T) {
	pool, a, b := twoNodePool(t, 2)
	a.setFinalized(1000, hash(1))
	b.setFinalized(1000, hash(1))
	b.setFinalizedDelay(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := pool.LatestFinalized(ctx)
	if !errors.Is(err, ErrNoAgreement) {
		t.Fatalf("expected ErrNoAgreement (only 1 of 2 required providers answered in time), got %v", err)
	}
}

func TestLatestFinalized_AmbiguousAgreement_HardError(t *testing.T) {
	// 4 providers, MinAgreement 2, split 2-2 on two different blocks --
	// neither group may be picked over the other.
	nodes := make([]*fakeNode, 4)
	providers := make([]Provider, 4)
	for i := range nodes {
		nodes[i] = newFakeNode()
		c, srv := nodes[i].client()
		t.Cleanup(srv.Close)
		providers[i] = Provider{Name: string(rune('A' + i)), Client: c}
	}
	nodes[0].setFinalized(1000, hash(1))
	nodes[1].setFinalized(1000, hash(1))
	nodes[2].setFinalized(1000, hash(2))
	nodes[3].setFinalized(1000, hash(2))

	pool, err := NewPool(providers, Config{MinAgreement: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = pool.LatestFinalized(context.Background())
	if !errors.Is(err, ErrAmbiguousAgreement) {
		t.Fatalf("expected ErrAmbiguousAgreement for an even 2-2 split, got %v", err)
	}
}

// ---------------------------------------------------------------------
// Provider health
// ---------------------------------------------------------------------

func TestProviderHealth_UnhealthyAfterConsecutiveFailures(t *testing.T) {
	a, b := newFakeNode(), newFakeNode()
	clientA, srvA := a.client()
	clientB, srvB := b.client()
	defer srvA.Close()
	defer srvB.Close()

	pool, err := NewPool([]Provider{{Name: "A", Client: clientA}, {Name: "B", Client: clientB}},
		Config{MinAgreement: 2, UnhealthyAfterConsecutiveFailures: 3})
	if err != nil {
		t.Fatal(err)
	}

	a.setFinalized(1000, hash(1))
	b.setFinalized(1000, hash(1))
	b.setFinalizedError("simulated failure")

	for i := 1; i <= 3; i++ {
		_, _, _ = pool.LatestFinalized(context.Background()) // expected to fail (only 1 of 2 agree)
		snap := healthByName(pool)
		wantHealthy := i < 3
		if snap["B"].Healthy != wantHealthy {
			t.Fatalf("round %d: B.Healthy = %v, want %v (consecutive failures = %d)",
				i, snap["B"].Healthy, wantHealthy, snap["B"].ConsecutiveFailures)
		}
	}

	// Recovering resets the streak and Healthy immediately.
	b.setFinalizedError("")
	_, _, err = pool.LatestFinalized(context.Background())
	if err != nil {
		t.Fatalf("expected recovery to succeed, got %v", err)
	}
	if snap := healthByName(pool); !snap["B"].Healthy || snap["B"].ConsecutiveFailures != 0 {
		t.Fatalf("expected B to recover to healthy with a reset streak: %+v", snap["B"])
	}
}

func healthByName(p *Pool) map[string]ProviderHealth {
	out := make(map[string]ProviderHealth)
	for _, h := range p.ProviderHealthSnapshot() {
		out[h.Name] = h
	}
	return out
}

// ---------------------------------------------------------------------
// LogsAt
// ---------------------------------------------------------------------

func sampleLog(blockNumber uint64, logIndex uint) types.Log {
	addr := common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
	return types.Log{
		Address:     addr,
		Topics:      []common.Hash{hash(0xAA)},
		Data:        []byte{1, 2, 3},
		BlockNumber: blockNumber,
		TxHash:      hash(0xBB),
		TxIndex:     0,
		BlockHash:   hash(0xCC), // deliberately allowed to differ between providers, see logEqual
		Index:       logIndex,
	}
}

func TestLogsAt_ProvidersAgree_Succeeds(t *testing.T) {
	pool, a, b := twoNodePool(t, 2)
	logs := []types.Log{sampleLog(100, 0), sampleLog(100, 1)}
	a.setLogs(logs)

	// Same logs, but a different BlockHash and reversed order -- both
	// deliberately, to prove logEqual ignores BlockHash and that
	// comparison is order-independent.
	reordered := []types.Log{sampleLog(100, 1), sampleLog(100, 0)}
	for i := range reordered {
		reordered[i].BlockHash = hash(0xDD)
	}
	b.setLogs(reordered)

	got, err := pool.LogsAt(context.Background(), 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(got))
	}
}

func TestLogsAt_ProvidersDisagree_HardError(t *testing.T) {
	pool, a, b := twoNodePool(t, 2)
	a.setLogs([]types.Log{sampleLog(100, 0)})
	b.setLogs([]types.Log{sampleLog(100, 0), sampleLog(100, 1)}) // an extra log

	_, err := pool.LogsAt(context.Background(), 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if !errors.Is(err, ErrLogMismatch) {
		t.Fatalf("expected ErrLogMismatch, got %v", err)
	}
}

func TestLogsAt_PrimaryErrors_HardError(t *testing.T) {
	pool, a, _ := twoNodePool(t, 2)
	a.setLogsError("primary is down")

	_, err := pool.LogsAt(context.Background(), 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if err == nil {
		t.Fatal("expected an error when the primary provider fails")
	}
}

// ---------------------------------------------------------------------
// LogsAt -- Step 1's own N-of-M quorum tests. Before this change, LogsAt
// only ever consulted providers[0]/providers[1] no matter how many were
// configured -- a 3rd provider (C, here) was silently never queried.
// ---------------------------------------------------------------------

// TestLogsAt_ThreeProviders_TwoAgreeOneDisagrees_SucceedsOnQuorum proves
// a 3rd configured provider now actually participates: with
// MinAgreement=2, A and B agreeing is enough even though C reports
// something different -- the exact liveness improvement Step 1 exists
// to deliver (a lagging/disagreeing single provider no longer blocks
// finalization outright when a quorum of the others agree).
func TestLogsAt_ThreeProviders_TwoAgreeOneDisagrees_SucceedsOnQuorum(t *testing.T) {
	pool, a, b, c := threeNodePool(t, 2)
	agreed := []types.Log{sampleLog(100, 0), sampleLog(100, 1)}
	a.setLogs(agreed)
	b.setLogs(agreed)
	c.setLogs([]types.Log{sampleLog(100, 0)}) // C is missing a log -- disagrees

	got, err := pool.LogsAt(context.Background(), 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if err != nil {
		t.Fatalf("expected success on a 2-of-3 quorum, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected the agreed 2-log result, got %d logs", len(got))
	}

	health := pool.ProviderHealthSnapshot()
	for _, h := range health {
		if h.Name == "C" && h.ConsecutiveFailures == 0 {
			t.Error("expected C's disagreement to be recorded as a failure, got 0 consecutive failures")
		}
		if (h.Name == "A" || h.Name == "B") && h.ConsecutiveFailures != 0 {
			t.Errorf("expected %s (in the winning group) to have 0 consecutive failures, got %d", h.Name, h.ConsecutiveFailures)
		}
	}
}

// TestLogsAt_ThreeProviders_AllPairwiseDisagree_HardError proves quorum
// requires an ACTUAL group of minAgreement, not just "more providers
// means more chances" -- three providers each reporting a distinct
// answer never reaches a quorum of 2, and must hard-fail exactly like
// the 2-provider case does.
func TestLogsAt_ThreeProviders_AllPairwiseDisagree_HardError(t *testing.T) {
	pool, a, b, c := threeNodePool(t, 2)
	a.setLogs([]types.Log{sampleLog(100, 0)})
	b.setLogs([]types.Log{sampleLog(100, 0), sampleLog(100, 1)})
	c.setLogs([]types.Log{sampleLog(100, 0), sampleLog(100, 1), sampleLog(100, 2)})

	_, err := pool.LogsAt(context.Background(), 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if !errors.Is(err, ErrLogMismatch) {
		t.Fatalf("expected ErrLogMismatch when no group reaches quorum, got %v", err)
	}
	if !errors.Is(err, ErrNoAgreement) {
		t.Fatalf("expected the wrapped error to also satisfy ErrNoAgreement, got %v", err)
	}
}

// TestLogsAt_ThreeProviders_TwoDistinctPairsBothReachQuorum_Ambiguous
// covers the tie case resolveAgreement shares with LatestFinalized: with
// MinAgreement=2 and 4 providers split 2-and-2 across two DIFFERENT
// answers, neither group is unambiguously the largest -- must hard-fail,
// never silently pick one side.
func TestLogsAt_ThreeProviders_TwoDistinctPairsBothReachQuorum_Ambiguous(t *testing.T) {
	d := newFakeNode()
	clientD, srvD := d.client()
	t.Cleanup(srvD.Close)

	pool, a, b, c := threeNodePool(t, 2)
	pool.providers = append(pool.providers, Provider{Name: "D", Client: clientD})
	pool.health["D"] = &health{}

	logsX := []types.Log{sampleLog(100, 0)}
	logsY := []types.Log{sampleLog(100, 0), sampleLog(100, 1)}
	a.setLogs(logsX)
	b.setLogs(logsX)
	c.setLogs(logsY)
	d.setLogs(logsY)

	_, err := pool.LogsAt(context.Background(), 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if !errors.Is(err, ErrAmbiguousAgreement) {
		t.Fatalf("expected ErrAmbiguousAgreement for a 2-vs-2 split on different answers, got %v", err)
	}
}
