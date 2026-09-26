//go:build integration

package candidates_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5/pgxpool"

	"depositwatcher/internal/candidates"
	"depositwatcher/internal/deposits"
	"depositwatcher/internal/finality"
)

func durableTracker(t *testing.T, pool *pgxpool.Pool, onFinal finality.FinalHandler) *finality.Tracker {
	t.Helper()
	tr, err := finality.New(finality.Config{
		ContractAddress: testContract, TransferTopic: testTopic, OnFinal: onFinal,
		ReorgReporter: noopReorgReporter{}, OrphanedDepositRecorder: noopOrphanedRecorder{},
		Store: deposits.NewStore(pool),
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	return tr
}

// A deposit detected but not yet reported survives a restart: the scan
// cursor has already moved past it, so recovering it from the deposits
// table is the only way it gets reported at all.
func TestDeposits_SurviveARestartAndAreNeverReportedTwice(t *testing.T) {
	pool := testPool(t)
	chainPool, a, b := twoNodePool(t)
	ctx := context.Background()

	orderID := uniqueOrderID()
	addr := assignAddress(t, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-restart")
	log := newTransferLog(200, newTxHash(), addressTopic(addr), 0, 3000_000000)
	for _, n := range []*fakeNode{a, b} {
		n.setLogs([]types.Log{log})
		n.setBlockTime(200, time.Now())
	}
	cfg := candidates.Config{ContractAddress: testContract, TransferTopic: testTopic, DustFloor: 1_000000}

	// First run: detects the deposit, then "crashes" before finality.
	first := durableTracker(t, pool, func(context.Context, finality.Candidate) error { return nil })
	if err := candidates.ScanRange(ctx, chainPool, pool, fakeQuotes{amount: 3000_000000}, first, cfg, 200, 200); err != nil {
		t.Fatalf("ScanRange: %v", err)
	}

	// Second run: nothing in memory; the deposit comes back from the table.
	reports := 0
	second := durableTracker(t, pool, func(context.Context, finality.Candidate) error { reports++; return nil })
	restored, err := second.Restore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if restored < 1 || second.PendingCount() < 1 {
		t.Fatalf("the detected deposit was not restored (restored %d, pending %d)", restored, second.PendingCount())
	}
	for _, n := range []*fakeNode{a, b} {
		n.setFinalized(200)
	}
	if err := second.CheckFinality(ctx, chainPool); err != nil {
		t.Fatal(err)
	}
	if reports != 1 {
		t.Fatalf("expected the restored deposit reported once, got %d", reports)
	}

	// A third run rescans the same block (an operator moved the cursor
	// back): the deposit is already reported and is not tracked again.
	third := durableTracker(t, pool, func(context.Context, finality.Candidate) error { reports++; return nil })
	if err := candidates.ScanRange(ctx, chainPool, pool, fakeQuotes{amount: 3000_000000}, third, cfg, 200, 200); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if third.PendingCount() != 0 {
		t.Fatalf("an already-reported deposit was tracked again on rescan")
	}
}
