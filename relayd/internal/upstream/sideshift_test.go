package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"relayd/internal/money"
)

func testSideshiftConfig(client *http.Client) SideshiftConfig {
	return SideshiftConfig{
		Secret: "test-secret", AffiliateID: "test-affiliate-id",
		USDTTRC20Coin: "USDT", USDTTRC20Network: "tron",
		USDTBEP20Coin: "USDT", USDTBEP20Network: "bsc",
		HTTPClient: client,
	}
}

func TestNewSideshiftProvider_RejectsMissingConfig(t *testing.T) {
	if _, err := NewSideshiftProvider(SideshiftConfig{}); err == nil {
		t.Fatal("expected an error for entirely empty config")
	}
	cfg := testSideshiftConfig(nil)
	cfg.AffiliateID = ""
	if _, err := NewSideshiftProvider(cfg); err == nil {
		t.Fatal("expected an error for a missing AffiliateID")
	}
}

// TestSideshiftQuote_SendsRequestAndParsesResponse uses the REAL
// response shape captured from a live POST /v2/quotes call against
// SideShift's own production API while building this integration (see
// sideshift.go's own top-of-file doc comment) as its fixture, not an
// invented one.
func TestSideshiftQuote_SendsRequestAndParsesResponse(t *testing.T) {
	var gotReq sideShiftQuoteRequest
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/quotes" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		gotHeader = r.Header.Get("x-sideshift-secret")
		json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(sideShiftQuoteResponse{
			ID: "9ce58669-e8a7-44ea-afe4-9c6b464850f7", ExpiresAt: "2026-09-15T09:18:05.087Z",
			DepositAmount: "100", SettleAmount: "95.71",
		})
	}))
	defer srv.Close()

	p, err := NewSideshiftProvider(testSideshiftConfig(srv.Client()))
	if err != nil {
		t.Fatalf("NewSideshiftProvider: %v", err)
	}
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_BEP20, To: money.USDT_TRC20}
	amountIn := money.Amount{Asset: money.USDT_BEP20, Units: 100_000000}
	quote, err := p.Quote(context.Background(), pair, amountIn)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if gotHeader != "test-secret" {
		t.Errorf("expected x-sideshift-secret header to be sent, got %q", gotHeader)
	}
	if gotReq.DepositCoin != "USDT" || gotReq.DepositNetwork != "bsc" || gotReq.SettleCoin != "USDT" || gotReq.SettleNetwork != "tron" {
		t.Errorf("unexpected request coin/network: %+v", gotReq)
	}
	if gotReq.DepositAmount != "100.000000" {
		t.Errorf("expected depositAmount=100.000000, got %q", gotReq.DepositAmount)
	}
	if quote.ProviderName != "sideshift" {
		t.Errorf("expected ProviderName=sideshift, got %q", quote.ProviderName)
	}
	if quote.AmountOut.Units != 95_710000 || quote.AmountOut.Asset != money.USDT_TRC20 {
		t.Errorf("expected amount_out 95_710000 USDT_TRC20, got %d %s", quote.AmountOut.Units, quote.AmountOut.Asset)
	}
}

func TestSideshiftQuote_ReturnsAPIErrorOnHTTPFailureStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Real error shape captured live: {"error":{"message":"...","code":"..."}}.
		w.Write([]byte(`{"error":{"message":"Invalid parameters: affiliateId: Invalid input: expected string, received undefined","code":"BAD_USER_INPUT"}}`))
	}))
	defer srv.Close()

	p, _ := NewSideshiftProvider(testSideshiftConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_BEP20, To: money.USDT_TRC20}
	amountIn := money.Amount{Asset: money.USDT_BEP20, Units: 100_000000}
	_, err := p.Quote(context.Background(), pair, amountIn)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v (%T)", err, err)
	}
	if apiErr.Code != http.StatusBadRequest {
		t.Errorf("expected code %d, got %d", http.StatusBadRequest, apiErr.Code)
	}
	if apiErr.Msg == "" {
		t.Error("expected the real nested error message to be extracted, got empty")
	}
}

// TestSideshiftCreateOrder_SendsDestinationAndAffiliateIDAndParsesOrder
// uses the REAL response shape captured from a live, harmless (never
// funded) POST /v2/shifts/fixed call against SideShift's own production
// API while building this integration.
func TestSideshiftCreateOrder_SendsDestinationAndAffiliateIDAndParsesOrder(t *testing.T) {
	var gotShiftReq sideShiftCreateShiftRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/quotes":
			json.NewEncoder(w).Encode(sideShiftQuoteResponse{
				ID: "9ce58669-e8a7-44ea-afe4-9c6b464850f7", ExpiresAt: "2026-09-15T09:18:05.087Z",
				DepositAmount: "100", SettleAmount: "95.71",
			})
		case "/shifts/fixed":
			json.NewDecoder(r.Body).Decode(&gotShiftReq)
			json.NewEncoder(w).Encode(sideShiftShiftResponse{
				ID: "7fb537b813f973767eb4", DepositCoin: "USDT", SettleCoin: "USDT",
				DepositNetwork: "bsc", SettleNetwork: "tron",
				DepositAddress: "0x3bf818081959D58E0fC67b34743391d186022159",
				SettleAddress:  "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
				DepositAmount:  "100", SettleAmount: "95.71", Status: "waiting",
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := NewSideshiftProvider(testSideshiftConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_BEP20, To: money.USDT_TRC20}
	amountIn := money.Amount{Asset: money.USDT_BEP20, Units: 100_000000}
	order, err := p.CreateOrder(context.Background(), pair, amountIn, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if gotShiftReq.SettleAddress != "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t" {
		t.Errorf("expected settleAddress to be the customer's destination, got %q", gotShiftReq.SettleAddress)
	}
	if gotShiftReq.AffiliateID != "test-affiliate-id" {
		t.Errorf("expected affiliateId to be sent, got %q", gotShiftReq.AffiliateID)
	}
	if gotShiftReq.QuoteID != "9ce58669-e8a7-44ea-afe4-9c6b464850f7" {
		t.Errorf("expected the real quote id from the /quotes call to be reused, got %q", gotShiftReq.QuoteID)
	}
	if order.ProviderOrderID != "7fb537b813f973767eb4" {
		t.Errorf("expected ProviderOrderID=7fb537b813f973767eb4, got %q", order.ProviderOrderID)
	}
	if order.DepositAddress != "0x3bf818081959D58E0fC67b34743391d186022159" {
		t.Errorf("expected the vendor's own deposit address to pass through, got %q", order.DepositAddress)
	}
	if order.Status != StatusAwaitingDeposit {
		t.Errorf("expected a freshly created \"waiting\" shift to map to AwaitingDeposit, got %s", order.Status)
	}
}

func TestSideshiftGetOrder_ParsesStatusAndAmounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/shifts/7fb537b813f973767eb4" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(sideShiftShiftResponse{
			ID: "7fb537b813f973767eb4", DepositCoin: "USDT", SettleCoin: "USDT",
			DepositNetwork: "bsc", SettleNetwork: "tron",
			DepositAddress: "0x3bf818081959D58E0fC67b34743391d186022159",
			SettleAddress:  "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
			DepositAmount:  "100", SettleAmount: "95.71", Status: "settled",
		})
	}))
	defer srv.Close()

	p, _ := NewSideshiftProvider(testSideshiftConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	order, err := p.GetOrder(context.Background(), "7fb537b813f973767eb4")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.Status != StatusComplete {
		t.Errorf("expected settled to map to StatusComplete, got %s", order.Status)
	}
	if order.AmountOutActual == nil || order.AmountOutActual.Units != 95_710000 {
		t.Error("expected a completed order to report its actual payout")
	}
	if order.AmountIn.Units != 100_000000 || order.AmountIn.Asset != money.USDT_BEP20 {
		t.Errorf("unexpected AmountIn: %+v", order.AmountIn)
	}
}

func TestStatusFromSideShift_MapsEveryKnownStatus(t *testing.T) {
	cases := map[string]SwapStatus{
		"waiting":    StatusAwaitingDeposit,
		"pending":    StatusConfirming,
		"processing": StatusExchanging,
		"settling":   StatusSending,
		"settled":    StatusComplete,
		"refund":     StatusFailed, "refunding": StatusFailed,
		"expired": StatusFailed, "review": StatusNeedsAttention, "multiple": StatusFailed,
		"some-unrecognized-future-status": StatusNeedsAttention,
	}
	for input, want := range cases {
		if got := statusFromSideShift(input); got != want {
			t.Errorf("statusFromSideShift(%q) = %s, want %s", input, got, want)
		}
	}
}
