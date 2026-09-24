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

// TestQuote_ParsesARealShapedResponseWithBareNumericAmount replays the
// EXACT shape a real, live POST /api/v2/price call returned (captured by
// the operator, not guessed): "amount" is a bare JSON number
// (`"amount":10`), never a quoted string. Written as raw JSON text
// rather than a marshaled Go struct so this test can't accidentally pass
// by relying on json.Number's own marshaling behavior the way the other
// tests in this file incidentally do -- this one pins the real,
// independently-confirmed wire shape directly.
func TestQuote_ParsesARealShapedResponseWithBareNumericAmount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"from":{"code":"USDTTRC","network":"TRX","coin":"USDT","amount":10,"rate":0.99,"precision":8,"min":1.363,"max":15000,"usd":10,"btc":0.00011642},"to":{"code":"USDTBSC","network":"BSC","coin":"USDT","amount":9.551,"rate":0.99,"precision":8,"min":1,"max":14849.651,"usd":9.55},"errors":[]}}`))
	}))
	defer srv.Close()

	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 10_000000}
	quote, err := p.Quote(context.Background(), pair, amountIn)
	if err != nil {
		t.Fatalf("Quote: %v (this is the exact real-shaped response that broke this integration on its first live call)", err)
	}
	if quote.AmountOut.Units != 9_551000 || quote.AmountOut.Asset != money.USDT_BEP20 {
		t.Errorf("expected amount_out 9_551000 USDT_BEP20, got %d %s", quote.AmountOut.Units, quote.AmountOut.Asset)
	}
}

// TestCreateOrder_ParsesARealShapedResponseWith8DecimalAmount replays
// the exact shape a real, live POST /api/v2/create call returned: an
// amount_in of "2.00000000" -- 8 decimal places, FixedFloat's own fixed
// internal precision, regardless of USDT_TRC20/USDT_BEP20 only
// supporting 6 on-chain. This is the exact real response that broke
// this integration's first live order-creation call (the /create
// endpoint has this issue; /price was separately proven fine at typical
// test amounts, but the same truncation now guards both).
func TestCreateOrder_ParsesARealShapedResponseWith8DecimalAmount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"id":"aB3xY9","token":"sekrit-token","type":"fixed","status":"NEW","from":{"code":"USDTTRC","address":"Trelayd-fixedfloat-deposit","amount":"2.00000000"},"to":{"code":"USDTBSC","address":"0xcustomer","amount":"1.62700000"}}}`))
	}))
	defer srv.Close()

	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 2_000000}
	order, err := p.CreateOrder(context.Background(), pair, amountIn, "0xcustomer")
	if err != nil {
		t.Fatalf("CreateOrder: %v (this is the exact real-shaped 8-decimal response that broke this integration on its first live order-creation call)", err)
	}
	if order.AmountIn.Units != 2_000000 || order.AmountIn.Asset != money.USDT_TRC20 {
		t.Errorf("expected amount_in 2_000000 USDT_TRC20, got %d %s", order.AmountIn.Units, order.AmountIn.Asset)
	}
	if order.AmountOutExpected.Units != 1_627000 || order.AmountOutExpected.Asset != money.USDT_BEP20 {
		t.Errorf("expected amount_out_expected 1_627000 USDT_BEP20, got %d %s", order.AmountOutExpected.Units, order.AmountOutExpected.Asset)
	}
}

// TestTruncateToDecimals_DropsExcessPrecisionOnly confirms the
// truncation helper only ever drops trailing digits beyond the target
// precision, never touches a value already within it.
func TestTruncateToDecimals_DropsExcessPrecisionOnly(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2.00000000", "2.000000"},
		{"1.62700000", "1.627000"},
		{"1.234567891", "1.234567"},
		{"5", "5"},
		{"5.5", "5.5"},
		{"5.123456", "5.123456"},
	}
	for _, c := range cases {
		if got := truncateToDecimals(c.in, 6); got != c.want {
			t.Errorf("truncateToDecimals(%q, 6) = %q, want %q", c.in, got, c.want)
		}
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

func TestSelfCheck_PassesWhenBothConfiguredCurrenciesAreListed(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"code":0,"msg":"OK","data":[{"code":"BTC"},{"code":"USDTBSC"},{"code":"USDTTRC"}]}`))
	}))
	defer srv.Close()
	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	if err := p.SelfCheck(context.Background()); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
	if gotPath != "/ccies" {
		t.Errorf("expected POST /ccies, got %s", gotPath)
	}
}

func TestSelfCheck_RejectsACurrencyCodeFixedFloatDoesNotList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":[{"code":"USDTTRC"}]}`))
	}))
	defer srv.Close()
	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	if err := p.SelfCheck(context.Background()); !errors.Is(err, ErrMisconfigured) {
		t.Fatalf("expected ErrMisconfigured for the missing USDTBSC code, got %v", err)
	}
}

// The exact response a placeholder API key got on 2026-09-23, with the
// code quoted as a string.
func TestSelfCheck_SurfacesARejectedKeyAsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"501","msg":"Not have permission","data":null}`))
	}))
	defer srv.Close()
	p, _ := NewFixedFloatProvider(testConfig(srv.Client()))
	p.overrideBaseURLForTest(srv.URL)

	var apiErr *APIError
	if err := p.SelfCheck(context.Background()); !errors.As(err, &apiErr) || apiErr.Code != 501 {
		t.Fatalf("expected an *APIError with code 501, got %v", err)
	}
}
