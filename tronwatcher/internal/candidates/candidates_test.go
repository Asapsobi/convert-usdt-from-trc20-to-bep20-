package candidates

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"tronwatcher/internal/addresses"
	"tronwatcher/internal/chain"
	"tronwatcher/internal/finality"
	"tronwatcher/internal/money"
)

type fakeQuotes struct {
	amounts map[string]money.Amount
}

func (f fakeQuotes) QuotedAmount(ctx context.Context, externalID string) (money.Amount, error) {
	amt, ok := f.amounts[externalID]
	if !ok {
		return 0, errors.New("no such order")
	}
	return amt, nil
}

// noopQueryer panics if any of its methods are actually invoked --
// used where a test's own code path must never touch the database.
type noopQueryer struct{}

func (noopQueryer) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	panic("unexpected Exec call")
}
func (noopQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	panic("unexpected QueryRow call")
}
func (noopQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	panic("unexpected Query call")
}

// recordingQueryer records every Exec call's SQL -- enough to assert
// orphaned.Record actually issued an INSERT, without needing a real
// Postgres for this package's own unit tests (integration coverage for
// orphaned.Record itself lives in internal/orphaned's own integration
// test).
type recordingQueryer struct {
	mu    sync.Mutex
	execs []string
}

func (r *recordingQueryer) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs = append(r.execs, sql)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (r *recordingQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	panic("unexpected QueryRow call")
}
func (r *recordingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	panic("unexpected Query call")
}

func TestProcessTransfer_TrackableTransferReachesTracker(t *testing.T) {
	var observedFinal []finality.ObservedTransfer
	tr, err := finality.New(finality.Config{
		OnFinal: func(ctx context.Context, c finality.Candidate) error {
			observedFinal = append(observedFinal, c.ObservedTransfer)
			return nil
		},
		OrphanedDepositRecorder: fakeRecorder{},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}

	wa := addresses.WatchedAddress{
		OrderID: 1, ExternalID: "ext-1", CustomerID: "cust-1",
		Address: "TWatchedAddress", Status: addresses.StatusWatching, AssignedAt: time.Now().UTC().Add(-time.Hour),
	}
	quotes := fakeQuotes{amounts: map[string]money.Amount{"ext-1": 100_000000}}
	cfg := Config{ContractAddress: chain.USDTTRC20ContractAddress, DustFloor: chain.DefaultDustFloor}

	transfer := chain.Transfer{
		TxID: "tx1", From: "TSender", To: string(wa.Address),
		ValueRaw: big.NewInt(100_000000), BlockTimestamp: time.Now().UTC(), ContractAddress: chain.USDTTRC20ContractAddress,
	}
	if err := processTransfer(context.Background(), noopQueryer{}, quotes, tr, cfg, wa, transfer); err != nil {
		t.Fatalf("processTransfer: %v", err)
	}

	if tr.PendingCount() != 1 {
		t.Fatalf("expected 1 pending candidate after a trackable transfer, got %d", tr.PendingCount())
	}
}

func TestProcessTransfer_RetiredAddressGoesToOrphaned(t *testing.T) {
	tr, err := finality.New(finality.Config{
		OnFinal:                 func(ctx context.Context, c finality.Candidate) error { return nil },
		OrphanedDepositRecorder: fakeRecorder{},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}

	wa := addresses.WatchedAddress{
		OrderID: 1, ExternalID: "ext-1", CustomerID: "cust-1",
		Address: "TWatchedAddress", Status: addresses.StatusRetired,
	}
	cfg := Config{ContractAddress: chain.USDTTRC20ContractAddress, DustFloor: chain.DefaultDustFloor}

	transfer := chain.Transfer{
		TxID: "tx1", From: "TSender", To: string(wa.Address),
		ValueRaw: big.NewInt(50_000000), BlockTimestamp: time.Now().UTC(), ContractAddress: chain.USDTTRC20ContractAddress,
	}

	recorder := &recordingQueryer{}
	if err := processTransfer(context.Background(), recorder, fakeQuotes{}, tr, cfg, wa, transfer); err != nil {
		t.Fatalf("processTransfer: %v", err)
	}
	if tr.PendingCount() != 0 {
		t.Fatalf("expected a retired-address transfer NOT to be tracked toward finality, got %d pending", tr.PendingCount())
	}
	if len(recorder.execs) == 0 {
		t.Fatal("expected orphaned.Record to issue an INSERT for the late deposit")
	}
}

func TestProcessTransfer_QuotedAmountFetchFailurePropagates(t *testing.T) {
	tr, err := finality.New(finality.Config{
		OnFinal:                 func(ctx context.Context, c finality.Candidate) error { return nil },
		OrphanedDepositRecorder: fakeRecorder{},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}

	wa := addresses.WatchedAddress{
		OrderID: 1, ExternalID: "ext-unknown", CustomerID: "cust-1",
		Address: "TWatchedAddress", Status: addresses.StatusWatching,
	}
	cfg := Config{ContractAddress: chain.USDTTRC20ContractAddress, DustFloor: chain.DefaultDustFloor}
	transfer := chain.Transfer{
		TxID: "tx1", From: "TSender", To: string(wa.Address),
		ValueRaw: big.NewInt(1_000000), BlockTimestamp: time.Now().UTC(), ContractAddress: chain.USDTTRC20ContractAddress,
	}

	err = processTransfer(context.Background(), noopQueryer{}, fakeQuotes{}, tr, cfg, wa, transfer)
	if err == nil {
		t.Fatal("expected an error when the quoted amount can't be fetched")
	}
}

type fakeRecorder struct{}

func (fakeRecorder) RecordOrphanedDeposit(ctx context.Context, c finality.Candidate, c1Error error) error {
	return nil
}
