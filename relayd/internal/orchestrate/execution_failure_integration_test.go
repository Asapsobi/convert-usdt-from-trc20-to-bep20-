//go:build integration

// Real, dedicated coverage for checkForwardExecutionAndFinish's and
// checkRefundExecutionAndFinish's own execution-FAILURE branch
// (forward_trc20.go, refund.go) -- the CRITICAL-alert-and-rebuild path
// for when a broadcast TRON transaction is accepted into a block but
// its own execution fails (e.g. OUT_OF_ENERGY), moving zero funds. Every
// OTHER test in this package that touches this machinery only ever
// exercises the SUCCESS path (fakeFinality's own default, per its own
// doc comment); this file is the one place fakeFinality.MarkFailed is
// actually called, for both the forward and refund directions --
// mirroring this package's own established pattern of covering both
// directions together rather than just the one a specific incident
// happened to hit live.
package orchestrate_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"

	"relayd/internal/alert"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
)

// fakeChainByOwner is fakeChain's own sibling (orchestrate_integration_test.go)
// for tests that need to recover which specific txid a leg's own
// broadcast was assigned, not just a count -- every broadcast is
// recorded keyed by the unsigned tx's own baked-in TRON OwnerAddress
// (txbuild.BuildTransfer's own fromAddress, decoded here), so a test
// can find its own leg's entry regardless of how many OTHER legs'
// broadcasts a shared-database RunTick call also happens to process
// alongside it in the same tick -- see capturingTRONChain's own doc
// comment (signing_trc20_e2e_integration_test.go) for why that is a
// real possibility every integration test in this package must account
// for, not a hypothetical one.
type fakeChainByOwner struct {
	mu         sync.Mutex
	broadcasts int
	byOwner    map[string]string // TRON OwnerAddress -> most recent txid
}

func newFakeChainByOwner() *fakeChainByOwner {
	return &fakeChainByOwner{byOwner: make(map[string]string)}
}

func (f *fakeChainByOwner) CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error) {
	return txbuild.BlockReference{
		BlockNumber: 1,
		BlockHash:   [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Timestamp:   time.Now().UTC(),
		Expiration:  time.Now().UTC().Add(2 * time.Minute),
	}, nil
}

func (f *fakeChainByOwner) BroadcastSigned(ctx context.Context, unsignedTx []byte, signature [65]byte) (string, error) {
	digest := txbuild.Digest(unsignedTx)
	txID := fmt.Sprintf("%x", digest[:8])

	var raw core.TransactionRaw
	if err := proto.Unmarshal(unsignedTx, &raw); err != nil {
		return "", fmt.Errorf("fakeChainByOwner: unmarshaling captured unsigned tx: %w", err)
	}
	if len(raw.GetContract()) != 1 {
		return "", fmt.Errorf("fakeChainByOwner: expected exactly 1 contract, got %d", len(raw.GetContract()))
	}
	var trigger core.TriggerSmartContract
	if err := raw.GetContract()[0].GetParameter().UnmarshalTo(&trigger); err != nil {
		return "", fmt.Errorf("fakeChainByOwner: unmarshaling TriggerSmartContract: %w", err)
	}
	ownerAddr := address.BytesToAddress(trigger.GetOwnerAddress()).String()

	f.mu.Lock()
	f.broadcasts++
	f.byOwner[ownerAddr] = txID
	f.mu.Unlock()
	return txID, nil
}

// TxIDFor returns the most recent txid this fake assigned to a
// broadcast whose own baked-in OwnerAddress is ownerAddr, and whether
// one was found.
func (f *fakeChainByOwner) TxIDFor(ownerAddr string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	txID, ok := f.byOwner[ownerAddr]
	return txID, ok
}

func (f *fakeChainByOwner) Broadcasts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.broadcasts
}

// TestForwardTRC20_RebuildsAndRetriesAfterAFailedExecution proves
// checkForwardExecutionAndFinish's own execution-FAILURE branch: a
// broadcast forward transfer that the network accepted into a block but
// whose own execution failed (e.g. OUT_OF_ENERGY) must NOT be marked
// FORWARDED, must fire exactly one CRITICAL relay_leg_forward_execution_failed
// alert, and must clear the leg's own cached broadcast so the NEXT tick
// rebuilds a fresh unsigned transaction (a new block reference, a new
// energy reservation) and retries -- never silently replaying the same
// failed txid forever.
func TestForwardTRC20_RebuildsAndRetriesAfterAFailedExecution(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-exec-fail-1")
	screened := ledger.AdvanceToScreened(order)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	depositDerivationIndex := uint32(20)
	leg, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-exec-fail-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:         "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4",
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	})
	if err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	mockProvider := upstream.NewMockProvider("mock-exec-fail", 1)
	mockProvider.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := newFakeChainByOwner()
	finality := newFakeFinality()
	evmChain := &fakeEVMChain{}
	evmFinality := newFakeEVMFinality()
	alerter := &fakeAlerter{}
	tronDepositWatcher := newFakeTronDepositWatcher()
	tronDepositWatcher.set(order.ID, "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4", depositDerivationIndex)

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, alerter, nil, tronDepositWatcher, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		EnergyPerTransferUnits: 65000,
	})

	ctx := context.Background()

	// Tick 1: broadcasts the forward transfer -- still FORWARDING,
	// awaiting its own execution check (checkForwardExecutionAndFinish
	// only ever runs from the NEXT call).
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	afterTick1, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1.Status != relay.StatusForwarding {
		t.Fatalf("after tick 1: expected still FORWARDING, got %s", afterTick1.Status)
	}
	firstTxID, ok := chain.TxIDFor(leg.DepositAddress)
	if !ok {
		t.Fatal("expected a captured broadcast for this leg's own deposit address")
	}

	// Tell the finality fake this specific broadcast's own execution
	// failed -- the real OUT_OF_ENERGY incident this machinery exists
	// for.
	finality.MarkFailed(firstTxID, "OUT_OF_ENERGY")

	// Tick 2: the early-return check at advanceForwardingOneTRC20's own
	// top short-circuits to checkForwardExecutionAndFinish, which must
	// now see the confirmed failure.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.Status != relay.StatusForwarding {
		t.Fatalf("after tick 2: expected still FORWARDING (execution failed, not FORWARDED), got %s", afterTick2.Status)
	}
	if afterTick2.ForwardTxID != nil {
		t.Fatalf("expected no forward_tx_id recorded after a failed execution, got %v", *afterTick2.ForwardTxID)
	}

	fired := alerter.Fired()
	if len(fired) != 1 {
		t.Fatalf("expected exactly 1 alert fired, got %d", len(fired))
	}
	if fired[0].Severity != alert.SeverityCritical {
		t.Errorf("expected CRITICAL severity, got %s", fired[0].Severity)
	}
	if fired[0].Reason != "relay_leg_forward_execution_failed" {
		t.Errorf("expected reason relay_leg_forward_execution_failed, got %s", fired[0].Reason)
	}
	if fired[0].ExternalID != externalID {
		t.Errorf("expected alert for %s, got %s", externalID, fired[0].ExternalID)
	}

	// Tick 3: the cache was cleared, so this must rebuild a FRESH
	// unsigned transaction (a new block reference -- txbuild bakes in a
	// timestamp, so a rebuilt tx never collides with the failed one's
	// own txid) and broadcast again, rather than silently replaying the
	// same failed txid forever.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	if chain.Broadcasts() != 2 {
		t.Fatalf("expected exactly 2 broadcasts (the failed attempt + the rebuilt retry), got %d", chain.Broadcasts())
	}
	secondTxID, ok := chain.TxIDFor(leg.DepositAddress)
	if !ok {
		t.Fatal("expected a second captured broadcast for this leg's own deposit address")
	}
	if secondTxID == firstTxID {
		t.Fatal("expected the retry to rebuild with a fresh txid, got the same failed one replayed")
	}
	afterTick3, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick3.Status != relay.StatusForwarding {
		t.Fatalf("after tick 3: expected still FORWARDING (the retry broadcast, awaiting ITS OWN execution check), got %s", afterTick3.Status)
	}

	// Tick 4: fakeFinality's own CheckExecution defaults to success for
	// any txid it wasn't told to fail -- the retry's own txid was never
	// marked failed, so this confirms it and marks the leg FORWARDED at
	// last.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 4: %v", err)
	}
	afterTick4, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick4.Status != relay.StatusForwarded {
		t.Fatalf("after tick 4: expected FORWARDED, got %s", afterTick4.Status)
	}
	if afterTick4.ForwardTxID == nil || *afterTick4.ForwardTxID != secondTxID {
		t.Fatalf("expected forward_tx_id to be the retry's own txid %s, got %v", secondTxID, afterTick4.ForwardTxID)
	}

	// Still exactly 1 alert -- the retry succeeded, no second failure.
	if len(alerter.Fired()) != 1 {
		t.Errorf("expected still exactly 1 alert after the retry succeeded, got %d", len(alerter.Fired()))
	}

	// Drive this leg to a terminal status before returning -- this
	// package's own integration tests share ONE Postgres database
	// across the whole test binary run (Makefile's own -p 1
	// requirement), and FORWARDED is one of staleCheckedStatuses
	// (reconcile.go) -- a leg left sitting there would still be picked
	// up by a LATER test's own checkStaleLegs scan (e.g.
	// TestStaleLegAlarm_FiresOnceThenNeverAgain, which deliberately
	// lowers StaleLegAlertAfter to a near-zero threshold), firing an
	// extra, unexpected alert through THAT test's own alerter. Every
	// sibling happy-path test in this package already takes care to
	// avoid this; this test needs the same discipline since it also
	// ends in FORWARDED.
	//
	// Deliberately UNRECOVERABLE (via a real upstream FAILED report,
	// TestUnrecoverable_PostForwardedUpstreamFailure's own pattern), NOT
	// SETTLED: settling would post this order's own 0.3 USDT_TRC20
	// commission into revenue:relay_commission:USDT_TRC20 -- a shared,
	// currency-keyed account (not scoped per-order) that
	// TestFullHappyPath_TRC20ToBEP20 asserts holds EXACTLY -300000.
	// CreateRelayOrder's own fee is fixed at 0.3 TRC20 for every caller
	// (no per-test override), so any other TRC20-direction settlement in
	// the same suite would double that total -- confirmed live: this
	// test settling here made TestFullHappyPath_TRC20ToBEP20 fail with
	// "got -600000". UNRECOVERABLE touches no ledger revenue account at
	// all.
	if err := mockProvider.SetOrderStatus(*afterTick4.UpstreamOrderID, upstream.StatusFailed, nil); err != nil {
		t.Fatalf("SetOrderStatus: %v", err)
	}
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 5 (unrecoverable): %v", err)
	}
	afterFinal, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFinal.Status != relay.StatusUnrecoverable {
		t.Fatalf("expected UNRECOVERABLE before returning, got %s", afterFinal.Status)
	}
	// Now exactly 2 alerts: the earlier execution-failure alert, plus
	// this UNRECOVERABLE one.
	if len(alerter.Fired()) != 2 {
		t.Errorf("expected exactly 2 alerts after the post-FORWARDED upstream failure, got %d", len(alerter.Fired()))
	}
}

// TestRefundTRC20_RebuildsAndRetriesAfterAFailedExecution is
// TestForwardTRC20_RebuildsAndRetriesAfterAFailedExecution's own
// refund-direction sibling, proving checkRefundExecutionAndFinish's
// identical execution-FAILURE branch (refund.go). Reuses
// TestRefund_StuckForwardingLegGetsRefunded's own fixture shape
// (orchestrate_integration_test.go) to reach REFUND_PENDING with a real
// broadcast in flight: deliberately never forces a valid upstream
// deposit address, so the FORWARD attempt can never build a transaction
// at all, leaving the leg stuck FORWARDING until refundStuckForwardingLegs
// commits the refund.
func TestRefundTRC20_RebuildsAndRetriesAfterAFailedExecution(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-refund-exec-fail-1")
	senderAddress := "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj"
	screened := ledger.AdvanceToScreenedWithSender(order, senderAddress)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	depositDerivationIndex := uint32(21)
	if _, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-refund-exec-fail-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:         "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4",
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	mockProvider := upstream.NewMockProvider("mock-refund-exec-fail", 1)
	// Deliberately no ForceDepositAddress -- see this test's own doc
	// comment and TestRefund_StuckForwardingLegGetsRefunded's identical
	// reasoning: the mock's own default placeholder is not a real TRON
	// address, so the forward attempt can never even build a
	// transaction, leaving this leg permanently stuck FORWARDING.
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := newFakeChainByOwner()
	finality := newFakeFinality()
	evmChain := &fakeEVMChain{}
	evmFinality := newFakeEVMFinality()
	alerter := &fakeAlerter{}

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, alerter, nil, nil, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		EnergyPerTransferUnits: 65000,
		ForwardingTimeout:      time.Millisecond,
	})

	ctx := context.Background()

	// Tick 1: the forward attempt fails to build (invalid upstream
	// deposit address), refundStuckForwardingLegs notices the leg is
	// already past its 1ms ForwardingTimeout and commits the refund on
	// C1, and advanceRefundPendingLegs -- later the same tick --
	// broadcasts the actual refund transfer, still REFUND_PENDING
	// (awaiting its own execution check).
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	afterTick1, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1.Status != relay.StatusRefundPending {
		t.Fatalf("after tick 1: expected REFUND_PENDING (broadcast, awaiting its own execution check), got %s", afterTick1.Status)
	}
	// The refund transfer's own OwnerAddress is the slot -- see
	// advanceRefundPendingOneTRC20's own txbuild.BuildTransfer call
	// (refund.go): it signs FROM o.Cfg.SlotAddress, TO the customer
	// being refunded, never from the leg's own (here, invalid) deposit
	// address.
	firstTxID, ok := chain.TxIDFor("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	if !ok {
		t.Fatal("expected a captured refund broadcast for the slot's own address")
	}

	finality.MarkFailed(firstTxID, "OUT_OF_ENERGY")

	// Tick 2: checkRefundExecutionAndFinish must now see the confirmed
	// failure -- still REFUND_PENDING, not REFUNDED.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.Status != relay.StatusRefundPending {
		t.Fatalf("after tick 2: expected still REFUND_PENDING (execution failed, not REFUNDED), got %s", afterTick2.Status)
	}
	if afterTick2.RefundTxID != nil {
		t.Fatalf("expected no refund_tx_id recorded after a failed execution, got %v", *afterTick2.RefundTxID)
	}

	fired := alerter.Fired()
	if len(fired) != 1 {
		t.Fatalf("expected exactly 1 alert fired, got %d", len(fired))
	}
	if fired[0].Severity != alert.SeverityCritical {
		t.Errorf("expected CRITICAL severity, got %s", fired[0].Severity)
	}
	if fired[0].Reason != "relay_leg_refund_execution_failed" {
		t.Errorf("expected reason relay_leg_refund_execution_failed, got %s", fired[0].Reason)
	}
	if fired[0].ExternalID != externalID {
		t.Errorf("expected alert for %s, got %s", externalID, fired[0].ExternalID)
	}

	// Tick 3: the cache was cleared, so this rebuilds a fresh refund
	// transaction and broadcasts again.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	if chain.Broadcasts() != 2 {
		t.Fatalf("expected exactly 2 broadcasts (the failed attempt + the rebuilt retry), got %d", chain.Broadcasts())
	}
	secondTxID, ok := chain.TxIDFor("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	if !ok {
		t.Fatal("expected a second captured refund broadcast for the slot's own address")
	}
	if secondTxID == firstTxID {
		t.Fatal("expected the retry to rebuild with a fresh txid, got the same failed one replayed")
	}

	// Tick 4: fakeFinality defaults to success for the retry's own,
	// never-marked-failed txid -- confirms it and marks REFUNDED.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 4: %v", err)
	}
	afterTick4, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick4.Status != relay.StatusRefunded {
		t.Fatalf("after tick 4: expected REFUNDED, got %s", afterTick4.Status)
	}
	if afterTick4.RefundTxID == nil || *afterTick4.RefundTxID != secondTxID {
		t.Fatalf("expected refund_tx_id to be the retry's own txid %s, got %v", secondTxID, afterTick4.RefundTxID)
	}

	if len(alerter.Fired()) != 1 {
		t.Errorf("expected still exactly 1 alert after the retry succeeded, got %d", len(alerter.Fired()))
	}
}
