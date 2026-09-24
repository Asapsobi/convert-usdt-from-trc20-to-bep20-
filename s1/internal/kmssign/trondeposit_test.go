package kmssign

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	bip32 "github.com/tyler-smith/go-bip32"

	"s1/internal/slots"
)

// TestDeriveChild_MatchesIndependentImplementation_Tron is
// bscdeposit_test.go's own TestDeriveChild_MatchesIndependentImplementation,
// applied to TronDepositKeys -- CKDpriv is shared, chain-agnostic math
// (ckdPrivChild, bscdeposit.go), so this proves the exact same property
// for the TRON-deposit path: a private key derived here really does sign
// for the same (seed, index) address tronwatcher's own CKDpub math would
// hand a customer.
func TestDeriveChild_MatchesIndependentImplementation_Tron(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("tron cross-validation fixture seed -- not a real seed, never use"))
	if err != nil {
		t.Fatal(err)
	}
	xprv := master.B58Serialize()
	xpub := master.PublicKey().B58Serialize()

	keys, err := NewTronDepositKeys(xprv, xpub)
	if err != nil {
		t.Fatalf("NewTronDepositKeys: %v", err)
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

		ourPubBytes, err := keys.PublicKey(context.Background(), idx)
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

// TestDeriveChild_SignatureVerifiesAndRecoversToDerivedPublicKey_Tron is
// bscdeposit_test.go's own equivalent test, applied to
// Wrapper.SignTronDeposit.
func TestDeriveChild_SignatureVerifiesAndRecoversToDerivedPublicKey_Tron(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("another tron fixture seed, also not real"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewTronDepositKeys(master.B58Serialize(), master.PublicKey().B58Serialize())
	if err != nil {
		t.Fatalf("NewTronDepositKeys: %v", err)
	}

	digest := sha256.Sum256([]byte("a fake unsigned TRC20 sweep transaction, for this test only"))
	const index = uint32(2042)

	pub, err := keys.PublicKey(context.Background(), index)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	wrapper := NewWrapper(nil)
	wrapper.SetTronDepositKeys(keys)
	sig, err := wrapper.SignTronDeposit(context.Background(), index, digest, pub)
	if err != nil {
		t.Fatalf("SignTronDeposit: %v", err)
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

// TestSignTronDeposit_RecoveredPublicKeyProducesTheSameTronAddress is
// this file's own addition beyond bscdeposit_test.go's mirror: the full
// sign-then-verify-ADDRESS round trip using TRON's own address encoding
// (slots.DeriveTronAddress), not just a bare public-key comparison. This
// is the property forward_trc20.go's own defense-in-depth cross-check
// and the new E2E test both ultimately depend on: a signature produced
// here recovers to a public key that encodes to the SAME TRON address
// the child key was derived for -- proving the whole
// derive-sign-recover-encode chain is internally consistent, not just
// each step in isolation.
func TestSignTronDeposit_RecoveredPublicKeyProducesTheSameTronAddress(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("tron address round-trip fixture seed -- not real"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewTronDepositKeys(master.B58Serialize(), master.PublicKey().B58Serialize())
	if err != nil {
		t.Fatalf("NewTronDepositKeys: %v", err)
	}

	digest := sha256.Sum256([]byte("another fake unsigned TRC20 sweep transaction"))
	const index = uint32(777)

	derivedPub, err := keys.PublicKey(context.Background(), index)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	derivedAddr, err := slots.DeriveTronAddress(derivedPub)
	if err != nil {
		t.Fatalf("DeriveTronAddress(derived): %v", err)
	}
	if derivedAddr == "" {
		t.Fatal("derived TRON address is empty")
	}

	wrapper := NewWrapper(nil)
	wrapper.SetTronDepositKeys(keys)
	sig, err := wrapper.SignTronDeposit(context.Background(), index, digest, derivedPub)
	if err != nil {
		t.Fatalf("SignTronDeposit: %v", err)
	}

	recovered, _, err := ecdsa.RecoverCompact(signatureToCompact(sig), digest[:])
	if err != nil {
		t.Fatalf("recovering public key from signature: %v", err)
	}
	var recoveredCompressed [33]byte
	copy(recoveredCompressed[:], recovered.SerializeCompressed())
	recoveredAddr, err := slots.DeriveTronAddress(recoveredCompressed)
	if err != nil {
		t.Fatalf("DeriveTronAddress(recovered): %v", err)
	}

	if recoveredAddr != derivedAddr {
		t.Fatalf("recovered signer's TRON address (%s) does not match the derived child key's own TRON address (%s)",
			recoveredAddr, derivedAddr)
	}
}

func TestNewTronDepositKeys_RejectsMismatchedXprvXpub(t *testing.T) {
	masterA, err := bip32.NewMasterKey([]byte("tron seed A"))
	if err != nil {
		t.Fatal(err)
	}
	masterB, err := bip32.NewMasterKey([]byte("tron seed B, deliberately different"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewTronDepositKeys(masterA.B58Serialize(), masterB.PublicKey().B58Serialize())
	if err == nil {
		t.Fatal("expected an error for a mismatched xprv/xpub pair")
	}
}

func TestNewTronDepositKeys_RejectsXpubPassedAsXprv(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("tron seed for the wrong-argument test"))
	if err != nil {
		t.Fatal(err)
	}
	xpub := master.PublicKey().B58Serialize()

	if _, err := NewTronDepositKeys(xpub, xpub); err == nil {
		t.Fatal("expected an error when an xpub is passed where an xprv is required")
	}
}
