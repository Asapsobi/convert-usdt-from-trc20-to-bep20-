//go:build integration

// Automatic resource provisioning before a deposit wallet sends: BNB gas
// topped up from the treasury on BSC; on TRON, activation (a TRX top-up)
// and exactly the energy the transfer needs, rented for the sender.
// Requires the same real Postgres + real ledgerd as the other
// integration tests.
package orchestrate_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/transfers"
	"relayd/internal/tronbroadcast"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
)

func openOf(t *testing.T, store *transfers.Store, purposes ...transfers.Purpose) []transfers.Attempt {
	t.Helper()
	list, err := store.ListOpen(context.Background(), purposes...)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// Scenario B on BSC: the deposit wallet can't pay its gas, so the treasury
// sends it BNB first; the forward goes out once that lands.
func TestResources_BSCGasIsToppedUpFromTheTreasury(t *testing.T) {
	f := newBEP20Fixture(t, 81)
	store := transfers.NewStore(f.pool)
	treasury := signing.FakeSlotEVMAddress(1)
	f.chain.setNative(f.deposit, big.NewInt(0))
	o := f.newOrchestrator(0)

	f.tick(o)
	if got := f.attempts(); len(got) != 0 {
		t.Fatalf("the forward was built before its wallet could pay gas: %+v", got)
	}
	var topUp *transfers.Attempt
	for _, a := range openOf(t, store, transfers.GasTopUp) {
		if a.ToAddress == f.deposit {
			a := a
			topUp = &a
		}
	}
	if topUp == nil {
		t.Fatal("expected a gas top-up to the deposit wallet")
	}
	if topUp.FromAddress != treasury || topUp.Amount.Units < 500_000_000_000_000 || !topUp.Status.MayLand() {
		t.Fatalf("expected a sent top-up of at least 0.0005 BNB from the treasury, got %+v", *topUp)
	}

	// The top-up lands.
	f.finality.MarkFinal(*topUp.TxHash)
	f.chain.setNative(f.deposit, big.NewInt(1_000_000_000_000_000))
	f.tick(o)

	if got := f.attempts(); len(got) != 1 || got[0].Purpose != transfers.Forward || !got[0].Status.MayLand() {
		t.Fatalf("expected the forward sent once the wallet has gas, got %+v", got)
	}
	confirmed, err := store.Get(context.Background(), topUp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != transfers.StatusConfirmed {
		t.Fatalf("expected the top-up tracked to CONFIRMED, got %s", confirmed.Status)
	}
}

// scriptedTRONChain is a TRON node whose per-address resources each test
// sets.
type scriptedTRONChain struct {
	mu        sync.Mutex
	resources map[string]tronbroadcast.Resources
	estimate  int64
	sent      []string
}

var plentyTRON = tronbroadcast.Resources{Exists: true, Energy: 1_000_000_000, Bandwidth: 1_000_000, BalanceSun: 1_000_000_000}

func (c *scriptedTRONChain) set(addr string, r tronbroadcast.Resources) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resources[addr] = r
}

func (c *scriptedTRONChain) CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error) {
	now := time.Now().UTC()
	return txbuild.BlockReference{BlockNumber: 1, BlockHash: [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Timestamp: now, Expiration: now.Add(2 * time.Minute)}, nil
}

func (c *scriptedTRONChain) BroadcastSigned(ctx context.Context, unsignedTx []byte, signature [65]byte) (string, error) {
	digest := txbuild.Digest(unsignedTx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, hex.EncodeToString(digest[:]))
	return hex.EncodeToString(digest[:]), nil
}

func (c *scriptedTRONChain) TokenBalance(ctx context.Context, holder string) (*big.Int, error) {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil), nil
}

func (c *scriptedTRONChain) AccountResources(ctx context.Context, holder string) (tronbroadcast.Resources, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.resources[holder]; ok {
		return r, nil
	}
	return plentyTRON, nil
}

func (c *scriptedTRONChain) EstimateTransferEnergy(ctx context.Context, from, to string, raw *big.Int) (int64, error) {
	return c.estimate, nil
}

// A brand-new TRON deposit wallet is activated with TRX from the treasury,
// then exactly the energy its transfer needs is rented -- for the wallet
// that sends, once -- and the forward goes out when the energy arrives.
func TestResources_TRONActivatesThenRentsExactlyTheShortfall(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	relayStore := relay.NewStore(pool)
	store := transfers.NewStore(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE relay_legs SET status = 'FAILED' WHERE status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'FORWARDED', 'REFUND_PENDING')`); err != nil {
		t.Fatal(err)
	}

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-tron-resources")
	if screened := ledger.AdvanceToScreened(order); screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}
	index := uint32(91)
	deposit := signing.FakeTronDepositAddress(index)
	if _, err := relayStore.Create(ctx, relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20, CustomerID: "cust-tron-resources",
		DestinationAddress: "0x4192cC99D3CB95573DcAF8dd76921476e0C7bCaF", DepositAddress: deposit, DepositDerivationIndex: &index,
		AmountIn:          money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected: money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		t.Fatal(err)
	}
	vendor := upstream.NewMockProvider(fmt.Sprintf("mock-tron-res-%d", time.Now().UnixNano()), 1)
	vendor.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	watcher := newFakeTronDepositWatcher()
	watcher.set(order.ID, deposit, index)
	chain := &scriptedTRONChain{resources: map[string]tronbroadcast.Resources{}, estimate: 64_285}
	chain.set(deposit, tronbroadcast.Resources{}) // never activated
	energy := newFakeEnergy()
	treasury := signing.FakeSlotTronAddress(1)
	o := orchestrate.New(relayStore, ledgerclient.New(ledger.BaseURL(), ledger.Token()), vendor, energy,
		signing.NewFakeSigningService(), chain, newFakeFinality(), &fakeEVMChain{}, newFakeEVMFinality(),
		&fakeAlerter{}, nil, watcher, orchestrate.Config{SlotID: 1, SlotAddress: treasury, SlotEVMAddress: signing.FakeSlotEVMAddress(1)})
	tick := func() {
		t.Helper()
		if err := o.RunTick(ctx); err != nil {
			t.Fatalf("RunTick: %v", err)
		}
	}
	forwards := func() int {
		list, err := store.ListForLeg(ctx, externalID)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	// 1. Not activated: the treasury sends it TRX.
	tick()
	// (The fake chain confirms instantly, so the top-up has already been
	// tracked to CONFIRMED within the same tick.)
	activations, err := store.ListForLeg(ctx, "trx_topup:"+externalID+":FORWARD:0")
	if err != nil {
		t.Fatal(err)
	}
	if len(activations) != 1 || activations[0].FromAddress != treasury || activations[0].ToAddress != deposit ||
		activations[0].Amount.Units != 2_000_000 || activations[0].Status != transfers.StatusConfirmed {
		t.Fatalf("expected one confirmed 2 TRX activation from the treasury to the deposit wallet, got %+v", activations)
	}
	if forwards() != 0 {
		t.Fatal("the forward was built before the wallet was activated")
	}

	// 2. Activated, but no energy: exactly the shortfall is rented, for the
	// wallet that sends.
	chain.set(deposit, tronbroadcast.Resources{Exists: true, Energy: 5_000, Bandwidth: 600, BalanceSun: 2_000_000})
	tick()
	rentals, err := store.RentalsForJob(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rentals) != 1 || rentals[0].Units != 64_285-5_000 || rentals[0].Address != deposit || rentals[0].Status != "CONFIRMED" {
		t.Fatalf("expected one confirmed rental of the 59285-energy shortfall for the deposit wallet, got %+v", rentals)
	}
	if got := energy.TargetAddressFor(externalID); got != deposit {
		t.Fatalf("energy rented for %s, want the sending wallet %s", got, deposit)
	}

	// 3. The rented energy hasn't shown up yet: wait, don't rent again.
	tick()
	if rentals, _ := store.RentalsForJob(ctx, externalID); len(rentals) != 1 {
		t.Fatalf("rented again while the first rental was still arriving: %d rentals", len(rentals))
	}

	// 4. It arrives: the forward goes out.
	chain.set(deposit, tronbroadcast.Resources{Exists: true, Energy: 70_000, Bandwidth: 600, BalanceSun: 2_000_000})
	tick()
	if forwards() != 1 {
		t.Fatalf("expected the forward sent once energy arrived, got %d attempts", forwards())
	}
}
