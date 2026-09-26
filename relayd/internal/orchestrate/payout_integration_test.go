//go:build integration

// A leg is marked complete only once the vendor's payout is final on the
// customer's chain and pays the customer's own address. Requires the same
// real Postgres + real ledgerd as orchestrate_integration_test.go.
package orchestrate_test

import (
	"context"
	"math/big"
	"sync"
	"testing"

	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/tronbroadcast"
	"relayd/internal/txbuild"
)

// scriptedTRONPayouts is a TRON chain whose view of a payout each test sets.
type scriptedTRONPayouts struct {
	mu        sync.Mutex
	transfers []tronbroadcast.TokenTransfer
	final     bool
	asked     []string
}

func (s *scriptedTRONPayouts) TokenTransfers(ctx context.Context, txID string) ([]tronbroadcast.TokenTransfer, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, txID)
	return s.transfers, s.final, nil
}

func (s *scriptedTRONPayouts) set(final bool, transfers ...tronbroadcast.TokenTransfer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.final, s.transfers = final, transfers
}

func TestPayout_LegCompletesOnlyOnceThePayoutIsOnChain(t *testing.T) {
	f := newBEP20FixtureWithDeposit(t, 85, "100.000000", bps(25))
	o := f.newOrchestrator(0)
	payouts := &scriptedTRONPayouts{}
	o.TRONPayouts = payouts

	f.tick(o)
	attempts := f.attempts()
	if len(attempts) != 1 {
		t.Fatalf("expected the forward sent, got %+v", attempts)
	}
	f.finality.MarkFinal(*attempts[0].TxHash)
	f.tick(o)
	leg := f.leg()
	reported := money.Amount{Asset: money.USDT_TRC20, Units: 98_000000}
	if err := f.vendor.SetOrderStatus(*leg.UpstreamOrderID, "complete", &reported); err != nil {
		t.Fatal(err)
	}
	customer := leg.DestinationAddress

	// Not final yet: the leg waits.
	payouts.set(false)
	f.tick(o)
	if got := f.leg().Status; got != relay.StatusForwarded {
		t.Fatalf("a payout not final yet must not complete the leg, got %s", got)
	}

	// Final, but it paid someone else.
	payouts.set(true, tronbroadcast.TokenTransfer{Token: txbuild.USDTContractAddress, To: "TT33eypdiDVYErJzg5PNK2d2zMtkS8gSt3", Amount: big.NewInt(98_000000)})
	f.tick(o)
	if got := f.leg().Status; got != relay.StatusForwarded {
		t.Fatalf("a payout to another address must not complete the leg, got %s", got)
	}
	if n := f.alertsWithReason("relay_leg_payout_unverified"); n != 1 {
		t.Fatalf("expected one payout_unverified alert, got %d", n)
	}

	// Final and paid to the customer: complete, recording what the chain paid.
	payouts.set(true, tronbroadcast.TokenTransfer{Token: txbuild.USDTContractAddress, To: customer, Amount: big.NewInt(97_900000)})
	f.tick(o)
	leg = f.leg()
	if leg.Status != relay.StatusSettled {
		t.Fatalf("expected SETTLED once the payout is on-chain, got %s", leg.Status)
	}
	if leg.AmountOutActual == nil || leg.AmountOutActual.Units != 97_900000 {
		t.Fatalf("expected the on-chain amount (97.9) recorded as paid, got %v", leg.AmountOutActual)
	}
	if len(payouts.asked) == 0 || payouts.asked[0] != "mock-payout-"+*leg.UpstreamOrderID {
		t.Fatalf("expected the vendor's own payout transaction checked, asked %v", payouts.asked)
	}
}
