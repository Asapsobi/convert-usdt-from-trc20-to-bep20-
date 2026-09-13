package upstream

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"relayd/internal/money"
)

// ErrMalformedResponse is returned by MockProvider for a provider
// configured via ForceMalformed. It exists so a caller can distinguish
// "the vendor sent garbage" from every other error shape -- never
// silently coerced into a zero-value Quote/SwapOrder treated as valid.
var ErrMalformedResponse = errors.New("upstream: malformed response")

// ErrOrderNotFound is MockProvider's GetOrder result for a
// providerOrderID it never issued.
var ErrOrderNotFound = errors.New("upstream: order not found")

// defaultRateNumerator/Denominator model a mock exchange rate close to
// 1:1 (both legs are USDT, on different chains) with a small spread --
// close enough to exercise real rounding/tolerance logic in later
// chunks without claiming to be a real vendor's actual rate.
const (
	defaultRateNumerator   = 997
	defaultRateDenominator = 1000
)

// MockProvider is a deterministic, seeded SwapProvider -- the fake every
// relayd package's tests use instead of a real vendor, mirroring
// energybroker/internal/provider.MockProvider's own Force*-knob and
// call-counter shape. Determinism here is by seeded PRNG, not
// content-hash: a real instant-exchange rate is a live, continuously
// fluctuating number, so "same seed always yields the same quote
// sequence" means call order matters and is reproducible, not
// call-order-independent.
type MockProvider struct {
	name string

	mu                   sync.Mutex
	rng                  *rand.Rand
	forcedAmountOut      *money.Amount
	forcedDepositAddress *string
	forceQuoteErr        error
	forceCreateErr       error
	forceGetErr          error
	forceTimeout         bool
	forceMalformed       bool
	validFor             time.Duration
	orders               map[string]SwapOrder
	orderSeq             int64
	quoteCalls           int
	createOrderCalls     int
	getOrderCalls        int
}

// NewMockProvider returns a MockProvider named name, seeded with seed.
// Two MockProviders constructed with the same name and seed, called the
// same number of times, produce an identical quote sequence.
func NewMockProvider(name string, seed int64) *MockProvider {
	return &MockProvider{
		name:     name,
		rng:      rand.New(rand.NewSource(seed)),
		validFor: 90 * time.Second,
		orders:   make(map[string]SwapOrder),
	}
}

// ForceQuoteError configures every subsequent Quote call to fail with
// err.
func (m *MockProvider) ForceQuoteError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceQuoteErr = err
}

// ForceCreateOrderError configures every subsequent CreateOrder call to
// fail with err.
func (m *MockProvider) ForceCreateOrderError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceCreateErr = err
}

// ForceGetOrderError configures every subsequent GetOrder call to fail
// with err.
func (m *MockProvider) ForceGetOrderError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceGetErr = err
}

// ForceTimeout configures every subsequent call to never return --
// blocks until ctx is done and returns ctx.Err(), exactly what a hung
// real vendor call would look like to a caller that applied its own
// timeout.
func (m *MockProvider) ForceTimeout() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceTimeout = true
}

// ForceMalformed configures every subsequent call to return
// ErrMalformedResponse, simulating a vendor response this client can't
// parse.
func (m *MockProvider) ForceMalformed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceMalformed = true
}

// ForceDepositAddress pins every subsequent CreateOrder's own
// DepositAddress to addr, instead of this mock's own default fake
// placeholder ("mock-deposit-addr-<seq>") -- needed by any caller that
// goes on to actually build a real chain transaction against the
// returned deposit address (e.g. relayd/internal/txbuild.BuildTransfer,
// which validates a TRON address's own base58check checksum before
// constructing anything), since a fabricated placeholder string is not
// a valid address on any real chain.
func (m *MockProvider) ForceDepositAddress(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forcedDepositAddress = &addr
}

// ForceAmountOut pins every subsequent Quote/CreateOrder's AmountOut,
// regardless of AmountIn -- lets a caller's test exercise a specific
// exchange outcome (e.g. a rate that moved outside tolerance between
// quote and forward time, per architecture doc §6) without depending on
// this mock's own default rate math.
func (m *MockProvider) ForceAmountOut(amt money.Amount) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forcedAmountOut = &amt
}

// SetValidFor overrides this mock's own Quote.ValidUntil window (default
// 90s).
func (m *MockProvider) SetValidFor(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validFor = d
}

// QuoteCallCount reports how many times Quote has been called.
func (m *MockProvider) QuoteCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quoteCalls
}

// CreateOrderCallCount reports how many times CreateOrder has been
// called -- exported so a caller (relayd's own state-machine tests) can
// assert exactly-once order placement per relay leg.
func (m *MockProvider) CreateOrderCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createOrderCalls
}

// GetOrderCallCount reports how many times GetOrder has been called.
func (m *MockProvider) GetOrderCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getOrderCalls
}

// SetOrderStatus lets a test advance an already-created mock order to a
// new status (e.g. simulating the upstream platform completing the swap
// asynchronously), without going through CreateOrder again.
func (m *MockProvider) SetOrderStatus(providerOrderID string, status SwapStatus, amountOutActual *money.Amount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.orders[providerOrderID]
	if !ok {
		return fmt.Errorf("mock provider %s: %w: %s", m.name, ErrOrderNotFound, providerOrderID)
	}
	order.Status = status
	order.AmountOutActual = amountOutActual
	m.orders[providerOrderID] = order
	return nil
}

func (m *MockProvider) computeAmountOut(pair Pair, amountIn money.Amount) (money.Amount, error) {
	if m.forcedAmountOut != nil {
		return *m.forcedAmountOut, nil
	}
	units := amountIn.Units * defaultRateNumerator / defaultRateDenominator
	return money.Amount{Asset: pair.To, Units: units}, nil
}

// Quote implements upstream.SwapProvider.
func (m *MockProvider) Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error) {
	m.mu.Lock()
	m.quoteCalls++
	forceTimeout := m.forceTimeout
	forceMalformed := m.forceMalformed
	forceErr := m.forceQuoteErr
	validFor := m.validFor
	m.mu.Unlock()

	if forceTimeout {
		<-ctx.Done()
		return Quote{}, ctx.Err()
	}
	if forceErr != nil {
		return Quote{}, forceErr
	}
	if forceMalformed {
		return Quote{}, fmt.Errorf("mock provider %s: %w", m.name, ErrMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return Quote{}, err
	}

	amountOut, err := m.computeAmountOut(pair, amountIn)
	if err != nil {
		return Quote{}, err
	}
	now := time.Now().UTC()
	// Advance the PRNG by exactly one draw per call, matching
	// energybroker's own MockProvider.nextPrice discipline, so a forced
	// value configured mid-sequence doesn't shift what a later,
	// un-forced call in the same run would have quoted.
	m.mu.Lock()
	_ = m.rng.Float64()
	m.mu.Unlock()

	return Quote{
		ProviderName: m.name,
		Pair:         pair,
		AmountIn:     amountIn,
		AmountOut:    amountOut,
		QuotedAt:     now,
		ValidUntil:   now.Add(validFor),
	}, nil
}

// CreateOrder implements upstream.SwapProvider.
func (m *MockProvider) CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error) {
	m.mu.Lock()
	m.createOrderCalls++
	forceTimeout := m.forceTimeout
	forceMalformed := m.forceMalformed
	forceErr := m.forceCreateErr
	m.mu.Unlock()

	if forceTimeout {
		<-ctx.Done()
		return SwapOrder{}, ctx.Err()
	}
	if forceErr != nil {
		return SwapOrder{}, forceErr
	}
	if forceMalformed {
		return SwapOrder{}, fmt.Errorf("mock provider %s: %w", m.name, ErrMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return SwapOrder{}, err
	}

	amountOut, err := m.computeAmountOut(pair, amountIn)
	if err != nil {
		return SwapOrder{}, err
	}

	m.mu.Lock()
	m.orderSeq++
	seq := m.orderSeq
	depositAddress := fmt.Sprintf("mock-deposit-addr-%d", seq)
	if m.forcedDepositAddress != nil {
		depositAddress = *m.forcedDepositAddress
	}
	m.mu.Unlock()

	order := SwapOrder{
		ProviderName:       m.name,
		ProviderOrderID:    fmt.Sprintf("mock-%s-%d", m.name, seq),
		DepositAddress:     depositAddress,
		DestinationAddress: destinationAddress,
		Status:             StatusAwaitingDeposit,
		AmountIn:           amountIn,
		AmountOutExpected:  amountOut,
		AmountOutActual:    nil,
		CreatedAt:          time.Now().UTC(),
	}
	m.mu.Lock()
	m.orders[order.ProviderOrderID] = order
	m.mu.Unlock()
	return order, nil
}

// GetOrder implements upstream.SwapProvider.
func (m *MockProvider) GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error) {
	m.mu.Lock()
	m.getOrderCalls++
	forceTimeout := m.forceTimeout
	forceMalformed := m.forceMalformed
	forceErr := m.forceGetErr
	m.mu.Unlock()

	if forceTimeout {
		<-ctx.Done()
		return SwapOrder{}, ctx.Err()
	}
	if forceErr != nil {
		return SwapOrder{}, forceErr
	}
	if forceMalformed {
		return SwapOrder{}, fmt.Errorf("mock provider %s: %w", m.name, ErrMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return SwapOrder{}, err
	}

	m.mu.Lock()
	order, ok := m.orders[providerOrderID]
	m.mu.Unlock()
	if !ok {
		return SwapOrder{}, fmt.Errorf("mock provider %s: %w: %s", m.name, ErrOrderNotFound, providerOrderID)
	}
	return order, nil
}
