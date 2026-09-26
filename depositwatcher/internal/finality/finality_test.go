package finality_test

// This chunk's own acceptance criteria call for "a forked/simulated chain
// capable of advancing a finalized tag on command." internal/chain's own
// fake JSON-RPC node (fake_node_test.go) already provides exactly that,
// but it's unexported and package-private to internal/chain -- Go test
// helpers don't cross package boundaries -- so this file has its own,
// deliberately smaller fake node, supporting only the two RPC methods
// chain.Pool.LatestFinalized and chain.Pool.LogsAt actually issue
// (eth_getBlockByNumber("finalized") and eth_getLogs). Tests still dial it
// with the real ethclient over real HTTP and drive a real chain.Pool, so
// what's under test is finality.Tracker's own agreement/re-verification
// logic against genuine wire-level RPC round trips, same posture as
// internal/chain's own tests.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/money"
)

// bigHash builds a distinct common.Hash from a small int, for readable
// synthetic tx hashes in test fixtures.
func bigHash(v int64) common.Hash { return common.BigToHash(big.NewInt(v)) }

// fakeNode is a minimal JSON-RPC 2.0 server standing in for one BSC node,
// supporting only what finality.Tracker needs from a chain.Pool.
type fakeNode struct {
	mu sync.Mutex

	finalizedHeight uint64
	finalizedHash   common.Hash
	finalizedErr    string

	logs    []types.Log
	logsErr string
}

func newFakeNode() *fakeNode { return &fakeNode{} }

func (n *fakeNode) client() (*ethclient.Client, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(n.handle))
	c, err := ethclient.DialContext(context.Background(), srv.URL)
	if err != nil {
		panic(fmt.Sprintf("dialing fake node: %v", err))
	}
	return c, srv
}

func (n *fakeNode) setFinalized(height uint64, h common.Hash) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finalizedHeight, n.finalizedHash = height, h
}

func (n *fakeNode) setLogs(logs []types.Log) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logs = logs
}

func (n *fakeNode) setLogsError(msg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logsErr = msg
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (n *fakeNode) handle(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	switch req.Method {
	case "eth_getBlockByNumber":
		if n.finalizedErr != "" {
			writeRPCError(w, req.ID, n.finalizedErr)
			return
		}
		writeRPCResult(w, req.ID, map[string]any{
			"number": fmt.Sprintf("0x%x", n.finalizedHeight),
			"hash":   n.finalizedHash.Hex(),
		})
	case "eth_getLogs":
		if n.logsErr != "" {
			writeRPCError(w, req.ID, n.logsErr)
			return
		}
		writeRPCResult(w, req.ID, n.logs)
	case "eth_chainId":
		writeRPCResult(w, req.ID, "0x38")
	default:
		writeRPCError(w, req.ID, fmt.Sprintf("fakeNode: unsupported method %q", req.Method))
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	body, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(body)})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32000, "message": message},
	})
}

// twoNodePool wires two fake nodes into a real chain.Pool, dialed with the
// real ethclient, and returns both so a test can independently control
// what each reports.
func twoNodePool(t *testing.T) (pool *chain.Pool, a, b *fakeNode) {
	t.Helper()
	a, b = newFakeNode(), newFakeNode()
	clientA, srvA := a.client()
	clientB, srvB := b.client()
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)

	pool, err := chain.NewPool([]chain.Provider{
		{Name: "A", Client: clientA},
		{Name: "B", Client: clientB},
	}, chain.Config{MinAgreement: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool, a, b
}

var testContract = common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
var testTopic = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

func testLog(height uint64, txHash common.Hash, index uint) types.Log {
	return types.Log{
		Address:     testContract,
		Topics:      []common.Hash{testTopic},
		BlockNumber: height,
		TxHash:      txHash,
		Index:       index,
	}
}

// fakeReorgReporter records every ReportReorg call it receives -- a
// no-op stand-in for ledgerclient.Client, tested against a real C1 only
// in ledgerclient's own integration test.
type fakeReorgReporter struct {
	mu    sync.Mutex
	calls []reorgCall
	err   error // if set, ReportReorg returns this on every call
}

type reorgCall struct {
	ExternalID       string
	OriginalEntryKey string
}

func (f *fakeReorgReporter) ReportReorg(_ context.Context, externalID, originalEntryKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, reorgCall{externalID, originalEntryKey})
	return nil
}

func (f *fakeReorgReporter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeOrphanedDepositRecorder records every RecordOrphanedDeposit call it
// receives -- a no-op stand-in for internal/orphaned, tested against a
// real database only in that package's own integration test.
type fakeOrphanedDepositRecorder struct {
	mu    sync.Mutex
	calls []finality.Candidate
	err   error // if set, RecordOrphanedDeposit returns this on every call
}

func (f *fakeOrphanedDepositRecorder) RecordOrphanedDeposit(_ context.Context, c finality.Candidate, _ error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, c)
	return nil
}

func (f *fakeOrphanedDepositRecorder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newTracker(t *testing.T, onFinal finality.FinalHandler) *finality.Tracker {
	t.Helper()
	tr, err := finality.New(finality.Config{
		ContractAddress:         testContract,
		TransferTopic:           testTopic,
		OnFinal:                 onFinal,
		ReorgReporter:           &fakeReorgReporter{},
		OrphanedDepositRecorder: &fakeOrphanedDepositRecorder{},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	return tr
}

// newTrackerWithRecorder is newTracker plus access to the fake
// OrphanedDepositRecorder itself, for the C2.8 tests below that need to
// assert on what was recorded.
func newTrackerWithRecorder(t *testing.T, onFinal finality.FinalHandler) (*finality.Tracker, *fakeOrphanedDepositRecorder) {
	t.Helper()
	recorder := &fakeOrphanedDepositRecorder{}
	tr, err := finality.New(finality.Config{
		ContractAddress:         testContract,
		TransferTopic:           testTopic,
		OnFinal:                 onFinal,
		ReorgReporter:           &fakeReorgReporter{},
		OrphanedDepositRecorder: recorder,
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	return tr, recorder
}

// newTrackerWithReporter is newTracker plus access to the fake
// ReorgReporter itself, for the post-final reorg tests below that need
// to assert on what was reported.
func newTrackerWithReporter(t *testing.T, onFinal finality.FinalHandler) (*finality.Tracker, *fakeReorgReporter) {
	t.Helper()
	reporter := &fakeReorgReporter{}
	tr, err := finality.New(finality.Config{
		ContractAddress:         testContract,
		TransferTopic:           testTopic,
		OnFinal:                 onFinal,
		ReorgReporter:           reporter,
		OrphanedDepositRecorder: &fakeOrphanedDepositRecorder{},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	return tr, reporter
}

func sampleObservedLog(height uint64, txHash common.Hash, index uint) finality.ObservedLog {
	return finality.ObservedLog{
		TxHash:     txHash,
		LogIndex:   index,
		Height:     height,
		BlockTime:  time.Unix(1_700_000_000, 0).UTC(),
		OrderID:    42,
		ExternalID: "order-42",
		CustomerID: "cust-1",
		Amount:     money.Amount(3000_000000),
	}
}

// ---------------------------------------------------------------------
// OnLogObserved
// ---------------------------------------------------------------------

func TestOnLogObserved_TracksExactOverUnderDustAsCandidates(t *testing.T) {
	tracker := newTracker(t, func(context.Context, finality.Candidate) error { return nil })

	classes := []chain.Classification{chain.Exact, chain.Overpay, chain.Underpay, chain.Dust}
	for i, c := range classes {
		log := sampleObservedLog(100, bigHash(int64(i+1)), 0)
		if err := tracker.OnLogObserved(context.Background(), log, c); err != nil {
			t.Fatalf("OnLogObserved(%s): %v", c, err)
		}
	}
	if got := tracker.PendingCount(); got != len(classes) {
		t.Fatalf("PendingCount() = %d, want %d", got, len(classes))
	}
}

func TestOnLogObserved_ZeroValueNeverTracked(t *testing.T) {
	tracker := newTracker(t, func(context.Context, finality.Candidate) error {
		t.Fatal("OnFinal must never be called for a ZeroValue log")
		return nil
	})
	log := sampleObservedLog(100, bigHash(1), 0)
	if err := tracker.OnLogObserved(context.Background(), log, chain.ZeroValue); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0 (ZeroValue must never become a candidate)", got)
	}
}

func TestOnLogObserved_DuplicateDeliveryIsANoOp(t *testing.T) {
	tracker := newTracker(t, func(context.Context, finality.Candidate) error { return nil })
	log := sampleObservedLog(100, bigHash(1), 0)

	for i := 0; i < 3; i++ {
		if err := tracker.OnLogObserved(context.Background(), log, chain.Exact); err != nil {
			t.Fatalf("OnLogObserved (delivery %d): %v", i, err)
		}
	}
	if got := tracker.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() = %d, want 1 (duplicate delivery must be a no-op)", got)
	}
}

// ---------------------------------------------------------------------
// CheckFinality
// ---------------------------------------------------------------------

func TestCheckFinality_EligibleExactlyWhenFinalizedHeightPassesIt(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	logSet := []types.Log{testLog(100, txHash, 0)}
	a.setLogs(logSet)
	b.setLogs(logSet)

	var finalized []finality.Candidate
	var mu sync.Mutex
	tracker := newTracker(t, func(_ context.Context, c finality.Candidate) error {
		mu.Lock()
		defer mu.Unlock()
		finalized = append(finalized, c)
		return nil
	})

	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	// One block early: finalized tag has NOT yet reached height 100.
	a.setFinalized(99, common.Hash{})
	b.setFinalized(99, common.Hash{})
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality: %v", err)
	}
	if len(finalized) != 0 {
		t.Fatalf("finalized too early: got %d candidates finalized at height 99, want 0", len(finalized))
	}
	if got := tracker.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() = %d, want 1 (still pending before finality)", got)
	}

	// Exactly at the candidate's height: now eligible.
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality: %v", err)
	}
	if len(finalized) != 1 {
		t.Fatalf("got %d candidates finalized at height 100, want exactly 1", len(finalized))
	}
	if finalized[0].TxHash != txHash {
		t.Fatalf("finalized the wrong candidate: got %s, want %s", finalized[0].TxHash, txHash)
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0 after finalization", got)
	}

	// A second tick must not re-finalize the same candidate.
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (second tick): %v", err)
	}
	if len(finalized) != 1 {
		t.Fatalf("candidate was finalized again: got %d calls to OnFinal, want exactly 1 total", len(finalized))
	}
}

func TestCheckFinality_ReorgedOutBeforeFinality_SilentlyDropped(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	// Both providers now agree the block at height 100 has DIFFERENT
	// logs than what was originally observed -- the routine, expected
	// pre-final reorg: the candidate's tx never made it into the
	// canonical chain.
	a.setLogs(nil)
	b.setLogs(nil)
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})

	var onFinalCalls int
	tracker := newTracker(t, func(context.Context, finality.Candidate) error {
		onFinalCalls++
		return nil
	})
	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality: %v", err)
	}
	if onFinalCalls != 0 {
		t.Fatalf("OnFinal called %d times, want 0 -- a reorged-out candidate must never be reported to C1", onFinalCalls)
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0 -- a reorged-out candidate must be dropped, not left pending forever", got)
	}

	// A later tick must not somehow resurrect and finalize it.
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (later tick): %v", err)
	}
	if onFinalCalls != 0 {
		t.Fatalf("OnFinal called %d times after a later tick, want 0", onFinalCalls)
	}
}

func TestCheckFinality_ProviderDisagreement_FinalizationWithheld(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	a.setLogs([]types.Log{testLog(100, txHash, 0)})
	b.setLogs([]types.Log{testLog(100, txHash, 0)})

	// Disagreement on the finalized tag itself: same height, different
	// hash -- invariant 5 must withhold finalization even though the
	// candidate's own height is comfortably covered by both providers'
	// claims individually.
	a.setFinalized(100, bigHash(0xA))
	b.setFinalized(100, bigHash(0xB))

	var onFinalCalls int
	tracker := newTracker(t, func(context.Context, finality.Candidate) error {
		onFinalCalls++
		return nil
	})
	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	err := tracker.CheckFinality(context.Background(), pool)
	if err == nil {
		t.Fatal("expected CheckFinality to return an error on provider disagreement, got nil")
	}
	if !errors.Is(err, chain.ErrNoAgreement) {
		t.Fatalf("expected an error wrapping chain.ErrNoAgreement, got %v", err)
	}
	if onFinalCalls != 0 {
		t.Fatalf("OnFinal called %d times despite provider disagreement, want 0 (never majority-of-one)", onFinalCalls)
	}
	if got := tracker.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() = %d, want 1 -- withheld, not dropped", got)
	}
}

func TestCheckFinality_OnFinalPermanentFailure_DropsCandidateWithoutRetry(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	logSet := []types.Log{testLog(100, txHash, 0)}
	a.setLogs(logSet)
	b.setLogs(logSet)
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})

	var onFinalCalls int
	tracker := newTracker(t, func(context.Context, finality.Candidate) error {
		onFinalCalls++
		return fmt.Errorf("order no longer in quoted: %w", finality.ErrPermanentFailure)
	})
	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 1): %v", err)
	}
	if onFinalCalls != 1 {
		t.Fatalf("OnFinal called %d times, want exactly 1", onFinalCalls)
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0 -- a permanent failure must be dropped, not left pending", got)
	}
	if got := tracker.FinalizedCount(); got != 0 {
		t.Fatalf("FinalizedCount() = %d, want 0 -- a permanent failure never counts as finalized", got)
	}

	// A later tick must not call OnFinal again -- the candidate is gone,
	// not retried.
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 2): %v", err)
	}
	if onFinalCalls != 1 {
		t.Fatalf("OnFinal called %d times after a later tick, want still exactly 1 (no retry after a permanent failure)", onFinalCalls)
	}
}

// ---------------------------------------------------------------------
// Orphaned deposits (C2.8)
// ---------------------------------------------------------------------

func TestCheckFinality_OrphanedDeposit_RecordedExactlyOnce(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	logSet := []types.Log{testLog(100, txHash, 0)}
	a.setLogs(logSet)
	b.setLogs(logSet)
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})

	tracker, recorder := newTrackerWithRecorder(t, func(context.Context, finality.Candidate) error {
		return fmt.Errorf("order no longer in quoted: %w: %w", finality.ErrPermanentFailure, finality.ErrOrphanedDeposit)
	})
	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 1): %v", err)
	}
	if recorder.callCount() != 1 {
		t.Fatalf("RecordOrphanedDeposit called %d times, want exactly 1", recorder.callCount())
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0 -- recorded, must not stay pending", got)
	}
	if got := tracker.FinalizedCount(); got != 0 {
		t.Fatalf("FinalizedCount() = %d, want 0 -- an orphaned deposit never counts as finalized", got)
	}

	// A later tick must not record it again.
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 2): %v", err)
	}
	if recorder.callCount() != 1 {
		t.Fatalf("RecordOrphanedDeposit called %d times after a later tick, want still exactly 1", recorder.callCount())
	}
}

func TestCheckFinality_EveryPermanentFailureIsRecordedForOperators(t *testing.T) {
	// A plain ErrPermanentFailure (e.g. C2.7's idempotency_conflict, a
	// structural bug alert, not customer money orphaned by an expired
	// order) must never reach the recorder -- there is nothing to record,
	// and HandleUnreportable is specifically for ErrOrphanedDeposit.
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	logSet := []types.Log{testLog(100, txHash, 0)}
	a.setLogs(logSet)
	b.setLogs(logSet)
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})

	tracker, recorder := newTrackerWithRecorder(t, func(context.Context, finality.Candidate) error {
		return fmt.Errorf("bug alert: %w", finality.ErrPermanentFailure)
	})
	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality: %v", err)
	}
	// A deposit we can't report for any permanent reason -- here our own
	// bug -- is real customer money, so it is recorded for an operator
	// exactly like an orphaned one, never just logged.
	if recorder.callCount() != 1 {
		t.Fatalf("RecordOrphanedDeposit called %d times for a permanent failure, want 1", recorder.callCount())
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0", got)
	}
}

func TestCheckFinality_OrphanedDeposit_RetriesRecordingOnFailureThenStops(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	logSet := []types.Log{testLog(100, txHash, 0)}
	a.setLogs(logSet)
	b.setLogs(logSet)
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})

	recorder := &fakeOrphanedDepositRecorder{err: errors.New("simulated database write failure")}
	tracker, err := finality.New(finality.Config{
		ContractAddress: testContract,
		TransferTopic:   testTopic,
		OnFinal: func(context.Context, finality.Candidate) error {
			return fmt.Errorf("%w: %w", finality.ErrPermanentFailure, finality.ErrOrphanedDeposit)
		},
		ReorgReporter:           &fakeReorgReporter{},
		OrphanedDepositRecorder: recorder,
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 1, recording fails): %v", err)
	}
	if recorder.callCount() != 0 {
		t.Fatalf("callCount() = %d, want 0 (the failed attempt is not recorded as a call)", recorder.callCount())
	}
	if got := tracker.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() = %d, want 1 -- a failed recording must leave the candidate pending for retry", got)
	}

	recorder.mu.Lock()
	recorder.err = nil
	recorder.mu.Unlock()

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 2, recording succeeds): %v", err)
	}
	if recorder.callCount() != 1 {
		t.Fatalf("callCount() = %d, want exactly 1 after the retry succeeds", recorder.callCount())
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0 after a successful recording", got)
	}

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 3): %v", err)
	}
	if recorder.callCount() != 1 {
		t.Fatalf("callCount() = %d after a further tick, want exactly 1 (no double recording)", recorder.callCount())
	}
}

func TestCheckFinality_ReVerificationQueryFails_LeavesCandidatePending(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})
	// The finalized-tag check itself succeeds (both agree), but the
	// re-verification LogsAt call for the candidate's own height fails --
	// e.g. a transient network blip. The candidate must be left pending
	// for the next tick to retry, never guessed at either way.
	a.setLogsError("transient RPC error")

	var onFinalCalls int
	tracker := newTracker(t, func(context.Context, finality.Candidate) error {
		onFinalCalls++
		return nil
	})
	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	// CheckFinality itself must not fail the whole tick over one height's
	// re-verification error -- it logs and defers, per its own doc comment.
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality: unexpected error: %v", err)
	}
	if onFinalCalls != 0 {
		t.Fatalf("OnFinal called %d times despite a failed re-verification query, want 0", onFinalCalls)
	}
	if got := tracker.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() = %d, want 1 -- left pending for retry, not dropped", got)
	}
}

func TestCheckFinality_StalePendingAlertsOnceAfterCeiling(t *testing.T) {
	pool, a, b := twoNodePool(t)
	// Finalized tag never reaches the candidate's height -- exactly the
	// "provider pool or chain itself is in trouble" scenario the ceiling
	// exists to catch.
	a.setFinalized(0, common.Hash{})
	b.setFinalized(0, common.Hash{})

	now := time.Unix(1_700_000_000, 0).UTC()
	clock := func() time.Time { return now }

	var stalled []finality.Candidate
	var mu sync.Mutex
	tracker, err := finality.New(finality.Config{
		ContractAddress:         testContract,
		TransferTopic:           testTopic,
		OnFinal:                 func(context.Context, finality.Candidate) error { return nil },
		ReorgReporter:           &fakeReorgReporter{},
		OrphanedDepositRecorder: &fakeOrphanedDepositRecorder{},
		StalePendingCeiling:     5 * time.Minute,
		Now:                     clock,
		OnStalePending: func(c finality.Candidate, pending time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			stalled = append(stalled, c)
		},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}

	txHash := bigHash(1)
	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	// Still within the ceiling: no alert yet.
	now = now.Add(4 * time.Minute)
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality: %v", err)
	}
	if len(stalled) != 0 {
		t.Fatalf("alerted before crossing the ceiling: got %d alerts, want 0", len(stalled))
	}

	// Past the ceiling: exactly one alert.
	now = now.Add(2 * time.Minute) // total pending: 6 minutes
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality: %v", err)
	}
	if len(stalled) != 1 {
		t.Fatalf("got %d alerts after crossing the ceiling, want exactly 1", len(stalled))
	}

	// A further tick, still stale, must not alert again.
	now = now.Add(1 * time.Minute)
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality: %v", err)
	}
	if len(stalled) != 1 {
		t.Fatalf("alerted again on a later tick: got %d total alerts, want exactly 1 (fire once, not every tick)", len(stalled))
	}
}

// ---------------------------------------------------------------------
// Post-final reorg (C2.6)
// ---------------------------------------------------------------------

func TestCheckFinality_PostFinalReorg_DetectedAndReportedExactlyOnce(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	logSet := []types.Log{testLog(100, txHash, 0)}
	a.setLogs(logSet)
	b.setLogs(logSet)
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})

	tracker, reporter := newTrackerWithReporter(t, func(context.Context, finality.Candidate) error { return nil })
	log := sampleObservedLog(100, txHash, 0)
	if err := tracker.OnLogObserved(context.Background(), log, chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	// Tick 1: finalizes normally. No reorg yet -- ReportReorg untouched.
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 1): %v", err)
	}
	if got := tracker.FinalizedCount(); got != 1 {
		t.Fatalf("FinalizedCount() = %d, want 1 after finalizing", got)
	}
	if reporter.callCount() != 0 {
		t.Fatalf("ReportReorg called before any reorg occurred: %d calls, want 0", reporter.callCount())
	}

	// The chain now violates its own prior finality: the same height's
	// logs no longer include this candidate. Deliberately misconfiguring
	// the fake node this way is exactly how a normal chain is NOT
	// supposed to behave -- BEP-126 finality is meant to make this
	// impossible, which is why this is the only way to reproduce it in a
	// test at all.
	a.setLogs(nil)
	b.setLogs(nil)

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 2, reorg): %v", err)
	}
	if reporter.callCount() != 1 {
		t.Fatalf("ReportReorg called %d times after the post-final reorg, want exactly 1", reporter.callCount())
	}
	got := reporter.calls[0]
	if got.ExternalID != log.ExternalID {
		t.Errorf("ReportReorg externalID = %q, want %q", got.ExternalID, log.ExternalID)
	}
	wantKey := finality.DepositFinalIdempotencyKey(txHash, 0)
	if got.OriginalEntryKey != wantKey {
		t.Errorf("ReportReorg originalEntryKey = %q, want %q", got.OriginalEntryKey, wantKey)
	}
	if got := tracker.FinalizedCount(); got != 0 {
		t.Fatalf("FinalizedCount() = %d, want 0 -- a reported reorg must stop being tracked", got)
	}

	// A later tick must not report it again -- nothing left to
	// re-verify, and even if it were, invariant 4 forbids a second
	// reversal for the same candidate.
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 3): %v", err)
	}
	if reporter.callCount() != 1 {
		t.Fatalf("ReportReorg called again on a later tick: %d total calls, want exactly 1", reporter.callCount())
	}
}

func TestCheckFinality_PostFinalReorg_RetriesReportOnFailureThenStops(t *testing.T) {
	pool, a, b := twoNodePool(t)
	txHash := bigHash(1)
	logSet := []types.Log{testLog(100, txHash, 0)}
	a.setLogs(logSet)
	b.setLogs(logSet)
	a.setFinalized(100, common.Hash{})
	b.setFinalized(100, common.Hash{})

	reporter := &fakeReorgReporter{}
	tracker, err := finality.New(finality.Config{
		ContractAddress:         testContract,
		TransferTopic:           testTopic,
		OnFinal:                 func(context.Context, finality.Candidate) error { return nil },
		ReorgReporter:           reporter,
		OrphanedDepositRecorder: &fakeOrphanedDepositRecorder{},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}

	if err := tracker.OnLogObserved(context.Background(), sampleObservedLog(100, txHash, 0), chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 1): %v", err)
	}

	// Trigger the reorg, but make the report itself fail (e.g. C1
	// unreachable) -- the candidate must stay tracked for a retry, safe
	// because a repeated report is idempotent on C1's side.
	a.setLogs(nil)
	b.setLogs(nil)
	reporter.mu.Lock()
	reporter.err = errors.New("simulated C1 unreachable")
	reporter.mu.Unlock()

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 2, report fails): %v", err)
	}
	if reporter.callCount() != 0 {
		t.Fatalf("callCount() = %d, want 0 (the failed attempt is not recorded as a call)", reporter.callCount())
	}
	if got := tracker.FinalizedCount(); got != 1 {
		t.Fatalf("FinalizedCount() = %d, want 1 -- a failed report must leave the candidate tracked for retry", got)
	}

	// Clear the failure; the next tick must retry and succeed.
	reporter.mu.Lock()
	reporter.err = nil
	reporter.mu.Unlock()

	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 3, report succeeds): %v", err)
	}
	if reporter.callCount() != 1 {
		t.Fatalf("callCount() = %d, want exactly 1 after the retry succeeds", reporter.callCount())
	}
	if got := tracker.FinalizedCount(); got != 0 {
		t.Fatalf("FinalizedCount() = %d, want 0 after a successful report", got)
	}

	// One more tick: must not report again.
	if err := tracker.CheckFinality(context.Background(), pool); err != nil {
		t.Fatalf("CheckFinality (tick 4): %v", err)
	}
	if reporter.callCount() != 1 {
		t.Fatalf("callCount() = %d after a further tick, want exactly 1 (no double report)", reporter.callCount())
	}
}
