//go:build integration

// Requires a real Postgres (RELAYD_TEST_DATABASE_URL): vendor health and
// administrator settings live in the database.
package vendors_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"relayd/internal/db"
	"relayd/internal/money"
	"relayd/internal/upstream"
	"relayd/internal/vendors"
)

func testStore(t *testing.T) (*vendors.Store, *db.Pool) {
	t.Helper()
	url := os.Getenv("RELAYD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RELAYD_TEST_DATABASE_URL not set; skipping integration test")
	}
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	_, thisFile, _, _ := runtime.Caller(0)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return vendors.NewStore(pool), pool
}

var nameSeq int64

// uniqueName keeps vendors from different tests apart in the shared
// test database.
func uniqueName(base string) string {
	return fmt.Sprintf("%s%d", base, atomic.AddInt64(&nameSeq, 1)+time.Now().UnixNano()%1_000_000)
}

var pair = upstream.Pair{From: money.USDT_BEP20, To: money.USDT_TRC20}
var hundred = money.Amount{Asset: money.USDT_BEP20, Units: 100_000000}

func usdtOut(units int64) money.Amount { return money.Amount{Asset: money.USDT_TRC20, Units: units} }

func vendorRow(t *testing.T, store *vendors.Store, service vendors.Service, name string) vendors.Vendor {
	t.Helper()
	v, err := store.Get(context.Background(), service, name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The best-paying vendor wins; when it goes down, the next one takes the
// order and the failed one sits out -- then comes back once it answers.
func TestConversionRouter_FailsOverOnAnOutageAndRestoresAfterward(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	if err := store.SetStrategy(ctx, vendors.Conversion, vendors.BestRate); err != nil {
		t.Fatal(err)
	}
	best, other := uniqueName("best"), uniqueName("other")
	a, b := upstream.NewMockProvider(best, 1), upstream.NewMockProvider(other, 2)
	a.ForceAmountOut(usdtOut(99_000000))
	b.ForceAmountOut(usdtOut(98_000000))
	router, err := vendors.NewConversionRouter(ctx, store, map[string]upstream.SwapProvider{best: a, other: b})
	if err != nil {
		t.Fatal(err)
	}

	q, err := router.Quote(ctx, pair, hundred)
	if err != nil || q.ProviderName != best {
		t.Fatalf("expected %s's better quote, got %s (%v)", best, q.ProviderName, err)
	}

	a.ForceCreateOrderError(errors.New("connection reset by peer")) // an outage
	order, err := router.CreateOrder(ctx, pair, hundred, "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	if err != nil || order.ProviderName != other {
		t.Fatalf("expected the order to fail over to %s, got %q (%v)", other, order.ProviderName, err)
	}
	if v := vendorRow(t, store, vendors.Conversion, best); v.Available(time.Now()) || v.ConsecutiveFailures != 1 || v.LastError == nil {
		t.Fatalf("expected %s out of the pool after its outage, got %+v", best, v)
	}
	if q, err := router.Quote(ctx, pair, hundred); err != nil || q.ProviderName != other {
		t.Fatalf("expected only %s quoting while %s sits out, got %s (%v)", other, best, q.ProviderName, err)
	}

	// Its back-off passes and it answers again: back in the pool, with no
	// administrator involved.
	a.ForceCreateOrderError(nil)
	if _, err := pool.Exec(ctx, `UPDATE vendors SET unavailable_until = now() - interval '1 second' WHERE service = 'conversion' AND name = $1`, best); err != nil {
		t.Fatal(err)
	}
	if q, err := router.Quote(ctx, pair, hundred); err != nil || q.ProviderName != best {
		t.Fatalf("expected %s's better quote to win again, got %s (%v)", best, q.ProviderName, err)
	}
	if v := vendorRow(t, store, vendors.Conversion, best); v.ConsecutiveFailures != 0 || v.UnavailableUntil != nil {
		t.Fatalf("expected %s's failure record cleared by its successful answer, got %+v", best, v)
	}
}

// A vendor refusing one order (its own business rule) is skipped for
// that order but stays in the pool.
func TestConversionRouter_ARejectionFailsOverButKeepsTheVendor(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	if err := store.SetStrategy(ctx, vendors.Conversion, vendors.BestRate); err != nil {
		t.Fatal(err)
	}
	picky, other := uniqueName("picky"), uniqueName("fallback")
	a, b := upstream.NewMockProvider(picky, 1), upstream.NewMockProvider(other, 2)
	a.ForceAmountOut(usdtOut(99_000000))
	b.ForceAmountOut(usdtOut(98_000000))
	a.ForceCreateOrderError(&upstream.APIError{Code: 400, Msg: "amount below minimum"})
	router, err := vendors.NewConversionRouter(ctx, store, map[string]upstream.SwapProvider{picky: a, other: b})
	if err != nil {
		t.Fatal(err)
	}
	order, err := router.CreateOrder(ctx, pair, hundred, "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	if err != nil || order.ProviderName != other {
		t.Fatalf("expected the order to go to %s, got %q (%v)", other, order.ProviderName, err)
	}
	if v := vendorRow(t, store, vendors.Conversion, picky); !v.Available(time.Now()) || v.ConsecutiveFailures != 0 {
		t.Fatalf("a refusal must not take %s out of the pool, got %+v", picky, v)
	}
}

// Under the priority strategy the administrator's order wins over rate;
// a disabled vendor is never used for new orders, but its existing
// orders are still followed.
func TestConversionRouter_PriorityAndDisabledVendors(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	preferred, cheaper := uniqueName("preferred"), uniqueName("cheaper")
	a, b := upstream.NewMockProvider(preferred, 1), upstream.NewMockProvider(cheaper, 2)
	a.ForceAmountOut(usdtOut(97_000000))
	b.ForceAmountOut(usdtOut(99_000000))
	router, err := vendors.NewConversionRouter(ctx, store, map[string]upstream.SwapProvider{preferred: a, cheaper: b})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPriority(ctx, vendors.Conversion, preferred, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPriority(ctx, vendors.Conversion, cheaper, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStrategy(ctx, vendors.Conversion, vendors.Priority); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.SetStrategy(context.Background(), vendors.Conversion, vendors.BestRate) })

	order, err := router.CreateOrder(ctx, pair, hundred, "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	if err != nil || order.ProviderName != preferred {
		t.Fatalf("expected the administrator's first choice %s, got %q (%v)", preferred, order.ProviderName, err)
	}

	if err := store.SetEnabled(ctx, vendors.Conversion, preferred, false); err != nil {
		t.Fatal(err)
	}
	next, err := router.CreateOrder(ctx, pair, hundred, "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	if err != nil || next.ProviderName != cheaper {
		t.Fatalf("expected %s once %s is disabled, got %q (%v)", cheaper, preferred, next.ProviderName, err)
	}
	if _, err := router.GetOrderFrom(ctx, preferred, order.ProviderOrderID); err != nil {
		t.Fatalf("an existing order with a disabled vendor must still be followed: %v", err)
	}
}

// fakeEnergyVendor is an energy vendor with a set price and outcome.
type fakeEnergyVendor struct {
	mu       sync.Mutex
	min      int64
	perUnit  int64
	err      error
	rentedTo []string
	units    []int64
}

func (f *fakeEnergyVendor) MinUnits() int64 { return f.min }
func (f *fakeEnergyVendor) QuoteSun(ctx context.Context, units int64) (int64, error) {
	return units * f.perUnit, nil
}
func (f *fakeEnergyVendor) Rent(ctx context.Context, target string, units int64) (vendors.Rental, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return vendors.Rental{}, f.err
	}
	f.rentedTo = append(f.rentedTo, target)
	f.units = append(f.units, units)
	return vendors.Rental{OrderID: "o1", CostSun: units * f.perUnit}, nil
}

// Energy goes to the cheapest vendor -- the exact shortfall, raised only
// to that vendor's minimum -- and fails over when it's down.
func TestEnergyRouter_RentsFromTheCheapestAndFailsOver(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	if err := store.SetStrategy(ctx, vendors.Energy, vendors.Cheapest); err != nil {
		t.Fatal(err)
	}
	cheap, pricey := uniqueName("cheap"), uniqueName("pricey")
	c := &fakeEnergyVendor{min: 65_000, perUnit: 60}
	p := &fakeEnergyVendor{min: 32_000, perUnit: 90}
	router, err := vendors.NewEnergyRouter(ctx, store, map[string]vendors.EnergyVendor{cheap: c, pricey: p})
	if err != nil {
		t.Fatal(err)
	}

	res, err := router.Reserve(ctx, "leg-1", "TDepositWallet", 59_285, "STANDARD", time.Now().Add(time.Minute), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if *res.Vendor != cheap || res.EnergyUnits != 65_000 || len(c.units) != 1 || c.units[0] != 65_000 {
		t.Fatalf("expected %s to rent its 65000 minimum, got %+v (units %v)", cheap, res, c.units)
	}
	if res.CostTRX == nil || *res.CostTRX != "3.900000" {
		t.Fatalf("expected the cost recorded as 3.900000 TRX, got %v", res.CostTRX)
	}

	c.err = errors.New("vendor timeout")
	res, err = router.Reserve(ctx, "leg-2", "TDepositWallet", 130_285, "STANDARD", time.Now().Add(time.Minute), "k2")
	if err != nil || *res.Vendor != pricey || res.EnergyUnits != 130_285 {
		t.Fatalf("expected a failover to %s renting exactly 130285, got %+v (%v)", pricey, res, err)
	}
	if v := vendorRow(t, store, vendors.Energy, cheap); v.Available(time.Now()) {
		t.Fatalf("expected %s out of the pool after its outage", cheap)
	}
}

// Under best_margin the vendor paying us the larger revenue share takes
// the order even at a slightly worse customer rate; under best_rate the
// customer's rate decides. Terms are kept on the vendor's record.
func TestConversionRouter_BestMarginPrefersOurRevenue(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	generous, cheap := uniqueName("generous"), uniqueName("cheap")
	a, b := upstream.NewMockProvider(generous, 1), upstream.NewMockProvider(cheap, 2)
	a.ForceAmountOut(usdtOut(98_500000))
	b.ForceAmountOut(usdtOut(99_000000))
	router, err := vendors.NewConversionRouter(ctx, store, map[string]upstream.SwapProvider{generous: a, cheap: b})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTerms(ctx, vendors.Conversion, generous, 50, "pays 0.5% referral"); err != nil {
		t.Fatal(err)
	}
	if v := vendorRow(t, store, vendors.Conversion, generous); v.RevenueBPS != 50 || v.Notes != "pays 0.5% referral" {
		t.Fatalf("expected the terms kept on the vendor record, got %+v", v)
	}
	t.Cleanup(func() { _ = store.SetStrategy(context.Background(), vendors.Conversion, vendors.BestRate) })

	if err := store.SetStrategy(ctx, vendors.Conversion, vendors.BestMargin); err != nil {
		t.Fatal(err)
	}
	if q, err := router.Quote(ctx, pair, hundred); err != nil || q.ProviderName != generous {
		t.Fatalf("best_margin: expected %s (it pays us), got %s (%v)", generous, q.ProviderName, err)
	}
	if err := store.SetStrategy(ctx, vendors.Conversion, vendors.BestRate); err != nil {
		t.Fatal(err)
	}
	if q, err := router.Quote(ctx, pair, hundred); err != nil || q.ProviderName != cheap {
		t.Fatalf("best_rate: expected %s (better for the customer), got %s (%v)", cheap, q.ProviderName, err)
	}
	if err := store.SetTerms(ctx, vendors.Conversion, generous, 10_000, ""); err == nil {
		t.Fatal("a revenue share of 100% or more must be refused")
	}
}
