package upstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"relayd/internal/money"
)

var trc20ToBep20 = Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}

func TestPlaceholderProviderAlwaysFails(t *testing.T) {
	p := PlaceholderProvider{}
	ctx := context.Background()
	amt := money.Amount{Asset: money.USDT_TRC20, Units: 1_000000}

	if _, err := p.Quote(ctx, trc20ToBep20, amt); !errors.Is(err, ErrNoVendorConfigured) {
		t.Errorf("Quote: got %v, want ErrNoVendorConfigured", err)
	}
	if _, err := p.CreateOrder(ctx, trc20ToBep20, amt, "dest"); !errors.Is(err, ErrNoVendorConfigured) {
		t.Errorf("CreateOrder: got %v, want ErrNoVendorConfigured", err)
	}
	if _, err := p.GetOrder(ctx, "any"); !errors.Is(err, ErrNoVendorConfigured) {
		t.Errorf("GetOrder: got %v, want ErrNoVendorConfigured", err)
	}
}

func TestMockProviderQuoteDeterministic(t *testing.T) {
	amt := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	m1 := NewMockProvider("mock", 42)
	m2 := NewMockProvider("mock", 42)

	q1, err := m1.Quote(context.Background(), trc20ToBep20, amt)
	if err != nil {
		t.Fatalf("m1.Quote: %v", err)
	}
	q2, err := m2.Quote(context.Background(), trc20ToBep20, amt)
	if err != nil {
		t.Fatalf("m2.Quote: %v", err)
	}
	if q1.AmountOut != q2.AmountOut {
		t.Errorf("same seed produced different quotes: %v vs %v", q1.AmountOut, q2.AmountOut)
	}
	if q1.AmountOut.Asset != money.USDT_BEP20 {
		t.Errorf("expected AmountOut asset USDT_BEP20, got %s", q1.AmountOut.Asset)
	}
}

func TestMockProviderCreateAndGetOrder(t *testing.T) {
	m := NewMockProvider("mock", 1)
	amt := money.Amount{Asset: money.USDT_TRC20, Units: 50_000000}
	ctx := context.Background()

	order, err := m.CreateOrder(ctx, trc20ToBep20, amt, "customer-bep20-address")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.DestinationAddress != "customer-bep20-address" {
		t.Errorf("destination address not preserved: got %q", order.DestinationAddress)
	}
	if order.Status != StatusAwaitingDeposit {
		t.Errorf("expected StatusAwaitingDeposit, got %s", order.Status)
	}
	if m.CreateOrderCallCount() != 1 {
		t.Errorf("expected 1 CreateOrder call, got %d", m.CreateOrderCallCount())
	}

	got, err := m.GetOrder(ctx, order.ProviderOrderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got != order {
		t.Errorf("GetOrder returned a different order than CreateOrder: %+v vs %+v", got, order)
	}
}

func TestMockProviderGetOrderNotFound(t *testing.T) {
	m := NewMockProvider("mock", 1)
	if _, err := m.GetOrder(context.Background(), "never-created"); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("got %v, want ErrOrderNotFound", err)
	}
}

func TestMockProviderSetOrderStatus(t *testing.T) {
	m := NewMockProvider("mock", 1)
	amt := money.Amount{Asset: money.USDT_TRC20, Units: 10_000000}
	order, err := m.CreateOrder(context.Background(), trc20ToBep20, amt, "dest")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	actual := money.Amount{Asset: money.USDT_BEP20, Units: 9_970000}
	if err := m.SetOrderStatus(order.ProviderOrderID, StatusComplete, &actual); err != nil {
		t.Fatalf("SetOrderStatus: %v", err)
	}

	got, err := m.GetOrder(context.Background(), order.ProviderOrderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.Status != StatusComplete {
		t.Errorf("expected StatusComplete, got %s", got.Status)
	}
	if got.AmountOutActual == nil || *got.AmountOutActual != actual {
		t.Errorf("AmountOutActual not updated: got %v", got.AmountOutActual)
	}
}

func TestMockProviderForceErrors(t *testing.T) {
	m := NewMockProvider("mock", 1)
	amt := money.Amount{Asset: money.USDT_TRC20, Units: 1_000000}
	sentinel := errors.New("boom")

	m.ForceQuoteError(sentinel)
	if _, err := m.Quote(context.Background(), trc20ToBep20, amt); !errors.Is(err, sentinel) {
		t.Errorf("Quote: got %v, want %v", err, sentinel)
	}

	m2 := NewMockProvider("mock", 1)
	m2.ForceMalformed()
	if _, err := m2.CreateOrder(context.Background(), trc20ToBep20, amt, "dest"); !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("CreateOrder: got %v, want ErrMalformedResponse", err)
	}
}

func TestMockProviderForceTimeoutRespectsContext(t *testing.T) {
	m := NewMockProvider("mock", 1)
	m.ForceTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	amt := money.Amount{Asset: money.USDT_TRC20, Units: 1_000000}
	_, err := m.Quote(ctx, trc20ToBep20, amt)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want context.DeadlineExceeded", err)
	}
}

func TestMockProviderForceAmountOut(t *testing.T) {
	m := NewMockProvider("mock", 1)
	forced := money.Amount{Asset: money.USDT_BEP20, Units: 1_234567}
	m.ForceAmountOut(forced)

	amt := money.Amount{Asset: money.USDT_TRC20, Units: 1_000000}
	q, err := m.Quote(context.Background(), trc20ToBep20, amt)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.AmountOut != forced {
		t.Errorf("got %v, want forced %v", q.AmountOut, forced)
	}
}
