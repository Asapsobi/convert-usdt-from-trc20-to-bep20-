package kmssign

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	bip32 "github.com/tyler-smith/go-bip32"
)

// TestDeriveChild_MatchesIndependentImplementation is this file's own
// counterpart to depositwatcher/internal/addresses/derive_test.go's
// TestCKDPub_MatchesIndependentImplementation -- the same "cross-check
// against a second, independently-implemented BIP32 library" discipline,
// applied to the private-derivation side. go-bip32 is test-only here too
// (see go.mod's own comment), never a production dependency.
//
// This is the single most important test in this file: it's the proof
// that a private key derived here actually signs FOR the exact address
// depositwatcher's own CKDpub math would hand a customer as a deposit
// address for the same (seed, index) -- not just "produces some
// plausible-looking keypair."
func TestDeriveChild_MatchesIndependentImplementation(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("cross-validation fixture seed -- not a real seed, never use"))
	if err != nil {
		t.Fatal(err)
	}
	xprv := master.B58Serialize()
	xpub := master.PublicKey().B58Serialize()

	keys, err := NewBSCDepositKeys(xprv, xpub)
	if err != nil {
		t.Fatalf("NewBSCDepositKeys: %v", err)
	}

	for _, idx := range []uint32{0, 1, 2, 100, 1 << 20, bscHardenedOffset - 1} {
		theirChild, err := master.NewChildKey(idx)
		if err != nil {
			t.Fatalf("index %d: go-bip32 NewChildKey: %v", idx, err)
		}
		theirPub, err := secp256k1.ParsePubKey(theirChild.PublicKey().Key)
		if err != nil {
			t.Fatalf("index %d: parsing go-bip32's derived public key: %v", idx, err)
		}

		ourPubBytes, err := keys.PublicKey(idx)
		if err != nil {
			t.Fatalf("index %d: our PublicKey: %v", idx, err)
		}
		ourPub, err := secp256k1.ParsePubKey(ourPubBytes[:])
		if err != nil {
			t.Fatalf("index %d: parsing our derived public key: %v", idx, err)
		}

		if !ourPub.IsEqual(theirPub) {
			t.Fatalf("index %d: derived a DIFFERENT public key than go-bip32:\n  ours:   %x\n  theirs: %x",
				idx, ourPubBytes, theirChild.PublicKey().Key)
		}

		ourChild, err := keys.deriveChild(idx)
		if err != nil {
			t.Fatalf("index %d: our deriveChild: %v", idx, err)
		}
		if !ourChild.PubKey().IsEqual(theirPub) {
			t.Fatalf("index %d: our derived PRIVATE key's own public key doesn't match go-bip32's derived public key", idx)
		}
	}
}

// TestDeriveChild_SignatureVerifiesAndRecoversToDerivedPublicKey proves
// the signing half end to end: sign with a derived child key, verify the
// signature against that same child's own derived public key using the
// standard library-independent secp256k1 verifier (not just "it didn't
// error").
func TestDeriveChild_SignatureVerifiesAndRecoversToDerivedPublicKey(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("another fixture seed, also not real"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewBSCDepositKeys(master.B58Serialize(), master.PublicKey().B58Serialize())
	if err != nil {
		t.Fatalf("NewBSCDepositKeys: %v", err)
	}

	digest := sha256.Sum256([]byte("a fake unsigned BEP20 sweep transaction, for this test only"))
	const index = uint32(1042)

	pub, err := keys.PublicKey(index)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	wrapper := NewWrapper(nil)
	wrapper.SetBSCDepositKeys(keys)
	sig, err := wrapper.SignBSCDeposit(context.Background(), index, digest, pub)
	if err != nil {
		t.Fatalf("SignBSCDeposit: %v", err)
	}

	recovered, _, err := ecdsa.RecoverCompact(signatureToCompact(sig), digest[:])
	if err != nil {
		t.Fatalf("recovering public key from signature: %v", err)
	}
	expected, err := secp256k1.ParsePubKey(pub[:])
	if err != nil {
		t.Fatalf("parsing expected public key: %v", err)
	}
	if !recovered.IsEqual(expected) {
		t.Fatal("signature does not recover to the derived public key")
	}
}

// signatureToCompact rebuilds the 65-byte compact-signature format
// ecdsa.RecoverCompact expects from Wrapper's own r||s||v output --
// mirroring wrapper.go's own compactSigRecoveryBase/compactSigCompressedFlag
// construction, test-side.
func signatureToCompact(sig [65]byte) []byte {
	compact := make([]byte, 65)
	compact[0] = compactSigRecoveryBase + sig[64] + compactSigCompressedFlag
	copy(compact[1:33], sig[0:32])
	copy(compact[33:65], sig[32:64])
	return compact
}

func TestNewBSCDepositKeys_RejectsMismatchedXprvXpub(t *testing.T) {
	masterA, err := bip32.NewMasterKey([]byte("seed A"))
	if err != nil {
		t.Fatal(err)
	}
	masterB, err := bip32.NewMasterKey([]byte("seed B, deliberately different"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewBSCDepositKeys(masterA.B58Serialize(), masterB.PublicKey().B58Serialize())
	if err == nil {
		t.Fatal("expected an error for a mismatched xprv/xpub pair")
	}
}

func TestNewBSCDepositKeys_RejectsXpubPassedAsXprv(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("seed for the wrong-argument test"))
	if err != nil {
		t.Fatal(err)
	}
	xpub := master.PublicKey().B58Serialize()

	if _, err := NewBSCDepositKeys(xpub, xpub); err == nil {
		t.Fatal("expected an error when an xpub is passed where an xprv is required")
	}
}
