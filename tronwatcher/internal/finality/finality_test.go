package finality

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"tronwatcher/internal/chain"
	"tronwatcher/internal/money"
)

// fakeChecker is a FinalityChecker whose answer for each txID is
// controlled by the test.
type fakeChecker struct {
	mu    sync.Mutex
	final map[string]bool
}

func newFakeChecker() *fakeChecker { return &fakeChecker{final: make(map[string]bool)} }

func (f *fakeChecker) SetFinal(txID string, final bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.final[txID] = final
}

func (f *fakeChecker) IsFinal(ctx context.Context, txID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.final[txID], nil
}

type erroringChecker struct{ err error }

func (e erroringChecker) IsFinal(ctx context.Context, txID string) (bool, error) { return false, e.err }

type fakeOrphanRecorder struct {
	mu       sync.Mutex
	recorded []Candidate
}

func (f *fakeOrphanRecorder) RecordOrphanedDeposit(ctx context.Context, c Candidate, c1Error error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, c)
	return nil
}

func testTransfer(txID string) ObservedTransfer {
	return ObservedTransfer{
		TxID:           txID,
		BlockTimestamp: time.Now().UTC(),
		OrderID:        1,
		ExternalID:     "ext-1",
		CustomerID:     "cust-1",
		Amount:         money.Amount(1_000000),
		SenderAddress:  "TSender",
	}
}

func TestTracker_OnTransferObserved_Deduplicates(t *testing.T) {
	tr, err := New(Config{
		OnFinal:                 func(ctx context.Context, c Candidate) error { return nil },
		OrphanedDepositRecorder: &fakeOrphanRecorder{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	obs := testTransfer("tx1")
	if err := tr.OnTransferObserved(context.Background(), obs, chain.Exact); err != nil {
		t.Fatalf("first observe: %v", err)
	}
	if err := tr.OnTransferObserved(context.Background(), obs, chain.Exact); err != nil {
		t.Fatalf("second observe: %v", err)
	}
	if tr.PendingCount() != 1 {
		t.Errorf("expected 1 pending candidate after duplicate delivery, got %d", tr.PendingCount())
	}
}

func TestTracker_OnTransferObserved_IgnoresZeroValue(t *testing.T) {
	tr, err := New(Config{
		OnFinal:                 func(ctx context.Context, c Candidate) error { return nil },
		OrphanedDepositRecorder: &fakeOrphanRecorder{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tr.OnTransferObserved(context.Background(), testTransfer("tx1"), chain.ZeroValue); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if tr.PendingCount() != 0 {
		t.Errorf("expected ZeroValue transfers not to be tracked, got %d pending", tr.PendingCount())
	}
}

func TestTracker_CheckFinality_PromotesOnceFinal(t *testing.T) {
	var finalized []Candidate
	var mu sync.Mutex
	tr, err := New(Config{
		OnFinal: func(ctx context.Context, c Candidate) error {
			mu.Lock()
			defer mu.Unlock()
			finalized = append(finalized, c)
			return nil
		},
		OrphanedDepositRecorder: &fakeOrphanRecorder{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	obs := testTransfer("tx1")
	if err := tr.OnTransferObserved(context.Background(), obs, chain.Exact); err != nil {
		t.Fatalf("observe: %v", err)
	}

	checker := newFakeChecker()
	tr.CheckFinality(context.Background(), checker)
	if tr.PendingCount() != 1 {
		t.Fatalf("expected candidate to remain pending before solidity confirmation, got %d pending", tr.PendingCount())
	}

	checker.SetFinal("tx1", true)
	tr.CheckFinality(context.Background(), checker)
	if tr.PendingCount() != 0 {
		t.Fatalf("expected candidate to be promoted once final, got %d still pending", tr.PendingCount())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finalized) != 1 || finalized[0].TxID != "tx1" {
		t.Fatalf("expected exactly one OnFinal call for tx1, got %+v", finalized)
	}
}

func TestTracker_CheckFinality_LeavesInPlaceOnCheckerError(t *testing.T) {
	tr, err := New(Config{
		OnFinal:                 func(ctx context.Context, c Candidate) error { return nil },
		OrphanedDepositRecorder: &fakeOrphanRecorder{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tr.OnTransferObserved(context.Background(), testTransfer("tx1"), chain.Exact); err != nil {
		t.Fatalf("observe: %v", err)
	}

	tr.CheckFinality(context.Background(), erroringChecker{err: errors.New("node unreachable")})
	if tr.PendingCount() != 1 {
		t.Errorf("expected candidate to remain pending after a checker error, got %d pending", tr.PendingCount())
	}
}

func TestTracker_CheckFinality_RetriesOnFinalHandlerError(t *testing.T) {
	attempts := 0
	tr, err := New(Config{
		OnFinal: func(ctx context.Context, c Candidate) error {
			attempts++
			if attempts == 1 {
				return errors.New("transient C1 error")
			}
			return nil
		},
		OrphanedDepositRecorder: &fakeOrphanRecorder{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tr.OnTransferObserved(context.Background(), testTransfer("tx1"), chain.Exact); err != nil {
		t.Fatalf("observe: %v", err)
	}

	checker := newFakeChecker()
	checker.SetFinal("tx1", true)

	tr.CheckFinality(context.Background(), checker)
	if tr.PendingCount() != 1 {
		t.Fatalf("expected candidate to remain pending after a transient OnFinal error, got %d pending", tr.PendingCount())
	}

	tr.CheckFinality(context.Background(), checker)
	if tr.PendingCount() != 0 {
		t.Fatalf("expected candidate to finalize on retry, got %d pending", tr.PendingCount())
	}
	if attempts != 2 {
		t.Errorf("expected exactly 2 OnFinal attempts, got %d", attempts)
	}
}

func TestTracker_CheckFinality_OrphanedDepositRecordedAndDropped(t *testing.T) {
	recorder := &fakeOrphanRecorder{}
	tr, err := New(Config{
		OnFinal: func(ctx context.Context, c Candidate) error {
			return errors.Join(ErrPermanentFailure, ErrOrphanedDeposit)
		},
		OrphanedDepositRecorder: recorder,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tr.OnTransferObserved(context.Background(), testTransfer("tx1"), chain.Exact); err != nil {
		t.Fatalf("observe: %v", err)
	}

	checker := newFakeChecker()
	checker.SetFinal("tx1", true)
	tr.CheckFinality(context.Background(), checker)

	if tr.PendingCount() != 0 {
		t.Errorf("expected orphaned candidate to be dropped from pending, got %d", tr.PendingCount())
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.recorded) != 1 || recorder.recorded[0].TxID != "tx1" {
		t.Errorf("expected orphaned deposit to be recorded, got %+v", recorder.recorded)
	}
}

func TestTracker_CheckFinality_PermanentNonOrphanedErrorDropsSilently(t *testing.T) {
	recorder := &fakeOrphanRecorder{}
	tr, err := New(Config{
		OnFinal: func(ctx context.Context, c Candidate) error {
			return ErrPermanentFailure
		},
		OrphanedDepositRecorder: recorder,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tr.OnTransferObserved(context.Background(), testTransfer("tx1"), chain.Exact); err != nil {
		t.Fatalf("observe: %v", err)
	}

	checker := newFakeChecker()
	checker.SetFinal("tx1", true)
	tr.CheckFinality(context.Background(), checker)

	if tr.PendingCount() != 0 {
		t.Errorf("expected candidate to be dropped from pending, got %d", tr.PendingCount())
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.recorded) != 0 {
		t.Errorf("expected no orphan recording for a non-orphaned permanent failure, got %+v", recorder.recorded)
	}
}

func TestTracker_StalePendingFiresOnce(t *testing.T) {
	var alerts int
	var mu sync.Mutex
	now := time.Now().UTC()
	clock := now

	tr, err := New(Config{
		OnFinal:                 func(ctx context.Context, c Candidate) error { return nil },
		OrphanedDepositRecorder: &fakeOrphanRecorder{},
		StalePendingCeiling:     time.Minute,
		Now:                     func() time.Time { return clock },
		OnStalePending: func(c Candidate, pending time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			alerts++
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tr.OnTransferObserved(context.Background(), testTransfer("tx1"), chain.Exact); err != nil {
		t.Fatalf("observe: %v", err)
	}

	checker := newFakeChecker()
	clock = now.Add(2 * time.Minute)
	tr.CheckFinality(context.Background(), checker)
	tr.CheckFinality(context.Background(), checker)

	mu.Lock()
	defer mu.Unlock()
	if alerts != 1 {
		t.Errorf("expected exactly 1 stale-pending alert, got %d", alerts)
	}
}

func TestNew_RequiresOnFinalAndRecorder(t *testing.T) {
	if _, err := New(Config{OrphanedDepositRecorder: &fakeOrphanRecorder{}}); err == nil {
		t.Error("expected an error when OnFinal is nil")
	}
	if _, err := New(Config{OnFinal: func(ctx context.Context, c Candidate) error { return nil }}); err == nil {
		t.Error("expected an error when OrphanedDepositRecorder is nil")
	}
}

func TestDepositFinalIdempotencyKey(t *testing.T) {
	got := DepositFinalIdempotencyKey("abc123")
	want := "tronwatcher:deposit_final:abc123"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
