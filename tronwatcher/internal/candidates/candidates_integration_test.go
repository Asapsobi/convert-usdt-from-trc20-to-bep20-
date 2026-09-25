//go:build integration

// Requires a real, reachable Postgres (TRONWATCHER_TEST_DATABASE_URL):
// attributing a transfer to a lease, and recording it durably, both live
// in the database.
package candidates

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/tyler-smith/go-bip32"

	"tronwatcher/internal/addresses"
	"tronwatcher/internal/chain"
	"tronwatcher/internal/deposits"
	"tronwatcher/internal/finality"
	"tronwatcher/internal/money"
)

type fakeQuotes struct {
	amounts map[string]money.Amount
	err     error
}

func (f fakeQuotes) QuotedAmount(ctx context.Context, externalID string) (money.Amount, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.amounts[externalID], nil
}

type fakeRecorder struct{}

func (fakeRecorder) RecordOrphanedDeposit(context.Context, finality.Candidate, error) error {
	return nil
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TRONWATCHER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TRONWATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}
	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	_, thisFile, _, _ := runtime.Caller(0)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	if _, err := sqlDB.Exec(`UPDATE pool_settings SET max_wallets = 1000000, cooldown_after_use = '0', cooldown_after_expiry = '0'`); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	master, err := bip32.NewMasterKey([]byte("tronwatcher candidates integration test fixture -- never use"))
	if err != nil {
		t.Fatal(err)
	}
	if err := addresses.Configure(master.PublicKey().B58Serialize()); err != nil {
		t.Fatal(err)
	}
	return pool
}

var orderSeq int64

func newLease(t *testing.T, pool *pgxpool.Pool) addresses.WatchedAddress {
	t.Helper()
	orderID := time.Now().UnixNano() + atomic.AddInt64(&orderSeq, 1)
	now := time.Now().UTC()
	if _, err := addresses.Assign(context.Background(), pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	wa, err := addresses.GetByOrderID(context.Background(), pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	return wa
}

func newTracker(t *testing.T, pool *pgxpool.Pool) *finality.Tracker {
	t.Helper()
	tr, err := finality.New(finality.Config{
		OnFinal:                 func(context.Context, finality.Candidate) error { return nil },
		OrphanedDepositRecorder: fakeRecorder{},
		Store:                   deposits.NewStore(pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func transferTo(addr addresses.Address, units int64, at time.Time) chain.Transfer {
	return chain.Transfer{
		TxID: fmt.Sprintf("tx-%d", time.Now().UnixNano()), From: "TSenderAddressForTests1111111111", To: string(addr),
		ValueRaw: big.NewInt(units), BlockTimestamp: at, ContractAddress: chain.USDTTRC20ContractAddress,
	}
}

func orphanReason(t *testing.T, pool *pgxpool.Pool, txID string) string {
	t.Helper()
	var reason string
	if err := pool.QueryRow(context.Background(), `SELECT order_state_at_detection FROM orphaned_deposits WHERE tx_id = $1`, txID).Scan(&reason); err != nil {
		return ""
	}
	return reason
}

var cfg = Config{ContractAddress: chain.USDTTRC20ContractAddress, DustFloor: chain.DefaultDustFloor}

// A transfer during an open lease is tracked, durably, and a restarted
// tracker picks it back up.
func TestProcessTransfer_DuringLeaseIsTrackedAndSurvivesARestart(t *testing.T) {
	pool := testPool(t)
	lease := newLease(t, pool)
	quotes := fakeQuotes{amounts: map[string]money.Amount{lease.ExternalID: 100_000000}}

	tr := newTracker(t, pool)
	transfer := transferTo(lease.Address, 100_000000, time.Now().UTC())
	if err := processTransfer(context.Background(), pool, quotes, tr, cfg, lease.Address, transfer); err != nil {
		t.Fatalf("processTransfer: %v", err)
	}
	if tr.PendingCount() != 1 {
		t.Fatalf("expected 1 pending candidate, got %d", tr.PendingCount())
	}

	restarted := newTracker(t, pool)
	if _, err := restarted.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.PendingCount() < 1 {
		t.Fatal("the detected deposit did not survive a restart")
	}
	// Seen again (an overlapping re-scan): not tracked twice.
	again := newTracker(t, pool)
	if err := processTransfer(context.Background(), pool, quotes, again, cfg, lease.Address, transfer); err != nil {
		t.Fatal(err)
	}
	if again.PendingCount() != 1 {
		t.Fatalf("a re-read transfer should be tracked once (still detected), got %d", again.PendingCount())
	}
}

// A payment after the lease ended funds nothing: it is recorded as
// orphaned for an operator.
func TestProcessTransfer_AfterTheLeaseEndedIsOrphaned(t *testing.T) {
	pool := testPool(t)
	lease := newLease(t, pool)
	if err := addresses.Retire(context.Background(), pool, lease.OrderID, "settled"); err != nil {
		t.Fatal(err)
	}
	tr := newTracker(t, pool)
	transfer := transferTo(lease.Address, 100_000000, time.Now().UTC().Add(time.Second))
	if err := processTransfer(context.Background(), pool, fakeQuotes{}, tr, cfg, lease.Address, transfer); err != nil {
		t.Fatal(err)
	}
	if tr.PendingCount() != 0 || orphanReason(t, pool, transfer.TxID) != "address_retired" {
		t.Fatalf("expected an address_retired orphan and nothing tracked (pending %d, reason %q)", tr.PendingCount(), orphanReason(t, pool, transfer.TxID))
	}
}

// Dust never funds an order (address poisoning, stray transfers).
func TestProcessTransfer_DustIsOrphaned(t *testing.T) {
	pool := testPool(t)
	lease := newLease(t, pool)
	quotes := fakeQuotes{amounts: map[string]money.Amount{lease.ExternalID: 100_000000}}
	tr := newTracker(t, pool)
	transfer := transferTo(lease.Address, 1000, time.Now().UTC())
	if err := processTransfer(context.Background(), pool, quotes, tr, cfg, lease.Address, transfer); err != nil {
		t.Fatal(err)
	}
	if tr.PendingCount() != 0 || orphanReason(t, pool, transfer.TxID) != "dust" {
		t.Fatalf("expected a dust orphan and nothing tracked")
	}
}

// A payment to a wallet before any lease on it started is orphaned.
func TestProcessTransfer_NoLeaseIsOrphaned(t *testing.T) {
	pool := testPool(t)
	lease := newLease(t, pool)
	tr := newTracker(t, pool)
	transfer := transferTo(lease.Address, 100_000000, lease.AssignedAt.Add(-time.Hour))
	if err := processTransfer(context.Background(), pool, fakeQuotes{}, tr, cfg, lease.Address, transfer); err != nil {
		t.Fatal(err)
	}
	if tr.PendingCount() != 0 || orphanReason(t, pool, transfer.TxID) != "no_lease" {
		t.Fatalf("expected a no_lease orphan and nothing tracked")
	}
}

func TestProcessTransfer_QuotedAmountFetchFailurePropagates(t *testing.T) {
	pool := testPool(t)
	lease := newLease(t, pool)
	tr := newTracker(t, pool)
	transfer := transferTo(lease.Address, 100_000000, time.Now().UTC())
	err := processTransfer(context.Background(), pool, fakeQuotes{err: errors.New("ledger down")}, tr, cfg, lease.Address, transfer)
	if err == nil {
		t.Fatal("expected the quote lookup failure to propagate (so the cursor doesn't move past this transfer)")
	}
}
