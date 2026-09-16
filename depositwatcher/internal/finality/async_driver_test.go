package finality_test

// Design B's own orchestration tests (Phase 2). Unlike this package's
// existing tests (finality_test.go's own real chain.Pool + real HTTP
// fakeNode), these drive finality.CheckFinalityAsync against a small,
// hand-written fake implementing finality.AsyncChainQuerier directly --
// no HTTP, no ethclient. That's a deliberate choice, not a shortcut:
// what's under test here is the ORCHESTRATION (sequencing, the
// monotonicity guard, candidate iteration, status handling, routing
// into the existing finalize/drop paths) -- internal/chain's own tests
// already prove the real wire-level RPC layer (FinalizedFrom/
// BlockHashAt/LogsAtFrom, chain/single_provider_test.go) works; nothing
// here needs to re-prove that over real HTTP too. The REAL, wire-level
// end-to-end seam (provider queries -> ProviderObservation -> async
// decision engine -> real ledgerclient.ReportDepositFinal) is proven
// separately, in async_integration_test.go, against a real chain.Pool
// and a real running ledgerd.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/money"
)

// fakeAsyncChainQuerier implements finality.AsyncChainQuerier entirely
// in memory -- per-provider finalized height/hash, per-provider
// per-height block hash, per-provider per-height logs, all
// independently settable and independently erroring, exactly mirroring
// what real, independently-operated RPC providers can do. jointLogsAt/
// jointLogsErr back the single LogsAt method (the shared,
// already-tested Design A primitive checkPostFinalReorgs itself calls)
// -- settable directly rather than re-implementing real N-of-M
// agreement here, since that algorithm is chain package's own, already
// proven (chain/agreement_test.go via pool_test.go), not something these
// tests exist to re-verify.
type fakeAsyncChainQuerier struct {
	mu sync.Mutex

	names []string

	finalizedHeight map[string]uint64
	finalizedErr    map[string]string

	blockHash    map[string]map[uint64]common.Hash
	blockHashErr map[string]string

	logsAt  map[string]map[uint64][]types.Log
	logsErr map[string]string

	jointLogsAt  map[uint64][]types.Log
	jointLogsErr string
}

func newFakeAsyncChainQuerier(names ...string) *fakeAsyncChainQuerier {
	return &fakeAsyncChainQuerier{
		names:           names,
		finalizedHeight: make(map[string]uint64),
		finalizedErr:    make(map[string]string),
		blockHash:       make(map[string]map[uint64]common.Hash),
		blockHashErr:    make(map[string]string),
		logsAt:          make(map[string]map[uint64][]types.Log),
		logsErr:         make(map[string]string),
		jointLogsAt:     make(map[uint64][]types.Log),
	}
}

func (f *fakeAsyncChainQuerier) ProviderNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

// setFinalized is the ONLY way a provider becomes eligible to check any
// candidate -- also registers the given block hash as what BlockHashAt
// returns for that SAME height from that SAME provider, unless
// overridden separately via setBlockHash, so a test that doesn't care
// about a hash mismatch doesn't have to set both every time.
func (f *fakeAsyncChainQuerier) setFinalized(name string, height uint64, h common.Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalizedHeight[name] = height
	if f.blockHash[name] == nil {
		f.blockHash[name] = make(map[uint64]common.Hash)
	}
	if _, ok := f.blockHash[name][height]; !ok {
		f.blockHash[name][height] = h
	}
}

func (f *fakeAsyncChainQuerier) setFinalizedErr(name, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalizedErr[name] = msg
}

func (f *fakeAsyncChainQuerier) clearFinalizedErr(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.finalizedErr, name)
}

func (f *fakeAsyncChainQuerier) setBlockHash(name string, height uint64, h common.Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blockHash[name] == nil {
		f.blockHash[name] = make(map[uint64]common.Hash)
	}
	f.blockHash[name][height] = h
}

// setLogsAt also folds logs into the joint jointLogsAt result for the
// same height (deduped by tx_hash/log_index) -- the joint LogsAt is
// what the EXISTING checkPostFinalReorgs (finality.go) re-verifies
// already-finalized candidates against every tick, and it should see
// the same on-chain reality every individual provider is independently
// reporting, unless a test deliberately calls setJointLogsAt afterward
// to simulate a genuine reorg (which overwrites, not merges).
func (f *fakeAsyncChainQuerier) setLogsAt(name string, height uint64, logs []types.Log) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logsAt[name] == nil {
		f.logsAt[name] = make(map[uint64][]types.Log)
	}
	f.logsAt[name][height] = logs
	for _, l := range logs {
		dup := false
		for _, existing := range f.jointLogsAt[height] {
			if existing.TxHash == l.TxHash && existing.Index == l.Index {
				dup = true
				break
			}
		}
		if !dup {
			f.jointLogsAt[height] = append(f.jointLogsAt[height], l)
		}
	}
}

func (f *fakeAsyncChainQuerier) setLogsErr(name, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsErr[name] = msg
}

func (f *fakeAsyncChainQuerier) clearLogsErr(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.logsErr, name)
}

func (f *fakeAsyncChainQuerier) setJointLogsAt(height uint64, logs []types.Log) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jointLogsAt[height] = logs
}

func (f *fakeAsyncChainQuerier) FinalizedFrom(_ context.Context, name string) (uint64, common.Hash, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg := f.finalizedErr[name]; msg != "" {
		return 0, common.Hash{}, errors.New(msg)
	}
	return f.finalizedHeight[name], common.Hash{}, nil
}

func (f *fakeAsyncChainQuerier) BlockHashAt(_ context.Context, name string, height uint64) (common.Hash, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg := f.blockHashErr[name]; msg != "" {
		return common.Hash{}, errors.New(msg)
	}
	h, ok := f.blockHash[name][height]
	if !ok {
		return common.Hash{}, fmt.Errorf("no block hash registered for provider %s at height %d", name, height)
	}
	return h, nil
}

func (f *fakeAsyncChainQuerier) LogsAtFrom(_ context.Context, name string, fromBlock, _ uint64, _ common.Address, _ [][]common.Hash) ([]types.Log, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg := f.logsErr[name]; msg != "" {
		return nil, errors.New(msg)
	}
	return f.logsAt[name][fromBlock], nil
}

func (f *fakeAsyncChainQuerier) LogsAt(_ context.Context, fromBlock, _ uint64, _ common.Address, _ [][]common.Hash) ([]types.Log, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jointLogsErr != "" {
		return nil, errors.New(f.jointLogsErr)
	}
	return f.jointLogsAt[fromBlock], nil
}

// rawOnChainAmount converts a ledger-facing money.Amount (6 decimals)
// into this token's real on-chain 18-decimal raw units, left-padded to
// the 32-byte word chain.ParseTransferLog expects as Data -- the exact
// inverse of money.FromOnChainUnits(raw, 18).
func rawOnChainAmount(amount money.Amount) []byte {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil) // 18 - money.Decimals(6)
	raw := new(big.Int).Mul(big.NewInt(int64(amount)), scale)
	buf := make([]byte, 32)
	raw.FillBytes(buf)
	return buf
}

var testFromAddr = common.HexToAddress("0xAE2166bd7901Ea67c1E2Bc4179418fC228108F0")
var testToAddr = common.HexToAddress("0x1111111111111111111111111111111111111a")

// hash builds a distinct common.Hash from a small int, for readable
// synthetic block hashes in these fixtures -- mirrors bigHash's own
// shape (finality_test.go), kept separate since block hashes and tx
// hashes are conceptually different axes in these tests even though
// both are just common.Hash values under the hood.
func hash(v int64) common.Hash { return common.BigToHash(big.NewInt(v)) }

// transferLog builds a real, chain.ParseTransferLog-decodable Transfer
// event log -- 3 topics (the real event signature + from/to, address-
// padded to 32 bytes each) and a 32-byte Data word, exactly what a real
// provider's own eth_getLogs would return for a real Transfer.
func transferLog(height uint64, txHash common.Hash, index uint, amount money.Amount) types.Log {
	return types.Log{
		Address: testContract,
		Topics: []common.Hash{
			testTopic,
			common.BytesToHash(testFromAddr.Bytes()),
			common.BytesToHash(testToAddr.Bytes()),
		},
		Data:        rawOnChainAmount(amount),
		BlockNumber: height,
		TxHash:      txHash,
		Index:       index,
	}
}

// malformedTransferLog builds a log that matches a candidate's own
// (tx_hash, log_index) identity -- exactly what checkOneCandidateAsync
// looks for -- but whose Data is the wrong length for
// chain.ParseTransferLog to decode (it requires exactly 32 bytes). This
// is what a corrupted/truncated real provider response looks like: the
// entry is found, but its content doesn't parse -- distinct from a
// genuinely empty logs slice (real absence) and from a well-formed
// present log.
func malformedTransferLog(height uint64, txHash common.Hash, index uint) types.Log {
	return types.Log{
		Address: testContract,
		Topics: []common.Hash{
			testTopic,
			common.BytesToHash(testFromAddr.Bytes()),
			common.BytesToHash(testToAddr.Bytes()),
		},
		Data:        []byte{0x01, 0x02, 0x03}, // not 32 bytes -- chain.ParseTransferLog must reject this
		BlockNumber: height,
		TxHash:      txHash,
		Index:       index,
	}
}

// countingFinalHandler counts OnFinal calls -- for the no-double-
// finalization tests below.
type countingFinalHandler struct {
	mu    sync.Mutex
	calls []finality.Candidate
}

func (h *countingFinalHandler) handle(_ context.Context, c finality.Candidate) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, c)
	return nil
}

func (h *countingFinalHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func newAsyncTracker(t *testing.T, minAgreement int, onFinal finality.FinalHandler) *finality.Tracker {
	t.Helper()
	tr, err := finality.New(finality.Config{
		ContractAddress:         testContract,
		TransferTopic:           testTopic,
		OnFinal:                 onFinal,
		ReorgReporter:           &fakeReorgReporter{},
		OrphanedDepositRecorder: &fakeOrphanedDepositRecorder{},
		AsyncMinAgreement:       minAgreement,
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	return tr
}

func track(t *testing.T, tr *finality.Tracker, height uint64, txHash common.Hash) {
	t.Helper()
	log := sampleObservedLog(height, txHash, 0)
	if err := tr.OnLogObserved(context.Background(), log, chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}
}

// ---------------------------------------------------------------------
// 2-of-3 quorum across different ticks.
// ---------------------------------------------------------------------

func TestAsyncDriver_TwoOfThree_AConfirmsTick1_BConfirmsTick3(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B", "C")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)

	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))
	blockHash := hash(0xAA)

	// Tick 1: only A has reached the height.
	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{log})
	// B and C: no finalized height set at all yet (stay at their zero
	// value, below the candidate's own height) -- realistic "hasn't
	// caught up yet".
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if final.count() != 0 {
		t.Fatalf("tick 1: OnFinal called %d times, want 0 (only 1 of 2 required providers)", final.count())
	}

	// Tick 2: still just A.
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if final.count() != 0 {
		t.Fatalf("tick 2: OnFinal called %d times, want 0", final.count())
	}

	// Tick 3: B finally catches up and agrees exactly with A.
	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, []types.Log{log})
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if final.count() != 1 {
		t.Fatalf("tick 3: OnFinal called %d times, want exactly 1", final.count())
	}
}

// ---------------------------------------------------------------------
// 1 stale provider + 2 matching providers.
// ---------------------------------------------------------------------

func TestAsyncDriver_OneStaleProviderNeverBlocksTwoMatchingOnes(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B", "stale")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{log})
	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, []types.Log{log})
	// "stale" never reaches the height at all -- its finalizedHeight
	// stays 0, forever, across every tick below.

	for i := 0; i < 3; i++ {
		if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if final.count() != 1 {
		t.Fatalf("OnFinal called %d times, want exactly 1 (2-of-3 quorum despite 1 permanently stale provider)", final.count())
	}
}

// ---------------------------------------------------------------------
// Same-height/different-hash and same-hash/different-facts -- terminal.
// ---------------------------------------------------------------------

func TestAsyncDriver_SameHeightDifferentHash_Terminal(t *testing.T) {
	final := &countingFinalHandler{}
	var disagreements []string
	tr, err := finality.New(finality.Config{
		ContractAddress: testContract, TransferTopic: testTopic,
		OnFinal: final.handle, ReorgReporter: &fakeReorgReporter{}, OrphanedDepositRecorder: &fakeOrphanedDepositRecorder{},
		AsyncMinAgreement: 2,
		OnDisagreement: func(c finality.Candidate, note string) {
			disagreements = append(disagreements, note)
		},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))

	q.setFinalized("A", 1000, hash(0xAA))
	q.setLogsAt("A", 1000, []types.Log{log})
	q.setFinalized("B", 1000, hash(0xBB)) // same height, DIFFERENT hash
	q.setLogsAt("B", 1000, []types.Log{log})

	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("CheckFinalityAsync: %v", err)
	}
	if final.count() != 0 {
		t.Fatalf("OnFinal called %d times, want 0 (must never finalize on a hash disagreement)", final.count())
	}
	if len(disagreements) != 1 {
		t.Fatalf("OnDisagreement fired %d times, want exactly 1", len(disagreements))
	}

	// A 3rd tick, nothing new -- must not retroactively finalize or
	// re-fire the alert.
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if final.count() != 0 || len(disagreements) != 1 {
		t.Fatalf("after a 2nd tick: OnFinal=%d, alerts=%d, want 0 and 1 (sticky, never re-fired)", final.count(), len(disagreements))
	}
}

func TestAsyncDriver_SameHashDifferentFacts_Terminal(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{transferLog(1000, txHash, 0, money.Amount(3000_000000))})
	q.setFinalized("B", 1000, blockHash)                                                        // same hash
	q.setLogsAt("B", 1000, []types.Log{transferLog(1000, txHash, 0, money.Amount(999_000000))}) // different amount

	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("CheckFinalityAsync: %v", err)
	}
	if final.count() != 0 {
		t.Fatalf("OnFinal called %d times, want 0 (must never finalize on a facts disagreement)", final.count())
	}
}

// ---------------------------------------------------------------------
// Same-hash PRESENT + ABSENT -- not a contradiction.
// ---------------------------------------------------------------------

func TestAsyncDriver_SameHashPresentAndAbsent_NotAContradiction(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B", "C")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{log})
	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, []types.Log{log})
	q.setFinalized("C", 1000, blockHash) // same hash as A/B
	q.setLogsAt("C", 1000, nil)          // C's own re-query finds nothing -- indexer lag, not a fork

	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("CheckFinalityAsync: %v", err)
	}
	if final.count() != 1 {
		t.Fatalf("OnFinal called %d times, want exactly 1 (A+B quorum must not be blocked by C's own absence)", final.count())
	}
}

// ---------------------------------------------------------------------
// RPC failure -> no vote; recovery -> confirms cleanly.
// ---------------------------------------------------------------------

func TestAsyncDriver_RPCFailureThenRecovery(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{log})
	q.setFinalized("B", 1000, blockHash)
	q.setLogsErr("B", "rate limited") // B reaches the height but its log query fails

	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if final.count() != 0 {
		t.Fatalf("tick 1: OnFinal called %d times, want 0 (B's failure must be no vote, not an absence)", final.count())
	}

	// B recovers.
	q.clearLogsErr("B")
	q.setLogsAt("B", 1000, []types.Log{log})
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if final.count() != 1 {
		t.Fatalf("tick 2: OnFinal called %d times, want exactly 1 (B's recovery counted cleanly, no penalty for the earlier failure)", final.count())
	}
}

// ---------------------------------------------------------------------
// Regression: a matching-but-unparseable log must never be treated as
// absence (security review finding #1). Before this fix,
// checkOneCandidateAsync fell through to obs.Present=false whenever a
// log matched the candidate's own (tx_hash, log_index) but failed
// chain.ParseTransferLog -- silently conflating "found something
// corrupted" with "genuinely looked and found nothing," letting a
// one-off decode glitch from a single provider count toward
// ASYNC_NOT_FOUND exactly like a real absence would.
// ---------------------------------------------------------------------

func TestAsyncDriver_MatchingLogFailsToParse_NoVoteNotAbsent(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	blockHash := hash(0xAA)

	// Tick 1: provider A finds a log matching this candidate's own
	// (tx_hash, log_index) exactly, but its Data fails to parse as a
	// Transfer (a corrupted/truncated response, not a real absence).
	// Provider B, independently, genuinely looked and found nothing.
	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{malformedTransferLog(1000, txHash, 0)})
	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, nil) // genuine absence

	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	// If A's parse failure had been recorded as an ABSENT vote, A+B
	// would already be 2 distinct absent confirmations -- enough to
	// reach ASYNC_NOT_FOUND (minAgreement=2) and drop the candidate
	// right here. It must NOT: A's failed observation must contribute
	// nothing at all, leaving only B's single genuine absent vote, one
	// short of quorum.
	if tr.PendingCount() != 1 {
		t.Fatalf("tick 1: PendingCount() = %d, want 1 (a matching-but-unparseable log must not count toward ASYNC_NOT_FOUND)", tr.PendingCount())
	}
	if final.count() != 0 {
		t.Fatalf("tick 1: OnFinal called %d times, want 0", final.count())
	}

	// Tick 2: provider A recovers with a clean, genuinely absent
	// observation (no matching log at all this time) -- its earlier
	// parse failure must not have poisoned or otherwise affected its
	// ability to vote normally now.
	q.setLogsAt("A", 1000, nil)
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if tr.PendingCount() != 0 {
		t.Fatalf("tick 2: PendingCount() = %d, want 0 (A's own recovered clean absence plus B's earlier one should now reach ASYNC_NOT_FOUND)", tr.PendingCount())
	}
	if final.count() != 0 {
		t.Fatalf("tick 2: OnFinal called %d times, want 0 (this candidate must be dropped via ASYNC_NOT_FOUND, never finalized)", final.count())
	}
}

// ---------------------------------------------------------------------
// Provider height regression.
// ---------------------------------------------------------------------

func TestAsyncDriver_ProviderHeightRegression_ExcludedThatTick(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{log})
	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, []types.Log{log})
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if final.count() != 1 {
		t.Fatalf("tick 1: OnFinal called %d times, want 1", final.count())
	}

	// A 2nd, INDEPENDENT candidate, to observe B's own regression
	// against without the first candidate already being finalized.
	txHash2 := bigHash(2)
	track(t, tr, 1000, txHash2)
	log2 := transferLog(1000, txHash2, 0, money.Amount(3000_000000))
	q.setLogsAt("A", 1000, []types.Log{log, log2})

	// B's own next reading regresses.
	q.setFinalized("B", 500, blockHash)
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if final.count() != 1 {
		t.Fatalf("tick 2: OnFinal called %d times, want still 1 (B's regressed reading must be excluded, not trusted)", final.count())
	}
}

// ---------------------------------------------------------------------
// Candidate reorg before quorum -> ASYNC_NOT_FOUND, dropped, no OnFinal.
// ---------------------------------------------------------------------

func TestAsyncDriver_ReorgBeforeQuorum_DroppedNotFinalized(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, nil) // genuinely absent
	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, nil)

	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("CheckFinalityAsync: %v", err)
	}
	if final.count() != 0 {
		t.Fatalf("OnFinal called %d times, want 0", final.count())
	}
	if tr.PendingCount() != 0 {
		t.Fatalf("PendingCount() = %d, want 0 (a corroborated-absent candidate must be dropped, not left pending forever)", tr.PendingCount())
	}
}

// ---------------------------------------------------------------------
// Candidate reorg AFTER quorum -- the existing checkPostFinalReorgs path.
// ---------------------------------------------------------------------

func TestAsyncDriver_ReorgAfterQuorum_ExistingPostFinalCheckCatchesIt(t *testing.T) {
	final := &countingFinalHandler{}
	tr, reporter := func() (*finality.Tracker, *fakeReorgReporter) {
		reporter := &fakeReorgReporter{}
		tr, err := finality.New(finality.Config{
			ContractAddress: testContract, TransferTopic: testTopic,
			OnFinal: final.handle, ReorgReporter: reporter, OrphanedDepositRecorder: &fakeOrphanedDepositRecorder{},
			AsyncMinAgreement: 2,
		})
		if err != nil {
			t.Fatalf("finality.New: %v", err)
		}
		return tr, reporter
	}()
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{log})
	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, []types.Log{log})
	q.setJointLogsAt(1000, []types.Log{log}) // the shared post-final check must ALSO see it present, for now

	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if final.count() != 1 {
		t.Fatalf("tick 1: OnFinal called %d times, want 1", final.count())
	}
	if reporter.callCount() != 0 {
		t.Fatalf("ReportReorg called %d times before any reorg, want 0", reporter.callCount())
	}

	// Now the chain reorgs the already-finalized deposit away -- the
	// EXISTING, unmodified checkPostFinalReorgs (called by
	// CheckFinalityAsync itself) must catch this, exactly as it already
	// does for Design A.
	q.setJointLogsAt(1000, nil)
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if reporter.callCount() != 1 {
		t.Fatalf("ReportReorg called %d times after a post-final reorg, want exactly 1", reporter.callCount())
	}
}

// ---------------------------------------------------------------------
// ASYNC_NOT_FOUND requires the full quorum, not one provider.
// ---------------------------------------------------------------------

func TestAsyncDriver_NotFoundRequiresFullQuorum(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, nil) // A alone: absent

	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if tr.PendingCount() != 1 {
		t.Fatalf("PendingCount() = %d after only 1 of 2 required absent confirmations, want 1 (still pending)", tr.PendingCount())
	}

	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, nil)
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if tr.PendingCount() != 0 {
		t.Fatalf("PendingCount() = %d after 2 of 2 required absent confirmations, want 0 (dropped)", tr.PendingCount())
	}
}

// ---------------------------------------------------------------------
// Duplicate/idempotent observations -- no double-count, no double-finalize.
// ---------------------------------------------------------------------

func TestAsyncDriver_DuplicateObservations_NoDoubleCount(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{log})
	// B never reaches the height -- A alone, repeatedly, across many ticks.
	for i := 0; i < 5; i++ {
		if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if final.count() != 0 {
		t.Fatalf("OnFinal called %d times, want 0 (A re-confirming itself 5 times must never count as 2 distinct providers)", final.count())
	}
}

func TestAsyncDriver_NoDoubleFinalization(t *testing.T) {
	final := &countingFinalHandler{}
	tr := newAsyncTracker(t, 2, final.handle)
	q := newFakeAsyncChainQuerier("A", "B")

	txHash := bigHash(1)
	track(t, tr, 1000, txHash)
	log := transferLog(1000, txHash, 0, money.Amount(3000_000000))
	blockHash := hash(0xAA)

	q.setFinalized("A", 1000, blockHash)
	q.setLogsAt("A", 1000, []types.Log{log})
	q.setFinalized("B", 1000, blockHash)
	q.setLogsAt("B", 1000, []types.Log{log})

	// Two back-to-back ticks after quorum is already reached on the
	// first -- OnFinal must fire exactly once, ever, for this candidate.
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := tr.CheckFinalityAsync(context.Background(), q); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if final.count() != 1 {
		t.Fatalf("OnFinal called %d times across 2 ticks after quorum, want exactly 1", final.count())
	}
}
