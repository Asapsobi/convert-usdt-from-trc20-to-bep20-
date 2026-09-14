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

func testChangeNowConfig(client *http.Client) ChangeNowConfig {
	return ChangeNowConfig{
		APIKey: "test-api-key", USDTTRC20Ccy: "usdttrc20", USDTBEP20Ccy: "usdtbsc",
		HTTPClient: client,
	}
}

func TestNewChangeNowProvider_RejectsMissingConfig(t *testing.T) {
	if _, err := NewChangeNowProvider(ChangeNowConfig{}); err == nil {
		t.Fatal("expected an error for entirely empty config")
	}
	cfg := testChangeNowConfig(nil)
	cfg.APIKey = ""
	if _, err := NewChangeNowProvider(cfg); err == nil {
		t.Fatal("expected an error for a missing APIKey")
	}
}

func TestChangeNowQuote_SendsRequestAndParsesResponse(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		json.NewEncoder(w).Encode(changeNowEstimateResponse{EstimatedAmount: 99.7})
	}))
	defer srv.Close()

	p, err := NewChangeNowProvider(testChangeNowConfig(srv.Client()))
	if err != nil {
		t.Fatalf("NewChangeNowProvider: %v", err)
	}
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	quote, err := p.Quote(context.Background(), pair, amountIn)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if gotPath != "/exchange-amount/100.000000/usdttrc20_usdtbsc?api_key=test-api-key" {
		t.Errorf("unexpected request path: %s", gotPath)
	}
	if quote.ProviderName != "changenow" {
		t.Errorf("expected ProviderName=changenow, got %q", quote.ProviderName)
	}
	if quote.AmountOut.Units != 99_700000 || quote.AmountOut.Asset != money.USDT_BEP20 {
		t.Errorf("expected amount_out 99_700000 USDT_BEP20, got %d %s", quote.AmountOut.Units, quote.AmountOut.Asset)
	}
	if quote.ValidUntil.Before(quote.QuotedAt) {
		t.Error("expected ValidUntil to be after QuotedAt")
	}
}

func TestChangeNowQuote_RejectsNonPositiveEstimate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(changeNowEstimateResponse{EstimatedAmount: 0})
	}))
	defer srv.Close()

	p, _ := NewChangeNowProvider(testChangeNowConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	if _, err := p.Quote(context.Background(), pair, amountIn); err == nil {
		t.Fatal("expected an error for a zero estimated amount")
	}
}

func TestChangeNowQuote_ReturnsAPIErrorOnHTTPFailureStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(changeNowErrorEnvelope{Message: "pair unavailable"})
	}))
	defer srv.Close()

	p, _ := NewChangeNowProvider(testChangeNowConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	_, err := p.Quote(context.Background(), pair, amountIn)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v (%T)", err, err)
	}
	if apiErr.Code != http.StatusBadRequest || apiErr.Msg != "pair unavailable" {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
}

func TestChangeNowCreateOrder_SendsDestinationAddressAndParsesOrder(t *testing.T) {
	var gotReq changeNowCreateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transactions/test-api-key" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(changeNowCreateResponse{
			ID: "cn-order-123", PayinAddress: "Trelayd-changenow-deposit", PayoutAddress: "0xcustomer",
			FromCurrency: "usdttrc20", ToCurrency: "usdtbsc", AmountExpectedTo: 99.7,
		})
	}))
	defer srv.Close()

	p, _ := NewChangeNowProvider(testChangeNowConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	order, err := p.CreateOrder(context.Background(), pair, amountIn, "0xcustomer")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if gotReq.Address != "0xcustomer" {
		t.Errorf("expected address=0xcustomer, got %q", gotReq.Address)
	}
	if order.ProviderOrderID != "cn-order-123" {
		t.Errorf("expected ProviderOrderID=cn-order-123 (unpacked, no token needed), got %q", order.ProviderOrderID)
	}
	if order.DepositAddress != "Trelayd-changenow-deposit" {
		t.Errorf("expected deposit address to pass through, got %q", order.DepositAddress)
	}
	if order.Status != StatusAwaitingDeposit {
		t.Errorf("expected a freshly created order to be AwaitingDeposit, got %s", order.Status)
	}
}

func TestChangeNowGetOrder_ParsesStatusAndAmounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transactions/cn-order-123/test-api-key" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(changeNowStatusResponse{
			ID: "cn-order-123", Status: "finished",
			PayinAddress: "Trelayd-changenow-deposit", PayoutAddress: "0xcustomer",
			FromCurrency: "usdttrc20", ToCurrency: "usdtbsc",
			AmountSend: 100, AmountReceive: 99.7,
		})
	}))
	defer srv.Close()

	p, _ := NewChangeNowProvider(testChangeNowConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	order, err := p.GetOrder(context.Background(), "cn-order-123")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.Status != StatusComplete {
		t.Errorf("expected finished to map to StatusComplete, got %s", order.Status)
	}
	if order.AmountOutActual == nil || order.AmountOutActual.Units != 99_700000 {
		t.Error("expected a completed order to report its actual payout")
	}
	if order.AmountIn.Units != 100_000000 || order.AmountIn.Asset != money.USDT_TRC20 {
		t.Errorf("unexpected AmountIn: %+v", order.AmountIn)
	}
}

func TestStatusFromChangeNow_MapsEveryKnownStatus(t *testing.T) {
	cases := map[string]SwapStatus{
		"new": StatusAwaitingDeposit, "waiting": StatusAwaitingDeposit,
		"confirming": StatusConfirming, "verifying": StatusConfirming,
		"exchanging": StatusExchanging, "sending": StatusSending,
		"finished": StatusComplete, "failed": StatusFailed, "refunded": StatusFailed,
		"some-unrecognized-future-status": StatusFailed,
	}
	for input, want := range cases {
		if got := statusFromChangeNow(input); got != want {
			t.Errorf("statusFromChangeNow(%q) = %s, want %s", input, got, want)
		}
	}
}
