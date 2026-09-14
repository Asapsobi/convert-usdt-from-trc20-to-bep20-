//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gateway/internal/c1client"
	"gateway/internal/c2client"
	"gateway/internal/customers"
	"gateway/internal/orders"
	"gateway/internal/quotes"
	"gateway/internal/ratelimit"
	"gateway/internal/retailcustomers"
	"gateway/internal/retailsessions"
)

// newRetailServer wires a full Server with BOTH the B2B (customers) and
// B2C (retailcustomers/retailsessions) stores live against the same
// real Postgres pool and the same fake C1/C2 -- deliberately, since the
// whole point of this file's own cross-owner tests is proving the two
// identity spaces never leak into each other despite sharing the exact
// same quotes/gateway_orders tables.
func newRetailServer(t *testing.T, c1URL, c2URL string) (*Server, *customers.Store, *retailcustomers.Store) {
	pool := testPool(t)
	custStore := customers.NewStore(pool)
	retailStore := retailcustomers.NewStore(pool)
	s := &Server{
		Pool: pool, Customers: custStore, RateLimiter: ratelimit.New(),
		Quotes: quotes.NewStore(pool), Orders: orders.NewStore(pool),
		Ledger: c1client.New(c1URL, "unused"), Watcher: c2client.New(c2URL, "unused"),
		RetailCustomers: retailStore, RetailSessions: retailsessions.NewStore(pool),
	}
	return s, custStore, retailStore
}

func retailRegister(t *testing.T, router http.Handler, email, password string) (sessionToken string, retailCustomerID int64) {
	t.Helper()
	body := `{"email":"` + email + `","password":"` + password + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/retail/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp retailSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding register response: %v", err)
	}
	return resp.SessionToken, resp.RetailCustomerID
}

func retailIssueQuote(t *testing.T, router http.Handler, sessionToken string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/retail/quotes",
		strings.NewReader(`{"tier":"STANDARD","amount_in":"3000.000000","recipient_address":"TRecipient"}`))
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retail postQuote: status %d, body %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestRetailRegisterLoginLogout_FullFlow(t *testing.T) {
	c1 := &fakeC1Full{}
	c1Srv := newFakeC1Full(t, c1)
	c2Srv := newFakeC2(t, &fakeC2{})
	s, _, _ := newRetailServer(t, c1Srv.URL, c2Srv.URL)
	router := NewRouter(s)

	email := uniqueExternalID("retail") + "@example.com"
	token, retailID := retailRegister(t, router, email, "correct-horse-battery")
	if token == "" || retailID == 0 {
		t.Fatalf("expected a real session token and retail customer id, got token=%q id=%d", token, retailID)
	}

	// Duplicate registration fails.
	req := httptest.NewRequest(http.MethodPost, "/v1/retail/register", strings.NewReader(`{"email":"`+email+`","password":"whatever12"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate register: status %d, want 409", rec.Code)
	}

	// Wrong password fails.
	req = httptest.NewRequest(http.MethodPost, "/v1/retail/login", strings.NewReader(`{"email":"`+email+`","password":"wrong-password"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password login: status %d, want 401", rec.Code)
	}

	// Right password succeeds and issues a NEW session.
	req = httptest.NewRequest(http.MethodPost, "/v1/retail/login", strings.NewReader(`{"email":"`+email+`","password":"correct-horse-battery"}`))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body.String())
	}
	var loginResp retailSessionResponse
	json.Unmarshal(rec.Body.Bytes(), &loginResp)
	if loginResp.SessionToken == token {
		t.Fatal("login issued the SAME token as register -- expected a fresh session")
	}

	// /v1/retail/me confirms who the session belongs to.
	req = httptest.NewRequest(http.MethodGet, "/v1/retail/me", nil)
	req.Header.Set("Authorization", "Bearer "+loginResp.SessionToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/me: status %d", rec.Code)
	}
	var me retailMeResponse
	json.Unmarshal(rec.Body.Bytes(), &me)
	if me.RetailCustomerID != retailID {
		t.Errorf("/me returned retail_customer_id %d, want %d", me.RetailCustomerID, retailID)
	}

	// Logout revokes the session -- it stops working immediately after.
	req = httptest.NewRequest(http.MethodPost, "/v1/retail/logout", nil)
	req.Header.Set("Authorization", "Bearer "+loginResp.SessionToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: status %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/retail/me", nil)
	req.Header.Set("Authorization", "Bearer "+loginResp.SessionToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("using a revoked session: status %d, want 401", rec.Code)
	}
}

func TestRetailQuoteAndOrder_FullFlow(t *testing.T) {
	c1 := &fakeC1Full{}
	c1Srv := newFakeC1Full(t, c1)
	c2Srv := newFakeC2(t, &fakeC2{})
	s, _, _ := newRetailServer(t, c1Srv.URL, c2Srv.URL)
	router := NewRouter(s)

	email := uniqueExternalID("retail") + "@example.com"
	token, _ := retailRegister(t, router, email, "correct-horse-battery")

	quote := retailIssueQuote(t, router, token)
	quoteID := int64(quote["quote_id"].(float64))

	externalID := uniqueExternalID("retail-order")
	orderBody := `{"quote_id":` + strconv.FormatInt(quoteID, 10) + `,"external_id":"` + externalID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/retail/orders", strings.NewReader(orderBody))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("postRetailOrder: status %d, body %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/retail/orders/"+externalID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("getRetailOrderStatus: status %d, body %s", rec.Code, rec.Body.String())
	}
	var status orderStatusResponse
	json.Unmarshal(rec.Body.Bytes(), &status)
	if status.ExternalID != externalID {
		t.Errorf("status external_id = %q, want %q", status.ExternalID, externalID)
	}
}

// TestCrossOwnerIsolation_RetailAndB2BNeverSeeEachOthers is the
// security-critical test for the whole owner-split design (migration
// 0009): a retail customer and a B2B customer can share the exact same
// numeric id by coincidence (they're different sequences in different
// tables) -- this proves that coincidence can never leak one's order to
// the other, on either the B2B or the B2C status endpoint.
func TestCrossOwnerIsolation_RetailAndB2BNeverSeeEachOthers(t *testing.T) {
	c1 := &fakeC1Full{}
	c1Srv := newFakeC1Full(t, c1)
	c2Srv := newFakeC2(t, &fakeC2{})
	s, custStore, _ := newRetailServer(t, c1Srv.URL, c2Srv.URL)
	router := NewRouter(s)

	// A real B2B customer creates a real order.
	_, rawKey, err := custStore.Create(t.Context(), "acme-corp-"+uniqueExternalID(""))
	if err != nil {
		t.Fatalf("creating B2B customer: %v", err)
	}
	b2bQuote := issueQuote(t, router, rawKey)
	b2bQuoteID := int64(b2bQuote["quote_id"].(float64))
	b2bExternalID := uniqueExternalID("b2b-order")
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(
		`{"quote_id":`+strconv.FormatInt(b2bQuoteID, 10)+`,"external_id":"`+b2bExternalID+`"}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Idempotency-Key", "idem-"+b2bExternalID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("B2B postOrder: status %d, body %s", rec.Code, rec.Body.String())
	}

	// A real retail customer creates a real order of their own.
	retailToken, _ := retailRegister(t, router, uniqueExternalID("retail")+"@example.com", "correct-horse-battery")
	retailQuote := retailIssueQuote(t, router, retailToken)
	retailQuoteID := int64(retailQuote["quote_id"].(float64))
	retailExternalID := uniqueExternalID("retail-order")
	req = httptest.NewRequest(http.MethodPost, "/v1/retail/orders", strings.NewReader(
		`{"quote_id":`+strconv.FormatInt(retailQuoteID, 10)+`,"external_id":"`+retailExternalID+`"}`))
	req.Header.Set("Authorization", "Bearer "+retailToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retail postOrder: status %d, body %s", rec.Code, rec.Body.String())
	}

	// The retail customer must NOT be able to read the B2B order via the
	// retail status endpoint, even by external_id.
	req = httptest.NewRequest(http.MethodGet, "/v1/retail/orders/"+b2bExternalID, nil)
	req.Header.Set("Authorization", "Bearer "+retailToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("retail customer reading the B2B order: status %d, want 404 (ownership leak)", rec.Code)
	}

	// The B2B customer must NOT be able to read the retail order via the
	// B2B status endpoint.
	req = httptest.NewRequest(http.MethodGet, "/v1/orders/"+retailExternalID, nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("B2B customer reading the retail order: status %d, want 404 (ownership leak)", rec.Code)
	}

	// Each can still read their own.
	req = httptest.NewRequest(http.MethodGet, "/v1/retail/orders/"+retailExternalID, nil)
	req.Header.Set("Authorization", "Bearer "+retailToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retail customer reading their OWN order: status %d, want 200", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/orders/"+b2bExternalID, nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("B2B customer reading their OWN order: status %d, want 200", rec.Code)
	}
}

func TestRetailOrder_CannotReadAnotherRetailCustomersOrder(t *testing.T) {
	c1 := &fakeC1Full{}
	c1Srv := newFakeC1Full(t, c1)
	c2Srv := newFakeC2(t, &fakeC2{})
	s, _, _ := newRetailServer(t, c1Srv.URL, c2Srv.URL)
	router := NewRouter(s)

	tokenA, _ := retailRegister(t, router, uniqueExternalID("retail-a")+"@example.com", "correct-horse-battery")
	tokenB, _ := retailRegister(t, router, uniqueExternalID("retail-b")+"@example.com", "correct-horse-battery")

	quoteA := retailIssueQuote(t, router, tokenA)
	quoteAID := int64(quoteA["quote_id"].(float64))
	externalID := uniqueExternalID("retail-a-order")
	req := httptest.NewRequest(http.MethodPost, "/v1/retail/orders", strings.NewReader(
		`{"quote_id":`+strconv.FormatInt(quoteAID, 10)+`,"external_id":"`+externalID+`"}`))
	req.Header.Set("Authorization", "Bearer "+tokenA)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("postRetailOrder for A: status %d, body %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/retail/orders/"+externalID, nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("customer B reading customer A's order: status %d, want 404", rec.Code)
	}
}
