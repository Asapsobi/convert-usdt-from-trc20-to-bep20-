//go:build integration

// Requires a real, reachable Postgres 16 instance for both relayd's own
// database and a real ledgerd (built from the sibling ledger module).
package driver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"relayd/internal/db"
	"relayd/internal/driver"
	"relayd/internal/ledgerclient"
	"relayd/internal/relay"
	"relayd/internal/testledger"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

var portSeq int64

func startLedger(t *testing.T) *testledger.Ledger {
	t.Helper()
	dbURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	return testledger.Start(t, dbURL, fmt.Sprintf(":%d", 19600+port), "driver-test-token", "relayd")
}

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("RELAYD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("RELAYD_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeWatcherServer stands in for tronwatcher's/depositwatcher's real
// POST /v1/addresses -- both share the identical contract (see
// internal/watcherclient's own package doc comment), so one fake
// server shape covers testing against either.
func fakeWatcherServer(t *testing.T, depositAddress string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			OrderID    int64  `json:"order_id"`
			ExternalID string `json:"external_id"`
			CustomerID string `json:"customer_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{
			"address": depositAddress, "order_id": req.OrderID, "external_id": req.ExternalID,
			"customer_id": req.CustomerID, "status": "WATCHING",
		})
	}))
}

var extIDSeq int64

func uniqueExternalID(t *testing.T) string {
	n := atomic.AddInt64(&extIDSeq, 1)
	return fmt.Sprintf("driver-ext:%s:%d:%d", t.Name(), time.Now().UnixNano(), n)
}

func TestCreateRelayLeg_TRC20ToBEP20(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	tronSrv := fakeWatcherServer(t, "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	defer tronSrv.Close()
	bep20Srv := fakeWatcherServer(t, "0xshould-not-be-called")
	defer bep20Srv.Close()

	mockProvider := upstream.NewMockProvider("mock", 1)

	d := &driver.Driver{
		Ledger: client, Upstream: mockProvider,
		TronWatcher:  watcherclient.New(tronSrv.URL, "tok"),
		BEP20Watcher: watcherclient.New(bep20Srv.URL, "tok"),
		Store:        store,
		Cfg:          driver.Config{FeeBasisPoints: 30, QuoteValidity: 10 * time.Minute},
	}

	externalID := uniqueExternalID(t)
	result, err := d.CreateRelayLeg(context.Background(), driver.CreateRelayLegRequest{
		ExternalID: externalID, CustomerID: "cust-driver-1", Direction: relay.TRC20ToBEP20,
		DestinationAddress: "0xcustomer-bep20-address", AmountIn: "100.000000",
	})
	if err != nil {
		t.Fatalf("CreateRelayLeg: %v", err)
	}
	if result.DepositAddress != "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj" {
		t.Errorf("expected the TRON watcher's own deposit address, got %s", result.DepositAddress)
	}
	if result.AmountIn != "100.000000" {
		t.Errorf("expected amount_in 100.000000, got %s", result.AmountIn)
	}
	// 30 bps of 100 = 0.3
	if result.FeeUnits != "0.300000" {
		t.Errorf("expected fee_units 0.300000, got %s", result.FeeUnits)
	}

	order := ledger.GetOrder(externalID)
	if order.State != "quoted" {
		t.Errorf("expected order state quoted, got %s", order.State)
	}

	leg, err := store.GetByExternalID(context.Background(), externalID)
	if err != nil {
		t.Fatalf("GetByExternalID: %v", err)
	}
	if leg.Status != relay.StatusAwaitingDeposit {
		t.Errorf("expected AWAITING_DEPOSIT, got %s", leg.Status)
	}
	if leg.Direction != relay.TRC20ToBEP20 {
		t.Errorf("expected TRC20_TO_BEP20, got %s", leg.Direction)
	}
	if leg.DepositAddress != "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj" {
		t.Errorf("expected the TRON watcher's own address recorded on the leg, got %s", leg.DepositAddress)
	}

	// GetStatus should reconstruct the same picture.
	status, err := d.GetStatus(context.Background(), externalID)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.Order.State != "quoted" || status.Leg.Status != relay.StatusAwaitingDeposit {
		t.Errorf("unexpected status: %+v", status)
	}
}

// TestListLegs covers the ops console's own read path
// (opsconsole/internal/opclient.RelaydClient.ListRelayLegs, over HTTP,
// against httpapi.getRelayLegs, which calls this same driver method) --
// creates two legs, confirms List(nil) returns (at least) both and
// List(&StatusAwaitingDeposit) filters correctly.
func TestListLegs(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	tronSrv := fakeWatcherServer(t, "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	defer tronSrv.Close()

	d := &driver.Driver{
		Ledger: client, Upstream: upstream.NewMockProvider("mock", 1),
		TronWatcher: watcherclient.New(tronSrv.URL, "tok"), BEP20Watcher: watcherclient.New(tronSrv.URL, "tok"),
		Store: store, Cfg: driver.Config{FeeBasisPoints: 30, QuoteValidity: 10 * time.Minute},
	}

	ext1 := uniqueExternalID(t)
	if _, err := d.CreateRelayLeg(context.Background(), driver.CreateRelayLegRequest{
		ExternalID: ext1, CustomerID: "cust-list-1", Direction: relay.TRC20ToBEP20,
		DestinationAddress: "0xdest1", AmountIn: "50.000000",
	}); err != nil {
		t.Fatalf("CreateRelayLeg 1: %v", err)
	}
	ext2 := uniqueExternalID(t)
	if _, err := d.CreateRelayLeg(context.Background(), driver.CreateRelayLegRequest{
		ExternalID: ext2, CustomerID: "cust-list-2", Direction: relay.TRC20ToBEP20,
		DestinationAddress: "0xdest2", AmountIn: "75.000000",
	}); err != nil {
		t.Fatalf("CreateRelayLeg 2: %v", err)
	}

	all, err := d.ListLegs(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListLegs(nil): %v", err)
	}
	foundBoth := 0
	for _, l := range all {
		if l.ExternalID == ext1 || l.ExternalID == ext2 {
			foundBoth++
		}
	}
	if foundBoth != 2 {
		t.Fatalf("expected both newly created legs in ListLegs(nil), found %d", foundBoth)
	}

	awaiting := relay.StatusAwaitingDeposit
	filtered, err := d.ListLegs(context.Background(), &awaiting)
	if err != nil {
		t.Fatalf("ListLegs(AWAITING_DEPOSIT): %v", err)
	}
	for _, l := range filtered {
		if l.Status != relay.StatusAwaitingDeposit {
			t.Errorf("ListLegs(AWAITING_DEPOSIT) returned a leg with status %s", l.Status)
		}
	}

	settled := relay.StatusSettled
	none, err := d.ListLegs(context.Background(), &settled)
	if err != nil {
		t.Fatalf("ListLegs(SETTLED): %v", err)
	}
	for _, l := range none {
		if l.ExternalID == ext1 || l.ExternalID == ext2 {
			t.Errorf("expected neither fresh leg to appear under SETTLED, found %s", l.ExternalID)
		}
	}
}

func TestCreateRelayLeg_RejectsNonPositiveAmount(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	tronSrv := fakeWatcherServer(t, "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	defer tronSrv.Close()

	d := &driver.Driver{
		Ledger: client, Upstream: upstream.NewMockProvider("mock", 1),
		TronWatcher: watcherclient.New(tronSrv.URL, "tok"), BEP20Watcher: watcherclient.New(tronSrv.URL, "tok"),
		Store: store, Cfg: driver.Config{FeeBasisPoints: 30, QuoteValidity: 10 * time.Minute},
	}

	_, err := d.CreateRelayLeg(context.Background(), driver.CreateRelayLegRequest{
		ExternalID: uniqueExternalID(t), CustomerID: "cust-1", Direction: relay.TRC20ToBEP20,
		DestinationAddress: "0xdest", AmountIn: "0.000000",
	})
	if err == nil {
		t.Fatal("expected an error for a zero amount_in")
	}
}
