//go:build integration

package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gateway/internal/c2client"
	"gateway/internal/customers"
	"gateway/internal/db"
	"gateway/internal/money"
	"gateway/internal/orders"
	"gateway/internal/pricing"
	"gateway/internal/quotes"
)

// validTronRecipient mirrors internal/httpapi's own identical constant
// (same value, same reasoning -- see that package's own doc comment):
// internal/quotes.Store.Create now validates recipient_address for real
// (internal/tronaddr, base58check), so a placeholder string like the
// old "TRecipient" no longer passes even called directly as an internal
// Go caller, which is exactly this file's own call shape -- proof the
// invariant holds for internal callers, not just HTTP handlers.
const validTronRecipient = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

// fakeC2 stands in for C2's own address-assignment endpoints, toggled
// between failing and succeeding so a test can prove a stuck row
// resolves the moment the fake starts succeeding.
type fakeC2 struct {
	fail        atomic.Bool
	assignCalls atomic.Int32
	getCalls    atomic.Int32
}

func newFakeC2(t *testing.T) (*httptest.Server, *fakeC2) {
	t.Helper()
	f := &fakeC2{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/addresses", func(w http.ResponseWriter, r *http.Request) {
		f.assignCalls.Add(1)
		if f.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"code":"internal","message":"forced failure"}}`))
			return
		}
		var body struct {
			OrderID    int64  `json:"order_id"`
			ExternalID string `json:"external_id"`
			CustomerID string `json:"customer_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(c2client.WatchedAddress{
			Address: "0xRESOLVED", OrderID: body.OrderID, ExternalID: body.ExternalID, CustomerID: body.CustomerID,
			Status: "WATCHING", AssignedAt: time.Now().UTC(),
		})
	})
	mux.HandleFunc("/v1/addresses/", func(w http.ResponseWriter, r *http.Request) {
		f.getCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"not_found","message":"no address assigned"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

func TestTick_RetriesUntilFakeSucceeds(t *testing.T) {
	pool := testPool(t)
	custStore := customers.NewStore(pool)
	quotesStore := quotes.NewStore(pool)
	ordersStore := orders.NewStore(pool)

	cust, _, err := custStore.Create(t.Context(), "reconcile-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}

	q, err := quotesStore.Create(t.Context(), cust.ID, pricing.Quote{
		Tier: pricing.Standard, AmountIn: money.Amount(3000_000000), FeeUnits: money.Amount(7_500000),
		NetworkFeeUnits: money.Amount(1_800000), AmountOut: money.Amount(2990_700000),
	}, validTronRecipient, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatalf("Create quote: %v", err)
	}

	externalID := fmt.Sprintf("ext-reconcile-%d", time.Now().UnixNano())
	gatewayOrder, err := ordersStore.Create(t.Context(), externalID, cust.ID, q.ID, 4242)
	if err != nil {
		t.Fatalf("Create gateway order: %v", err)
	}
	if gatewayOrder.C2AddressAssigned {
		t.Fatalf("fresh row should not already have an address assigned")
	}
	backdateCreatedAt(t, pool, externalID)

	srv, fake := newFakeC2(t)
	fake.fail.Store(true)
	watcher := c2client.New(srv.URL, "unused")

	r := NewReconciler(ordersStore, quotesStore, watcher, nil, Config{GracePeriod: time.Millisecond})

	if err := r.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (failing fake): %v", err)
	}
	row, err := ordersStore.Get(t.Context(), externalID)
	if err != nil {
		t.Fatalf("Orders.Get: %v", err)
	}
	if row.C2AddressAssigned {
		t.Fatalf("row resolved despite fake C2 still failing")
	}
	// gateway_test is a real, persistent database shared across test
	// runs (see C6.2's own "no reset between tests" note), so other
	// stuck rows from earlier runs may also be pending -- PendingCount
	// is checked as a lower bound, not an exact count.
	if r.PendingCount() < 1 {
		t.Errorf("PendingCount = %d, want >= 1", r.PendingCount())
	}

	fake.fail.Store(false)
	if err := r.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (succeeding fake): %v", err)
	}
	row, err = ordersStore.Get(t.Context(), externalID)
	if err != nil {
		t.Fatalf("Orders.Get: %v", err)
	}
	if !row.C2AddressAssigned {
		t.Fatalf("row did not resolve once the fake started succeeding")
	}
	if row.DepositAddress == nil || *row.DepositAddress != "0xRESOLVED" {
		t.Errorf("DepositAddress = %v, want 0xRESOLVED", row.DepositAddress)
	}
}

func TestTick_AlertsExactlyOncePastThreshold(t *testing.T) {
	pool := testPool(t)
	custStore := customers.NewStore(pool)
	quotesStore := quotes.NewStore(pool)
	ordersStore := orders.NewStore(pool)

	cust, _, err := custStore.Create(t.Context(), "alert-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}
	q, err := quotesStore.Create(t.Context(), cust.ID, pricing.Quote{
		Tier: pricing.Standard, AmountIn: money.Amount(3000_000000), FeeUnits: money.Amount(7_500000),
		NetworkFeeUnits: money.Amount(1_800000), AmountOut: money.Amount(2990_700000),
	}, validTronRecipient, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatalf("Create quote: %v", err)
	}
	externalID := fmt.Sprintf("ext-alert-%d", time.Now().UnixNano())
	if _, err := ordersStore.Create(t.Context(), externalID, cust.ID, q.ID, 4243); err != nil {
		t.Fatalf("Create gateway order: %v", err)
	}
	backdateCreatedAt(t, pool, externalID)

	srv, fake := newFakeC2(t)
	fake.fail.Store(true) // stays stuck for both ticks
	watcher := c2client.New(srv.URL, "unused")

	alerter := &countingAlerter{}
	r := NewReconciler(ordersStore, quotesStore, watcher, alerter, Config{GracePeriod: time.Millisecond, AlertThreshold: time.Millisecond})

	if err := r.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if err := r.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if got := alerter.calls.Load(); got != 1 {
		t.Errorf("alert fired %d times across two ticks, want exactly 1", got)
	}
}

type countingAlerter struct {
	calls atomic.Int32
}

func (a *countingAlerter) AlertAddressPending(ctx context.Context, o orders.GatewayOrder, age time.Duration) {
	a.calls.Add(1)
}

// backdateCreatedAt pushes a fresh row's created_at into the past so it
// clears reconcile's own grace period immediately -- avoids a real
// sleep in the test.
func backdateCreatedAt(t *testing.T, pool *db.Pool, externalID string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `UPDATE gateway_orders SET created_at = now() - interval '1 hour' WHERE external_id = $1`, externalID); err != nil {
		t.Fatalf("backdating created_at: %v", err)
	}
}
