//go:build integration

// Sweeping profit from deposit wallets to the treasury. Requires the same
// real Postgres + real ledgerd as orchestrate_integration_test.go.
package orchestrate_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/sweeps"
	"relayd/internal/transfers"
)

// Deposits of 8000 USDT at 25 bps leave 20 USDT of profit per order;
// the sweep minimum of 15 USDT is above anything other tests in this
// package leave behind, so only these tests' wallets are swept.
const sweepTestDeposit = "8000.000000"

var sweepTestProfit = money.Amount{Asset: money.USDT_BEP20, Units: 20_000000}

// settledWithProfit drives f's leg to SETTLED, leaving its profit in the
// deposit wallet.
func settledWithProfit(t *testing.T, f *bep20Fixture, o *orchestrate.Orchestrator) {
	t.Helper()
	f.tick(o)
	leg := f.leg()
	if leg.ProfitAmount == nil || *leg.ProfitAmount != sweepTestProfit {
		t.Fatalf("expected %v of profit recorded, got %v", sweepTestProfit, leg.ProfitAmount)
	}
	attempts := f.attempts()
	if len(attempts) != 1 || attempts[0].TxHash == nil {
		t.Fatalf("expected the forward sent, got %+v", attempts)
	}
	f.finality.MarkFinal(*attempts[0].TxHash)
	f.tick(o)
	vendorOrder, err := f.vendor.GetOrder(context.Background(), *leg.UpstreamOrderID)
	if err != nil {
		t.Fatal(err)
	}
	payout := vendorOrder.AmountOutExpected
	if err := f.vendor.SetOrderStatus(*leg.UpstreamOrderID, "complete", &payout); err != nil {
		t.Fatal(err)
	}
	f.tick(o)
	if got := f.leg().Status; got != relay.StatusSettled {
		t.Fatalf("expected SETTLED, got %s", got)
	}
}

func enableSweeps(t *testing.T, store *sweeps.Store, enabled bool) {
	t.Helper()
	if err := store.PutSettings(context.Background(), sweeps.Settings{
		Enabled: enabled, Interval: 5 * time.Minute,
		MinAmount: map[money.Asset]int64{money.USDT_BEP20: 15_000000, money.USDT_TRC20: 15_000000},
	}, "test"); err != nil {
		t.Fatal(err)
	}
}

func sweepsFrom(t *testing.T, store *sweeps.Store, address string) []sweeps.Sweep {
	t.Helper()
	all, err := store.List(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []sweeps.Sweep
	for _, s := range all {
		if s.FromAddress == address {
			out = append(out, s)
		}
	}
	return out
}

func sweepAttempts(t *testing.T, f *bep20Fixture, s sweeps.Sweep) []transfers.Attempt {
	t.Helper()
	list, err := transfers.NewStore(f.pool).ListForLeg(context.Background(), "sweep:"+itoa(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func itoa(n int64) string { return big.NewInt(n).String() }

// A settled order's profit is swept from its idle wallet to the treasury
// in one transfer signed by the wallet's own key, then booked: out of the
// commission wallets, into the treasury. Nothing is swept twice.
func TestSweep_MovesSettledProfitToTheTreasuryAndBooksIt(t *testing.T) {
	f := newBEP20FixtureWithDeposit(t, 91, sweepTestDeposit, bps(25))
	store := sweeps.NewStore(f.pool)
	o := f.newOrchestrator(0)
	o.Sweeps = store
	enableSweeps(t, store, false)
	settledWithProfit(t, f, o)
	if got := sweepsFrom(t, store, f.deposit); len(got) != 0 {
		t.Fatalf("sweeping is disabled, yet %d sweeps were started", len(got))
	}

	treasuryBefore := f.ledger.AccountBalance("asset:relay:treasury:USDT_BEP20")
	walletsBefore := f.ledger.AccountBalance("asset:relay:commission_wallet:USDT_BEP20")
	enableSweeps(t, store, true)
	o.RequestSweepScan()
	f.tick(o)

	got := sweepsFrom(t, store, f.deposit)
	if len(got) != 1 {
		t.Fatalf("expected one sweep of %s, got %+v", f.deposit, got)
	}
	s := got[0]
	if s.Status != sweeps.StatusPending || s.Amount != sweepTestProfit || s.ToAddress != signing.FakeSlotEVMAddress(1) || s.DepositIndex != 91 {
		t.Fatalf("expected a pending 20 USDT sweep to the treasury signed by index 91, got %+v", s)
	}
	attempts := sweepAttempts(t, f, s)
	if len(attempts) != 1 || attempts[0].Status != transfers.StatusBroadcast || attempts[0].FromAddress != f.deposit ||
		attempts[0].Amount != sweepTestProfit || attempts[0].Purpose != transfers.Sweep {
		t.Fatalf("expected one broadcast 20 USDT sweep transfer from the deposit wallet, got %+v", attempts)
	}

	// A new scan while it's in flight starts nothing more.
	o.RequestSweepScan()
	f.tick(o)
	if got := sweepsFrom(t, store, f.deposit); len(got) != 1 {
		t.Fatalf("expected no second sweep while one is in flight, got %d", len(got))
	}

	f.finality.MarkFinal(*attempts[0].TxHash)
	f.tick(o)
	s = sweepsFrom(t, store, f.deposit)[0]
	if s.Status != sweeps.StatusConfirmed || s.TxHash == nil || *s.TxHash != *attempts[0].TxHash || s.LedgerEntryID == nil {
		t.Fatalf("expected the sweep CONFIRMED with its transaction and ledger entry, got %+v", s)
	}
	if d := f.ledger.AccountBalance("asset:relay:treasury:USDT_BEP20") - treasuryBefore; d != sweepTestProfit.Units {
		t.Errorf("treasury grew by %d, want %d", d, sweepTestProfit.Units)
	}
	if d := f.ledger.AccountBalance("asset:relay:commission_wallet:USDT_BEP20") - walletsBefore; d != -sweepTestProfit.Units {
		t.Errorf("commission wallets changed by %d, want %d", d, -sweepTestProfit.Units)
	}

	o.RequestSweepScan()
	f.tick(o)
	if got := sweepsFrom(t, store, f.deposit); len(got) != 1 {
		t.Fatalf("the profit was already swept, yet %d sweeps exist", len(got))
	}
}

// A wallet an order is still using is left alone; a wallet holding less
// than its recorded profit is not swept but alerted on; once both clear,
// it is swept.
func TestSweep_LeavesBusyAndShortWalletsAlone(t *testing.T) {
	f := newBEP20FixtureWithDeposit(t, 92, sweepTestDeposit, bps(25))
	store := sweeps.NewStore(f.pool)
	o := f.newOrchestrator(0)
	o.Sweeps = store
	enableSweeps(t, store, false)
	settledWithProfit(t, f, o)

	// A new order leases the same wallet.
	index := uint32(92)
	next, err := f.store.Create(context.Background(), relay.Leg{
		ExternalID: f.externalID + "-next", OrderID: f.orderID + 900_000_000, Direction: relay.BEP20ToTRC20,
		CustomerID: f.customerID, DestinationAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		DepositAddress: f.deposit, DepositDerivationIndex: &index,
		AmountIn:          money.Amount{Asset: money.USDT_BEP20, Units: 100_000000},
		AmountOutExpected: money.Amount{Asset: money.USDT_TRC20, Units: 99_000000},
	})
	if err != nil {
		t.Fatal(err)
	}
	enableSweeps(t, store, true)
	o.RequestSweepScan()
	f.tick(o)
	if got := sweepsFrom(t, store, f.deposit); len(got) != 0 {
		t.Fatalf("a wallet in use by an order was swept: %+v", got)
	}

	// The order ends, but the wallet holds less than the books say.
	if _, err := f.pool.Exec(context.Background(), `UPDATE relay_legs SET status = 'EXPIRED' WHERE external_id = $1`, next.ExternalID); err != nil {
		t.Fatal(err)
	}
	f.chain.mu.Lock()
	f.chain.tokenBalance = new(big.Int).Mul(big.NewInt(19_000000), big.NewInt(1_000_000_000_000)) // 19 USDT, 18 decimals
	f.chain.mu.Unlock()
	o.RequestSweepScan()
	f.tick(o)
	if got := sweepsFrom(t, store, f.deposit); len(got) != 0 {
		t.Fatalf("a wallet short of its recorded profit was swept: %+v", got)
	}
	short := 0
	for _, a := range f.alerts.Fired() {
		if a.Reason == "sweep_balance_short" && a.ExternalID == "sweep:"+f.deposit {
			short++
		}
	}
	if short != 1 {
		t.Fatalf("expected one sweep_balance_short alert for %s, got %d", f.deposit, short)
	}

	f.chain.mu.Lock()
	f.chain.tokenBalance = new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	f.chain.mu.Unlock()
	o.RequestSweepScan()
	f.tick(o)
	if got := sweepsFrom(t, store, f.deposit); len(got) != 1 || got[0].Amount != sweepTestProfit {
		t.Fatalf("expected the wallet swept once it is idle and funded, got %+v", got)
	}
}

// A sweep that never gets a transaction onto the chain is given up after
// its deadline, and the wallet waits a day before it is tried again.
func TestSweep_GivesUpAStuckSweep(t *testing.T) {
	f := newBEP20FixtureWithDeposit(t, 93, sweepTestDeposit, bps(25))
	store := sweeps.NewStore(f.pool)
	o := f.newOrchestrator(0)
	o.Sweeps = store
	enableSweeps(t, store, false)
	settledWithProfit(t, f, o)

	// Neither the wallet nor the treasury can pay the sweep's gas.
	f.chain.setNative(f.deposit, big.NewInt(0))
	f.chain.setNative(signing.FakeSlotEVMAddress(1), big.NewInt(0))
	enableSweeps(t, store, true)
	o.RequestSweepScan()
	f.tick(o)
	got := sweepsFrom(t, store, f.deposit)
	if len(got) != 1 || got[0].Status != sweeps.StatusPending {
		t.Fatalf("expected one pending sweep, got %+v", got)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE sweeps SET created_at = now() - interval '7 hours' WHERE id = $1`, got[0].ID); err != nil {
		t.Fatal(err)
	}
	f.tick(o)
	s := sweepsFrom(t, store, f.deposit)[0]
	if s.Status != sweeps.StatusFailed || s.Error == nil {
		t.Fatalf("expected the stuck sweep given up, got %+v", s)
	}
	for _, a := range sweepAttempts(t, f, s) {
		if a.Status.Open() {
			t.Fatalf("a given-up sweep left an open transfer behind: %+v", a)
		}
	}

	o.RequestSweepScan()
	f.tick(o)
	if got := sweepsFrom(t, store, f.deposit); len(got) != 1 {
		t.Fatalf("expected the wallet left alone for a day after a failed sweep, got %d sweeps", len(got))
	}
}
