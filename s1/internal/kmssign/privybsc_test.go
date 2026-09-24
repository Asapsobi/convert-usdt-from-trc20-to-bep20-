package kmssign

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"s1/internal/slots"
)

// TestDeriveEVMAddressForRecovery_MatchesSlotsPackage proves
// deriveEVMAddressForRecovery (duplicated here rather than imported --
// see privybsc.go's own top-of-file doc comment for why) produces the
// same 20-byte address slots.DeriveEVMAddress does, for the same public
// key -- the two are independent implementations of the same external
// standard (Keccak256 of the uncompressed point, last 20 bytes), and
// this is what makes that duplication safe rather than a silent
// divergence risk. Compared case-insensitively, since this file's own
// copy deliberately skips EIP-55 checksum casing (see that function's
// own doc comment).
func TestDeriveEVMAddressForRecovery_MatchesSlotsPackage(t *testing.T) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	var compressed [33]byte
	copy(compressed[:], priv.PubKey().SerializeCompressed())

	want, err := slots.DeriveEVMAddress(compressed)
	if err != nil {
		t.Fatalf("slots.DeriveEVMAddress: %v", err)
	}
	got := deriveEVMAddressForRecovery(priv.PubKey())

	if !strings.EqualFold(got, want) {
		t.Errorf("deriveEVMAddressForRecovery = %s, want %s (slots.DeriveEVMAddress, case-insensitive)", got, want)
	}
}

// TestRecoverPubKeyMatchingAddress_RecoversTheRealSigner signs a real
// digest with a real key, derives that key's own EVM address, and
// confirms recoverPubKeyMatchingAddress independently recovers the exact
// same compressed public key given only the signature, the digest, and
// the address -- the live mechanism cmd/privy-probe already proved
// against Privy's real API, exercised here in isolation.
func TestRecoverPubKeyMatchingAddress_RecoversTheRealSigner(t *testing.T) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	address := deriveEVMAddressForRecovery(priv.PubKey())
	digest := sha256.Sum256([]byte("privybsc_test.go: TestRecoverPubKeyMatchingAddress_RecoversTheRealSigner"))

	sig := ecdsa.Sign(priv, digest[:])
	rBytes, sBytes := signatureRS(t, sig)

	got, err := recoverPubKeyMatchingAddress(rBytes, sBytes, digest, address)
	if err != nil {
		t.Fatalf("recoverPubKeyMatchingAddress: %v", err)
	}
	var wantCompressed [33]byte
	copy(wantCompressed[:], priv.PubKey().SerializeCompressed())
	if got != wantCompressed {
		t.Errorf("recovered pubkey = %x, want %x", got, wantCompressed)
	}
}

// TestRecoverPubKeyMatchingAddress_RejectsNonMatchingAddress confirms a
// real, valid signature recovered against the WRONG expected address is
// ErrBSCDepositPublicKeyRecoveryFailed, not silently accepted -- the
// exact condition Provision refuses to record custody for.
func TestRecoverPubKeyMatchingAddress_RejectsNonMatchingAddress(t *testing.T) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("privybsc_test.go: TestRecoverPubKeyMatchingAddress_RejectsNonMatchingAddress"))
	sig := ecdsa.Sign(priv, digest[:])
	rBytes, sBytes := signatureRS(t, sig)

	wrongAddress := "0x0000000000000000000000000000000000dEaD"
	_, err = recoverPubKeyMatchingAddress(rBytes, sBytes, digest, wrongAddress)
	if err == nil {
		t.Fatal("recoverPubKeyMatchingAddress() against the wrong address: want an error, got nil")
	}
}

// signatureRS splits a real ecdsa.Sign result into raw (r, s) -- test
// helper only, mirroring finishRecoverableSignature's own identical
// extraction (wrapper.go).
func signatureRS(t *testing.T, sig *ecdsa.Signature) (rBytes, sBytes [32]byte) {
	t.Helper()
	r := sig.R()
	s := sig.S()
	r.PutBytesUnchecked(rBytes[:])
	s.PutBytesUnchecked(sBytes[:])
	return rBytes, sBytes
}
