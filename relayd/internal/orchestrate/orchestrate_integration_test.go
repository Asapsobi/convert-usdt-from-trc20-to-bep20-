//go:build integration

// Requires a real, reachable Postgres 16 instance for BOTH relayd's own
// database and a real ledgerd (built from the sibling ledger module) --
// run via `make test-integration`. This is R3's own ship-gate proof for
// the happy flow: the full state machine, against real ledgerd HTTP
// behavior, not a fake standing in for C1.
package orchestrate_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gethtypes "github.com/ethereum/go-ethereum/core/types"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"relayd/internal/alert"
	"relayd/internal/db"
	"relayd/internal/driver"
	"relayd/internal/energy"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/testledger"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

var portSeq int64

func startLedger(t *testing.T) *testledger.Ledger {
	t.Helper()
	dbURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	return testledger.Start(t, dbURL, fmt.Sprintf(":%d", 19500+port), "orchestrate-test-token", "relayd")
}

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("RELAYD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("RELAYD_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeEnergy always confirms immediately -- mirrors
// dispatcher/internal/orchestrate's own identical fakeEnergy. Records
// targetAddress per call (not just a count) so a test can assert energy
// was requested for the RIGHT address -- added after a real, live run
// against actual CatFee/TRON infrastructure found energy being delegated
// to the vendor's own deposit address instead of the customer's own
// per-order deposit address (the transaction's real sender, and the only
// address that actually needs it): the previous version of this fake
// discarded targetAddress entirely, which is exactly how that bug shipped
// with no test catching it.
type fakeEnergy struct {
	mu             sync.Mutex
	calls          map[string]int
	targetAddress  map[string]string
	lastExternalID string
}

func newFakeEnergy() *fakeEnergy {
	return &fakeEnergy{calls: make(map[string]int), targetAddress: make(map[string]string)}
}

func (f *fakeEnergy) Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (energy.Reservation, error) {
	f.mu.Lock()
	f.calls[externalID]++
	f.targetAddress[externalID] = targetAddress
	f.lastExternalID = externalID
	f.mu.Unlock()
	return energy.Reservation{Status: "CONFIRMED", EnergyUnits: units}, nil
}

// TargetAddressFor reports which address Reserve was actually asked to
// delegate energy to for externalID -- test-only visibility.
func (f *fakeEnergy) TargetAddressFor(externalID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.targetAddress[externalID]
}

// fakeChain stands in for a real TRON node -- deterministic block
// reference, and BroadcastSigned records what it was asked to broadcast
// and returns a fake but stable txid derived from the unsigned bytes.
type fakeChain struct {
	mu         sync.Mutex
	broadcasts int
}

func (f *fakeChain) CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error) {
	return txbuild.BlockReference{
		BlockNumber: 1,
		BlockHash:   [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Timestamp:   time.Now().UTC(),
		Expiration:  time.Now().UTC().Add(2 * time.Minute),
	}, nil
}

func (f *fakeChain) BroadcastSigned(ctx context.Context, unsignedTx []byte, signature [65]byte) (string, error) {
	f.mu.Lock()
	f.broadcasts++
	f.mu.Unlock()
	digest := txbuild.Digest(unsignedTx)
	return fmt.Sprintf("%x", digest[:8]), nil
}

// fakeFinality reports final=true for any txid this test has told it
// about via MarkFinal -- deterministic, no polling-count flakiness.
// CheckExecution defaults to a successful execution for any txid: every
// one of this fake's own construction sites builds it purely to satisfy
// TRC20FinalityChecker's own required-dependency shape, with no interest
// in finality-timing or execution-outcome behavior itself. MarkFailed is
// the opt-in a test would use to exercise the OTHER real outcome
// CheckExecution exists to distinguish (a confirmed on-chain failure,
// e.g. OUT_OF_ENERGY) -- as of this writing, no test in this package
// actually calls it; this is real, missing coverage for
// checkForwardExecutionAndFinish's/checkRefundExecutionAndFinish's own
// failure branch, not a completed cross-reference.
type fakeFinality struct {
	mu     sync.Mutex
	final  map[string]bool
	failed map[string]string // txID -> failureReason, only set via MarkFailed
}

func newFakeFinality() *fakeFinality {
	return &fakeFinality{final: make(map[string]bool), failed: make(map[string]string)}
}

func (f *fakeFinality) MarkFinal(txID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.final[txID] = true
}

// MarkFailed configures CheckExecution to report txID as finalized but
// with a failed on-chain execution -- opt-in, mirroring the real
// OUT_OF_ENERGY incident CheckExecution's own doc comment describes.
func (f *fakeFinality) MarkFailed(txID, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[txID] = reason
}

func (f *fakeFinality) IsFinal(ctx context.Context, txID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return true, nil
}

func (f *fakeFinality) CheckExecution(ctx context.Context, txID string) (final, success bool, failureReason string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if reason, isFailed := f.failed[txID]; isFailed {
		return true, false, reason, nil
	}
	return true, true, "", nil
}

// fakeEVMChain stands in for a real BSC node -- deterministic
// nonce/gas price, and Broadcast records what it was asked to send and
// returns the signed transaction's own real hash (computed by
// go-ethereum itself, not faked), the EVM-direction sibling of
// fakeChain above.
type fakeEVMChain struct {
	mu         sync.Mutex
	broadcasts int
	nonce      uint64
	last       *gethtypes.Transaction
}

// Last returns the most recently broadcast transaction, or nil.
func (f *fakeEVMChain) Last() *gethtypes.Transaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeEVMChain) CurrentNonce(ctx context.Context, address string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nonce
	return n, nil
}

func (f *fakeEVMChain) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return big.NewInt(3_000_000_000), nil
}

func (f *fakeEVMChain) Broadcast(ctx context.Context, signed *gethtypes.Transaction) (string, error) {
	f.mu.Lock()
	f.broadcasts++
	f.nonce++
	f.last = signed
	f.mu.Unlock()
	return signed.Hash().Hex(), nil
}

// fakeBEP20DepositWatcher stands in for depositwatcher's own real
// address book -- advanceForwardingOneBEP20's own defense-in-depth
// cross-check calls GetAddress before ever requesting a deposit-sweep
// signature, so TestFullHappyPath_BEP20ToTRC20 must configure this
// consistently with its own fixture leg's DepositAddress/
// DepositDerivationIndex.
type fakeBEP20DepositWatcher struct {
	mu   sync.Mutex
	byID map[int64]watcherclient.Address
}

func newFakeBEP20DepositWatcher() *fakeBEP20DepositWatcher {
	return &fakeBEP20DepositWatcher{byID: make(map[int64]watcherclient.Address)}
}

func (f *fakeBEP20DepositWatcher) set(orderID int64, address string, index uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[orderID] = watcherclient.Address{Address: address, DerivationIndex: &index, OrderID: orderID}
}

func (f *fakeBEP20DepositWatcher) GetAddress(ctx context.Context, orderID int64) (watcherclient.Address, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.byID[orderID]
	if !ok {
		return watcherclient.Address{}, fmt.Errorf("fakeBEP20DepositWatcher: no address configured for order %d", orderID)
	}
	return addr, nil
}

// fakeTronDepositWatcher is fakeBEP20DepositWatcher's own TRC20-direction
// counterpart -- tronwatcher's own real address book, stood in for.
// advanceForwardingOneTRC20's own identical defense-in-depth cross-check
// calls GetAddress before ever requesting a TRON deposit-sweep signature,
// so any test exercising a TRC20_TO_BEP20 leg that reaches FORWARDING
// must configure this consistently with that leg's own fixture
// DepositAddress/DepositDerivationIndex.
type fakeTronDepositWatcher struct {
	mu   sync.Mutex
	byID map[int64]watcherclient.Address
}

func newFakeTronDepositWatcher() *fakeTronDepositWatcher {
	return &fakeTronDepositWatcher{byID: make(map[int64]watcherclient.Address)}
}

func (f *fakeTronDepositWatcher) set(orderID int64, address string, index uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[orderID] = watcherclient.Address{Address: address, DerivationIndex: &index, OrderID: orderID}
}

func (f *fakeTronDepositWatcher) GetAddress(ctx context.Context, orderID int64) (watcherclient.Address, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.byID[orderID]
	if !ok {
		return watcherclient.Address{}, fmt.Errorf("fakeTronDepositWatcher: no address configured for order %d", orderID)
	}
	return addr, nil
}

// fakeEVMFinality mirrors fakeFinality above for the BEP20 direction.
// fakeEVMFinality defaults to a successful, final execution for any
// txHash -- mirroring fakeFinality's own identical default and the same
// reasoning: before checkForwardEVMExecutionAndFinish/
// checkRefundEVMExecutionAndFinish existed to consult EVMFinalityChecker
// at all, every one of this fake's own construction sites (newFakeEVMFinality)
// built it purely to satisfy EVMBroadcaster/EVMFinalityChecker's own
// required-dependency shape, with no test relying on IsFinal's return
// value -- so changing that default here is safe for every existing
// caller. MarkReverted is the opt-in a test uses to exercise the other
// real outcome IsFinal exists to distinguish (mirrors fakeFinality's own
// MarkFailed).
type fakeEVMFinality struct {
	mu       sync.Mutex
	reverted map[string]error
}

func newFakeEVMFinality() *fakeEVMFinality { return &fakeEVMFinality{reverted: make(map[string]error)} }

// MarkReverted configures IsFinal to report txHash as final but reverted
// -- err is returned verbatim, matching evmbroadcast.Client.IsFinal's own
// contract of a non-nil error meaning a confirmed revert, not "not yet
// mined."
func (f *fakeEVMFinality) MarkReverted(txHash string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reverted[txHash] = err
}

func (f *fakeEVMFinality) IsFinal(ctx context.Context, txHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.reverted[txHash]; ok {
		return false, err
	}
	return true, nil
}

var extIDSeq int64

func uniqueExternalID(t *testing.T) string {
	n := atomic.AddInt64(&extIDSeq, 1)
	return fmt.Sprintf("relay-ext:%s:%d:%d", t.Name(), time.Now().UnixNano(), n)
}

// TestFullHappyPath_TRC20ToBEP20 drives one relay leg through the ENTIRE
// happy flow against a real ledgerd: order creation and funding/
// screening (simulating what tronwatcher and C3 would do in a real
// deployment), then RunTick repeatedly until the leg reaches SETTLED,
// asserting the real account balances this package's own
// orchestrate.go doc comment describes.
func TestFullHappyPath_TRC20ToBEP20(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-happy-1")
	screened := ledger.AdvanceToScreened(order)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	// A real, valid, live-verified TRON address (see internal/txbuild's
	// own tests), distinct from both the slot's own address and the
	// upstream platform's own address below -- required because this
	// test drives a REAL txbuild.BuildTransfer call FROM this leg's own
	// deposit address, which validates its own base58check checksum,
	// unlike a placeholder string. depositDerivationIndex mirrors what a
	// real tronwatcher's own AssignAddress response would have set (this
	// test creates the leg directly via store.Create, not the real
	// driver.CreateRelayLeg, so fakeTronDepositWatcher below is
	// configured to agree with this fixture value).
	depositDerivationIndex := uint32(11)
	leg, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-happy-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:         "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4",
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	})
	if err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}
	if leg.Status != relay.StatusAwaitingDeposit {
		t.Fatalf("expected AWAITING_DEPOSIT, got %s", leg.Status)
	}

	mockProvider := upstream.NewMockProvider("mock", 1)
	// A real, valid, live-verified TRON address (see internal/txbuild's
	// own tests) -- required because this test drives a REAL
	// txbuild.BuildTransfer call, which validates the recipient's own
	// base58check checksum, unlike the mock's own default fake
	// placeholder string.
	mockProvider.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := &fakeChain{}
	finality := newFakeFinality()
	evmChain := &fakeEVMChain{}
	evmFinality := newFakeEVMFinality()
	tronDepositWatcher := newFakeTronDepositWatcher()
	tronDepositWatcher.set(order.ID, "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4", depositDerivationIndex)

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, alert.LogAlerter{}, nil, tronDepositWatcher, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		SlotEVMAddress:         "0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf",
		EnergyPerTransferUnits: 65000,
	})

	ctx := context.Background()

	// Tick 1: RunTick's own phases feed into each other within one call
	// (phase 2 scans every currently-FORWARDING leg, including one phase
	// 1 just started this same tick), so a single tick can legitimately
	// carry this leg from AWAITING_DEPOSIT all the way to a broadcast
	// forward transfer -- discover the screened order, start forwarding
	// (create the upstream order, post relay_forward_start), then
	// immediately reserve energy, sign, and broadcast. It does NOT yet
	// reach FORWARDED, though: checkForwardExecutionAndFinish's own
	// on-chain execution check only ever runs from the NEXT call to
	// advanceForwardingOneTRC20 (its early-return check, at this
	// function's own top, is what short-circuits to it) -- see
	// forward_trc20.go's own doc comment on why a broadcast being
	// accepted is deliberately never treated as proof it executed
	// successfully.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	dispatching := ledger.GetOrder(externalID)
	if dispatching.State != "dispatching" {
		t.Fatalf("after tick 1: expected C1 order state dispatching, got %s", dispatching.State)
	}
	// The relay-leg suspense account should be fully closed, its balance
	// moved to the forwarding-specific suspense account.
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:%d", order.ID)); got != 0 {
		t.Errorf("expected asset:relay:leg:%d to be zeroed after forward_start, got %d", order.ID, got)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:forwarding:%d", order.ID)); got != 100_000000 {
		t.Errorf("expected asset:relay:leg:forwarding:%d to hold 100_000000, got %d", order.ID, got)
	}

	afterTick1, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1.Status != relay.StatusForwarding {
		t.Fatalf("after tick 1: expected still FORWARDING (broadcast, awaiting its own execution check), got %s", afterTick1.Status)
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected exactly 1 broadcast, got %d", chain.broadcasts)
	}
	// This leg's forward signing now goes through the per-order
	// deposit-sweep path (RequestTronDepositSweepSignature), never the
	// shared-slot RequestSignature -- see forward_trc20.go's own fix.
	if signer.RequestSignatureCallCount() != 0 {
		t.Errorf("expected 0 shared-slot signature requests for a TRC20 forward leg, got %d", signer.RequestSignatureCallCount())
	}
	if signer.RequestTronDepositSweepSignatureCallCount() != 1 {
		t.Errorf("expected exactly 1 TRON deposit-sweep signature request, got %d", signer.RequestTronDepositSweepSignatureCallCount())
	}

	// Tick 2: fakeFinality's own CheckExecution defaults to a successful
	// execution for any txid (see its own doc comment), so this tick's
	// early-return check reaches checkForwardExecutionAndFinish and
	// marks the leg FORWARDED.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.Status != relay.StatusForwarded {
		t.Fatalf("after tick 2: expected FORWARDED, got %s", afterTick2.Status)
	}
	if afterTick2.ForwardTxID == nil || *afterTick2.ForwardTxID == "" {
		t.Fatal("expected a forward_tx_id to be recorded")
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected still exactly 1 broadcast after tick 2, got %d", chain.broadcasts)
	}

	// Mark the upstream order complete so tick 3 can settle.
	if err := mockProvider.SetOrderStatus(*afterTick2.UpstreamOrderID, upstream.StatusComplete, ptrAmount(money.Amount{Asset: money.USDT_BEP20, Units: 99_650000})); err != nil {
		t.Fatalf("SetOrderStatus: %v", err)
	}

	// Tick 3: should see the upstream order complete and settle.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	afterTick3, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick3.Status != relay.StatusSettled {
		t.Fatalf("after tick 3: expected SETTLED, got %s", afterTick3.Status)
	}
	if afterTick3.AmountOutActual == nil || afterTick3.AmountOutActual.Units != 99_650000 {
		t.Errorf("expected amount_out_actual 99650000, got %v", afterTick3.AmountOutActual)
	}

	settledOrder := ledger.GetOrder(externalID)
	if settledOrder.State != "settled" {
		t.Fatalf("expected C1 order state settled, got %s", settledOrder.State)
	}

	// Verify the real double-entry outcome: the customer's TRC20
	// liability closed to zero, the forwarding suspense account closed
	// to zero, and the commission revenue account holds exactly
	// fee_units (0.3 USDT_TRC20 = 300000 minor units).
	if got := ledger.AccountBalance("liability:customer:cust-happy-1:USDT_TRC20"); got != 0 {
		t.Errorf("expected customer liability to close to 0, got %d", got)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:forwarding:%d", order.ID)); got != 0 {
		t.Errorf("expected forwarding suspense account to close to 0, got %d", got)
	}
	if got := ledger.AccountBalance("revenue:relay_commission:USDT_TRC20"); got != -300000 {
		t.Errorf("expected commission revenue of -300000 (credit-normal), got %d", got)
	}

	// Idempotency: a 4th tick must not re-broadcast, re-sign, or
	// re-reserve, and must not error.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 4 (idempotent replay): %v", err)
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected still exactly 1 broadcast after a 4th tick, got %d", chain.broadcasts)
	}
}

// TestRefund_StuckForwardingLegGetsRefunded drives a relay leg to
// FORWARDING (a real upstream order created, a real relay_forward_start
// entry posted) and then makes its own forward-transfer signature
// permanently fail, simulating a leg that can never actually broadcast
// -- exactly the case refund.go's own package doc comment describes.
// Asserts the FULL R5 refund path against a real ledgerd: the
// relay_forward_abandon reversal, the relay_refund entry, and the actual
// on-chain refund broadcast, all within ticks driven by RunTick alone
// (no direct Store/Ledger calls bypassing the orchestrator), ending with
// every account closed to exactly zero -- the same discipline
// TestFullHappyPath_TRC20ToBEP20 applies to the settle path.
func TestRefund_StuckForwardingLegGetsRefunded(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-refund-1")
	// A real, valid, live-verified TRON address (see internal/txbuild's
	// own tests) -- required because this test drives a REAL
	// txbuild.BuildTransfer call for the refund, which validates the
	// recipient's own base58check checksum.
	senderAddress := "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj"
	screened := ledger.AdvanceToScreenedWithSender(order, senderAddress)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	depositDerivationIndex := uint32(12)
	leg, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-refund-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:         "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4",
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	})
	if err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}
	if leg.Status != relay.StatusAwaitingDeposit {
		t.Fatalf("expected AWAITING_DEPOSIT, got %s", leg.Status)
	}

	mockProvider := upstream.NewMockProvider("mock-refund", 1)
	// Deliberately do NOT call ForceDepositAddress here -- MockProvider's
	// own default placeholder ("mock-deposit-addr-<seq>") is not a real
	// TRON address, so txbuild.BuildTransfer (forward_trc20.go) fails on
	// every single attempt, before signing or broadcasting is ever
	// reached. That's what keeps this leg stuck FORWARDING with no
	// broadcast ever cached (hasPendingBroadcast stays false), the
	// precondition refundStuckForwardingLegs looks for -- forcing the
	// SIGNING step to fail instead is not viable here: the real signing
	// idempotency key is scoped to the unsigned tx's own digest
	// (forward_trc20.go's own fix), which this test has no way to
	// predict in advance.
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := &fakeChain{}
	finality := newFakeFinality()
	evmChain := &fakeEVMChain{}
	evmFinality := newFakeEVMFinality()
	tronDepositWatcher := newFakeTronDepositWatcher()
	tronDepositWatcher.set(order.ID, "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4", depositDerivationIndex)

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, alert.LogAlerter{}, nil, tronDepositWatcher, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		EnergyPerTransferUnits: 65000,
		// A near-zero timeout so this test doesn't need to sleep for a
		// realistic operational duration -- ForwardingTimeout is an
		// operational tuning knob, not correctness-critical (see its own
		// doc comment), so a tiny value here is a legitimate, not a
		// cheating, test configuration.
		ForwardingTimeout: time.Millisecond,
	})

	ctx := context.Background()

	// One tick is enough to carry this leg all the way from
	// AWAITING_DEPOSIT to REFUND_PENDING with the refund transfer itself
	// already broadcast: startScreenedLegs creates the upstream order and
	// posts relay_forward_start (screened -> dispatching);
	// advanceForwardingLegs tries and fails to build the forward transfer
	// (this leg's forced-invalid upstream deposit address, above); by the
	// time refundStuckForwardingLegs runs later in this SAME tick, the
	// leg's own MarkForwarding timestamp is already older than the 1ms
	// ForwardingTimeout (a handful of real HTTP round trips to ledgerd
	// take far longer than that) and hasPendingBroadcast correctly
	// reports no broadcast ever happened, so it immediately posts
	// relay_forward_abandon + relay_refund and marks REFUND_PENDING;
	// advanceRefundPendingLegs, later still in the same tick, signs and
	// broadcasts the actual refund transfer -- but, mirroring the forward
	// path's own two-phase execution-check fix, does NOT yet mark
	// REFUNDED: that only happens from a LATER tick's own early-return
	// check into checkRefundExecutionAndFinish. The defensive second tick
	// below is what actually reaches REFUNDED, not a fallback for a
	// slower environment.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	// The refund transfer broadcasts on tick 1 (above) but is only marked
	// REFUNDED once a later tick's own execution check confirms it (see
	// checkRefundExecutionAndFinish) -- expected to still be REFUND_PENDING
	// here, not a "slower environment" edge case.
	afterTick, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick.Status == relay.StatusRefundPending {
		if err := orch.RunTick(ctx); err != nil {
			t.Fatalf("RunTick 2: %v", err)
		}
		afterTick, err = store.GetByExternalID(ctx, externalID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if afterTick.Status != relay.StatusRefunded {
		t.Fatalf("expected REFUNDED, got %s", afterTick.Status)
	}
	if afterTick.RefundTxID == nil || *afterTick.RefundTxID == "" {
		t.Fatal("expected a refund_tx_id to be recorded")
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected exactly 1 TRC20 broadcast (the refund; the forward transfer never signed), got %d", chain.broadcasts)
	}

	refundedOrder := ledger.GetOrder(externalID)
	if refundedOrder.State != "refunded" {
		t.Fatalf("expected C1 order state refunded, got %s", refundedOrder.State)
	}

	// Every account this leg touched must close to exactly zero -- the
	// full amount_in went back to the customer, nothing was withheld as
	// commission (nothing was ever delivered).
	if got := ledger.AccountBalance("liability:customer:cust-refund-1:USDT_TRC20"); got != 0 {
		t.Errorf("expected customer liability to close to 0, got %d", got)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:%d", order.ID)); got != 0 {
		t.Errorf("expected asset:relay:leg:%d to close to 0, got %d", order.ID, got)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:forwarding:%d", order.ID)); got != 0 {
		t.Errorf("expected asset:relay:leg:forwarding:%d to close to 0, got %d", order.ID, got)
	}

	// Idempotency: one more tick must not re-broadcast or error -- the
	// leg is no longer FORWARDING or REFUND_PENDING, so neither refund
	// phase should touch it again.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick (idempotent replay): %v", err)
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected still exactly 1 broadcast after one more tick, got %d", chain.broadcasts)
	}
}

// fakeAlerter captures every Alert fired, for
// TestUnrecoverable_PostForwardedUpstreamFailure to assert against --
// the one thing alert.LogAlerter itself cannot be asserted on directly
// (it only writes to the process log).
type fakeAlerter struct {
	mu    sync.Mutex
	fired []alert.Alert
}

func (f *fakeAlerter) Fire(ctx context.Context, a alert.Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fired = append(f.fired, a)
	return nil
}

func (f *fakeAlerter) Fired() []alert.Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]alert.Alert, len(f.fired))
	copy(out, f.fired)
	return out
}

// TestUnrecoverable_PostForwardedUpstreamFailure drives a relay leg all
// the way to FORWARDED (a real broadcast succeeds), then has the
// upstream platform report FAILED -- asserting the leg lands
// UNRECOVERABLE (never REFUND_PENDING: once the forward transfer
// confirms on-chain, R5's refund path no longer applies, per this
// package's own doc comment) and that a real alert fires exactly once.
func TestUnrecoverable_PostForwardedUpstreamFailure(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-unrecoverable-1")
	screened := ledger.AdvanceToScreened(order)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	depositDerivationIndex := uint32(13)
	if _, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-unrecoverable-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:         "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4",
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	mockProvider := upstream.NewMockProvider("mock-unrecoverable", 1)
	mockProvider.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := &fakeChain{}
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

	// Tick 1: broadcasts the forward transfer but does not yet mark it
	// FORWARDED -- see checkForwardExecutionAndFinish's own doc comment.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	afterTick1, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1.Status != relay.StatusForwarding {
		t.Fatalf("after tick 1: expected still FORWARDING (broadcast, awaiting its own execution check), got %s", afterTick1.Status)
	}

	// Tick 2: fakeFinality's own CheckExecution defaults to success, so
	// this tick's early-return check confirms the broadcast and marks
	// the leg FORWARDED, same as the happy path.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.Status != relay.StatusForwarded {
		t.Fatalf("after tick 2: expected FORWARDED, got %s", afterTick2.Status)
	}

	if err := mockProvider.SetOrderStatus(*afterTick2.UpstreamOrderID, upstream.StatusFailed, nil); err != nil {
		t.Fatalf("SetOrderStatus: %v", err)
	}

	// Tick 3: the upstream order reports FAILED -- no on-chain lever left
	// (the forward transfer already confirmed), so this must land
	// UNRECOVERABLE and fire an alert, never REFUND_PENDING/REFUNDED.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	afterTick3, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick3.Status != relay.StatusUnrecoverable {
		t.Fatalf("after tick 3: expected UNRECOVERABLE, got %s", afterTick3.Status)
	}

	fired := alerter.Fired()
	if len(fired) != 1 {
		t.Fatalf("expected exactly 1 alert fired, got %d", len(fired))
	}
	if fired[0].Severity != alert.SeverityCritical {
		t.Errorf("expected CRITICAL severity, got %s", fired[0].Severity)
	}
	if fired[0].ExternalID != externalID {
		t.Errorf("expected alert for %s, got %s", externalID, fired[0].ExternalID)
	}

	// Idempotency: a 4th tick must not fire a second alert -- the leg is
	// no longer FORWARDED, so it no longer appears in that phase's own
	// ListByStatus scan.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 4 (idempotent replay): %v", err)
	}
	if len(alerter.Fired()) != 1 {
		t.Errorf("expected still exactly 1 alert after a 4th tick, got %d", len(alerter.Fired()))
	}
}

// TestFullHappyPath_BEP20ToTRC20 is TestFullHappyPath_TRC20ToBEP20's own
// mirror-direction sibling: the same real-ledgerd, real-double-entry
// happy flow, but the customer deposits USDT_BEP20 and relayd's forward
// leg is a real evmtx-built ERC20 transfer (broadcast via fakeEVMChain,
// not a real BSC node, but real go-ethereum construction/signing math --
// see evmtx_test.go's own TestSignRoundTrip_RecoversCorrectSender for
// the piece that proves the signature hand-off itself works).
func TestFullHappyPath_BEP20ToTRC20(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrderBEP20ToTRC20(externalID, "cust-happy-2")
	screened := ledger.AdvanceToScreenedBEP20ToTRC20(order)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	// This test creates the leg directly via store.Create rather than the
	// real driver.CreateRelayLeg (no real depositwatcher instance runs
	// here), so there is no real AssignAddress response to take a
	// derivation index from -- fakeBEP20DepositWatcher below is
	// configured to agree with this fixture value, standing in for what
	// a real depositwatcher would report.
	// The deposit address must be the one the fake signer's key for this
	// index controls: the pre-broadcast check recovers the real signer.
	depositDerivationIndex := uint32(7)
	depositAddress := signing.FakeBSCDepositAddress(depositDerivationIndex)
	leg, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.BEP20ToTRC20,
		CustomerID: "cust-happy-2", DestinationAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		DepositAddress:         depositAddress,
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_BEP20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_TRC20, Units: 99_700000},
	})
	if err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}
	if leg.Status != relay.StatusAwaitingDeposit {
		t.Fatalf("expected AWAITING_DEPOSIT, got %s", leg.Status)
	}

	// A distinct provider name from the TRC20-direction test's own
	// "mock" -- ProviderOrderID is "mock-<name>-<seq>" with seq reset
	// per MockProvider instance, and both tests share one real Postgres
	// (relayd_test, -p 1 serialized, not per-test-isolated), so reusing
	// "mock" here would collide with that test's own
	// relay_legs_upstream_order_unique row.
	mockProvider := upstream.NewMockProvider("mock-bep20", 1)
	// A real, valid EVM address -- required because this test drives a
	// REAL evmtx.BuildTransfer call, which validates the recipient via
	// common.IsHexAddress, unlike the mock's own default fake placeholder
	// string.
	mockProvider.ForceDepositAddress("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf")
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := &fakeChain{}
	finality := newFakeFinality()
	evmChain := &fakeEVMChain{}
	evmFinality := newFakeEVMFinality()
	bep20DepositWatcher := newFakeBEP20DepositWatcher()
	bep20DepositWatcher.set(order.ID, depositAddress, depositDerivationIndex)

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, alert.LogAlerter{},
		bep20DepositWatcher, nil, orchestrate.Config{
			SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
			SlotEVMAddress:         "0x1111111111111111111111111111111111abcd",
			EnergyPerTransferUnits: 65000,
		})

	ctx := context.Background()

	// Tick 1: same inter-phase efficiency as the TRC20 direction's own
	// test -- can carry this leg from AWAITING_DEPOSIT all the way to a
	// broadcast forward transfer in one tick, but not yet to FORWARDED:
	// checkForwardEVMExecutionAndFinish's own on-chain execution check
	// only ever runs from the NEXT call to advanceForwardingOneBEP20 (see
	// forward_bep20.go's own doc comment, mirroring forward_trc20.go's).
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	dispatching := ledger.GetOrder(externalID)
	if dispatching.State != "dispatching" {
		t.Fatalf("after tick 1: expected C1 order state dispatching, got %s", dispatching.State)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:%d", order.ID)); got != 0 {
		t.Errorf("expected asset:relay:leg:%d to be zeroed after forward_start, got %d", order.ID, got)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:forwarding:%d", order.ID)); got != 100_000000 {
		t.Errorf("expected asset:relay:leg:forwarding:%d to hold 100_000000, got %d", order.ID, got)
	}

	afterTick1, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1.Status != relay.StatusForwarding {
		t.Fatalf("after tick 1: expected still FORWARDING (broadcast, awaiting its own execution check), got %s", afterTick1.Status)
	}
	if evmChain.broadcasts != 1 {
		t.Errorf("expected exactly 1 EVM broadcast, got %d", evmChain.broadcasts)
	}
	if chain.broadcasts != 0 {
		t.Errorf("expected 0 TRC20 broadcasts for a BEP20-direction leg, got %d", chain.broadcasts)
	}
	// This leg's forward signing now goes through the per-order
	// deposit-sweep path (RequestDepositSweepSignature), never the
	// shared-slot RequestSignature -- see forward_bep20.go's own fix.
	if signer.RequestSignatureCallCount() != 0 {
		t.Errorf("expected 0 shared-slot signature requests for a BEP20 forward leg, got %d", signer.RequestSignatureCallCount())
	}
	if signer.RequestDepositSweepSignatureCallCount() != 1 {
		t.Errorf("expected exactly 1 deposit-sweep signature request, got %d", signer.RequestDepositSweepSignatureCallCount())
	}

	// Tick 2: fakeEVMFinality's own IsFinal defaults to (true, nil) for
	// any txHash (mirroring fakeFinality's own default), so this tick's
	// early-return check reaches checkForwardEVMExecutionAndFinish and
	// marks the leg FORWARDED.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.Status != relay.StatusForwarded {
		t.Fatalf("after tick 2: expected FORWARDED, got %s", afterTick2.Status)
	}
	if afterTick2.ForwardTxID == nil || *afterTick2.ForwardTxID == "" {
		t.Fatal("expected a forward_tx_id to be recorded")
	}
	if evmChain.broadcasts != 1 {
		t.Errorf("expected still exactly 1 EVM broadcast after tick 2, got %d", evmChain.broadcasts)
	}

	if err := mockProvider.SetOrderStatus(*afterTick2.UpstreamOrderID, upstream.StatusComplete, ptrAmount(money.Amount{Asset: money.USDT_TRC20, Units: 99_650000})); err != nil {
		t.Fatalf("SetOrderStatus: %v", err)
	}

	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	afterTick3, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick3.Status != relay.StatusSettled {
		t.Fatalf("after tick 3: expected SETTLED, got %s", afterTick3.Status)
	}
	if afterTick3.AmountOutActual == nil || afterTick3.AmountOutActual.Units != 99_650000 {
		t.Errorf("expected amount_out_actual 99650000, got %v", afterTick3.AmountOutActual)
	}

	settledOrder := ledger.GetOrder(externalID)
	if settledOrder.State != "settled" {
		t.Fatalf("expected C1 order state settled, got %s", settledOrder.State)
	}

	if got := ledger.AccountBalance("liability:customer:cust-happy-2:USDT_BEP20"); got != 0 {
		t.Errorf("expected customer liability to close to 0, got %d", got)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:forwarding:%d", order.ID)); got != 0 {
		t.Errorf("expected forwarding suspense account to close to 0, got %d", got)
	}
	if got := ledger.AccountBalance("revenue:relay_commission:USDT_BEP20"); got != -300000 {
		t.Errorf("expected commission revenue of -300000 (credit-normal), got %d", got)
	}

	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 4 (idempotent replay): %v", err)
	}
	if evmChain.broadcasts != 1 {
		t.Errorf("expected still exactly 1 EVM broadcast after a 4th tick, got %d", evmChain.broadcasts)
	}
}

func ptrAmount(a money.Amount) *money.Amount { return &a }

// TestExternalRefund_ManuallyRejectedHoldGetsRefunded proves the manual
// HELD->REFUNDED path end to end: a real held RELAY order, refunded
// exactly the way screening/internal/holds.go's own Reject flow would
// (fetch the entry from driver.BuildRefundEntry -- what relayd's own
// GET .../refund-entry endpoint serves -- then post held->refunded to
// C1 with it, mirroring ledgerclient.RejectHold's own request shape
// verbatim since this test has no real screend process to call through),
// then picked up by internal/orchestrate's own startExternallyRefundedLegs
// and carried through REFUND_PENDING to REFUNDED with a real on-chain
// broadcast -- the same advanceRefundPendingLegs machinery R5's own
// timeout-triggered refund already uses from that point on.
func TestExternalRefund_ManuallyRejectedHoldGetsRefunded(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-external-refund-1")
	senderAddress := "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj"
	held := ledger.AdvanceToHeld(order, senderAddress)
	if held.State != "held" {
		t.Fatalf("fixture setup: expected held, got %s", held.State)
	}

	// The refund is sent from this deposit address, so it must be a real
	// one that the watcher agrees with.
	depositDerivationIndex := uint32(31)
	depositAddress := signing.FakeTronDepositAddress(depositDerivationIndex)
	if _, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-external-refund-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:         depositAddress,
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	// This is screening's own job in a real deployment (its own
	// RelayAwareRefundEntryBuilder calls relayd's HTTP endpoint) --
	// reproduced directly here via the same Driver method that endpoint
	// wraps, since this test has no real screend/relayd HTTP boundary to
	// cross.
	d := &driver.Driver{Ledger: client, Store: store}
	entry, err := d.BuildRefundEntry(context.Background(), externalID)
	if err != nil {
		t.Fatalf("BuildRefundEntry: %v", err)
	}
	entryLines := make([]map[string]any, len(entry.Lines))
	for i, l := range entry.Lines {
		entryLines[i] = map[string]any{"account_code": l.AccountCode, "asset": l.Asset, "amount": l.Amount}
	}
	refunded := ledger.RejectHeld(held, map[string]any{
		"entry_type": entry.EntryType, "occurred_at": entry.OccurredAt, "lines": entryLines,
	})
	if refunded.State != "refunded" {
		t.Fatalf("expected C1 order state refunded, got %s", refunded.State)
	}

	// The leg is still AWAITING_DEPOSIT locally -- relayd never engaged
	// it (it never reached screened).
	preTick, err := store.GetByExternalID(context.Background(), externalID)
	if err != nil {
		t.Fatal(err)
	}
	if preTick.Status != relay.StatusAwaitingDeposit {
		t.Fatalf("expected AWAITING_DEPOSIT before any tick, got %s", preTick.Status)
	}

	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := &fakeChain{}
	finality := newFakeFinality()
	evmChain := &fakeEVMChain{}
	evmFinality := newFakeEVMFinality()
	mockProvider := upstream.NewMockProvider("mock-external-refund", 1)
	tronDepositWatcher := newFakeTronDepositWatcher()
	tronDepositWatcher.set(order.ID, depositAddress, depositDerivationIndex)

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, alert.LogAlerter{}, nil, tronDepositWatcher, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		EnergyPerTransferUnits: 65000,
	})

	ctx := context.Background()

	// One tick: startExternallyRefundedLegs notices the refunded order,
	// marks REFUND_PENDING; advanceRefundPendingLegs, later the same
	// tick, signs and broadcasts the real refund transfer and marks
	// REFUNDED -- the same one-tick-carries-multiple-phases behavior
	// this package's own other tests already document.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	afterTick, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick.Status == relay.StatusRefundPending {
		if err := orch.RunTick(ctx); err != nil {
			t.Fatalf("RunTick 2: %v", err)
		}
		afterTick, err = store.GetByExternalID(ctx, externalID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if afterTick.Status != relay.StatusRefunded {
		t.Fatalf("expected REFUNDED, got %s", afterTick.Status)
	}
	if afterTick.RefundTxID == nil || *afterTick.RefundTxID == "" {
		t.Fatal("expected a refund_tx_id to be recorded")
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected exactly 1 TRC20 broadcast (the refund), got %d", chain.broadcasts)
	}

	if got := ledger.AccountBalance("liability:customer:cust-external-refund-1:USDT_TRC20"); got != 0 {
		t.Errorf("expected customer liability to close to 0, got %d", got)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:%d", order.ID)); got != 0 {
		t.Errorf("expected asset:relay:leg:%d to close to 0, got %d", order.ID, got)
	}

	// Idempotency: one more tick must not re-broadcast or error.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick (idempotent replay): %v", err)
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected still exactly 1 broadcast after one more tick, got %d", chain.broadcasts)
	}
}

// TestStaleLegAlarm_FiresOnceThenNeverAgain drives a leg to FORWARDED
// (a real broadcast succeeds, matching TestUnrecoverable_PostForwardedUpstreamFailure's
// own setup), then enables the stale-leg reconciliation alarm and waits
// past its threshold -- asserting a SeverityWarning alert fires exactly
// once (never again on a later tick) with the leg's own status recorded,
// and that stale_alerted_at is persisted so a process restart could not
// re-fire it either.
func TestStaleLegAlarm_FiresOnceThenNeverAgain(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-stale-1")
	screened := ledger.AdvanceToScreened(order)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	depositDerivationIndex := uint32(14)
	if _, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-stale-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:         "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4",
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	mockProvider := upstream.NewMockProvider("mock-stale", 1)
	mockProvider.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := &fakeChain{}
	finality := newFakeFinality()
	evmChain := &fakeEVMChain{}
	evmFinality := newFakeEVMFinality()
	alerter := &fakeAlerter{}

	// StaleLegAlertAfter starts at 0 (disabled) so driving to FORWARDED
	// below is deterministic and produces no alarm noise of its own.
	tronDepositWatcher := newFakeTronDepositWatcher()
	tronDepositWatcher.set(order.ID, "TLQ5Xwr2YEJWhNydyMt5rHN8KDZ4yduNs4", depositDerivationIndex)

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, alerter, nil, tronDepositWatcher, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		EnergyPerTransferUnits: 65000,
	})

	ctx := context.Background()
	// Tick 1: broadcasts the forward transfer but does not yet mark it
	// FORWARDED -- see checkForwardExecutionAndFinish's own doc comment.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	afterTick1, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1.Status != relay.StatusForwarding {
		t.Fatalf("after tick 1: expected still FORWARDING (broadcast, awaiting its own execution check), got %s", afterTick1.Status)
	}

	// Tick 2: fakeFinality's own CheckExecution defaults to success, so
	// this tick's early-return check confirms the broadcast and marks
	// the leg FORWARDED.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	afterTick1b, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1b.Status != relay.StatusForwarded {
		t.Fatalf("after tick 2: expected FORWARDED, got %s", afterTick1b.Status)
	}
	if len(alerter.Fired()) != 0 {
		t.Fatalf("expected no alerts yet (StaleLegAlertAfter was disabled), got %d", len(alerter.Fired()))
	}

	// Now enable the alarm with a near-zero threshold and wait past it --
	// Orchestrator.Cfg is an exported field precisely so a test can do
	// this without needing a second Orchestrator sharing the same store.
	orch.Cfg.StaleLegAlertAfter = time.Millisecond
	time.Sleep(5 * time.Millisecond)

	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	fired := alerter.Fired()
	if len(fired) != 1 {
		t.Fatalf("expected exactly 1 alert fired, got %d", len(fired))
	}
	if fired[0].Severity != alert.SeverityWarning {
		t.Errorf("expected WARNING severity, got %s", fired[0].Severity)
	}
	if fired[0].Reason != "relay_leg_stale" {
		t.Errorf("expected reason relay_leg_stale, got %s", fired[0].Reason)
	}
	if fired[0].ExternalID != externalID {
		t.Errorf("expected alert for %s, got %s", externalID, fired[0].ExternalID)
	}

	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.StaleAlertedAt == nil {
		t.Fatal("expected stale_alerted_at to be recorded")
	}
	// Still FORWARDED -- the alarm is purely observational, it never
	// changes the leg's own status.
	if afterTick2.Status != relay.StatusForwarded {
		t.Errorf("expected the alarm to leave status unchanged (FORWARDED), got %s", afterTick2.Status)
	}

	// Idempotency: further ticks must never fire a second alert.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	if len(alerter.Fired()) != 1 {
		t.Errorf("expected still exactly 1 alert after a 3rd tick, got %d", len(alerter.Fired()))
	}
}

// TestRefund_StuckAwaitingDepositLegGetsRefunded proves the last
// remaining self-contained R5 gap: a leg whose own C1 order reached
// screened but whose upstream.CreateOrder call never once succeeds
// (a real, permanent vendor rejection, not a transient one) is
// automatically refunded on-chain, directly from screened -- no
// forwarding-suspense reversal needed, since relay_forward_start never
// ran for this leg.
func TestRefund_StuckAwaitingDepositLegGetsRefunded(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrder(externalID, "cust-awaiting-refund-1")
	senderAddress := "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj"
	screened := ledger.AdvanceToScreenedWithSender(order, senderAddress)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	depositDerivationIndex := uint32(32)
	depositAddress := signing.FakeTronDepositAddress(depositDerivationIndex)
	if _, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: screened.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-awaiting-refund-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:         depositAddress,
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	mockProvider := upstream.NewMockProvider("mock-awaiting-refund", 1)
	mockProvider.ForceCreateOrderError(fmt.Errorf("replay: simulated permanent vendor rejection"))
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	chain := &fakeChain{}
	finality := newFakeFinality()
	evmChain := &fakeEVMChain{}
	evmFinality := newFakeEVMFinality()
	alerter := &fakeAlerter{}
	tronDepositWatcher := newFakeTronDepositWatcher()
	tronDepositWatcher.set(screened.ID, depositAddress, depositDerivationIndex)
	energy := newFakeEnergy()

	// ForwardingTimeout starts at 0 (disabled) so tick 1 -- which
	// records forward_attempt_started_at and fails CreateOrder -- is
	// deterministic, matching TestStaleLegAlarm_FiresOnceThenNeverAgain's
	// own construction.
	orch := orchestrate.New(store, client, mockProvider, energy, signer, chain, finality, evmChain, evmFinality, alerter, nil, tronDepositWatcher, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		EnergyPerTransferUnits: 65000,
	})

	ctx := context.Background()
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	afterTick1, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1.Status != relay.StatusAwaitingDeposit {
		t.Fatalf("after tick 1: expected AWAITING_DEPOSIT (CreateOrder should have failed), got %s", afterTick1.Status)
	}
	if afterTick1.ForwardAttemptStartedAt == nil {
		t.Fatal("expected forward_attempt_started_at to be recorded even though CreateOrder failed")
	}
	stillScreened := ledger.GetOrder(externalID)
	if stillScreened.State != "screened" {
		t.Fatalf("expected C1 order to still be screened, got %s", stillScreened.State)
	}

	// Now enable the timeout and wait past it.
	orch.Cfg.ForwardingTimeout = time.Millisecond
	time.Sleep(5 * time.Millisecond)

	// Tick 2: refundStuckAwaitingDepositLegs commits the refund on C1 and
	// marks REFUND_PENDING; advanceRefundPendingLegs, later the same
	// tick, signs and broadcasts the refund transfer -- but, mirroring
	// the forward path's own two-phase execution-check fix, does NOT yet
	// mark REFUNDED: that only happens from a LATER tick's own
	// early-return check into checkRefundExecutionAndFinish.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.Status != relay.StatusRefundPending {
		t.Fatalf("after tick 2: expected still REFUND_PENDING (broadcast, awaiting its own execution check), got %s", afterTick2.Status)
	}

	// Tick 3: fakeFinality's own CheckExecution defaults to success, so
	// this tick's early-return check confirms the broadcast and marks
	// the leg REFUNDED.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	afterTick2b, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2b.Status != relay.StatusRefunded {
		t.Fatalf("after tick 3: expected REFUNDED, got %s", afterTick2b.Status)
	}
	if afterTick2b.RefundTxID == nil || *afterTick2b.RefundTxID == "" {
		t.Fatal("expected a refund_tx_id to be recorded")
	}
	// The customer's funds never left the deposit address, so the refund
	// is sent (and energy delegated) from there, signed with its own
	// per-order key -- relayd's own slot key is never involved.
	if got := energy.TargetAddressFor(externalID); got != depositAddress {
		t.Errorf("expected refund energy delegated to the deposit address %s, got %s", depositAddress, got)
	}
	if signer.RequestTronDepositSweepSignatureCallCount() == 0 {
		t.Error("expected the refund to be signed with the deposit address's own key")
	}
	if n := signer.RequestSignatureCallCount(); n != 0 {
		t.Errorf("expected relayd's slot key never to sign a refund, got %d slot signing requests", n)
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected exactly 1 TRC20 broadcast (the refund), got %d", chain.broadcasts)
	}

	refundedOrder := ledger.GetOrder(externalID)
	if refundedOrder.State != "refunded" {
		t.Fatalf("expected C1 order state refunded, got %s", refundedOrder.State)
	}

	if got := ledger.AccountBalance("liability:customer:cust-awaiting-refund-1:USDT_TRC20"); got != 0 {
		t.Errorf("expected customer liability to close to 0, got %d", got)
	}
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:%d", order.ID)); got != 0 {
		t.Errorf("expected asset:relay:leg:%d to close to 0, got %d", order.ID, got)
	}
	// The forwarding-suspense account was never opened for this leg --
	// relay_forward_start never ran, unlike the stuck-FORWARDING case.
	if got := ledger.AccountBalance(fmt.Sprintf("asset:relay:leg:forwarding:%d", order.ID)); got != 0 {
		t.Errorf("expected asset:relay:leg:forwarding:%d to stay 0 (never opened), got %d", order.ID, got)
	}

	// Idempotency: one more tick must not re-broadcast or error, and
	// must not try CreateOrder again either.
	createOrderCallsBefore := mockProvider.CreateOrderCallCount()
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 4 (idempotent replay): %v", err)
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected still exactly 1 broadcast after one more tick, got %d", chain.broadcasts)
	}
	if mockProvider.CreateOrderCallCount() != createOrderCallsBefore {
		t.Errorf("expected no further CreateOrder calls once refunded, got %d more",
			mockProvider.CreateOrderCallCount()-createOrderCallsBefore)
	}
}

// TestRefundBEP20_IsSentFromTheDepositAddress covers the BEP20 refund
// path end to end -- previously untested, and where refunding from the
// wrong address mattered most: on BSC the signer IS the sender, so the
// refund must be signed by the deposit address's own per-order key, not
// relayd's slot, and must return the full deposit to the depositor C1
// recorded.
func TestRefundBEP20_IsSentFromTheDepositAddress(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	externalID := uniqueExternalID(t)
	order := ledger.CreateRelayOrderBEP20ToTRC20(externalID, "cust-bep20-refund-1")
	depositor := "0xAe2166bD7901eA67c1E2BC4179418fc228108f07"
	screened := ledger.AdvanceToScreenedBEP20ToTRC20WithSender(order, depositor)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	depositDerivationIndex := uint32(33)
	depositAddress := signing.FakeBSCDepositAddress(depositDerivationIndex)
	if _, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.BEP20ToTRC20,
		CustomerID: "cust-bep20-refund-1", DestinationAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		DepositAddress:         depositAddress,
		DepositDerivationIndex: &depositDerivationIndex,
		AmountIn:               money.Amount{Asset: money.USDT_BEP20, Units: 100_000000},
		AmountOutExpected:      money.Amount{Asset: money.USDT_TRC20, Units: 99_700000},
	}); err != nil {
		t.Fatalf("creating relay leg: %v", err)
	}

	mockProvider := upstream.NewMockProvider("mock-bep20-refund", 1)
	mockProvider.ForceCreateOrderError(fmt.Errorf("replay: simulated permanent vendor rejection"))
	signer := signing.NewFakeSigningService()
	evmChain := &fakeEVMChain{}
	bep20DepositWatcher := newFakeBEP20DepositWatcher()
	bep20DepositWatcher.set(order.ID, depositAddress, depositDerivationIndex)

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, &fakeChain{}, newFakeFinality(), evmChain, newFakeEVMFinality(), &fakeAlerter{},
		bep20DepositWatcher, nil, orchestrate.Config{
			SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", SlotEVMAddress: signing.FakeSlotEVMAddress(1),
			EnergyPerTransferUnits: 65000,
		})
	ctx := context.Background()

	// Tick 1: CreateOrder fails, recording forward_attempt_started_at.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 1: %v", err)
	}
	orch.Cfg.ForwardingTimeout = time.Millisecond
	time.Sleep(5 * time.Millisecond)

	// Tick 2: the refund is committed on C1 and broadcast.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	sent := evmChain.Last()
	if sent == nil {
		t.Fatal("expected a BEP20 refund broadcast")
	}
	from, err := gethtypes.Sender(gethtypes.NewEIP155Signer(big.NewInt(56)), sent)
	if err != nil {
		t.Fatalf("recovering the refund's sender: %v", err)
	}
	if !strings.EqualFold(from.Hex(), depositAddress) {
		t.Errorf("refund sent from %s, want the deposit address %s", from.Hex(), depositAddress)
	}
	data := sent.Data()
	if got := "0x" + hex.EncodeToString(data[4+12:4+32]); !strings.EqualFold(got, depositor) {
		t.Errorf("refund pays %s, want the recorded depositor %s", got, depositor)
	}
	wantRaw, _ := new(big.Int).SetString("100000000000000000000", 10) // 100 USDT at 18 decimals
	if got := new(big.Int).SetBytes(data[4+32 : 4+64]); got.Cmp(wantRaw) != 0 {
		t.Errorf("refund moves %s raw units, want %s (the full deposit)", got, wantRaw)
	}
	if n := signer.RequestSignatureCallCount(); n != 0 {
		t.Errorf("expected relayd's slot key never to sign a refund, got %d slot signing requests", n)
	}

	// Tick 3: the execution check confirms it.
	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 3: %v", err)
	}
	leg, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if leg.Status != relay.StatusRefunded {
		t.Fatalf("expected REFUNDED, got %s", leg.Status)
	}
	if got := ledger.AccountBalance("liability:customer:cust-bep20-refund-1:USDT_BEP20"); got != 0 {
		t.Errorf("expected customer liability to close to 0, got %d", got)
	}
}
