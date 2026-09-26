//go:build integration

// Coverage for transfer.go's durability guarantees: a restart at any
// point resumes the exact transaction that may already be on its way,
// a new transaction is only built once the previous one provably can't
// land, and nothing is built the deposit address can't pay. Requires the
// same real Postgres + real ledgerd as orchestrate_integration_test.go.
package orchestrate_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	gethtypes "github.com/ethereum/go-ethereum/core/types"

	"relayd/internal/alert"
	"relayd/internal/db"
	"relayd/internal/evmbroadcast"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/pricing"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/testledger"
	"relayd/internal/transfers"
	"relayd/internal/upstream"
)

const vendorBSCDepositAddress = "0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf"

// scriptedEVMChain is a BSC node whose balances, nonces, mining and
// send errors each test sets explicitly.
type scriptedEVMChain struct {
	mu             sync.Mutex
	tokenBalance   *big.Int
	nativeBalance  *big.Int
	pendingNonce   uint64
	confirmedNonce uint64
	mined          map[string]bool
	natives        map[string]*big.Int // per-address BNB; nativeBalance for any other address
	sendErr        error
	sent           []string // hash of every Broadcast call, in order
}

func newScriptedEVMChain() *scriptedEVMChain {
	plenty := new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	return &scriptedEVMChain{tokenBalance: plenty, nativeBalance: plenty, mined: make(map[string]bool), natives: make(map[string]*big.Int)}
}

func (c *scriptedEVMChain) CurrentNonce(ctx context.Context, address string) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingNonce, nil
}

func (c *scriptedEVMChain) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return big.NewInt(100_000_000), nil // 0.1 gwei
}

func (c *scriptedEVMChain) Broadcast(ctx context.Context, signed *gethtypes.Transaction) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hash := signed.Hash().Hex()
	c.sent = append(c.sent, hash)
	if c.sendErr != nil {
		return "", c.sendErr
	}
	if signed.Nonce() >= c.pendingNonce {
		c.pendingNonce = signed.Nonce() + 1
	}
	return hash, nil
}

func (c *scriptedEVMChain) ConfirmedNonce(ctx context.Context, address string) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.confirmedNonce, nil
}

func (c *scriptedEVMChain) TransactionMined(ctx context.Context, txHash string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mined[txHash], nil
}

func (c *scriptedEVMChain) TokenBalance(ctx context.Context, holder string) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(c.tokenBalance), nil
}

func (c *scriptedEVMChain) NativeBalance(ctx context.Context, holder string) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.natives[holder]; ok {
		return new(big.Int).Set(b), nil
	}
	return new(big.Int).Set(c.nativeBalance), nil
}

func (c *scriptedEVMChain) setNative(holder string, wei *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.natives[holder] = wei
}

func (c *scriptedEVMChain) Sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

// scriptedEVMFinality reports a transaction final or reverted only once
// a test says so; everything else is "not final yet".
type scriptedEVMFinality struct {
	mu       sync.Mutex
	final    map[string]bool
	reverted map[string]bool
}

func newScriptedEVMFinality() *scriptedEVMFinality {
	return &scriptedEVMFinality{final: make(map[string]bool), reverted: make(map[string]bool)}
}

func (f *scriptedEVMFinality) MarkFinal(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.final[hash] = true
}

func (f *scriptedEVMFinality) MarkReverted(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reverted[hash] = true
}

func (f *scriptedEVMFinality) IsFinal(ctx context.Context, txHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reverted[txHash] {
		return false, fmt.Errorf("transaction %s reverted: %w", txHash, evmbroadcast.ErrReverted)
	}
	return f.final[txHash], nil
}

// bep20Fixture is one screened BEP20_TO_TRC20 leg plus everything an
// Orchestrator needs to drive it. newOrchestrator builds a fresh
// Orchestrator over the same database, chain and signer -- the stand-in
// for a relayd restart, which keeps nothing from the previous process.
type bep20Fixture struct {
	t          *testing.T
	pool       *db.Pool
	store      *relay.Store
	client     *ledgerclient.Client
	externalID string
	orderID    int64
	deposit    string
	vendor     *upstream.MockProvider
	signer     *signing.FakeSigningService
	chain      *scriptedEVMChain
	finality   *scriptedEVMFinality
	watcher    *fakeBEP20DepositWatcher
	alerts     *fakeAlerter
	pricing    *pricing.Store // optional admin pricing for the orchestrator
	ledger     *testledger.Ledger
	customerID string
}

func zeroIfSet(p *int64) *int64 {
	if p == nil {
		return nil
	}
	zero := int64(0)
	return &zero
}

func newBEP20Fixture(t *testing.T, index uint32) *bep20Fixture {
	t.Helper()
	return newBEP20FixtureWithDeposit(t, index, "100.000000", nil)
}

// newBEP20FixtureWithDeposit is newBEP20Fixture for a customer quoted 100
// USDT who actually sent deposit. A non-nil profitBPS snapshots that
// pricing on the leg, as legs created through the driver do.
func newBEP20FixtureWithDeposit(t *testing.T, index uint32, deposit string, profitBPS *int64) *bep20Fixture {
	t.Helper()
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)

	// Tests share one relayd_test database and run one at a time. Park
	// every leg an earlier test left in flight, so this test's
	// Orchestrator drives only its own leg and its counts are its own.
	if _, err := pool.Exec(context.Background(), `
		UPDATE relay_legs SET status = 'FAILED'
		WHERE status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'FORWARDED', 'REFUND_PENDING')
	`); err != nil {
		t.Fatalf("parking leftover legs: %v", err)
	}

	externalID := uniqueExternalID(t)
	customerID := "cust-transfer-" + fmt.Sprint(index)
	order := ledger.CreateRelayOrderBEP20ToTRC20(externalID, customerID)
	if screened := ledger.AdvanceToScreenedBEP20ToTRC20WithDeposit(order, "0xAe2166bD7901eA67c1E2BC4179418fc228108f07", deposit); screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	depositAddress := signing.FakeBSCDepositAddress(index)
	if _, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.BEP20ToTRC20,
		CustomerID: customerID, DestinationAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		DepositAddress: depositAddress, DepositDerivationIndex: &index,
		AmountIn:  money.Amount{Asset: money.USDT_BEP20, Units: 100_000000},
		ProfitBPS: profitBPS, MinProfit: zeroIfSet(profitBPS),
		AmountOutExpected: money.Amount{Asset: money.USDT_TRC20, Units: 99_700000},
	}); err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	vendor := upstream.NewMockProvider(fmt.Sprintf("mock-transfer-%d-%d", index, time.Now().UnixNano()), 1)
	vendor.ForceDepositAddress(vendorBSCDepositAddress)
	watcher := newFakeBEP20DepositWatcher()
	watcher.set(order.ID, depositAddress, index)

	return &bep20Fixture{
		t: t, pool: pool, store: store, client: ledgerclient.New(ledger.BaseURL(), ledger.Token()),
		externalID: externalID, orderID: order.ID, deposit: depositAddress, vendor: vendor,
		signer: signing.NewFakeSigningService(), chain: newScriptedEVMChain(), finality: newScriptedEVMFinality(),
		watcher: watcher, alerts: &fakeAlerter{}, ledger: ledger, customerID: customerID,
	}
}

func (f *bep20Fixture) newOrchestrator(forwardingTimeout time.Duration) *orchestrate.Orchestrator {
	o := orchestrate.New(f.store, f.client, f.vendor, newFakeEnergy(), f.signer, &fakeChain{}, newFakeFinality(),
		f.chain, f.finality, f.alerts, f.watcher, nil, orchestrate.Config{
			SlotID: 1, SlotEVMAddress: signing.FakeSlotEVMAddress(1), SlotAddress: signing.FakeSlotTronAddress(1),
			EnergyPerTransferUnits: 65000, ForwardingTimeout: forwardingTimeout,
		})
	o.Pricing = f.pricing
	return o
}

func (f *bep20Fixture) tick(o *orchestrate.Orchestrator) {
	f.t.Helper()
	if err := o.RunTick(context.Background()); err != nil {
		f.t.Fatalf("RunTick: %v", err)
	}
}

func (f *bep20Fixture) leg() relay.Leg {
	f.t.Helper()
	leg, err := f.store.GetByExternalID(context.Background(), f.externalID)
	if err != nil {
		f.t.Fatal(err)
	}
	return leg
}

func (f *bep20Fixture) attempts() []transfers.Attempt {
	f.t.Helper()
	list, err := transfers.NewStore(f.pool).ListForLeg(context.Background(), f.externalID)
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

func (f *bep20Fixture) exec(sql string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, f.externalID); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *bep20Fixture) alertsWithReason(reason string) int {
	n := 0
	for _, a := range f.alerts.Fired() {
		if a.Reason == reason && a.ExternalID == f.externalID {
			n++
		}
	}
	return n
}

// A restart right after the forward was sent must resume that exact
// transaction: no second signature, no second transaction, and no
// timeout refund of a deposit that is already on its way to the vendor.
func TestTransfer_RestartMidForwardResumesTheSameTransaction(t *testing.T) {
	f := newBEP20Fixture(t, 41)

	f.tick(f.newOrchestrator(0))
	sent := f.chain.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected the forward to be sent once, got %d sends", len(sent))
	}
	if got := f.leg().Status; got != relay.StatusForwarding {
		t.Fatalf("expected FORWARDING while the forward is unconfirmed, got %s", got)
	}

	// "Restart": a brand-new Orchestrator with nothing in memory, and a
	// forwarding timeout that has long since passed.
	restarted := f.newOrchestrator(time.Nanosecond)
	f.tick(restarted)
	f.tick(restarted)

	if got := f.leg().Status; got != relay.StatusForwarding {
		t.Fatalf("a restart refunded (or otherwise moved) a leg whose forward may still land: now %s", got)
	}
	if n := f.signer.RequestDepositSweepSignatureCallCount(); n != 1 {
		t.Errorf("expected exactly 1 signature over the whole run, got %d -- the restart built a second transaction", n)
	}
	if n := len(f.chain.Sent()); n != 1 {
		t.Errorf("expected no re-send inside the rebroadcast interval, got %d sends", n)
	}

	f.finality.MarkFinal(sent[0])
	f.tick(restarted)

	leg := f.leg()
	if leg.Status != relay.StatusForwarded {
		t.Fatalf("expected FORWARDED once the original transaction is final, got %s", leg.Status)
	}
	if leg.ForwardTxID == nil || *leg.ForwardTxID != sent[0] {
		t.Fatalf("expected forward_tx_id %s (the original transaction), got %v", sent[0], leg.ForwardTxID)
	}
	if got := f.attempts(); len(got) != 1 || got[0].Status != transfers.StatusConfirmed {
		t.Fatalf("expected exactly one CONFIRMED forward attempt, got %+v", got)
	}
}

// A transaction that was signed and recorded but never reached a node
// (a crash, or a node error, between signing and sending) is re-sent
// byte for byte -- never re-signed into a different transaction.
func TestTransfer_SignedButUnsentIsResentNotRebuilt(t *testing.T) {
	f := newBEP20Fixture(t, 42)
	f.chain.sendErr = errors.New("connection reset by peer")

	f.tick(f.newOrchestrator(0))
	attempts := f.attempts()
	if len(attempts) != 1 || attempts[0].Status != transfers.StatusSigned {
		t.Fatalf("expected one SIGNED (sent but not accepted) attempt, got %+v", attempts)
	}

	f.chain.sendErr = nil
	f.exec(`UPDATE transfer_attempts SET last_broadcast_at = now() - interval '1 minute' WHERE external_id = $1`)
	f.tick(f.newOrchestrator(0))

	sent := f.chain.Sent()
	if len(sent) != 2 || sent[0] != sent[1] {
		t.Fatalf("expected the same transaction sent twice, got %v", sent)
	}
	if n := f.signer.RequestDepositSweepSignatureCallCount(); n != 1 {
		t.Errorf("expected 1 signature, got %d", n)
	}
	if got := f.attempts(); len(got) != 1 || got[0].Status != transfers.StatusBroadcast {
		t.Fatalf("expected the one attempt to be BROADCAST, got %+v", got)
	}
}

// A node answering "already known" (it has the transaction from an
// earlier send whose reply was lost) is a success, not a failure.
func TestTransfer_AlreadyKnownCountsAsSent(t *testing.T) {
	f := newBEP20Fixture(t, 43)
	f.chain.sendErr = errors.New("already known")

	f.tick(f.newOrchestrator(0))
	if got := f.attempts(); len(got) != 1 || got[0].Status != transfers.StatusBroadcast {
		t.Fatalf("expected BROADCAST after an already-known reply, got %+v", got)
	}
}

// A deposit address that doesn't hold the amount gets no transaction at
// all -- no signature, no gas spent on a transfer that can only revert --
// and one alert, not one per tick.
func TestTransfer_InsufficientBalanceBuildsNothing(t *testing.T) {
	f := newBEP20Fixture(t, 44)
	f.chain.tokenBalance = big.NewInt(1) // 1 wei of USDT

	o := f.newOrchestrator(0)
	f.tick(o)
	f.tick(o)

	if got := f.attempts(); len(got) != 0 {
		t.Fatalf("expected no transfer attempt, got %+v", got)
	}
	if n := f.signer.RequestDepositSweepSignatureCallCount(); n != 0 {
		t.Errorf("expected no signature request, got %d", n)
	}
	if n := f.alertsWithReason("relay_leg_insufficient_funds"); n != 1 {
		t.Errorf("expected exactly 1 insufficient-funds alert over 2 ticks, got %d", n)
	}
}

// A deposit wallet short of gas is topped up from the treasury (see
// resources_integration_test.go). When the treasury itself can't cover
// it, nothing is built or sent -- and the treasury, not the order, is what
// the alert names.
func TestTransfer_MissingGasWithAnEmptyTreasuryBuildsNothing(t *testing.T) {
	f := newBEP20Fixture(t, 45)
	f.chain.nativeBalance = big.NewInt(0) // every address, the treasury included

	f.tick(f.newOrchestrator(0))
	if got := f.attempts(); len(got) != 0 {
		t.Fatalf("expected no forward attempt, got %+v", got)
	}
	n := 0
	for _, a := range f.alerts.Fired() {
		if a.Reason == "treasury_needs_bnb" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected 1 treasury-needs-BNB alert, got %d", n)
	}
}

// An EVM transaction whose nonce was used by another mined transaction
// can never land. Only after that has held for the grace period is it
// declared dropped -- and only then is a replacement built.
func TestTransfer_DroppedEVMForwardIsRebuiltOnlyAfterTheGracePeriod(t *testing.T) {
	f := newBEP20Fixture(t, 46)
	o := f.newOrchestrator(0)

	f.tick(o)
	first := f.chain.Sent()[0]

	f.chain.mu.Lock()
	f.chain.confirmedNonce = 1 // nonce 0 used, and not by `first`
	f.chain.mu.Unlock()
	f.tick(o)
	if got := f.attempts(); len(got) != 1 || got[0].NonceConsumedSince == nil || !got[0].Status.MayLand() {
		t.Fatalf("expected the attempt still open with the nonce-consumed clock started, got %+v", got)
	}

	f.exec(`UPDATE transfer_attempts SET nonce_consumed_since = now() - interval '10 minutes' WHERE external_id = $1`)
	f.tick(o) // declares it dropped
	f.tick(o) // builds and sends a replacement

	attempts := f.attempts()
	if len(attempts) != 2 || attempts[0].Status != transfers.StatusDropped || !attempts[1].Status.MayLand() {
		t.Fatalf("expected [DROPPED, open replacement], got %+v", attempts)
	}
	if *attempts[1].TxHash == first {
		t.Fatal("the replacement is the same transaction as the dropped one")
	}
	if n := f.alertsWithReason("relay_leg_transfer_dropped"); n != 1 {
		t.Errorf("expected 1 dropped alert, got %d", n)
	}
}

// A transfer that keeps reverting on-chain is retried a bounded number
// of times, then left for a human -- never looped forever burning gas.
func TestTransfer_StopsRetryingAfterRepeatedOnChainFailures(t *testing.T) {
	f := newBEP20Fixture(t, 47)
	o := f.newOrchestrator(0)

	for i := 0; i < 12 && len(f.chain.Sent()) < 4; i++ {
		f.tick(o)
		for _, hash := range f.chain.Sent() {
			f.finality.MarkReverted(hash)
		}
	}
	f.tick(o)
	f.tick(o)

	if n := len(f.chain.Sent()); n != 3 {
		t.Fatalf("expected exactly 3 transactions before giving up, got %d", n)
	}
	if n := f.alertsWithReason("relay_leg_transfer_gave_up"); n != 1 {
		t.Errorf("expected 1 gave-up alert, got %d", n)
	}
	if got := f.leg().Status; got != relay.StatusForwarding {
		t.Errorf("expected the leg left FORWARDING for a human, got %s", got)
	}
}

// Once no forward can land, a timed-out leg IS refunded -- from the
// deposit address, to the original sender -- and a restart mid-refund
// resumes that refund rather than sending a second one.
func TestTransfer_TimedOutLegIsRefundedOnceAcrossARestart(t *testing.T) {
	f := newBEP20Fixture(t, 48)
	f.chain.tokenBalance = big.NewInt(0) // deposit not visible yet: nothing is built

	f.tick(f.newOrchestrator(0))
	leg := f.leg()
	if leg.Status != relay.StatusForwarding || leg.UpstreamOrderID == nil {
		t.Fatalf("expected FORWARDING with a vendor order, got %s", leg.Status)
	}

	// The vendor order expires before the forward goes out, so the
	// forward is signed but refused before sending -- it stays unsigned
	// on record (BUILT) and can never land.
	if err := f.vendor.SetOrderStatus(*leg.UpstreamOrderID, upstream.StatusExpired, nil); err != nil {
		t.Fatal(err)
	}
	f.chain.mu.Lock()
	f.chain.tokenBalance = new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	f.chain.mu.Unlock()
	f.exec(`UPDATE relay_legs SET updated_at = now() - interval '2 hours' WHERE external_id = $1`)
	timedOut := f.newOrchestrator(time.Hour)
	f.tick(timedOut) // refuses the forward, abandons it, commits the refund and sends it

	if got := f.leg().Status; got != relay.StatusRefundPending {
		t.Fatalf("expected REFUND_PENDING, got %s", got)
	}
	sent := f.chain.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected the refund sent once, got %d sends", len(sent))
	}

	restarted := f.newOrchestrator(time.Hour)
	f.tick(restarted)
	f.finality.MarkFinal(sent[0])
	f.tick(restarted)

	leg = f.leg()
	if leg.Status != relay.StatusRefunded || leg.RefundTxID == nil || *leg.RefundTxID != sent[0] {
		t.Fatalf("expected REFUNDED by %s, got %s / %v", sent[0], leg.Status, leg.RefundTxID)
	}
	if n := len(f.chain.Sent()); n != 1 {
		t.Errorf("expected exactly one refund transaction, got %d sends", n)
	}
	var refunds []transfers.Attempt
	for _, a := range f.attempts() {
		if a.Purpose == transfers.Refund {
			refunds = append(refunds, a)
		}
	}
	if len(refunds) != 1 || refunds[0].ToAddress != "0xAe2166bD7901eA67c1E2BC4179418fc228108f07" || refunds[0].FromAddress != f.deposit {
		t.Fatalf("expected one refund from the deposit address to the original sender, got %+v", refunds)
	}
}

// silentTRONFinality never sees any transaction on the solidified chain.
type silentTRONFinality struct{}

func (silentTRONFinality) IsFinal(ctx context.Context, txID string) (bool, error) { return false, nil }

func (silentTRONFinality) CheckExecution(ctx context.Context, txID string) (final, success bool, failureReason string, err error) {
	return false, false, "", nil
}

// A TRON transaction that never appears on the solidified chain is only
// replaced once it is past its own expiration (plus a margin) -- from
// then on the network will never accept it, so a replacement can't
// double-spend.
func TestTransfer_ExpiredTRONForwardIsDroppedThenRebuilt(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	if _, err := pool.Exec(context.Background(), `
		UPDATE relay_legs SET status = 'FAILED'
		WHERE status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'FORWARDED', 'REFUND_PENDING')
	`); err != nil {
		t.Fatalf("parking leftover legs: %v", err)
	}

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-transfer-tron")
	if screened := ledger.AdvanceToScreened(order); screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}
	index := uint32(51)
	deposit := signing.FakeTronDepositAddress(index)
	if _, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-transfer-tron", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress: deposit, DepositDerivationIndex: &index,
		AmountIn:          money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected: money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	vendor := upstream.NewMockProvider(fmt.Sprintf("mock-transfer-tron-%d", time.Now().UnixNano()), 1)
	vendor.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	watcher := newFakeTronDepositWatcher()
	watcher.set(order.ID, deposit, index)
	chain := &fakeChain{}
	alerts := &fakeAlerter{}
	o := orchestrate.New(store, ledgerclient.New(ledger.BaseURL(), ledger.Token()), vendor, newFakeEnergy(),
		signing.NewFakeSigningService(), chain, silentTRONFinality{}, &fakeEVMChain{}, newFakeEVMFinality(),
		alerts, nil, watcher, orchestrate.Config{SlotID: 1, EnergyPerTransferUnits: 65000})
	tick := func() {
		t.Helper()
		if err := o.RunTick(context.Background()); err != nil {
			t.Fatalf("RunTick: %v", err)
		}
	}
	attempts := func() []transfers.Attempt {
		t.Helper()
		list, err := transfers.NewStore(pool).ListForLeg(context.Background(), externalID)
		if err != nil {
			t.Fatal(err)
		}
		return list
	}

	tick()
	tick()
	if got := attempts(); len(got) != 1 || !got[0].Status.MayLand() {
		t.Fatalf("expected one sent attempt still waiting on the chain, got %+v", got)
	}

	// Past expiration, but not yet past the margin: still waiting.
	if _, err := pool.Exec(context.Background(),
		`UPDATE transfer_attempts SET tron_expires_at = now() - interval '1 minute' WHERE external_id = $1`, externalID); err != nil {
		t.Fatal(err)
	}
	tick()
	if got := attempts(); len(got) != 1 || !got[0].Status.MayLand() {
		t.Fatalf("expected the attempt still open inside the drop margin, got %+v", got)
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE transfer_attempts SET tron_expires_at = now() - interval '10 minutes' WHERE external_id = $1`, externalID); err != nil {
		t.Fatal(err)
	}
	tick() // dropped
	tick() // rebuilt and sent

	got := attempts()
	if len(got) != 2 || got[0].Status != transfers.StatusDropped || !got[1].Status.MayLand() {
		t.Fatalf("expected [DROPPED, open replacement], got %+v", got)
	}
	if *got[0].TxHash == *got[1].TxHash {
		t.Fatal("the replacement is the same transaction as the dropped one")
	}
	if leg, err := store.GetByExternalID(context.Background(), externalID); err != nil || leg.Status != relay.StatusForwarding {
		t.Fatalf("expected the leg still FORWARDING, got %v / %v", leg.Status, err)
	}
}

var _ alert.Alerter = (*fakeAlerter)(nil)

// A vendor order that expires while the forward is still held up (here,
// waiting on gas) is replaced by a fresh one -- nothing was ever sent to
// it -- and the forward goes to the replacement.
func TestForward_ExpiredVendorOrderIsReplacedBeforeAnythingIsSent(t *testing.T) {
	f := newBEP20Fixture(t, 86)
	f.chain.setNative(f.deposit, big.NewInt(0))
	f.chain.setNative(signing.FakeSlotEVMAddress(1), big.NewInt(0)) // the treasury can't pay the gas yet
	o := f.newOrchestrator(0)
	f.tick(o)
	leg := f.leg()
	if leg.Status != relay.StatusForwarding || len(f.attempts()) != 0 {
		t.Fatalf("expected FORWARDING with nothing sent yet, got %s / %+v", leg.Status, f.attempts())
	}
	first := *leg.UpstreamOrderID
	if err := f.vendor.SetOrderStatus(first, "expired", nil); err != nil {
		t.Fatal(err)
	}

	f.chain.setNative(f.deposit, big.NewInt(1_000_000_000_000_000)) // gas arrives
	f.tick(o)
	leg = f.leg()
	if *leg.UpstreamOrderID == first {
		t.Fatalf("expected the expired vendor order %s replaced", first)
	}
	attempts := f.attempts()
	if len(attempts) != 1 || !attempts[0].Status.MayLand() || attempts[0].ToAddress != *leg.UpstreamDepositAddress {
		t.Fatalf("expected the forward sent to the replacement order, got %+v", attempts)
	}
	if n := f.alertsWithReason("relay_leg_vendor_order_renewed"); n != 1 {
		t.Fatalf("expected one renewal alert, got %d", n)
	}
}
