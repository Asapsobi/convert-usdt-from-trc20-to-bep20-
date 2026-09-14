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

func TestNewMultiProvider_RequiresAtLeastTwoProviders(t *testing.T) {
	if _, err := NewMultiProvider(nil); err == nil {
		t.Fatal("expected an error for zero providers")
	}
	if _, err := NewMultiProvider([]NamedProvider{{Name: "a", Provider: NewMockProvider("a", 1)}}); err == nil {
		t.Fatal("expected an error for exactly one provider")
	}
}

func TestNewMultiProvider_RejectsBadNames(t *testing.T) {
	a := NewMockProvider("a", 1)
	b := NewMockProvider("b", 2)
	if _, err := NewMultiProvider([]NamedProvider{{Name: "", Provider: a}, {Name: "b", Provider: b}}); err == nil {
		t.Fatal("expected an error for an empty name")
	}
	if _, err := NewMultiProvider([]NamedProvider{{Name: "has:colon", Provider: a}, {Name: "b", Provider: b}}); err == nil {
		t.Fatal("expected an error for a name containing ':'")
	}
	if _, err := NewMultiProvider([]NamedProvider{{Name: "dup", Provider: a}, {Name: "dup", Provider: b}}); err == nil {
		t.Fatal("expected an error for duplicate names")
	}
}

func TestMultiProviderQuote_PicksTheBestRateAmongSuccesses(t *testing.T) {
	cheap := NewMockProvider("cheap", 1)
	cheap.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 95_000000})
	rich := NewMockProvider("rich", 2)
	rich.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 99_000000})

	m, err := NewMultiProvider([]NamedProvider{{Name: "cheap", Provider: cheap}, {Name: "rich", Provider: rich}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	quote, err := m.Quote(context.Background(), pair, amountIn)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if quote.ProviderName != "rich" {
		t.Errorf("expected the router to pick the better-rate provider (rich), got %q", quote.ProviderName)
	}
	if quote.AmountOut.Units != 99_000000 {
		t.Errorf("expected AmountOut 99_000000, got %d", quote.AmountOut.Units)
	}
}

func TestMultiProviderQuote_SkipsAFailingProviderAndUsesTheOtherOne(t *testing.T) {
	broken := NewMockProvider("broken", 1)
	broken.ForceQuoteError(errors.New("simulated vendor outage"))
	working := NewMockProvider("working", 2)
	working.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 97_000000})

	m, err := NewMultiProvider([]NamedProvider{{Name: "broken", Provider: broken}, {Name: "working", Provider: working}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	quote, err := m.Quote(context.Background(), pair, amountIn)
	if err != nil {
		t.Fatalf("Quote: %v (a single failing vendor must not fail the whole call)", err)
	}
	if quote.ProviderName != "working" {
		t.Errorf("expected the router to fall back to the working provider, got %q", quote.ProviderName)
	}
}

func TestMultiProviderQuote_FailsOnlyWhenEveryProviderFails(t *testing.T) {
	a := NewMockProvider("a", 1)
	a.ForceQuoteError(errors.New("a is down"))
	b := NewMockProvider("b", 2)
	b.ForceQuoteError(errors.New("b is down"))

	m, err := NewMultiProvider([]NamedProvider{{Name: "a", Provider: a}, {Name: "b", Provider: b}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	_, err = m.Quote(context.Background(), pair, amountIn)
	if !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("expected ErrAllProvidersFailed, got %v", err)
	}
}

func TestMultiProviderCreateOrder_RoutesToTheWinnerAndPrefixesOrderID(t *testing.T) {
	cheap := NewMockProvider("cheap", 1)
	cheap.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 90_000000})
	rich := NewMockProvider("rich", 2)
	rich.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 98_000000})

	m, err := NewMultiProvider([]NamedProvider{{Name: "cheap", Provider: cheap}, {Name: "rich", Provider: rich}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	order, err := m.CreateOrder(context.Background(), pair, amountIn, "0xcustomer")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.ProviderName != "rich" {
		t.Errorf("expected the order to be created with the winning provider (rich), got %q", order.ProviderName)
	}
	if cheap.CreateOrderCallCount() != 0 {
		t.Errorf("expected the losing provider to never have CreateOrder called on it, got %d calls", cheap.CreateOrderCallCount())
	}
	if rich.CreateOrderCallCount() != 1 {
		t.Errorf("expected the winning provider's CreateOrder to be called exactly once, got %d", rich.CreateOrderCallCount())
	}
	wantPrefix := "rich:"
	if len(order.ProviderOrderID) <= len(wantPrefix) || order.ProviderOrderID[:len(wantPrefix)] != wantPrefix {
		t.Errorf("expected ProviderOrderID to start with %q, got %q", wantPrefix, order.ProviderOrderID)
	}
}

func TestMultiProviderCreateOrder_ReQuotesRatherThanReusingAnEarlierQuote(t *testing.T) {
	a := NewMockProvider("a", 1)
	a.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 99_000000})
	b := NewMockProvider("b", 2)
	b.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 90_000000})

	m, err := NewMultiProvider([]NamedProvider{{Name: "a", Provider: a}, {Name: "b", Provider: b}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}

	// An earlier Quote() call picks "a" -- the customer-facing preview.
	firstQuote, err := m.Quote(context.Background(), pair, amountIn)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if firstQuote.ProviderName != "a" {
		t.Fatalf("test setup: expected the first quote to favor provider a, got %q", firstQuote.ProviderName)
	}

	// Rates move: "b" becomes the better deal before CreateOrder happens
	// (the deposit-wait gap architecture doc §6 flags). CreateOrder must
	// re-decide, not blindly honor the stale "a" winner.
	b.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 99_900000})

	order, err := m.CreateOrder(context.Background(), pair, amountIn, "0xcustomer")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.ProviderName != "b" {
		t.Errorf("expected CreateOrder to re-quote and route to the NOW-better provider (b), got %q -- stale routing decision", order.ProviderName)
	}
}

func TestMultiProviderCreateOrder_FailsOnlyWhenEveryProviderFailsToReQuote(t *testing.T) {
	a := NewMockProvider("a", 1)
	a.ForceQuoteError(errors.New("a down"))
	b := NewMockProvider("b", 2)
	b.ForceQuoteError(errors.New("b down"))

	m, err := NewMultiProvider([]NamedProvider{{Name: "a", Provider: a}, {Name: "b", Provider: b}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	if _, err := m.CreateOrder(context.Background(), pair, amountIn, "0xcustomer"); !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("expected ErrAllProvidersFailed, got %v", err)
	}
}

func TestMultiProviderGetOrder_RoutesBackToTheOriginatingProviderAndPreservesNestedSeparators(t *testing.T) {
	a := NewMockProvider("a", 1)
	a.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 90_000000})
	b := NewMockProvider("b", 2)
	b.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 99_000000})
	m, err := NewMultiProvider([]NamedProvider{{Name: "a", Provider: a}, {Name: "b", Provider: b}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	order, err := m.CreateOrder(context.Background(), pair, amountIn, "0xcustomer")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.ProviderName != "b" {
		t.Fatalf("test setup: expected provider b to win, got %q", order.ProviderName)
	}

	got, err := m.GetOrder(context.Background(), order.ProviderOrderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.ProviderOrderID != order.ProviderOrderID {
		t.Errorf("expected GetOrder to preserve the full prefixed order id, got %q want %q", got.ProviderOrderID, order.ProviderOrderID)
	}
	if b.GetOrderCallCount() != 1 {
		t.Errorf("expected exactly 1 GetOrder call against the originating provider, got %d", b.GetOrderCallCount())
	}
	if a.GetOrderCallCount() != 0 {
		t.Errorf("expected zero GetOrder calls against the non-originating provider, got %d", a.GetOrderCallCount())
	}
}

func TestMultiProviderGetOrder_SurvivesFixedFloatsOwnNestedPipeSeparator(t *testing.T) {
	ffSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/price":
			json.NewEncoder(w).Encode(ffEnvelope{Code: 0, Data: mustMarshal(t, ffPriceData{
				From: ffPriceSide{Code: "USDTTRC", Amount: "100.000000"},
				To:   ffPriceSide{Code: "USDTBSC", Amount: "99.900000"},
			})})
		case "/create":
			json.NewEncoder(w).Encode(ffEnvelope{Code: 0, Data: mustMarshal(t, ffOrderData{
				ID: "aB3xY9", Token: "sekrit-token", Status: "NEW",
				From: ffOrderSide{Code: "USDTTRC", Address: "Tff-deposit", Amount: "100.000000"},
				To:   ffOrderSide{Code: "USDTBSC", Address: "0xcustomer", Amount: "99.900000"},
			})})
		case "/order":
			var req ffStatusRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.ID != "aB3xY9" || req.Token != "sekrit-token" {
				t.Fatalf("GetOrder did not send the full unpacked id/token pair: %+v", req)
			}
			json.NewEncoder(w).Encode(ffEnvelope{Code: 0, Data: mustMarshal(t, ffOrderData{
				ID: "aB3xY9", Token: "sekrit-token", Status: "DONE",
				From: ffOrderSide{Code: "USDTTRC", Address: "Tff-deposit", Amount: "100.000000"},
				To:   ffOrderSide{Code: "USDTBSC", Address: "0xcustomer", Amount: "99.900000"},
			})})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer ffSrv.Close()

	ff, err := NewFixedFloatProvider(testConfig(ffSrv.Client()))
	if err != nil {
		t.Fatalf("NewFixedFloatProvider: %v", err)
	}
	ff.overrideBaseURLForTest(ffSrv.URL)

	loser := NewMockProvider("loser", 1)
	loser.ForceAmountOut(money.Amount{Asset: money.USDT_BEP20, Units: 1_000000}) // deliberately bad rate

	m, err := NewMultiProvider([]NamedProvider{{Name: "fixedfloat", Provider: ff}, {Name: "loser", Provider: loser}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	pair := Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	order, err := m.CreateOrder(context.Background(), pair, amountIn, "0xcustomer")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.ProviderOrderID != "fixedfloat:aB3xY9|sekrit-token" {
		t.Fatalf("expected the router's own prefix wrapping FixedFloat's own id|token ref, got %q", order.ProviderOrderID)
	}

	got, err := m.GetOrder(context.Background(), order.ProviderOrderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.Status != StatusComplete {
		t.Errorf("expected DONE to map to StatusComplete through the router too, got %s", got.Status)
	}
}

func TestMultiProviderGetOrder_RejectsAnUnprefixedOrUnknownVendorID(t *testing.T) {
	a := NewMockProvider("a", 1)
	b := NewMockProvider("b", 2)
	m, err := NewMultiProvider([]NamedProvider{{Name: "a", Provider: a}, {Name: "b", Provider: b}})
	if err != nil {
		t.Fatalf("NewMultiProvider: %v", err)
	}

	if _, err := m.GetOrder(context.Background(), "no-colon-here"); err == nil {
		t.Fatal("expected an error for an order id with no vendor prefix")
	}
	if _, err := m.GetOrder(context.Background(), "nonexistent-vendor:some-id"); err == nil {
		t.Fatal("expected an error for a prefix naming an unconfigured vendor")
	}
}
