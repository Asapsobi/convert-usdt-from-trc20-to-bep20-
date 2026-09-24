//go:build integration

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gateway/internal/c1client"
	"gateway/internal/c2client"
	"gateway/internal/customers"
	"gateway/internal/orders"
	"gateway/internal/quotes"
	"gateway/internal/ratelimit"
)

// fakeC1Full stands in for C1's own order-creation endpoint (plus halt,
// reused from fakeC1) -- same "assert by call count against fakes"
// convention c6-api-gateway-build-prompts.md's own C6.3 acceptance
// criteria call for explicitly.
type fakeC1Full struct {
	halted      bool
	createCalls int32
	getCalls    int32
	nextOrderID int64
	failCreate  bool
	getState    atomic.Value // string; C6.5's own status handler reads this via GetOrder
}

func newFakeC1Full(t *testing.T, f *fakeC1Full) *httptest.Server {
	t.Helper()
	if f.nextOrderID == 0 {
		f.nextOrderID = 1
	}
	f.getState.Store("quoted")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/system/halt", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(c1client.HaltState{Halted: f.halted})
	})
	mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.createCalls, 1)
		if f.failCreate {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"code":"internal","message":"forced failure"}}`))
			return
		}
		var body struct {
			ExternalID       string `json:"external_id"`
			CustomerID       string `json:"customer_id"`
			Tier             string `json:"tier"`
			AmountIn         string `json:"amount_in"`
			AmountOut        string `json:"amount_out"`
			FeeUnits         string `json:"fee_units"`
			NetworkFeeUnits  string `json:"network_fee_units"`
			RecipientAddress string `json:"recipient_address"`
			QuotedAt         string `json:"quoted_at"`
			QuoteExpiresAt   string `json:"quote_expires_at"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		id := atomic.AddInt64(&f.nextOrderID, 1) - 1
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "external_id": body.ExternalID, "customer_id": body.CustomerID, "tier": body.Tier, "state": "quoted",
			"amount_in": body.AmountIn, "amount_out": body.AmountOut, "fee_units": body.FeeUnits, "network_fee_units": body.NetworkFeeUnits,
			"recipient_address": body.RecipientAddress, "quoted_at": body.QuotedAt, "quote_expires_at": body.QuoteExpiresAt, "version": 0,
		})
	})
	mux.HandleFunc("GET /v1/orders/{external_id}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.getCalls, 1)
		externalID := r.PathValue("external_id")
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "external_id": externalID, "customer_id": "1", "tier": "STANDARD", "state": f.getState.Load().(string),
			"amount_in": "3000.000000", "amount_out": "2990.700000", "fee_units": "7.500000", "network_fee_units": "1.800000",
			"recipient_address": validTronRecipient, "quoted_at": time.Now().UTC().Format(time.RFC3339), "quote_expires_at": time.Now().UTC().Format(time.RFC3339), "version": 0,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeC2 stands in for C2's own address-assignment endpoint.
type fakeC2 struct {
	assignCalls int32
	failAssign  bool
}

func newFakeC2(t *testing.T, f *fakeC2) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/addresses", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.assignCalls, 1)
		if f.failAssign {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"code":"internal","message":"forced failure"}}`))
			return
		}
		var body struct {
			OrderID        int64  `json:"order_id"`
			ExternalID     string `json:"external_id"`
			CustomerID     string `json:"customer_id"`
			QuotedAt       string `json:"quoted_at"`
			QuoteExpiresAt string `json:"quote_expires_at"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(c2client.WatchedAddress{
			Address: "0xDEPOSITADDRESS", OrderID: body.OrderID, ExternalID: body.ExternalID, CustomerID: body.CustomerID,
			Status: "WATCHING", AssignedAt: time.Now().UTC(),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newOrdersServer(t *testing.T, c1URL, c2URL string) (*Server, *customers.Store) {
	pool := testPool(t)
	custStore := customers.NewStore(pool)
	s := &Server{
		Pool:        pool,
		Customers:   custStore,
		RateLimiter: ratelimit.New(),
		Quotes:      quotes.NewStore(pool),
		Orders:      orders.NewStore(pool),
		Ledger:      c1client.New(c1URL, "unused"),
		Watcher:     c2client.New(c2URL, "unused"),
	}
	return s, custStore
}

// uniqueExternalID keeps external_id (a real primary key in a real,
// persistent gateway_test database that is never reset between test
// runs) from colliding with a leftover row from a previous run.
func uniqueExternalID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func issueQuote(t *testing.T, router http.Handler, rawKey string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(`{"tier":"STANDARD","amount_in":"3000.000000","recipient_address":"`+validTronRecipient+`"}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issuing quote: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

// TestPostOrder_IdempotentReplay is C6.3's own acceptance criterion:
// same Idempotency-Key + same body called twice -> identical response
// both times, C1 and C2 each called exactly once.
func TestPostOrder_IdempotentReplay(t *testing.T) {
	c1f := &fakeC1Full{}
	c1 := newFakeC1Full(t, c1f)
	c2f := &fakeC2{}
	c2 := newFakeC2(t, c2f)
	s, custStore := newOrdersServer(t, c1.URL, c2.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "order-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}
	quote := issueQuote(t, router, rawKey)

	externalID := uniqueExternalID("ext-idem")
	body := `{"quote_id":` + strconv.Itoa(int(quote["quote_id"].(float64))) + `,"external_id":"` + externalID + `"}`
	var bodies []string
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rawKey)
		req.Header.Set("Idempotency-Key", "idem-key-1")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /v1/orders (%d): status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("responses differ between the two identical requests:\n1: %s\n2: %s", bodies[0], bodies[1])
	}
	if got := atomic.LoadInt32(&c1f.createCalls); got != 1 {
		t.Errorf("C1 POST /v1/orders call count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&c2f.assignCalls); got != 1 {
		t.Errorf("C2 POST /v1/addresses call count = %d, want 1", got)
	}
}

// TestPostOrder_C2FailureLeavesAddressPending is C6.3's own acceptance
// criterion: step 5 forced to fail -> response still comes back with
// address_pending, gateway_orders shows c1_order_created=true,
// c2_address_assigned=false, and a second identical request does NOT
// create a second C1 order.
func TestPostOrder_C2FailureLeavesAddressPending(t *testing.T) {
	c1f := &fakeC1Full{}
	c1 := newFakeC1Full(t, c1f)
	c2f := &fakeC2{failAssign: true}
	c2 := newFakeC2(t, c2f)
	s, custStore := newOrdersServer(t, c1.URL, c2.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "pending-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}
	quote := issueQuote(t, router, rawKey)
	externalID := uniqueExternalID("ext-pending")
	body := `{"quote_id":` + strconv.Itoa(int(quote["quote_id"].(float64))) + `,"external_id":"` + externalID + `"}`

	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Idempotency-Key", "idem-key-2")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "address_pending" {
		t.Errorf("status = %v, want address_pending", resp["status"])
	}
	if resp["deposit_address"] != nil {
		t.Errorf("deposit_address = %v, want null", resp["deposit_address"])
	}

	row, err := s.Orders.Get(t.Context(), externalID)
	if err != nil {
		t.Fatalf("Orders.Get: %v", err)
	}
	if !row.C1OrderCreated || row.C2AddressAssigned {
		t.Errorf("gateway_orders row = %+v, want c1_order_created=true c2_address_assigned=false", row)
	}

	// A second identical request must not create a second C1 order.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+rawKey)
	req2.Header.Set("Idempotency-Key", "idem-key-2")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("retry: status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	if got := atomic.LoadInt32(&c1f.createCalls); got != 1 {
		t.Errorf("C1 POST /v1/orders call count after retry = %d, want still 1", got)
	}
}

// TestPostOrder_ExpiredQuoteRejectedWithNoUpstreamCalls is C6.3's own
// acceptance criterion: expired quote -> 409, no C1 or C2 call made at
// all.
func TestPostOrder_ExpiredQuoteRejectedWithNoUpstreamCalls(t *testing.T) {
	c1f := &fakeC1Full{}
	c1 := newFakeC1Full(t, c1f)
	c2f := &fakeC2{}
	c2 := newFakeC2(t, c2f)
	s, custStore := newOrdersServer(t, c1.URL, c2.URL)
	// quoteValidity() treats <= 0 as "use the default," so a truly-expired
	// fixture needs a tiny positive validity plus a real sleep past it,
	// not a negative value.
	s.QuoteValidity = time.Millisecond
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "expired-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}
	quote := issueQuote(t, router, rawKey)
	time.Sleep(10 * time.Millisecond)

	body := `{"quote_id":` + strconv.Itoa(int(quote["quote_id"].(float64))) + `,"external_id":"` + uniqueExternalID("ext-expired") + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Idempotency-Key", "idem-key-3")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}

	if got := atomic.LoadInt32(&c1f.createCalls); got != 0 {
		t.Errorf("C1 POST /v1/orders call count = %d, want 0 (halt check itself also never reached C1's create endpoint)", got)
	}
	if got := atomic.LoadInt32(&c2f.assignCalls); got != 0 {
		t.Errorf("C2 POST /v1/addresses call count = %d, want 0", got)
	}
}
