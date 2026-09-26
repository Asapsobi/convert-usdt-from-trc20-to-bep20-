//go:build integration

// What relayd forwards, keeps, and refunds is computed from what the
// customer actually sent -- never from their quote. Requires the same
// real Postgres + real ledgerd as orchestrate_integration_test.go.
package orchestrate_test

import (
	"context"
	"fmt"
	"testing"

	"relayd/internal/money"
	"relayd/internal/pricing"
	"relayd/internal/relay"
	"relayd/internal/transfers"
)

func bps(v int64) *int64 { return &v }

// A customer quoted 100 USDT who sends 150 has 150 forwarded, minus our
// profit on 150 -- the vendor order, the on-chain forward, and the ledger
// all agree on the same figures.
func TestAmounts_OverpaidDepositIsForwardedInFull(t *testing.T) {
	f := newBEP20FixtureWithDeposit(t, 61, "150.000000", bps(25))
	o := f.newOrchestrator(0)
	f.tick(o)

	leg := f.leg()
	if leg.ReceivedAmount == nil || leg.ReceivedAmount.Units != 150_000000 {
		t.Fatalf("expected the 150 USDT that arrived to be recorded, got %v", leg.ReceivedAmount)
	}
	// 25 bps of 150 = 0.375.
	if leg.ProfitAmount == nil || leg.ProfitAmount.Units != 375000 || leg.ForwardAmount == nil || leg.ForwardAmount.Units != 149_625000 {
		t.Fatalf("expected profit 0.375 and forward 149.625, got %v / %v", leg.ProfitAmount, leg.ForwardAmount)
	}
	vendorOrder, err := f.vendor.GetOrder(context.Background(), *leg.UpstreamOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if vendorOrder.AmountIn.Units != 149_625000 {
		t.Fatalf("vendor order was created for %v, want the 149.625 actually forwarded", vendorOrder.AmountIn)
	}
	attempts := f.attempts()
	if len(attempts) != 1 || attempts[0].Amount.Units != 149_625000 {
		t.Fatalf("expected one forward transaction of 149.625, got %+v", attempts)
	}
	if leg.VendorFeeAmount == nil || *leg.VendorFeeAmount != 149_625000-vendorOrder.AmountOutExpected.Units {
		t.Fatalf("expected the vendor's fee recorded as forward minus its payout, got %v", leg.VendorFeeAmount)
	}

	// Land the forward and let the vendor finish.
	f.finality.MarkFinal(*attempts[0].TxHash)
	f.tick(o)
	payout := money.Amount{Asset: money.USDT_TRC20, Units: vendorOrder.AmountOutExpected.Units}
	if err := f.vendor.SetOrderStatus(*leg.UpstreamOrderID, "complete", &payout); err != nil {
		t.Fatal(err)
	}
	f.tick(o)
	if got := f.leg().Status; got != relay.StatusSettled {
		t.Fatalf("expected SETTLED, got %s", got)
	}
	if tx := f.leg().PayoutTxID; tx == nil || *tx != "mock-payout-"+*leg.UpstreamOrderID {
		t.Fatalf("expected the vendor's payout transaction recorded on the leg, got %v", tx)
	}

	ledger := f.ledger
	for account, want := range map[string]int64{
		fmt.Sprintf("asset:relay:leg:%d", f.orderID):            0,
		fmt.Sprintf("asset:relay:leg:forwarding:%d", f.orderID): 0,
		"liability:customer:" + f.customerID + ":USDT_BEP20":    0,
	} {
		if got := ledger.AccountBalance(account); got != want {
			t.Errorf("%s: balance %d, want %d", account, got, want)
		}
	}
}

// A deposit too small to forward -- below both the customer's own quote
// and the admin minimum -- is refunded in full, to the sender, from the
// deposit address.
func TestAmounts_TooSmallDepositIsRefundedInFull(t *testing.T) {
	f := newBEP20FixtureWithDeposit(t, 62, "0.500000", bps(25))
	f.pricing = pricing.NewStore(f.pool)
	if err := f.pricing.Put(context.Background(), pricing.Config{
		ProfitBPS: 25, MinAmountIn: 5_000000, MaxAmountIn: 10_000_000000,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	o := f.newOrchestrator(0)
	f.tick(o)

	leg := f.leg()
	if leg.Status != relay.StatusRefundPending {
		t.Fatalf("expected REFUND_PENDING for a 0.5 USDT deposit, got %s", leg.Status)
	}
	if leg.UpstreamOrderID != nil {
		t.Fatalf("no vendor order should exist for a deposit we won't forward, got %s", *leg.UpstreamOrderID)
	}
	var refund *transfers.Attempt
	for _, a := range f.attempts() {
		if a.Purpose == transfers.Refund {
			a := a
			refund = &a
		}
	}
	if refund == nil || refund.Amount.Units != 500000 || refund.ToAddress != "0xAe2166bD7901eA67c1E2BC4179418fc228108f07" {
		t.Fatalf("expected a 0.5 USDT refund to the original sender, got %+v", refund)
	}
}
