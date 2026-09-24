//go:build integration

// The real-signer-correctness E2E gate for the BEP20 forward-leg sweep
// signing fix. Every OTHER test in this package proves the leg reaches
// FORWARDED; none of them recover the real signer from the broadcast
// transaction and check it against the leg's own actual deposit
// address -- exactly the property that was silently wrong before this
// fix (relayd's own shared slot key signing a transaction that claimed
// to move funds out of a customer's own unique, unrelated address).
//
// Four real processes/databases: a real ledgerd (testledger, already
// used by every sibling test in this package), a real depositwatcher
// watcherd in HTTP-boundary-only mode (testdepositwatcher -- real
// address derivation, no fake standing in for it), a real S1 (tests1 --
// real BIP32 CKDpriv + real ECDSA signing, S1_KMS_CLIENT=fake but that
// only affects custody, never the cryptography itself), and relayd's
// own Postgres. The one thing still faked is the BSC chain node itself
// (capturingEVMChain, below) -- broadcasting to a fake, capturing what
// was asked to be sent, so the test can recover and check the real
// signer without needing a real BSC node.
package orchestrate_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/tyler-smith/go-bip32"

	"relayd/internal/alert"
	"relayd/internal/driver"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/testdepositwatcher"
	"relayd/internal/tests1"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

// capturingEVMChain is a minimal fake BSC node that only ever records
// the final SIGNED transaction it's asked to broadcast, keyed by its own
// hash -- deliberately distinct from this package's own shared
// fakeEVMChain (which only counts broadcasts), since this test's whole
// point is to inspect the real signature on that captured transaction
// afterward.
//
// Keyed rather than a single shared "last broadcast" field, mirroring
// capturingTRONChain's own identical fix (signing_trc20_e2e_integration_test.go)
// for the same reason: every integration test in this package shares
// ONE un-truncated database across a whole `go test -tags integration`
// invocation, and Orchestrator.RunTick scans every FORWARDING/
// REFUND_PENDING leg in that shared database on every tick, not just
// the one leg a given test created -- so a different test's own
// leftover leg can legitimately get broadcast through THIS test's own
// chain fake within the same RunTick call this test is polling, and a
// single shared field would let that unrelated broadcast silently
// overwrite what this test itself was waiting to inspect. Keying by
// hash -- the same identifier that becomes a leg's own ForwardTxID once
// MarkForwarded runs -- means an unrelated leg's broadcast is simply
// recorded under its own, different key.
type capturingEVMChain struct {
	mu     sync.Mutex
	nonce  uint64
	byHash map[string]*gethtypes.Transaction
}

func newCapturingEVMChain() *capturingEVMChain {
	return &capturingEVMChain{byHash: make(map[string]*gethtypes.Transaction)}
}

func (c *capturingEVMChain) CurrentNonce(ctx context.Context, address string) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonce, nil
}

func (c *capturingEVMChain) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return big.NewInt(3_000_000_000), nil
}

func (c *capturingEVMChain) Broadcast(ctx context.Context, signed *gethtypes.Transaction) (string, error) {
	hash := signed.Hash().Hex()
	c.mu.Lock()
	c.byHash[hash] = signed
	c.nonce++
	c.mu.Unlock()
	return hash, nil
}

// CapturedFor returns the signed transaction this fake recorded for
// hash (a leg's own ForwardTxID, once FORWARDED), and whether one was
// found.
func (c *capturingEVMChain) CapturedFor(hash string) (*gethtypes.Transaction, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx, ok := c.byHash[hash]
	return tx, ok
}

func startTestDepositWatcher(t *testing.T, xpub string) *testdepositwatcher.Watcher {
	t.Helper()
	dbURL := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	return testdepositwatcher.Start(t, dbURL, fmt.Sprintf(":%d", 19600+port), xpub, "e2e-watcher-token", "relayd")
}

func startTestS1(t *testing.T, xprv, xpub string) *tests1.Signer {
	t.Helper()
	dbURL := os.Getenv("S1_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("S1_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	return tests1.Start(t, dbURL, fmt.Sprintf(":%d", 19700+port), "e2e-c5-token", "e2e-approver-token", tests1.Config{
		BSCDepositXPRV: xprv, BSCDepositXPUB: xpub, ApprovalThresholdUSD: 1_000_000,
	})
}

// TestForwardBEP20_SignsFromLegsOwnRealDepositAddress is the critical
// regression test for the fix: a relay leg's own real, uniquely-derived
// BSC deposit address (from a real depositwatcher, via the real
// driver.CreateRelayLeg path -- never a hand-built fixture address) must
// be exactly who the real, recovered on-chain signer is once relayd
// forwards that leg -- never relayd's own shared slot address.
func TestForwardBEP20_SignsFromLegsOwnRealDepositAddress(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	// One real BIP32 master key, shared between watcherd's own
	// WATCHER_XPUB and s1d's own S1_BSC_DEPOSIT_XPRV/XPUB -- the two
	// real, independently-running processes must be cryptographically
	// the same node for a signature requested by index to actually
	// authorize spending from the address that same index derives to.
	master, err := bip32.NewMasterKey([]byte("relayd forward-bep20 signing e2e -- throwaway, never use for real funds"))
	if err != nil {
		t.Fatalf("generating throwaway master key: %v", err)
	}
	xprv := master.B58Serialize()
	xpub := master.PublicKey().B58Serialize()

	watcher := startTestDepositWatcher(t, xpub)
	signer := startTestS1(t, xprv, xpub)

	externalID := uniqueExternalID(t)

	// Real order creation + real address assignment, through the real
	// driver.CreateRelayLeg path -- this is what actually calls
	// watcherd's own POST /v1/addresses and records the REAL derivation
	// index onto the leg, exactly as production relayd does.
	// CreateRelayLeg itself creates the C1 order (d.Ledger.CreateOrder),
	// so this is NOT preceded by a separate ledger.CreateRelayOrder* call
	// -- that would race a second, conflicting CreateOrder for the same
	// external_id.
	mockProvider := upstream.NewMockProvider("mock-e2e-sign", 1)
	mockProvider.ForceDepositAddress("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf")

	d := &driver.Driver{
		Ledger: client, Upstream: mockProvider,
		BEP20Watcher: watcherclient.New(watcher.BaseURL(), watcher.Token()),
		Store:        store,
		Cfg:          driver.Config{FeeBasisPoints: 30, QuoteValidity: 10 * time.Minute},
	}

	if _, err := d.CreateRelayLeg(context.Background(), driver.CreateRelayLegRequest{
		ExternalID: externalID, CustomerID: "cust-e2e-sign", Direction: relay.BEP20ToTRC20,
		DestinationAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", AmountIn: "100.000000",
	}); err != nil {
		t.Fatalf("CreateRelayLeg: %v", err)
	}

	leg, err := store.GetByExternalID(context.Background(), externalID)
	if err != nil {
		t.Fatalf("GetByExternalID: %v", err)
	}
	if leg.DepositDerivationIndex == nil {
		t.Fatal("expected a real derivation index recorded on the leg, got nil")
	}
	depositAddr := common.HexToAddress(leg.DepositAddress)
	t.Logf("leg's own real deposit address: %s (derivation index %d)", leg.DepositAddress, *leg.DepositDerivationIndex)

	// Advance the real C1 order (already created by CreateRelayLeg above)
	// to screened -- same fixture step every sibling happy-path test in
	// this package uses, driven off a fresh GetOrder for its own current
	// Version.
	createdOrder := ledger.GetOrder(externalID)
	screened := ledger.AdvanceToScreenedBEP20ToTRC20(createdOrder)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	evmChain := newCapturingEVMChain()
	watcherClientForOrchestrator := watcherclient.New(watcher.BaseURL(), watcher.Token())
	signingClient := signing.New(signer.BaseURL(), signer.Token())

	orch := orchestrate.New(store, client, mockProvider, newFakeEnergy(), signingClient, &fakeChain{}, newFakeFinality(),
		evmChain, newFakeEVMFinality(), alert.LogAlerter{}, watcherClientForOrchestrator, nil,
		orchestrate.Config{
			SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", SlotEVMAddress: "0x1111111111111111111111111111111111abcd",
			EnergyPerTransferUnits: 65000,
		})

	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := orch.RunTick(ctx); err != nil {
			t.Fatalf("RunTick: %v", err)
		}
		current, err := store.GetByExternalID(ctx, externalID)
		if err != nil {
			t.Fatalf("GetByExternalID: %v", err)
		}
		if current.Status == relay.StatusForwarded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leg never reached FORWARDED (last status: %s)", current.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}

	afterForward, err := store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatalf("GetByExternalID: %v", err)
	}
	if afterForward.ForwardTxID == nil || *afterForward.ForwardTxID == "" {
		t.Fatal("expected a forward_tx_id to be recorded")
	}
	// Looked up by THIS leg's own forward_tx_id, not a single shared
	// "last broadcast" field -- see capturingEVMChain's own doc comment
	// for why that matters when the full suite runs together.
	capturedTx, ok := evmChain.CapturedFor(*afterForward.ForwardTxID)
	if !ok {
		t.Fatal("expected a captured broadcast transaction for this leg's own forward_tx_id, got none")
	}

	// The critical assertion: the real, recovered on-chain signer must
	// be this leg's own real deposit address -- never the slot.
	recoveredSigner, err := gethtypes.NewEIP155Signer(big.NewInt(56)).Sender(capturedTx)
	if err != nil {
		t.Fatalf("recovering signer from the broadcast transaction: %v", err)
	}
	if recoveredSigner != depositAddr {
		t.Fatalf("recovered signer = %s, want the leg's own real deposit address %s", recoveredSigner.Hex(), depositAddr.Hex())
	}
	// Negative control: proves this assertion actually distinguishes
	// the two, rather than the slot happening to coincide.
	slotAddr := common.HexToAddress("0x1111111111111111111111111111111111abcd")
	if recoveredSigner == slotAddr {
		t.Fatal("recovered signer must not be the slot's own EVM address")
	}

	// Defense-in-depth cross-check exercised for real: re-fetch from the
	// real watcherd and confirm it still agrees with what was used.
	reconfirmed, err := watcherClientForOrchestrator.GetAddress(ctx, leg.OrderID)
	if err != nil {
		t.Fatalf("re-fetching address from watcherd: %v", err)
	}
	if reconfirmed.Address != leg.DepositAddress || reconfirmed.DerivationIndex == nil || *reconfirmed.DerivationIndex != *leg.DepositDerivationIndex {
		t.Fatalf("watcherd's own live record disagrees with what was used: got address=%s index=%v, want address=%s index=%d",
			reconfirmed.Address, reconfirmed.DerivationIndex, leg.DepositAddress, *leg.DepositDerivationIndex)
	}

	// Drive this leg to a terminal status (SETTLED) before returning --
	// this package's own integration tests share ONE Postgres database
	// across the whole test binary run (Makefile's own -p 1 requirement),
	// so a leg left sitting in FORWARDED would still be picked up by
	// every LATER test's own orchestrator (ListByStatus has no per-test
	// scoping), exactly as every sibling happy-path test in this package
	// already takes care to avoid.
	afterForward, err = store.GetByExternalID(ctx, externalID)
	if err != nil {
		t.Fatalf("GetByExternalID: %v", err)
	}
	if afterForward.UpstreamOrderID == nil {
		t.Fatal("expected an upstream_order_id to be recorded")
	}
	if err := mockProvider.SetOrderStatus(*afterForward.UpstreamOrderID, upstream.StatusComplete,
		ptrAmount(money.Amount{Asset: money.USDT_TRC20, Units: 99_650000})); err != nil {
		t.Fatalf("SetOrderStatus: %v", err)
	}
	settleDeadline := time.Now().Add(10 * time.Second)
	for {
		if err := orch.RunTick(ctx); err != nil {
			t.Fatalf("RunTick (settling): %v", err)
		}
		current, err := store.GetByExternalID(ctx, externalID)
		if err != nil {
			t.Fatalf("GetByExternalID: %v", err)
		}
		if current.Status == relay.StatusSettled {
			break
		}
		if time.Now().After(settleDeadline) {
			t.Fatalf("leg never reached SETTLED (last status: %s)", current.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
