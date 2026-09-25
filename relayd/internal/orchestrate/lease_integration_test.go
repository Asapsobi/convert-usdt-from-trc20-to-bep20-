//go:build integration

package orchestrate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"relayd/internal/alert"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

// releasingWatcher is a deposit watcher that also records wallet releases.
type releasingWatcher struct {
	*fakeBEP20DepositWatcher
	mu       sync.Mutex
	released map[int64]string // order id -> reason
	err      error
}

func (r *releasingWatcher) RetireAddress(ctx context.Context, orderID int64, reason, idempotencyKey string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.released[orderID] = reason
	return nil
}

func (r *releasingWatcher) reason(orderID int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.released[orderID]
}

// An order nobody paid for is expired once its deposit window and grace
// pass: C1 expires it (so a late payment can't fund it) and its wallet
// goes back to the pool.
func TestLease_UnpaidLegExpiresAndReleasesItsWallet(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	ctx := context.Background()

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrderBEP20ToTRC20(externalID, "cust-expire")
	index := uint32(71)
	if _, err := store.Create(ctx, relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.BEP20ToTRC20, CustomerID: "cust-expire",
		DestinationAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", DepositAddress: signing.FakeBSCDepositAddress(index),
		DepositDerivationIndex: &index,
		AmountIn:               money.Amount{Asset: money.USDT_BEP20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_TRC20, Units: 99_700000},
	}); err != nil {
		t.Fatal(err)
	}
	watcher := &releasingWatcher{fakeBEP20DepositWatcher: newFakeBEP20DepositWatcher(), released: map[int64]string{}}
	o := orchestrate.New(store, ledgerclient.New(ledger.BaseURL(), ledger.Token()), upstream.NewMockProvider("mock-expire", 1),
		newFakeEnergy(), signing.NewFakeSigningService(), &fakeChain{}, newFakeFinality(), &fakeEVMChain{}, newFakeEVMFinality(),
		alert.LogAlerter{}, watcher, nil, orchestrate.Config{SlotID: 1, DepositGrace: 30 * time.Minute})

	// Still inside the window: nothing happens.
	if err := o.RunTick(ctx); err != nil {
		t.Fatal(err)
	}
	if leg, _ := store.GetByExternalID(ctx, externalID); leg.Status != relay.StatusAwaitingDeposit {
		t.Fatalf("a leg inside its deposit window was touched: %s", leg.Status)
	}

	ledger.BackdateDeposit(externalID)
	if _, err := pool.Exec(ctx, `UPDATE relay_legs SET created_at = now() - interval '2 hours' WHERE external_id = $1`, externalID); err != nil {
		t.Fatal(err)
	}
	if err := o.RunTick(ctx); err != nil {
		t.Fatal(err)
	}
	leg, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if leg.Status != relay.StatusExpired {
		t.Fatalf("expected EXPIRED, got %s", leg.Status)
	}
	if got := ledger.GetOrder(externalID).State; got != "expired" {
		t.Fatalf("expected the C1 order expired, got %s", got)
	}
	if watcher.reason(order.ID) != "expired" || leg.LeaseReleasedAt == nil {
		t.Fatalf("expected the wallet released as expired (reason %q, released_at %v)", watcher.reason(order.ID), leg.LeaseReleasedAt)
	}
}

// A settled leg's wallet is handed back once, and a watcher that has no
// lease for it (404) doesn't block the release forever.
func TestLease_FinishedLegReleasesItsWalletOnce(t *testing.T) {
	f := newBEP20Fixture(t, 72)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE relay_legs SET status = 'SETTLED' WHERE external_id = $1`, f.externalID); err != nil {
		t.Fatal(err)
	}
	watcher := &releasingWatcher{fakeBEP20DepositWatcher: f.watcher, released: map[int64]string{},
		err: &watcherclient.APIError{Status: 404, Code: "order_not_found"}}
	o := orchestrate.New(f.store, f.client, f.vendor, newFakeEnergy(), f.signer, &fakeChain{}, newFakeFinality(),
		f.chain, f.finality, f.alerts, watcher, nil, orchestrate.Config{SlotID: 1})
	if err := o.RunTick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.leg().LeaseReleasedAt == nil {
		t.Fatal("a 404 from the watcher (no lease to release) should still mark the lease released")
	}

	// A second finished leg, released normally.
	g := newBEP20Fixture(t, 73)
	if _, err := g.pool.Exec(ctx, `UPDATE relay_legs SET status = 'REFUNDED' WHERE external_id = $1`, g.externalID); err != nil {
		t.Fatal(err)
	}
	ok := &releasingWatcher{fakeBEP20DepositWatcher: g.watcher, released: map[int64]string{}}
	o = orchestrate.New(g.store, g.client, g.vendor, newFakeEnergy(), g.signer, &fakeChain{}, newFakeFinality(),
		g.chain, g.finality, g.alerts, ok, nil, orchestrate.Config{SlotID: 1})
	for i := 0; i < 2; i++ {
		if err := o.RunTick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if ok.reason(g.orderID) != "refunded" || g.leg().LeaseReleasedAt == nil {
		t.Fatalf("expected the refunded leg's wallet released (reason %q)", ok.reason(g.orderID))
	}
}
