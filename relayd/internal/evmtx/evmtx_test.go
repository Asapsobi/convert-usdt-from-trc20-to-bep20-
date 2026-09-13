package evmtx

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"relayd/internal/money"
)

func mustAmount(t *testing.T, units int64) money.Amount {
	t.Helper()
	return money.Amount{Asset: money.USDT_BEP20, Units: units}
}

func testParams() TxParams {
	return TxParams{Nonce: 5, GasPrice: big.NewInt(3_000_000_000), GasLimit: DefaultGasLimit}
}

func TestBuildTransfer_CalldataHasCorrectSelectorAndShape(t *testing.T) {
	tx, _, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 1_000000), testParams())
	if err != nil {
		t.Fatalf("BuildTransfer: %v", err)
	}
	data := tx.Data()
	if len(data) != 4+32+32 {
		t.Fatalf("expected calldata length 68, got %d", len(data))
	}
	wantSelector, _ := hex.DecodeString("a9059cbb")
	if !bytes.Equal(data[:4], wantSelector) {
		t.Fatalf("expected transfer(address,uint256) selector a9059cbb, got %x", data[:4])
	}
	// Recipient word: 12 zero bytes then the 20-byte address.
	for i := 4; i < 4+12; i++ {
		if data[i] != 0 {
			t.Fatalf("expected zero-padding before the recipient address, got %x", data[4:4+32])
		}
	}
}

func TestBuildTransfer_Deterministic(t *testing.T) {
	params := testParams()
	tx1, digest1, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 100_500000), params)
	if err != nil {
		t.Fatalf("BuildTransfer (1st): %v", err)
	}
	tx2, digest2, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 100_500000), params)
	if err != nil {
		t.Fatalf("BuildTransfer (2nd): %v", err)
	}
	if digest1 != digest2 {
		t.Fatalf("BuildTransfer digest is not deterministic: %x vs %x", digest1, digest2)
	}
	if tx1.Hash() != tx2.Hash() {
		t.Fatalf("BuildTransfer tx hash is not deterministic (unsigned): %s vs %s", tx1.Hash(), tx2.Hash())
	}
}

func TestBuildTransfer_DifferentAmountProducesDifferentDigest(t *testing.T) {
	params := testParams()
	_, digestA, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 1_000000), params)
	if err != nil {
		t.Fatal(err)
	}
	_, digestB, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 2_000000), params)
	if err != nil {
		t.Fatal(err)
	}
	if digestA == digestB {
		t.Fatal("expected different amounts to produce different signing digests")
	}
}

func TestBuildTransfer_RejectsMalformedRecipient(t *testing.T) {
	_, _, err := BuildTransfer("not-an-address", mustAmount(t, 1_000000), testParams())
	if !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("got %v, want ErrInvalidAddress", err)
	}
}

func TestBuildTransfer_RejectsNonPositiveAmount(t *testing.T) {
	for _, units := range []int64{0, -1} {
		_, _, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, units), testParams())
		if !errors.Is(err, ErrNonPositiveAmount) {
			t.Fatalf("units=%d: got %v, want ErrNonPositiveAmount", units, err)
		}
	}
}

func TestBuildTransfer_RejectsNilGasPrice(t *testing.T) {
	params := TxParams{Nonce: 1, GasLimit: DefaultGasLimit}
	_, _, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 1_000000), params)
	if !errors.Is(err, ErrNilGasPrice) {
		t.Fatalf("got %v, want ErrNilGasPrice", err)
	}
}

func TestBuildTransfer_DefaultGasLimitAppliedWhenZero(t *testing.T) {
	params := TxParams{Nonce: 1, GasPrice: big.NewInt(1)}
	tx, _, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 1_000000), params)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Gas() != DefaultGasLimit {
		t.Errorf("expected default gas limit %d, got %d", DefaultGasLimit, tx.Gas())
	}
}

// TestSignRoundTrip_RecoversCorrectSender is the strongest test in this
// package: builds a real unsigned transaction, signs its own real
// EIP-155 digest with a real (test-only, throwaway) secp256k1 key using
// go-ethereum's own crypto.Sign -- which returns exactly the same
// r||s||v shape (v a raw 0/1 recovery byte) s1/internal/kmssign.Wrapper.Sign
// contractually returns -- applies it via WithSignature, and confirms
// go-ethereum's own signer recovers the EXACT sender address the test
// key derives to. This is what proves the S1 signature hand-off
// actually works end to end (real recoverable-ECDSA math, not just
// "the code compiles and the shapes match"), the same discipline
// txbuild_test.go's own live-verified-digest test and
// depositwatcher/internal/addresses's own cross-validation test apply
// elsewhere in this codebase.
func TestSignRoundTrip_RecoversCorrectSender(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	wantSender := crypto.PubkeyToAddress(key.PublicKey)

	tx, digest, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 42_000000), testParams())
	if err != nil {
		t.Fatalf("BuildTransfer: %v", err)
	}

	sigBytes, err := crypto.Sign(digest[:], key)
	if err != nil {
		t.Fatalf("crypto.Sign: %v", err)
	}
	if len(sigBytes) != 65 {
		t.Fatalf("expected a 65-byte signature, got %d", len(sigBytes))
	}
	var sig [65]byte
	copy(sig[:], sigBytes)
	// crypto.Sign's own recovery byte is already 0/1 -- the exact
	// contract s1/internal/kmssign.Wrapper.Sign promises, confirmed here
	// rather than assumed.
	if sig[64] > 1 {
		t.Fatalf("expected crypto.Sign's own recovery byte to be 0 or 1, got %d", sig[64])
	}

	signed, err := WithSignature(tx, sig)
	if err != nil {
		t.Fatalf("WithSignature: %v", err)
	}

	signer := types.NewEIP155Signer(big.NewInt(BSCChainID))
	recoveredSender, err := types.Sender(signer, signed)
	if err != nil {
		t.Fatalf("recovering sender: %v", err)
	}
	if recoveredSender != wantSender {
		t.Fatalf("recovered sender %s does not match the signing key's own address %s", recoveredSender, wantSender)
	}

	// The signed transaction must also survive a real RLP marshal/
	// unmarshal round trip with its own hash unchanged -- what a real
	// eth_sendRawTransaction call and a later txHash lookup both depend
	// on.
	raw, err := MarshalForBroadcast(signed)
	if err != nil {
		t.Fatalf("MarshalForBroadcast: %v", err)
	}
	var roundTripped types.Transaction
	if err := roundTripped.UnmarshalBinary(raw); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if roundTripped.Hash() != signed.Hash() {
		t.Fatalf("hash changed across marshal round trip: %s vs %s", signed.Hash(), roundTripped.Hash())
	}
	if TxHash(signed) != signed.Hash() {
		t.Fatalf("TxHash helper disagrees with the transaction's own Hash()")
	}
}

func TestSignRoundTrip_WrongKeyRecoversDifferentSender(t *testing.T) {
	signingKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	otherAddress := crypto.PubkeyToAddress(otherKey.PublicKey)

	tx, digest, err := BuildTransfer("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", mustAmount(t, 1_000000), testParams())
	if err != nil {
		t.Fatal(err)
	}
	sigBytes, err := crypto.Sign(digest[:], signingKey)
	if err != nil {
		t.Fatal(err)
	}
	var sig [65]byte
	copy(sig[:], sigBytes)

	signed, err := WithSignature(tx, sig)
	if err != nil {
		t.Fatal(err)
	}
	signer := types.NewEIP155Signer(big.NewInt(BSCChainID))
	recovered, err := types.Sender(signer, signed)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == otherAddress {
		t.Fatal("recovered sender unexpectedly matched an unrelated key's own address")
	}
}
