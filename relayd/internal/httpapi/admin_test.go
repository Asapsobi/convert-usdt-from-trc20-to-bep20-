package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdmin_RequiresAValidToken(t *testing.T) {
	var seen string
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = operatorFrom(r.Context()) })
	admin := &Admin{Tokens: map[string]string{"a-long-enough-admin-token-1": "alice"}}

	for name, tc := range map[string]struct {
		admin  *Admin
		header string
		want   int
	}{
		"admin API off": {nil, "Bearer a-long-enough-admin-token-1", http.StatusNotFound},
		"no token":      {admin, "", http.StatusUnauthorized},
		"wrong token":   {admin, "Bearer a-long-enough-admin-token-2", http.StatusUnauthorized},
		"not bearer":    {admin, "a-long-enough-admin-token-1", http.StatusUnauthorized},
		"valid":         {admin, "Bearer a-long-enough-admin-token-1", http.StatusOK},
	} {
		seen = ""
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/pricing", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		tc.admin.require(ok).ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: got %d, want %d", name, rec.Code, tc.want)
		}
		if tc.want == http.StatusOK && seen != "alice" {
			t.Errorf("%s: expected the operator recorded as alice, got %q", name, seen)
		}
	}
}

// The order list is operator-only; a customer's own status stays public.
func TestRouter_OrderListNeedsAdmin(t *testing.T) {
	router := NewRouter(&Server{Admin: &Admin{Tokens: map[string]string{"a-long-enough-admin-token-1": "alice"}}})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/relay-legs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/relay-legs without a token: got %d, want 401", rec.Code)
	}
}

func TestDecimalString(t *testing.T) {
	for units, want := range map[int64]string{0: "0", 1_500_000: "1.5", 2_000_000: "2", 1: "0.000001", -3_250_000: "-3.25"} {
		if got := decimalString(units, 6); got != want {
			t.Errorf("decimalString(%d, 6) = %q, want %q", units, got, want)
		}
	}
}

func TestRateLimit_BoundsEachClient(t *testing.T) {
	l := &RateLimit{PerMinute: 3, Burst: 2}
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	call := func(addr string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/relay-legs", nil)
		req.RemoteAddr = addr
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if a, b, c := call("1.2.3.4:5000"), call("1.2.3.4:5001"), call("1.2.3.4:5002"); a != 200 || b != 200 || c != http.StatusTooManyRequests {
		t.Fatalf("expected a burst of 2 then 429, got %d %d %d", a, b, c)
	}
	if got := call("5.6.7.8:5000"); got != 200 {
		t.Fatalf("another client has its own allowance, got %d", got)
	}
}
