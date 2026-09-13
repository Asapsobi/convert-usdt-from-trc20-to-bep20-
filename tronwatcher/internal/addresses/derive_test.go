package addresses

import (
	"errors"
	"math/rand"
	"testing"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/tyler-smith/go-bip32"
)

// testXpub returns a real, valid BIP32 extended PUBLIC key for tests.
// Generated from a fixed, clearly-throwaway seed inside the test itself --
// this secures nothing, it exists purely as a fixture for exercising this
// package's own public-derivation math, so a hardcoded, obviously-fake
// seed is the right choice, not a security concern.
func testXpub(t *testing.T) string {
	t.Helper()
	master, err := bip32.NewMasterKey([]byte("tronwatcher test fixture -- not a real seed, never use"))
	if err != nil {
		t.Fatalf("generating test master key: %v", err)
	}
	return master.PublicKey().B58Serialize()
}

func TestDeriveAddress_DeterministicValidNoCollisions(t *testing.T) {
	xpub := testXpub(t)
	seed := int64(1)
	rng := rand.New(rand.NewSource(seed))
	seen := make(map[Address]uint32, 10000)

	defer func() {
		if t.Failed() {
			t.Logf("seed=%d", seed)
		}
	}()

	for i := 0; i < 10000; i++ {
		idx := uint32(rng.Int31()) // always < hardenedOffset (2^31)

		addr1, err := DeriveAddress(xpub, idx)
		if err != nil {
			t.Fatalf("index %d: %v", idx, err)
		}
		addr2, err := DeriveAddress(xpub, idx)
		if err != nil {
			t.Fatalf("index %d (second call): %v", idx, err)
		}
		if addr1 != addr2 {
			t.Fatalf("index %d: not deterministic: %s vs %s", idx, addr1, addr2)
		}
		if !Valid(addr1) {
			t.Fatalf("index %d: %s is not a valid TRON address", idx, addr1)
		}
		if prevIdx, dup := seen[addr1]; dup {
			t.Fatalf("collision: indices %d and %d both produced %s", prevIdx, idx, addr1)
		}
		seen[addr1] = idx
	}
}

func TestDeriveAddress_DifferentXpubsDifferentAddresses(t *testing.T) {
	master1, err := bip32.NewMasterKey([]byte("fixture seed one"))
	if err != nil {
		t.Fatal(err)
	}
	master2, err := bip32.NewMasterKey([]byte("fixture seed two"))
	if err != nil {
		t.Fatal(err)
	}
	addr1, err := DeriveAddress(master1.PublicKey().B58Serialize(), 0)
	if err != nil {
		t.Fatal(err)
	}
	addr2, err := DeriveAddress(master2.PublicKey().B58Serialize(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if addr1 == addr2 {
		t.Fatalf("two different master keys produced the same address at index 0: %s", addr1)
	}
}

func TestDeriveAddress_RejectsHardenedIndex(t *testing.T) {
	xpub := testXpub(t)
	_, err := DeriveAddress(xpub, hardenedOffset)
	if !errors.Is(err, ErrHardenedIndex) {
		t.Fatalf("expected ErrHardenedIndex, got %v", err)
	}
	_, err = DeriveAddress(xpub, hardenedOffset+5)
	if !errors.Is(err, ErrHardenedIndex) {
		t.Fatalf("expected ErrHardenedIndex, got %v", err)
	}
}

func TestDeriveAddress_RejectsExtendedPrivateKey(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("fixture seed for xprv rejection test"))
	if err != nil {
		t.Fatal(err)
	}
	xprv := master.B58Serialize() // the PRIVATE extended key, not .PublicKey()

	_, err = DeriveAddress(xprv, 0)
	if !errors.Is(err, ErrPrivateKeyMaterial) {
		t.Fatalf("expected ErrPrivateKeyMaterial for an xprv, got %v", err)
	}
}

func TestDeriveAddress_RejectsWIFShapedInput(t *testing.T) {
	// A real Bitcoin mainnet WIF (compressed), well-known test-vector shaped
	// but not tied to any real funds -- shape is all that matters here.
	wif := "L1aW4aubDFB7yfras2S1mN3bqg9nwySY8nkoLmJebSLD5BWv3ENZ"
	_, err := DeriveAddress(wif, 0)
	if !errors.Is(err, ErrPrivateKeyMaterial) {
		t.Fatalf("expected ErrPrivateKeyMaterial for a WIF-shaped string, got %v", err)
	}
}

func TestDeriveAddress_RejectsMnemonicShapedInput(t *testing.T) {
	mnemonic := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	_, err := DeriveAddress(mnemonic, 0)
	if !errors.Is(err, ErrPrivateKeyMaterial) {
		t.Fatalf("expected ErrPrivateKeyMaterial for a mnemonic-shaped string, got %v", err)
	}
}

func TestDeriveAddress_RejectsGarbageInput(t *testing.T) {
	for _, bad := range []string{"", "not-a-key-at-all", "0x1234"} {
		if _, err := DeriveAddress(bad, 0); err == nil {
			t.Fatalf("expected an error for garbage input %q, got none", bad)
		}
	}
}

func TestValid_RejectsMalformed(t *testing.T) {
	xpub := testXpub(t)
	addr, err := DeriveAddress(xpub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(addr) {
		t.Fatalf("expected a freshly-derived address to be valid: %s", addr)
	}
	if Valid("") {
		t.Fatal("empty string must not validate")
	}
	if Valid("not-a-tron-address-at-all") {
		t.Fatal("garbage input must not validate")
	}
	if Valid(Address(string(addr) + "x")) {
		t.Fatal("appending a character to a valid address must break its checksum")
	}
	// Flip the first character of the base58 payload (after the leading
	// 'T'), which changes the decoded bytes and must break the checksum.
	tampered := "T" + "1" + string(addr)[2:]
	if Valid(Address(tampered)) {
		t.Fatalf("tampered address unexpectedly validated: %s", tampered)
	}
}

// TestCKDPub_MatchesIndependentImplementation cross-validates this
// package's own CKDpub math (bip32.go) against tyler-smith/go-bip32's
// entirely separate implementation of the same BIP32 derivation formula.
// Same rationale as depositwatcher's own identical test: landing on the
// exact same point as a second, independently-implemented codebase for
// the same (seed, index) is not something a subtly-wrong implementation
// could do by chance.
func TestCKDPub_MatchesIndependentImplementation(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("cross-validation fixture seed -- not a real seed, never use"))
	if err != nil {
		t.Fatal(err)
	}
	xpub := master.PublicKey().B58Serialize()

	parsed, err := parseXpub(xpub)
	if err != nil {
		t.Fatalf("parseXpub: %v", err)
	}

	for _, idx := range []uint32{0, 1, 2, 100, 1 << 20, hardenedOffset - 1} {
		theirChild, err := master.PublicKey().NewChildKey(idx)
		if err != nil {
			t.Fatalf("index %d: go-bip32 NewChildKey: %v", idx, err)
		}

		ourUncompressed, err := ckdPub(parsed, idx)
		if err != nil {
			t.Fatalf("index %d: our ckdPub: %v", idx, err)
		}

		ourPub, err := secp256k1.ParsePubKey(ourUncompressed)
		if err != nil {
			t.Fatalf("index %d: parsing our derived point: %v", idx, err)
		}
		theirPub, err := secp256k1.ParsePubKey(theirChild.Key)
		if err != nil {
			t.Fatalf("index %d: parsing go-bip32's derived point: %v", idx, err)
		}

		if !ourPub.IsEqual(theirPub) {
			t.Fatalf("index %d: derived a DIFFERENT public key than go-bip32:\n  ours:  %x\n  theirs: %x",
				idx, ourUncompressed, theirChild.Key)
		}
	}
}
