//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gateway/internal/customers"
	"gateway/internal/db"
	"gateway/internal/orders"
	"gateway/internal/ratelimit"
	"gateway/internal/sandbox"
)

// newSandboxServer builds a Server wired with what the sandbox routes
// need plus Orders -- TestSandbox_ExternalIDInvisibleToProductionStatus
// drives a real production GET /v1/orders/{id} request through
// getOrderStatus, which needs a real (non-nil) Orders store even when
// the row it's looking for was never written.
func newSandboxServer(t *testing.T, pool *db.Pool) (*Server, *customers.Store) {
	t.Helper()
	custStore := customers.NewStore(pool)
	s := &Server{
		Pool:        pool,
		Customers:   custStore,
		RateLimiter: ratelimit.New(),
		Orders:      orders.NewStore(pool),
		Sandbox:     sandbox.NewStore(pool),
	}
	return s, custStore
}

func postSandboxOrder(t *testing.T, router http.Handler, rawKey, trigger string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"tier":"STANDARD","amount_in":"3000.000000","recipient_address":"` + validTronRecipient + `","trigger":"` + trigger + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox/orders", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func strPtr(s string) *string { return &s }

// TestSandbox_AllFourTriggersDeterministic is C6.7's own named
// acceptance gate: "a single test suite run exercises all four
// triggers end to end and asserts each one's documented behavior."
func TestSandbox_AllFourTriggersDeterministic(t *testing.T) {
	pool := testPool(t)
	s, custStore := newSandboxServer(t, pool)
	router := NewRouter(s)

	_, rawKey, err := custStore.CreateSandbox(t.Context(), "sandbox-all-four-co")
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	cases := []struct {
		trigger          string
		wantState        string
		wantHoldReason   *string
		wantWebhookTries int
		wantExhausted    bool
	}{
		{trigger: "reorg", wantState: "quoted"},
		{trigger: "screening_hold", wantState: "held", wantHoldReason: strPtr("screening_hold_flagged")},
		{trigger: "energy_exhaustion", wantState: "dispatching"},
		{trigger: "retry_storm", wantState: "settled", wantWebhookTries: 8, wantExhausted: true},
	}

	for _, tc := range cases {
		t.Run(tc.trigger, func(t *testing.T) {
			rec := postSandboxOrder(t, router, rawKey, tc.trigger)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp sandboxOrderResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			if resp.State != tc.wantState {
				t.Errorf("State = %q, want %q", resp.State, tc.wantState)
			}
			if tc.wantHoldReason != nil {
				if resp.HoldReason == nil || *resp.HoldReason != *tc.wantHoldReason {
					t.Errorf("HoldReason = %v, want %q", resp.HoldReason, *tc.wantHoldReason)
				}
			} else if resp.HoldReason != nil {
				t.Errorf("HoldReason = %v, want nil", *resp.HoldReason)
			}
			if resp.WebhookAttempts != tc.wantWebhookTries {
				t.Errorf("WebhookAttempts = %d, want %d", resp.WebhookAttempts, tc.wantWebhookTries)
			}
			if resp.WebhookExhausted != tc.wantExhausted {
				t.Errorf("WebhookExhausted = %v, want %v", resp.WebhookExhausted, tc.wantExhausted)
			}
			if !strings.HasPrefix(resp.ExternalID, "sbx_") {
				t.Errorf("ExternalID = %q, want sbx_ prefix", resp.ExternalID)
			}

			// Re-fetching via GET must report the identical outcome --
			// "same request, same sequence of states... every time."
			getReq := httptest.NewRequest(http.MethodGet, "/v1/sandbox/orders/"+resp.ExternalID, nil)
			getReq.Header.Set("Authorization", "Bearer "+rawKey)
			getRec := httptest.NewRecorder()
			router.ServeHTTP(getRec, getReq)
			if getRec.Code != http.StatusOK {
				t.Fatalf("GET: status = %d, body = %s", getRec.Code, getRec.Body.String())
			}
			var getResp sandboxOrderResponse
			json.Unmarshal(getRec.Body.Bytes(), &getResp)
			if getResp.State != tc.wantState {
				t.Errorf("GET State = %q, want %q", getResp.State, tc.wantState)
			}
		})
	}
}

// TestSandbox_ExternalIDInvisibleToProductionStatus is invariant 4's
// own acceptance criterion: "a production GET /v1/orders/{external_id}
// for a sandbox id returns 404, never sandbox data."
func TestSandbox_ExternalIDInvisibleToProductionStatus(t *testing.T) {
	pool := testPool(t)
	s, custStore := newSandboxServer(t, pool)
	router := NewRouter(s)

	_, prodRawKey, err := custStore.Create(t.Context(), "sandbox-visibility-prod-co")
	if err != nil {
		t.Fatalf("Create production customer: %v", err)
	}
	_, sandboxRawKey, err := custStore.CreateSandbox(t.Context(), "sandbox-visibility-sbx-co")
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	rec := postSandboxOrder(t, router, sandboxRawKey, "reorg")
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating sandbox order: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp sandboxOrderResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)

	// The production status endpoint, queried by a REAL production
	// customer, for a sandbox external_id.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/orders/"+resp.ExternalID, nil)
	getReq.Header.Set("Authorization", "Bearer "+prodRawKey)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Errorf("production GET /v1/orders/{sandbox_id}: status = %d, want 404", getRec.Code)
	}
}

// TestSandbox_KeyIsolationBothDirections is invariant 4's own "never
// converted between the two": a sandbox key on a production route, and
// a production key on a sandbox route, are both refused.
func TestSandbox_KeyIsolationBothDirections(t *testing.T) {
	pool := testPool(t)
	s, custStore := newSandboxServer(t, pool)
	router := NewRouter(s)

	_, prodRawKey, err := custStore.Create(t.Context(), "sandbox-isolation-prod-co")
	if err != nil {
		t.Fatalf("Create production customer: %v", err)
	}
	_, sandboxRawKey, err := custStore.CreateSandbox(t.Context(), "sandbox-isolation-sbx-co")
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	// Sandbox key against a production route.
	quoteReq := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(`{"tier":"STANDARD","amount_in":"3000.000000","recipient_address":"`+validTronRecipient+`"}`))
	quoteReq.Header.Set("Authorization", "Bearer "+sandboxRawKey)
	quoteRec := httptest.NewRecorder()
	router.ServeHTTP(quoteRec, quoteReq)
	if quoteRec.Code != http.StatusForbidden {
		t.Errorf("sandbox key on POST /v1/quotes: status = %d, want 403", quoteRec.Code)
	}

	// Production key against a sandbox route.
	sandboxReq := httptest.NewRequest(http.MethodPost, "/v1/sandbox/orders", strings.NewReader(`{"tier":"STANDARD","amount_in":"3000.000000","recipient_address":"`+validTronRecipient+`","trigger":"reorg"}`))
	sandboxReq.Header.Set("Authorization", "Bearer "+prodRawKey)
	sandboxRec := httptest.NewRecorder()
	router.ServeHTTP(sandboxRec, sandboxReq)
	if sandboxRec.Code != http.StatusForbidden {
		t.Errorf("production key on POST /v1/sandbox/orders: status = %d, want 403", sandboxRec.Code)
	}
}

func TestSandbox_UnknownTriggerRejected(t *testing.T) {
	pool := testPool(t)
	s, custStore := newSandboxServer(t, pool)
	router := NewRouter(s)

	_, rawKey, err := custStore.CreateSandbox(t.Context(), "sandbox-unknown-trigger-co")
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	rec := postSandboxOrder(t, router, rawKey, "not_a_real_trigger")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}
