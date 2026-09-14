package upstream

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"relayd/internal/money"
)

func testConfig(baseURLClient *http.Client) FixedFloatConfig {
	return FixedFloatConfig{
		APIKey: "test-key", APISecret: "test-secret",
		RefCode:      "test-ref",
		USDTTRC20Ccy: "USDTTRC", USDTBEP20Ccy: "USDTBSC",
		HTTPClient: baseURLClient,
	}
}

func TestNewFixedFloatProvider_RejectsMissingConfig(t *testing.T) {
	if _, err := NewFixedFloatProvider(FixedFloatConfig{}); err == nil {
		t.Fatal("expected an error for entirely empty config")
	}
	cfg := testConfig(nil)
	cfg.APISecret = ""
	if _, err := NewFixedFloatProvider(cfg); err == nil {
		t.Fatal("expected an error for a missing APISecret")
	}
}

func TestPackUnpackOrderRef_RoundTrips(t *testing.T) {
	ref := packOrderRef("aB3xY9", "a1b2c3d4e5f6")
	id, token, err := unpackOrderRef(ref)
	if err != nil {
		t.Fatalf("unpackOrderRef: %v", err)
	}
	if id != "aB3xY9" || token != "a1b2c3d4e5f6" {
		t.Errorf("expected id=aB3xY9 token=a1b2c3d4e5f6, got id=%s token=%s", id, token)
	}
}

func TestUnpackOrderRef_RejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "no-separator", "|missing-id", "missing-token|"} {
		if _, _, err := unpackOrderRef(bad); err == nil {
			t.Errorf("expected an error unpacking %q, got none", bad)
		}
	}
}

func TestSign_MatchesHMACSHA256OfExactBody(t *testing.T) {
	body := []byte(`{"a":1}`)
	got := sign("my-secret", body)

	mac := hmac.New(sha256.New, []byte("my-secret"))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Errorf("sign produced %s, want %s", got, want)
	}
}

func TestQuote_SendsSignedRequestAndParsesResponse(t *testing.T) {
	var gotReq ffPriceRequest
	var gotSig, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/price" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		gotKey = r.Header.Get("X-API-KEY")
		gotSig = r.Header.Get("X-API-SIGN")
		json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(ffEnvelope{
			Code: 0,
			Data: mustMarshal(t, ffPriceData{
				From: ffPriceSide{Code: "USDTTRC", Amount: "100.000000"},
				To:   ffPriceSide{Code: "USDTBSC", Amount: "99.700000"},
			}),
		})
	}))
	defer srv.Close()

	p, err := NewFixedFloatProvider(testConfig(srv.Client()))
	if err != nil {
		t.Fatalf("NewFixedFloatProvider: %v", err)
	}
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	quote, err := p.Quote(context.Background(), pair, amountIn)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if gotKey != "test-key" {
		t.Errorf("expected X-API-KEY=test-key, got %q", gotKey)
	}
	if gotSig == "" {
		t.Error("expected a non-empty X-API-SIGN header")
	}
	if gotReq.Type != "fixed" || gotReq.Direction != "from" {
		t.Errorf("expected type=fixed direction=from, got type=%s direction=%s", gotReq.Type, gotReq.Direction)
	}
	if gotReq.FromCcy != "USDTTRC" || gotReq.ToCcy != "USDTBSC" {
		t.Errorf("expected fromCcy=USDTTRC toCcy=USDTBSC, got fromCcy=%s toCcy=%s", gotReq.FromCcy, gotReq.ToCcy)
	}
	if gotReq.RefCode != "test-ref" {
		t.Errorf("expected refcode=test-ref, got %q", gotReq.RefCode)
	}
	if quote.AmountOut.Units != 99_700000 || quote.AmountOut.Asset != money.USDT_BEP20 {
		t.Errorf("expected amount_out 99_700000 USDT_BEP20, got %d %s", quote.AmountOut.Units, quote.AmountOut.Asset)
	}
	if quote.ValidUntil.Before(quote.QuotedAt) {
		t.Error("expected ValidUntil to be after QuotedAt")
	}
}

func TestQuote_ReturnsErrorWhenPairUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ffEnvelope{
			Code: 0,
			Data: mustMarshal(t, ffPriceData{Errors: []string{"OFFLINE_FROM"}}),
		})
	}))
	defer srv.Close()

	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	if _, err := p.Quote(context.Background(), pair, amountIn); err == nil {
		t.Fatal("expected an error when the vendor reports the pair offline")
	}
}

func TestQuote_ReturnsAPIErrorOnNonZeroCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ffEnvelope{Code: 429, Msg: "too many requests"})
	}))
	defer srv.Close()

	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	_, err := p.Quote(context.Background(), pair, amountIn)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v (%T)", err, err)
	}
	if apiErr.Code != 429 {
		t.Errorf("expected code 429, got %d", apiErr.Code)
	}
}

func TestCreateOrder_PacksIDAndTokenAndParsesOrder(t *testing.T) {
	var gotReq ffCreateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/create" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(ffEnvelope{
			Code: 0,
			Data: mustMarshal(t, ffOrderData{
				ID: "aB3xY9", Token: "sekrit-token", Type: "fixed", Status: "NEW",
				From: ffOrderSide{Code: "USDTTRC", Address: "Trelayd-fixedfloat-deposit", Amount: "100.000000"},
				To:   ffOrderSide{Code: "USDTBSC", Address: "0xcustomer", Amount: "99.700000"},
			}),
		})
	}))
	defer srv.Close()

	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	order, err := p.CreateOrder(context.Background(), pair, amountIn, "0xcustomer")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if gotReq.ToAddress != "0xcustomer" {
		t.Errorf("expected toAddress=0xcustomer, got %q", gotReq.ToAddress)
	}
	if order.ProviderOrderID != "aB3xY9|sekrit-token" {
		t.Errorf("expected packed order ref aB3xY9|sekrit-token, got %q", order.ProviderOrderID)
	}
	if order.DepositAddress != "Trelayd-fixedfloat-deposit" {
		t.Errorf("expected deposit address to pass through, got %q", order.DepositAddress)
	}
	if order.Status != StatusAwaitingDeposit {
		t.Errorf("expected NEW to map to StatusAwaitingDeposit, got %s", order.Status)
	}
	if order.AmountOutActual != nil {
		t.Error("expected a NEW order to have no actual payout yet")
	}
}

func TestGetOrder_UnpacksRefAndSendsBoth(t *testing.T) {
	var gotReq ffStatusRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/order" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(ffEnvelope{
			Code: 0,
			Data: mustMarshal(t, ffOrderData{
				ID: "aB3xY9", Token: "sekrit-token", Status: "DONE",
				From: ffOrderSide{Code: "USDTTRC", Address: "Trelayd-fixedfloat-deposit", Amount: "100.000000"},
				To:   ffOrderSide{Code: "USDTBSC", Address: "0xcustomer", Amount: "99.700000"},
			}),
		})
	}))
	defer srv.Close()

	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	order, err := p.GetOrder(context.Background(), "aB3xY9|sekrit-token")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if gotReq.ID != "aB3xY9" || gotReq.Token != "sekrit-token" {
		t.Errorf("expected id/token to be split back apart, got id=%s token=%s", gotReq.ID, gotReq.Token)
	}
	if order.Status != StatusComplete {
		t.Errorf("expected DONE to map to StatusComplete, got %s", order.Status)
	}
	if order.AmountOutActual == nil || order.AmountOutActual.Units != 99_700000 {
		t.Error("expected a completed order to report its actual payout")
	}
}

func TestGetOrder_RejectsUnpackedRef(t *testing.T) {
	p, _ := NewFixedFloatProvider(testConfig(nil))
	if _, err := p.GetOrder(context.Background(), "not-a-packed-ref"); err == nil {
		t.Fatal("expected an error for a providerOrderID with no packed token")
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling test fixture: %v", err)
	}
	return b
}
