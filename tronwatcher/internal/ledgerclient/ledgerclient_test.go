package ledgerclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tronwatcher/internal/finality"
	"tronwatcher/internal/money"
)

func testCandidate() finality.Candidate {
	return finality.Candidate{
		ObservedTransfer: finality.ObservedTransfer{
			TxID:           "tx1",
			BlockTimestamp: time.Now().UTC(),
			OrderID:        42,
			ExternalID:     "ext-42",
			CustomerID:     "cust-1",
			Amount:         money.Amount(100_000000),
			SenderAddress:  "TSenderAddress",
		},
	}
}

func TestGetOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing/wrong Authorization header: %q", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: 3, AmountIn: "100.000000"})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	order, err := c.GetOrder(context.Background(), "ext-42")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.Version != 3 {
		t.Errorf("expected version 3, got %d", order.Version)
	}
}

func TestQuotedAmount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: 1, AmountIn: "50.500000"})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	amt, err := c.QuotedAmount(context.Background(), "ext-42")
	if err != nil {
		t.Fatalf("QuotedAmount: %v", err)
	}
	if amt != 50_500000 {
		t.Errorf("got %d, want 50500000", amt)
	}
}

func TestEnsureAccount_Created(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/accounts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req postAccountRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Code != "asset:relay:leg:42" || req.Asset != "USDT_TRC20" {
			t.Errorf("unexpected request: %+v", req)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	err := c.EnsureAccount(context.Background(), "asset:relay:leg:42", AccountAsset, "USDT_TRC20", "idem-key")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
}

func TestReportDepositFinal_Success(t *testing.T) {
	var transitionBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/orders/ext-42":
			json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: 1, AmountIn: "100.000000"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/accounts":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/orders/ext-42/transitions":
			json.NewDecoder(r.Body).Decode(&transitionBody)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: 2, State: "funded"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	if err := c.ReportDepositFinal(context.Background(), testCandidate()); err != nil {
		t.Fatalf("ReportDepositFinal: %v", err)
	}

	if transitionBody["to_state"] != "funded" {
		t.Errorf("expected to_state=funded, got %v", transitionBody["to_state"])
	}
	if transitionBody["sender_address"] != "TSenderAddress" {
		t.Errorf("expected sender_address to be forwarded, got %v", transitionBody["sender_address"])
	}
	entry, ok := transitionBody["entry"].(map[string]any)
	if !ok {
		t.Fatalf("expected entry object in request, got %v", transitionBody["entry"])
	}
	lines, ok := entry["lines"].([]any)
	if !ok || len(lines) != 2 {
		t.Fatalf("expected 2 entry lines, got %v", entry["lines"])
	}
	first := lines[0].(map[string]any)
	if first["account_code"] != "asset:relay:leg:42" {
		t.Errorf("expected first line to credit the relay-leg suspense account, got %v", first["account_code"])
	}
}

func TestReportDepositFinal_VersionConflictRetriesOnce(t *testing.T) {
	getOrderCalls := 0
	transitionCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/orders/ext-42":
			getOrderCalls++
			json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: int32(getOrderCalls), AmountIn: "100.000000"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/accounts":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/orders/ext-42/transitions":
			transitionCalls++
			if transitionCalls == 1 {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "version_conflict"}})
				return
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: 3, State: "funded"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	if err := c.ReportDepositFinal(context.Background(), testCandidate()); err != nil {
		t.Fatalf("ReportDepositFinal: %v", err)
	}
	if transitionCalls != 2 {
		t.Errorf("expected exactly 2 transition attempts (1 conflict + 1 retry), got %d", transitionCalls)
	}
}

func TestReportDepositFinal_IllegalTransitionWrapsPermanentAndOrphaned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: 1, AmountIn: "100.000000"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/accounts":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "illegal_transition"}})
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	err := c.ReportDepositFinal(context.Background(), testCandidate())
	if !errors.Is(err, finality.ErrPermanentFailure) {
		t.Errorf("expected ErrPermanentFailure, got %v", err)
	}
	if !errors.Is(err, finality.ErrOrphanedDeposit) {
		t.Errorf("expected ErrOrphanedDeposit, got %v", err)
	}
}

func TestReportDepositFinal_IdempotencyConflictIsPermanentNotOrphaned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: 1, AmountIn: "100.000000"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/accounts":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "idempotency_conflict"}})
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	err := c.ReportDepositFinal(context.Background(), testCandidate())
	if !errors.Is(err, finality.ErrPermanentFailure) {
		t.Errorf("expected ErrPermanentFailure, got %v", err)
	}
	if errors.Is(err, finality.ErrOrphanedDeposit) {
		t.Errorf("idempotency_conflict must NOT be treated as an orphaned deposit, got %v", err)
	}
}

func TestReportDepositFinal_SystemHaltedIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(Order{ExternalID: "ext-42", Version: 1, AmountIn: "100.000000"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/accounts":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusLocked)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "system_halted"}})
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	err := c.ReportDepositFinal(context.Background(), testCandidate())
	if err == nil {
		t.Fatal("expected an error for system_halted")
	}
	if errors.Is(err, finality.ErrPermanentFailure) {
		t.Errorf("system_halted must be retryable, not permanent: %v", err)
	}
}
