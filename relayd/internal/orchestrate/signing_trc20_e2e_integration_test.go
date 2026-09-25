//go:build integration

// The real-signer-correctness E2E gate for the TRC20 forward-leg sweep
// signing fix -- signing_e2e_integration_test.go's own TRC20-direction
// sibling. Every OTHER test in this package proves the leg reaches
// FORWARDED; none of them recover the real signer from the broadcast
// transaction and check it against the leg's own actual deposit
// address -- exactly the property that was silently wrong before this
// fix (relayd's own shared slot key signing a transaction that claimed
// to move funds out of a customer's own unique, unrelated TRON address).
//
// TRON adds a property EVM never had, and this test proves BOTH halves
// of it: unlike an EVM transaction (where the sender is purely implied
// by whoever signs, never named in the transaction itself), TRON bakes
// the sender address directly into the signed bytes
// (TriggerSmartContract.OwnerAddress, see internal/txbuild's own
// BuildTransfer) -- so this test checks both that OwnerAddress itself is
// the leg's own real deposit address, AND that the real, independently
// recovered signer of the broadcast signature is too. A bug that set the
// right OwnerAddress but signed with the wrong key (or vice versa) would
// be caught here even though a real TRON node's own OwnerAddress-vs-
// signature check would also reject it outright -- this test doesn't
// rely on that on-chain check, since nothing here talks to a real chain.
//
// Four real processes/databases: a real ledgerd (testledger, already
// used by every sibling test in this package), a real tronwatcher
// tronwatcherd in HTTP-boundary-only mode (testtronwatcher -- real
// address derivation, no fake standing in for it), a real S1 (tests1 --
// real BIP32 CKDpriv + real ECDSA signing, S1_KMS_CLIENT=fake but that
// only affects custody, never the cryptography itself), and relayd's own
// Postgres. The one thing still faked is the TRON chain node itself
// (capturingTRONChain, below) -- broadcasting to a fake, capturing what
// was asked to be sent, so the test can recover and check the real
// signer without needing a real TRON node.
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

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/tyler-smith/go-bip32"
	"google.golang.org/protobuf/proto"

	"relayd/internal/alert"
	"relayd/internal/driver"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/tests1"
	"relayd/internal/testtronwatcher"
	"relayd/internal/tronbroadcast"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

// capturedTRONBroadcast is one broadcast capturingTRONChain recorded --
// see that type's own doc comment for why these are kept per-txid rather
// than in a single shared "last broadcast" field.
type capturedTRONBroadcast struct {
	unsignedTx []byte
	sig        [65]byte
}

// capturingTRONChain is a minimal fake TRON node that only ever records
// the unsigned bytes and signature it's asked to broadcast, keyed by the
// txid it itself assigns -- deliberately distinct from this package's
// own shared fakeChain (which only counts broadcasts), since this
// test's whole point is to inspect both the signed bytes' own baked-in
// OwnerAddress and the real signature afterward.
//
// Keyed rather than a single shared field: every integration test in
// this package shares ONE un-truncated Postgres database across a whole
// `go test -tags integration` invocation (Makefile's own -p 1
// requirement), and Orchestrator.RunTick scans every FORWARDING/
// REFUND_PENDING leg in that shared database on every tick -- not just
// the one leg a given test created. That means a DIFFERENT test's own
// leftover leg (e.g. a refund whose broadcast correctly uses the slot
// address as sender, not a leg's own deposit address) can legitimately
// get broadcast through THIS test's own chain fake within the same
// RunTick call this test is polling. A single "last broadcast" field
// meant last-write-wins, so that unrelated leg's broadcast could
// silently overwrite what this test itself was waiting to inspect --
// causing a spurious OwnerAddress mismatch that only ever reproduced
// running the full suite together, never with -run in isolation (the
// exact flakiness this type was rewritten to fix). Keying by txid --
// the same identifier that becomes a leg's own ForwardTxID once
// MarkForwarded runs -- means an unrelated leg's broadcast is simply
// recorded under its own, different key, never overwriting this leg's.
type capturingTRONChain struct {
	mu     sync.Mutex
	byTxID map[string]capturedTRONBroadcast
}

func newCapturingTRONChain() *capturingTRONChain {
	return &capturingTRONChain{byTxID: make(map[string]capturedTRONBroadcast)}
}

func (c *capturingTRONChain) CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error) {
	return txbuild.BlockReference{
		BlockNumber: 1,
		BlockHash:   [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Timestamp:   time.Now().UTC(),
		Expiration:  time.Now().UTC().Add(2 * time.Minute),
	}, nil
}

func (c *capturingTRONChain) BroadcastSigned(ctx context.Context, unsignedTx []byte, signature [65]byte) (string, error) {
	digest := txbuild.Digest(unsignedTx)
	txID := fmt.Sprintf("%x", digest[:])
	c.mu.Lock()
	c.byTxID[txID] = capturedTRONBroadcast{unsignedTx: unsignedTx, sig: signature}
	c.mu.Unlock()
	return txID, nil
}

// TokenBalance reports plenty of USDT for every holder -- balance
// shortfalls are exercised by tests that configure them explicitly.
func (c *capturingTRONChain) TokenBalance(ctx context.Context, holder string) (*big.Int, error) {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil), nil
}

// AccountResources reports an activated account with plenty of energy,
// bandwidth, and TRX -- resource shortfalls are exercised by tests that
// configure them explicitly.
func (c *capturingTRONChain) AccountResources(ctx context.Context, holder string) (tronbroadcast.Resources, error) {
	return tronbroadcast.Resources{Exists: true, Energy: 1_000_000_000, Bandwidth: 1_000_000, BalanceSun: 1_000_000_000}, nil
}

func (c *capturingTRONChain) EstimateTransferEnergy(ctx context.Context, from, to string, raw *big.Int) (int64, error) {
	return 64_285, nil
}

// CapturedFor returns the broadcast this fake recorded for txID (a
// leg's own ForwardTxID, once FORWARDED), and whether one was found.
func (c *capturingTRONChain) CapturedFor(txID string) (capturedTRONBroadcast, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cb, ok := c.byTxID[txID]
	return cb, ok
}

func (c *capturingTRONChain) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	return true, nil
}

// CheckExecution always reports success -- this test's own subject is
// real Privy-backed signing correctness, not on-chain execution-failure
// handling (fakeFinality.MarkFailed, in orchestrate_integration_test.go,
// is the opt-in for that outcome, but as of this writing no test in this
// package actually exercises it against a real broadcast path).
func (c *capturingTRONChain) CheckExecution(ctx context.Context, tronTxID string) (final, success bool, failureReason string, err error) {
	return true, true, "", nil
}

func startTestTronWatcher(t *testing.T, xpub string) *testtronwatcher.Watcher {
	t.Helper()
	dbURL := os.Getenv("TRONWATCHER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TRONWATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	return testtronwatcher.Start(t, dbURL, fmt.Sprintf(":%d", 19800+port), xpub, "e2e-tronwatcher-token", "relayd")
}

func startTestS1WithTronDeposit(t *testing.T, xprv, xpub string) *tests1.Signer {
	t.Helper()
	dbURL := os.Getenv("S1_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("S1_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	return tests1.Start(t, dbURL, fmt.Sprintf(":%d", 19900+port), "e2e-tron-c5-token", "e2e-tron-approver-token", tests1.Config{
		TronDepositXPRV: xprv, TronDepositXPUB: xpub, ApprovalThresholdUSD: 1_000_000,
	})
}

// compactFromR65 rebuilds the 65-byte compact-signature format
// ecdsa.RecoverCompact expects from S1's own r||s||v SignedTx output --
// mirrors s1/internal/kmssign/wrapper.go's own
// compactSigRecoveryBase/compactSigCompressedFlag construction, test-side
// (the same helper s1/internal/requests/store_integration_test.go's own
// compactFromR65 already uses, duplicated here since these are separate
// Go modules).
func compactFromR65(sig [65]byte) []byte {
	const (
		compactSigRecoveryBase   = 27
		compactSigCompressedFlag = 4
	)
	compact := make([]byte, 65)
	compact[0] = compactSigRecoveryBase + sig[64] + compactSigCompressedFlag
	copy(compact[1:33], sig[0:32])
	copy(compact[33:65], sig[32:64])
	return compact
}

// TestForwardTRC20_SignsFromLegsOwnRealDepositAddress is
// TestForwardBEP20_SignsFromLegsOwnRealDepositAddress's own TRC20-direction
// mirror: a relay leg's own real, uniquely-derived TRON deposit address
// (from a real tronwatcher, via the real driver.CreateRelayLeg path --
// never a hand-built fixture address) must be exactly who the real,
// recovered on-chain signer is AND what the broadcast transaction's own
// baked-in OwnerAddress says, once relayd forwards that leg -- never
// relayd's own shared slot address.
func TestForwardTRC20_SignsFromLegsOwnRealDepositAddress(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	store := relay.NewStore(pool)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	// One real BIP32 master key, shared between tronwatcherd's own
	// TRONWATCHER_XPUB and s1d's own S1_TRON_DEPOSIT_XPRV/XPUB -- the two
	// real, independently-running processes must be cryptographically
	// the same node for a signature requested by index to actually
	// authorize spending from the address that same index derives to.
	master, err := bip32.NewMasterKey([]byte("relayd forward-trc20 signing e2e -- throwaway, never use for real funds"))
	if err != nil {
		t.Fatalf("generating throwaway master key: %v", err)
	}
	xprv := master.B58Serialize()
	xpub := master.PublicKey().B58Serialize()

	watcher := startTestTronWatcher(t, xpub)
	signer := startTestS1WithTronDeposit(t, xprv, xpub)

	externalID := uniqueExternalID(t)

	// Real order creation + real address assignment, through the real
	// driver.CreateRelayLeg path -- this is what actually calls
	// tronwatcherd's own POST /v1/addresses and records the REAL
	// derivation index onto the leg, exactly as production relayd does.
	mockProvider := upstream.NewMockProvider("mock-e2e-sign-trc20", 1)
	// A real, valid, live-verified TRON address (see internal/txbuild's
	// own tests) -- this is the upstream platform's own TRON deposit
	// address (the forward transfer's own recipient), distinct from the
	// customer's own leg deposit address below.
	mockProvider.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")

	d := &driver.Driver{
		Ledger: client, Upstream: mockProvider,
		TronWatcher: watcherclient.New(watcher.BaseURL(), watcher.Token()),
		Store:       store,
		Pricing:     e2ePricing(t, store),
		Cfg:         driver.Config{QuoteValidity: 10 * time.Minute, DepositWindow: 30 * time.Minute},
	}

	if _, err := d.CreateRelayLeg(context.Background(), driver.CreateRelayLegRequest{
		ExternalID: externalID, CustomerLabel: "cust-e2e-sign-trc20", Direction: relay.TRC20ToBEP20,
		DestinationAddress: "0x4192cC99D3CB95573DcAF8dd76921476e0C7bCaF", AmountIn: "100.000000",
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
	t.Logf("leg's own real deposit address: %s (derivation index %d)", leg.DepositAddress, *leg.DepositDerivationIndex)

	// Advance the real C1 order (already created by CreateRelayLeg above)
	// to screened -- same fixture step every sibling happy-path test in
	// this package uses, driven off a fresh GetOrder for its own current
	// Version.
	createdOrder := ledger.GetOrder(externalID)
	screened := ledger.AdvanceToScreened(createdOrder)
	if screened.State != "screened" {
		t.Fatalf("fixture setup: expected screened, got %s", screened.State)
	}

	chain := newCapturingTRONChain()
	watcherClientForOrchestrator := watcherclient.New(watcher.BaseURL(), watcher.Token())
	signingClient := signing.New(signer.BaseURL(), signer.Token())

	fakeEnergyClient := newFakeEnergy()
	orch := orchestrate.New(store, client, mockProvider, fakeEnergyClient, signingClient, chain, chain,
		&fakeEVMChain{}, newFakeEVMFinality(), alert.LogAlerter{}, nil, watcherClientForOrchestrator,
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
	// "last broadcast" field -- see capturingTRONChain's own doc comment
	// for why that matters when the full suite runs together.
	captured, ok := chain.CapturedFor(*afterForward.ForwardTxID)
	if !ok {
		t.Fatal("expected a captured broadcast transaction for this leg's own forward_tx_id, got none")
	}

	// Critical assertion, part 0: energy must be delegated to the leg's
	// own real deposit address -- the transaction's actual SENDER (per
	// the OwnerAddress assertion below), and the only address that needs
	// energy to execute the TRC20 transfer contract call. A real, live
	// run against actual CatFee/TRON infrastructure found this reserving
	// energy for the vendor's own deposit address instead, leaving the
	// real sender with no resources and no accepted broadcast ("account
	// does not exist"). fakeEnergy previously discarded targetAddress
	// entirely, which is exactly how that shipped with no test catching
	// it.
	// Energy for the forward is rented for the sender (the deposit
	// address) only when it is short -- see
	// TestResources_TRONRentsExactlyTheShortfallForTheSender.

	// Critical assertion, part 1: decode the broadcast transaction's own
	// baked-in OwnerAddress (proto-marshaled by txbuild.BuildTransfer's
	// fromAddress argument) and confirm it's the leg's own real deposit
	// address -- the field a real TRON node checks against the signature.
	var raw core.TransactionRaw
	if err := proto.Unmarshal(captured.unsignedTx, &raw); err != nil {
		t.Fatalf("unmarshaling captured unsigned tx: %v", err)
	}
	if len(raw.GetContract()) != 1 {
		t.Fatalf("expected exactly 1 contract in the captured tx, got %d", len(raw.GetContract()))
	}
	var trigger core.TriggerSmartContract
	if err := raw.GetContract()[0].GetParameter().UnmarshalTo(&trigger); err != nil {
		t.Fatalf("unmarshaling TriggerSmartContract: %v", err)
	}
	ownerAddr := address.BytesToAddress(trigger.GetOwnerAddress()).String()
	if ownerAddr != leg.DepositAddress {
		t.Fatalf("broadcast tx's own OwnerAddress = %s, want the leg's own real deposit address %s", ownerAddr, leg.DepositAddress)
	}

	// Critical assertion, part 2: independently recover the real signer
	// from the captured signature and confirm it's ALSO the leg's own
	// real deposit address -- proves the SIGNATURE is correct, not just
	// the OwnerAddress field (a bug could set the right OwnerAddress but
	// still sign with the wrong key; a real TRON node would reject that
	// outright, but nothing here talks to a real chain, so this check
	// does that job instead).
	digest := txbuild.Digest(captured.unsignedTx)
	recoveredPub, _, err := ecdsa.RecoverCompact(compactFromR65(captured.sig), digest[:])
	if err != nil {
		t.Fatalf("recovering public key from signature: %v", err)
	}
	// recoveredPub's type (*secp256k1.PublicKey) is a direct Go type
	// alias for gotron-sdk's own *btcec.PublicKey parameter type
	// (github.com/btcsuite/btcd/btcec/v2 defines "type PublicKey =
	// github.com/decred/dcrd/dcrec/secp256k1/v4.PublicKey") -- passed
	// straight through, no conversion needed.
	recoveredAddr := address.BTCECPubkeyToAddress(recoveredPub).String()
	if recoveredAddr != leg.DepositAddress {
		t.Fatalf("recovered signer's TRON address = %s, want the leg's own real deposit address %s", recoveredAddr, leg.DepositAddress)
	}

	// Negative control: proves both assertions above actually distinguish
	// the leg's own address from the slot's, rather than the slot
	// happening to coincide.
	slotAddr := "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH"
	if ownerAddr == slotAddr {
		t.Fatal("broadcast tx's own OwnerAddress must not be the slot's own TRON address")
	}
	if recoveredAddr == slotAddr {
		t.Fatal("recovered signer must not be the slot's own TRON address")
	}

	// Defense-in-depth cross-check exercised for real: re-fetch from the
	// real tronwatcherd and confirm it still agrees with what was used.
	reconfirmed, err := watcherClientForOrchestrator.GetAddress(ctx, leg.OrderID)
	if err != nil {
		t.Fatalf("re-fetching address from tronwatcherd: %v", err)
	}
	if reconfirmed.Address != leg.DepositAddress || reconfirmed.DerivationIndex == nil || *reconfirmed.DerivationIndex != *leg.DepositDerivationIndex {
		t.Fatalf("tronwatcherd's own live record disagrees with what was used: got address=%s index=%v, want address=%s index=%d",
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
		ptrAmount(money.Amount{Asset: money.USDT_BEP20, Units: 99_650000})); err != nil {
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
