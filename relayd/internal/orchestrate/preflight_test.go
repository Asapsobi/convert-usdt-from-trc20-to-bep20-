package orchestrate

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	tronaddress "github.com/fbsobreira/gotron-sdk/pkg/address"

	"relayd/internal/alert"
	"relayd/internal/evmtx"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
)

const (
	vendorBSCDeposit = "0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf"
	customerTRON     = "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj"
	depositTRON      = "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH"
)

type recordingAlerter struct {
	mu     sync.Mutex
	alerts []alert.Alert
}

func (r *recordingAlerter) Fire(ctx context.Context, a alert.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
	return nil
}

func (r *recordingAlerter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.alerts)
}

func bep20(units int64) money.Amount { return money.Amount{Asset: money.USDT_BEP20, Units: units} }
func trc20(units int64) money.Amount { return money.Amount{Asset: money.USDT_TRC20, Units: units} }

func signEVM(t *testing.T, key *ecdsa.PrivateKey, tx *types.Transaction) *types.Transaction {
	t.Helper()
	digest := types.NewEIP155Signer(big.NewInt(evmtx.BSCChainID)).Hash(tx)
	sigBytes, err := crypto.Sign(digest[:], key)
	if err != nil {
		t.Fatalf("crypto.Sign: %v", err)
	}
	var sig [65]byte
	copy(sig[:], sigBytes)
	signed, err := evmtx.WithSignature(tx, sig)
	if err != nil {
		t.Fatalf("WithSignature: %v", err)
	}
	return signed
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

func buildEVM(t *testing.T, recipient string, amount money.Amount) *types.Transaction {
	t.Helper()
	tx, _, err := evmtx.BuildTransfer(recipient, amount, evmtx.TxParams{Nonce: 1, GasPrice: big.NewInt(50_000_000)})
	if err != nil {
		t.Fatalf("BuildTransfer: %v", err)
	}
	return tx
}

func TestCheckEVMTransfer_AcceptsAMatchingTransfer(t *testing.T) {
	key := mustKey(t)
	signed := signEVM(t, key, buildEVM(t, vendorBSCDeposit, bep20(1_995000)))
	intent := transferIntent{sender: crypto.PubkeyToAddress(key.PublicKey).Hex(), recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	if err := checkEVMTransfer(signed, intent); err != nil {
		t.Fatalf("expected a matching transfer to pass, got %v", err)
	}
}

// The real 2026-09-23 incident: the transfer encoded 1_995000 raw units
// (money.Amount's 6-decimal Units) instead of 1.995e18, moving 1e-12 of
// the intended amount. Rebuilt here by hand, since evmtx now scales.
func TestCheckEVMTransfer_CatchesAnUnscaledAmount(t *testing.T) {
	key := mustKey(t)
	good := buildEVM(t, vendorBSCDeposit, bep20(1_995000))
	data := append([]byte(nil), good.Data()...)
	unscaled := new(big.Int).SetInt64(1_995000).Bytes()
	for i := 4 + 32; i < 4+64; i++ {
		data[i] = 0
	}
	copy(data[4+64-len(unscaled):], unscaled)
	bad := types.NewTransaction(good.Nonce(), *good.To(), big.NewInt(0), good.Gas(), good.GasPrice(), data)

	intent := transferIntent{sender: crypto.PubkeyToAddress(key.PublicKey).Hex(), recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	err := checkEVMTransfer(signEVM(t, key, bad), intent)
	if err == nil || !strings.Contains(err.Error(), "raw units") {
		t.Fatalf("expected an amount mismatch, got %v", err)
	}
}

// The 2026-09-23 wrong-XPUB incident: the address holding the funds and
// the key S1 signed with didn't match. On BSC that silently sends from
// a different address.
func TestCheckEVMTransfer_CatchesTheWrongSigningKey(t *testing.T) {
	holder, signer := mustKey(t), mustKey(t)
	signed := signEVM(t, signer, buildEVM(t, vendorBSCDeposit, bep20(1_995000)))
	intent := transferIntent{sender: crypto.PubkeyToAddress(holder.PublicKey).Hex(), recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	err := checkEVMTransfer(signed, intent)
	if err == nil || !strings.Contains(err.Error(), "signed by") {
		t.Fatalf("expected a signer mismatch, got %v", err)
	}
}

func TestCheckEVMTransfer_CatchesTheWrongRecipient(t *testing.T) {
	key := mustKey(t)
	signed := signEVM(t, key, buildEVM(t, "0x000000000000000000000000000000000000dEaD", bep20(1_995000)))
	intent := transferIntent{sender: crypto.PubkeyToAddress(key.PublicKey).Hex(), recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	if err := checkEVMTransfer(signed, intent); err == nil || !strings.Contains(err.Error(), "pays") {
		t.Fatalf("expected a recipient mismatch, got %v", err)
	}
}

func buildTRON(t *testing.T, from, to string, amount money.Amount) []byte {
	t.Helper()
	ref := txbuild.BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1788859176552), Expiration: time.UnixMilli(1788859233000)}
	unsigned, err := txbuild.BuildTransfer(from, to, amount, ref)
	if err != nil {
		t.Fatalf("txbuild.BuildTransfer: %v", err)
	}
	return unsigned
}

func TestCheckTRONTransfer_AcceptsAMatchingTransferAndCatchesMismatches(t *testing.T) {
	unsigned := buildTRON(t, depositTRON, customerTRON, trc20(1_630000))
	if err := checkTRONTransfer(unsigned, transferIntent{sender: depositTRON, recipient: customerTRON, amount: trc20(1_630000)}); err != nil {
		t.Fatalf("expected a matching transfer to pass, got %v", err)
	}
	for name, intent := range map[string]transferIntent{
		"sender":    {sender: customerTRON, recipient: customerTRON, amount: trc20(1_630000)},
		"recipient": {sender: depositTRON, recipient: depositTRON, amount: trc20(1_630000)},
		"amount":    {sender: depositTRON, recipient: customerTRON, amount: trc20(1_000000)},
	} {
		if err := checkTRONTransfer(unsigned, intent); err == nil {
			t.Errorf("wrong %s: expected a mismatch, got nil", name)
		}
	}
}

func TestCheckTRONSigner_AcceptsTheHoldingKeyAndCatchesAnyOther(t *testing.T) {
	holder, other := mustKey(t), mustKey(t)
	sender := tronaddress.PubkeyToAddress(holder.PublicKey).String()
	digest := txbuild.Digest(buildTRON(t, sender, customerTRON, trc20(1_630000)))

	sign := func(key *ecdsa.PrivateKey) [65]byte {
		raw, err := crypto.Sign(digest[:], key)
		if err != nil {
			t.Fatalf("crypto.Sign: %v", err)
		}
		var sig [65]byte
		copy(sig[:], raw)
		return sig
	}

	if err := checkTRONSigner(digest, sign(holder), sender); err != nil {
		t.Fatalf("expected the holding key's signature to pass, got %v", err)
	}
	withLegacyV := sign(holder)
	withLegacyV[64] += 27
	if err := checkTRONSigner(digest, withLegacyV, sender); err != nil {
		t.Fatalf("expected a 27/28-style recovery byte to pass too, got %v", err)
	}
	if err := checkTRONSigner(digest, sign(other), sender); err == nil || !strings.Contains(err.Error(), "signed by") {
		t.Fatalf("expected a signature from another key to be caught, got %v", err)
	}
}

// vendorFixture creates a real MockProvider order and a leg pointing at
// it, matching what startOne records in production.
func vendorFixture(t *testing.T, sent money.Amount) (*Orchestrator, *upstream.MockProvider, relay.Leg, *recordingAlerter) {
	t.Helper()
	mock := upstream.NewMockProvider("preflight", 1)
	mock.ForceDepositAddress(vendorBSCDeposit)
	vendorOrder, err := mock.CreateOrder(context.Background(), upstream.Pair{From: money.USDT_BEP20, To: money.USDT_TRC20}, sent, customerTRON)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	alerter := &recordingAlerter{}
	o := &Orchestrator{Upstream: mock, Alert: alerter, preflightAlerted: make(map[string]string)}
	leg := relay.Leg{
		ExternalID: "relay-preflight-test", DestinationAddress: customerTRON,
		UpstreamOrderID: &vendorOrder.ProviderOrderID, UpstreamDepositAddress: &vendorOrder.DepositAddress,
	}
	return o, mock, leg, alerter
}

func TestCheckVendorOrder_PassesWhenTheVendorExpectsExactlyThisDeposit(t *testing.T) {
	o, _, leg, alerter := vendorFixture(t, bep20(1_995000))
	intent := transferIntent{recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	if err := o.checkVendorOrder(context.Background(), leg, intent); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
	if alerter.count() != 0 {
		t.Fatalf("expected no alerts, got %d", alerter.count())
	}
}

// The real 2026-09-23 fee incident: the vendor order was created for the
// full 2.00 deposit while only 1.995 was ever going to be sent.
func TestCheckVendorOrder_RefusesWhenTheVendorExpectsADifferentAmount(t *testing.T) {
	o, _, leg, _ := vendorFixture(t, bep20(2_000000))
	intent := transferIntent{recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	err := o.checkVendorOrder(context.Background(), leg, intent)
	if !errors.Is(err, ErrPreflightFailed) || !strings.Contains(err.Error(), "vendor expects 2") {
		t.Fatalf("expected an amount refusal, got %v", err)
	}
}

func TestCheckVendorOrder_RefusesAnExpiredOrderAndAlertsOnlyOnce(t *testing.T) {
	o, mock, leg, alerter := vendorFixture(t, bep20(1_995000))
	if err := mock.SetOrderStatus(*leg.UpstreamOrderID, upstream.StatusExpired, nil); err != nil {
		t.Fatalf("SetOrderStatus: %v", err)
	}
	intent := transferIntent{recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	for i := 0; i < 3; i++ {
		if err := o.checkVendorOrder(context.Background(), leg, intent); !errors.Is(err, ErrPreflightFailed) {
			t.Fatalf("tick %d: expected a refusal, got %v", i, err)
		}
	}
	if alerter.count() != 1 {
		t.Fatalf("expected exactly 1 alert across 3 ticks of the same failure, got %d", alerter.count())
	}
}

func TestCheckVendorOrder_RefusesWhenTheVendorWouldPaySomeoneElse(t *testing.T) {
	o, _, leg, _ := vendorFixture(t, bep20(1_995000))
	leg.DestinationAddress = depositTRON // not the address the vendor order pays out to
	intent := transferIntent{recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	if err := o.checkVendorOrder(context.Background(), leg, intent); !errors.Is(err, ErrPreflightFailed) {
		t.Fatalf("expected a payout-address refusal, got %v", err)
	}
}

// A vendor outage is not a reason to alert -- or to send without
// confirming. The caller just retries next tick.
func TestCheckVendorOrder_TreatsALookupFailureAsTransient(t *testing.T) {
	o, mock, leg, alerter := vendorFixture(t, bep20(1_995000))
	mock.ForceGetOrderError(errors.New("vendor unreachable"))
	intent := transferIntent{recipient: vendorBSCDeposit, amount: bep20(1_995000)}
	err := o.checkVendorOrder(context.Background(), leg, intent)
	if err == nil || errors.Is(err, ErrPreflightFailed) {
		t.Fatalf("expected a plain transient error, got %v", err)
	}
	if alerter.count() != 0 {
		t.Fatalf("expected no alert for a transient lookup failure, got %d", alerter.count())
	}
}

func TestSameAddress_ComparesEVMHexCaseInsensitivelyAndTRONExactly(t *testing.T) {
	if !sameAddress(strings.ToLower(vendorBSCDeposit), common.HexToAddress(vendorBSCDeposit).Hex()) {
		t.Error("expected EVM addresses differing only in case to match")
	}
	if sameAddress(depositTRON, strings.ToLower(depositTRON)) {
		t.Error("expected TRON base58 addresses to compare exactly")
	}
}
