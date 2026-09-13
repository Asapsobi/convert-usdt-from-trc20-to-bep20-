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
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gethtypes "github.com/ethereum/go-ethereum/core/types"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"relayd/internal/db"
	"relayd/internal/energy"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/testledger"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
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
// dispatcher/internal/orchestrate's own identical fakeEnergy.
type fakeEnergy struct {
	mu    sync.Mutex
	calls map[string]int
}

func newFakeEnergy() *fakeEnergy { return &fakeEnergy{calls: make(map[string]int)} }

func (f *fakeEnergy) Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (energy.Reservation, error) {
	f.mu.Lock()
	f.calls[externalID]++
	f.mu.Unlock()
	return energy.Reservation{Status: "CONFIRMED", EnergyUnits: units}, nil
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
type fakeFinality struct {
	mu    sync.Mutex
	final map[string]bool
}

func newFakeFinality() *fakeFinality { return &fakeFinality{final: make(map[string]bool)} }

func (f *fakeFinality) MarkFinal(txID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.final[txID] = true
}

func (f *fakeFinality) IsFinal(ctx context.Context, txID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.final[txID], nil
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
	f.mu.Unlock()
	return signed.Hash().Hex(), nil
}

// fakeEVMFinality mirrors fakeFinality above for the BEP20 direction.
type fakeEVMFinality struct {
	mu    sync.Mutex
	final map[string]bool
}

func newFakeEVMFinality() *fakeEVMFinality { return &fakeEVMFinality{final: make(map[string]bool)} }

func (f *fakeEVMFinality) MarkFinal(txHash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.final[txHash] = true
}

func (f *fakeEVMFinality) IsFinal(ctx context.Context, txHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.final[txHash], nil
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

	leg, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: "cust-happy-1", DestinationAddress: "0xcustomer-bep20-address",
		DepositAddress:    "Trelayd-fixture-deposit-address",
		AmountIn:          money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOutExpected: money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
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

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		SlotEVMAddress:         "0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf",
		EnergyPerTransferUnits: 65000,
	})

	ctx := context.Background()

	// Tick 1: RunTick's own phases feed into each other within one call
	// (phase 2 scans every currently-FORWARDING leg, including one phase
	// 1 just started this same tick), so a single tick can legitimately
	// carry this leg all the way from AWAITING_DEPOSIT to FORWARDED --
	// discover the screened order, start forwarding (create the upstream
	// order, post relay_forward_start), then immediately reserve energy,
	// sign, and broadcast. Asserted as one step rather than pinned to an
	// exact tick, since that inter-phase efficiency is a real, correct
	// property of RunTick, not something a test should fight.
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

	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.Status != relay.StatusForwarded {
		t.Fatalf("after tick 1: expected FORWARDED, got %s", afterTick2.Status)
	}
	if afterTick2.ForwardTxID == nil || *afterTick2.ForwardTxID == "" {
		t.Fatal("expected a forward_tx_id to be recorded")
	}
	if chain.broadcasts != 1 {
		t.Errorf("expected exactly 1 broadcast, got %d", chain.broadcasts)
	}
	if signer.RequestSignatureCallCount() != 1 {
		t.Errorf("expected exactly 1 signature request, got %d", signer.RequestSignatureCallCount())
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

	leg, err := store.Create(context.Background(), relay.Leg{
		ExternalID: externalID, OrderID: order.ID, Direction: relay.BEP20ToTRC20,
		CustomerID: "cust-happy-2", DestinationAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		DepositAddress:    "0xrelayd-fixture-deposit-address",
		AmountIn:          money.Amount{Asset: money.USDT_BEP20, Units: 100_000000},
		AmountOutExpected: money.Amount{Asset: money.USDT_TRC20, Units: 99_700000},
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

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signer, chain, finality, evmChain, evmFinality, orchestrate.Config{
		SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		SlotEVMAddress:         "0x1111111111111111111111111111111111abcd",
		EnergyPerTransferUnits: 65000,
	})

	ctx := context.Background()

	// Tick 1: same inter-phase efficiency as the TRC20 direction's own
	// test -- can carry this leg from AWAITING_DEPOSIT all the way to
	// FORWARDED in one tick.
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
	if afterTick1.Status != relay.StatusForwarded {
		t.Fatalf("after tick 1: expected FORWARDED, got %s", afterTick1.Status)
	}
	if afterTick1.ForwardTxID == nil || *afterTick1.ForwardTxID == "" {
		t.Fatal("expected a forward_tx_id to be recorded")
	}
	if evmChain.broadcasts != 1 {
		t.Errorf("expected exactly 1 EVM broadcast, got %d", evmChain.broadcasts)
	}
	if chain.broadcasts != 0 {
		t.Errorf("expected 0 TRC20 broadcasts for a BEP20-direction leg, got %d", chain.broadcasts)
	}
	if signer.RequestSignatureCallCount() != 1 {
		t.Errorf("expected exactly 1 signature request, got %d", signer.RequestSignatureCallCount())
	}

	if err := mockProvider.SetOrderStatus(*afterTick1.UpstreamOrderID, upstream.StatusComplete, ptrAmount(money.Amount{Asset: money.USDT_TRC20, Units: 99_650000})); err != nil {
		t.Fatalf("SetOrderStatus: %v", err)
	}

	if err := orch.RunTick(ctx); err != nil {
		t.Fatalf("RunTick 2: %v", err)
	}
	afterTick2, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick2.Status != relay.StatusSettled {
		t.Fatalf("after tick 2: expected SETTLED, got %s", afterTick2.Status)
	}
	if afterTick2.AmountOutActual == nil || afterTick2.AmountOutActual.Units != 99_650000 {
		t.Errorf("expected amount_out_actual 99650000, got %v", afterTick2.AmountOutActual)
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
		t.Fatalf("RunTick 3 (idempotent replay): %v", err)
	}
	if evmChain.broadcasts != 1 {
		t.Errorf("expected still exactly 1 EVM broadcast after a 3rd tick, got %d", evmChain.broadcasts)
	}
}

func ptrAmount(a money.Amount) *money.Amount { return &a }
