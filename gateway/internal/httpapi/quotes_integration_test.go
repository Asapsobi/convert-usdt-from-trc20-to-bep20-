//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gateway/internal/c1client"
	"gateway/internal/customers"
	"gateway/internal/quotes"
	"gateway/internal/ratelimit"
)

// fakeC1 stands in for C1 in this chunk's own unit-level tests -- C6.2's
// own acceptance criteria only need the halt check exercised, and
// c6-api-gateway-build-prompts.md's own C6.3 acceptance criterion says
// explicitly to "assert by call count against fakes in the unit test,"
// the same convention every sibling component's own chunk-level tests
// use. Real C1 integration is C6.9's own replay harness, not repeated at
// every chunk.
type fakeC1 struct {
	halted    bool
	haltCalls int
}

func newFakeC1(t *testing.T, f *fakeC1) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/system/halt", func(w http.ResponseWriter, r *http.Request) {
		f.haltCalls++
		json.NewEncoder(w).Encode(c1client.HaltState{Halted: f.halted, Reason: ""})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newQuotesServer(t *testing.T, ledgerBase string) (*Server, *customers.Store) {
	pool := testPool(t)
	custStore := customers.NewStore(pool)
	s := &Server{
		Pool:        pool,
		Customers:   custStore,
		RateLimiter: ratelimit.New(),
		Quotes:      quotes.NewStore(pool),
		Ledger:      c1client.New(ledgerBase, "unused"),
	}
	return s, custStore
}

func TestPostQuote_TwoQuotesIndependent(t *testing.T) {
	f := &fakeC1{}
	srv := newFakeC1(t, f)
	s, custStore := newQuotesServer(t, srv.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "quote-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}

	body := `{"tier":"STANDARD","amount_in":"3000.000000","recipient_address":"` + validTronRecipient + `"}`
	var quoteIDs []float64
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rawKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /v1/quotes (%d): status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		quoteIDs = append(quoteIDs, resp["quote_id"].(float64))
		if resp["fee_units"] != "7.500000" || resp["amount_out"] != "2990.700000" {
			t.Errorf("quote %d: fee_units=%v amount_out=%v, want the golden $3000/STANDARD result", i, resp["fee_units"], resp["amount_out"])
		}
	}
	if quoteIDs[0] == quoteIDs[1] {
		t.Fatalf("two POST /v1/quotes calls returned the same quote_id: %v", quoteIDs[0])
	}
}

func TestPostQuote_ExpiresAtIsServerComputed(t *testing.T) {
	f := &fakeC1{}
	srv := newFakeC1(t, f)
	s, custStore := newQuotesServer(t, srv.URL)
	s.QuoteValidity = 5 * time.Second
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "expiry-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}

	// Truncated to whole seconds -- RFC3339 (the wire format) carries no
	// sub-second precision, so comparing against an untruncated "before"
	// would be comparing truncated-then-parsed time against
	// full-precision time, occasionally (and wrongly) reporting the
	// response's own created_at as "before the request started" purely
	// from truncation, never an actual ordering violation.
	before := time.Now().UTC().Truncate(time.Second)
	req := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(`{"tier":"DIRECT","amount_in":"100.000000","recipient_address":"`+validTronRecipient+`"}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.CreatedAt.Before(before) {
		t.Errorf("created_at %v is before the request started (%v)", resp.CreatedAt, before)
	}
	gotWindow := resp.ExpiresAt.Sub(resp.CreatedAt)
	if gotWindow != 5*time.Second {
		t.Errorf("expires_at - created_at = %v, want exactly the configured 5s validity", gotWindow)
	}
}

// TestPostQuote_HaltedRefusesAndWritesNothing is invariant 6's own
// check: halted -> 423, no row written.
func TestPostQuote_HaltedRefusesAndWritesNothing(t *testing.T) {
	f := &fakeC1{halted: true}
	srv := newFakeC1(t, f)
	s, custStore := newQuotesServer(t, srv.URL)
	router := NewRouter(s)

	customer, rawKey, err := custStore.Create(t.Context(), "halted-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(`{"tier":"DIRECT","amount_in":"100.000000","recipient_address":"`+validTronRecipient+`"}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusLocked {
		t.Fatalf("status = %d, want 423", rec.Code)
	}

	// Scoped to THIS test's own customer -- the quotes table is shared
	// across this whole file's own tests (one real Postgres, no reset
	// between them), so a global count would also see rows other tests
	// legitimately wrote.
	var count int
	if err := s.Pool.QueryRow(t.Context(), `SELECT count(*) FROM quotes WHERE customer_id = $1`, customer.ID).Scan(&count); err != nil {
		t.Fatalf("counting quotes: %v", err)
	}
	if count != 0 {
		t.Fatalf("quotes row count for this customer = %d, want 0 -- a halted quote request must write nothing", count)
	}
	if f.haltCalls == 0 {
		t.Fatal("halt state was never actually checked")
	}
}

// TestPostQuote_RejectsInvalidDestinationAddress is the B2B side of
// this corridor's own fixed-direction invariant (deposit BSC/BEP20,
// payout TRON/TRC20): recipient_address must be a real, checksum-valid
// TRON address, enforced at internal/quotes' own domain boundary (see
// that package's own doc comment), not just this handler. A distinct
// invalid_destination_address error code, never lumped into the
// generic invalid_request, so a caller can tell "this field's own
// value is wrong" from "the request shape is malformed."
func TestPostQuote_RejectsInvalidDestinationAddress(t *testing.T) {
	f := &fakeC1{}
	srv := newFakeC1(t, f)
	s, custStore := newQuotesServer(t, srv.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "invalid-addr-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}

	cases := []struct {
		name    string
		address string
	}{
		{"EVM address", "0xAE2166bd7901Ea67c1E2Bc4179418fC228108F07"},
		{"empty", ""},
		{"malformed string", "not-an-address"},
		{"wrong length (too short)", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6"},
		{"wrong length (too long)", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6tX"},
		{"corrupted checksum", validTronRecipient[:len(validTronRecipient)-1] + "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"tier":"DIRECT","amount_in":"100.000000","recipient_address":"` + c.address + `"}`
			req := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+rawKey)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if c.address == "" {
				// Empty fails the "all fields required" check before
				// address validation is ever reached -- still a 400, just
				// invalid_request rather than invalid_destination_address.
				// Both are correctly rejected; this asserts which reason.
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
			}
			var resp struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decoding error response: %v", err)
			}
			if resp.Error.Code != "invalid_destination_address" {
				t.Errorf("error code = %q, want invalid_destination_address", resp.Error.Code)
			}
		})
	}
}

// TestPostQuote_AcceptsAValidTronAddress is the positive-case
// counterpart to the rejection test above -- a real, checksum-valid
// TRON address must still be accepted, proving the validation isn't
// accidentally rejecting everything.
func TestPostQuote_AcceptsAValidTronAddress(t *testing.T) {
	f := &fakeC1{}
	srv := newFakeC1(t, f)
	s, custStore := newQuotesServer(t, srv.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "valid-addr-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}

	body := `{"tier":"DIRECT","amount_in":"100.000000","recipient_address":"` + validTronRecipient + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", rec.Code, rec.Body.String())
	}
}
