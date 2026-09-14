// This file is this package's own extension to a second kind of signing
// authority: per-order BSC deposit addresses, not just the 6 fixed TRON/
// EVM slot keys Wrapper's own Sign already covers. It exists to close a
// real gap docs/03-build/c2-deposit-watcher-build-prompts.md's own "Read
// this first" flags: depositwatcher/internal/addresses derives a real,
// customer-facing deposit address for every order via BIP32 CKDpub
// (public-key-only derivation from an xpub) -- but nothing anywhere
// could ever produce the matching PRIVATE key to sign a sweep
// transaction off of that exact address, because CKDpub is structurally
// incapable of it (see that package's own bip32.go doc comment). This
// file is the CKDpriv counterpart, living here rather than in a new
// package because this package's own top-of-file doc comment ("the only
// package in this module that may hold, derive, or produce anything
// signing-capable") applies to this just as much as it does to the 6
// slot keys, enforced by the same dependency_test.go.
//
// PRODUCTION CAVEAT, stated up front rather than buried: BSCDepositKeys
// holds real derived private key material directly in this process's
// own memory to sign with -- unlike Wrapper's own KMSClient-mediated
// slot signing, where a real KMS/HSM boundary means private key
// material never leaves it at all. That is a genuine, deliberate
// custody-model gap from what the rest of this package enforces, not an
// oversight: BIP32 private-key derivation is not an operation most real
// cloud KMS/HSM products expose (AWS KMS, for one, does not), so
// deriving-then-signing without ever exporting the parent seed or a
// child key requires either a specialized HSM that supports it natively
// or a different custody scheme (e.g. one pre-generated, individually
// KMS-imported key per address, abandoning on-the-fly derivation
// entirely). Deciding which is real, later work -- this file is the
// same kind of fake-standing-in-for-a-real-KMS this package's own
// FakeKMSClient already is for the 6 slots, just for a capability no
// real KMS in this repo has ever backed at all. Never wire
// S1_BSC_DEPOSIT_XPRV to a real seed protecting real customer funds
// without resolving this first.
package kmssign

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// xprvVersion/xpubVersion are BIP32's own mainnet extended-key version
// bytes -- duplicated from depositwatcher/internal/addresses/bip32.go,
// not shared (separate Go modules, no common internal package, the same
// convention every other cross-module boundary in this repo already
// uses).
var (
	bscXprvVersion = [4]byte{0x04, 0x88, 0xAD, 0xE4}
	bscXpubVersion = [4]byte{0x04, 0x88, 0xB2, 0x1E}
)

var (
	// ErrNotExtendedPrivateKey means the input's own version bytes (or
	// length) don't match a mainnet BIP32 xprv at all.
	ErrNotExtendedPrivateKey = errors.New("kmssign: not a mainnet BIP32 extended private key (xprv)")
	// ErrXprvXpubMismatch means the configured xprv and xpub do not
	// describe the same node -- their chain codes or public keys differ.
	// Fail loud at startup: this is exactly the misconfiguration that
	// would otherwise cause a sweep to sign from the wrong key entirely,
	// silently.
	ErrXprvXpubMismatch = errors.New("kmssign: configured BSC deposit xprv and xpub do not describe the same node")
	// ErrDepositChildOverflow mirrors addresses.ErrChildOverflow --
	// astronomically rare, but a real possible CKD result, never silently
	// coerced into something else.
	ErrDepositChildOverflow = errors.New("kmssign: derived child scalar exceeds the curve order (astronomically rare; this index cannot be used)")
)

const bscHardenedOffset = uint32(0x80000000)

// BSCDepositKeys holds one BIP32 node's own private key and chain code
// in memory, and derives+signs with any non-hardened child index on
// demand. See this file's own top-of-file doc comment for the
// production caveat this represents.
type BSCDepositKeys struct {
	chainCode  [32]byte
	privKey    *secp256k1.PrivateKey
	compressed [33]byte // this node's own compressed public key -- the "parent.pubKey" input CKD math needs
}

// NewBSCDepositKeys parses xprv and validates it against xpub -- both
// required, never just the xprv alone, specifically so a deployer who
// pastes the wrong xprv (right format, wrong seed) gets ErrXprvXpubMismatch
// at startup instead of a sweep silently signing from an address that
// doesn't match what depositwatcher's own C2 has been handing customers.
// xpub should be the exact same value configured as C2's own WATCHER_XPUB.
func NewBSCDepositKeys(xprv, xpub string) (*BSCDepositKeys, error) {
	chainCode, priv, err := parseXprv(xprv)
	if err != nil {
		return nil, err
	}
	compressed := priv.PubKey().SerializeCompressed()
	var compressedArr [33]byte
	copy(compressedArr[:], compressed)

	xpubChainCode, xpubKey, err := parseXpubForCrossCheck(xpub)
	if err != nil {
		return nil, fmt.Errorf("kmssign: parsing S1_BSC_DEPOSIT_XPUB for cross-check: %w", err)
	}
	if chainCode != xpubChainCode || compressedArr != xpubKey {
		return nil, ErrXprvXpubMismatch
	}

	return &BSCDepositKeys{chainCode: chainCode, privKey: priv, compressed: compressedArr}, nil
}

// deriveChild returns the non-hardened BIP32 child private key at index,
// via CKDpriv: k_i = (IL + k_par) mod n, where IL is the first 32 bytes
// of HMAC-SHA512(chainCode, parentPubCompressed || index) -- the exact
// same HMAC input depositwatcher/internal/addresses/bip32.go's own
// ckdPub uses (parent PUBLIC key || index, never the private key
// itself), which is precisely why the two are mathematically guaranteed
// to describe the same child address: point(k_i) = point(k_par) + IL*G,
// identical to what ckdPub computes on the public side alone. See
// bscdeposit_test.go's own cross-derivation test, which proves this
// against a real fixture rather than asserting it from the math alone.
func (k *BSCDepositKeys) deriveChild(index uint32) (*secp256k1.PrivateKey, error) {
	if index >= bscHardenedOffset {
		return nil, fmt.Errorf("kmssign: hardened index %d requested, but deposit addresses are always non-hardened", index)
	}
	var idxBytes [4]byte
	binary.BigEndian.PutUint32(idxBytes[:], index)

	mac := hmac.New(sha512.New, k.chainCode[:])
	mac.Write(k.compressed[:])
	mac.Write(idxBytes[:])
	i := mac.Sum(nil)

	var il secp256k1.ModNScalar
	if overflow := il.SetByteSlice(i[:32]); overflow {
		return nil, ErrDepositChildOverflow
	}

	var parentScalar secp256k1.ModNScalar
	if overflow := parentScalar.SetByteSlice(k.privKey.Serialize()); overflow {
		return nil, ErrDepositChildOverflow
	}

	childScalar := new(secp256k1.ModNScalar).Add2(&il, &parentScalar)
	if childScalar.IsZero() {
		return nil, ErrDepositChildOverflow
	}
	return secp256k1.NewPrivateKey(childScalar), nil
}

// PublicKey returns the compressed public key for child index -- safe
// to expose freely (it's what an address is made from), used by S1's
// own httpapi to report a deposit address's expected signing key and by
// Wrapper.SignBSCDeposit's own expectedPubKey check.
func (k *BSCDepositKeys) PublicKey(index uint32) ([33]byte, error) {
	child, err := k.deriveChild(index)
	if err != nil {
		return [33]byte{}, err
	}
	var out [33]byte
	copy(out[:], child.PubKey().SerializeCompressed())
	return out, nil
}

// sign produces a DER-encoded ECDSA signature over digest using the
// child private key at index -- the direct, in-process counterpart to
// KMSClient.Sign, never exported outside this package (Wrapper's own
// SignBSCDeposit is the only caller, applying the exact same low-s
// normalization and recovery-id matching Sign already applies to a real
// KMS response, so a caller of either method gets an identically-shaped
// guarantee regardless of which key type backed it).
func (k *BSCDepositKeys) sign(index uint32, digest [32]byte) ([]byte, error) {
	child, err := k.deriveChild(index)
	if err != nil {
		return nil, err
	}
	sig := ecdsa.Sign(child, digest[:])
	return sig.Serialize(), nil
}

// --- xprv parsing: base58check, self-contained (stdlib math/big +
// crypto/sha256 only), mirroring depositwatcher/internal/addresses's own
// xpub parsing exactly, for the private-key side. ---

func parseXprv(s string) ([32]byte, *secp256k1.PrivateKey, error) {
	payload, err := bscBase58CheckDecode(s)
	if err != nil {
		return [32]byte{}, nil, err
	}
	if len(payload) != 78 {
		return [32]byte{}, nil, fmt.Errorf("%w: payload is %d bytes, want 78", ErrNotExtendedPrivateKey, len(payload))
	}
	var version [4]byte
	copy(version[:], payload[0:4])
	switch version {
	case bscXprvVersion:
		// fall through
	case bscXpubVersion:
		return [32]byte{}, nil, fmt.Errorf("%w: version bytes are the PUBLIC key prefix (xpub) -- S1_BSC_DEPOSIT_XPRV must be the private key", ErrNotExtendedPrivateKey)
	default:
		return [32]byte{}, nil, fmt.Errorf("%w: unrecognized version bytes %x", ErrNotExtendedPrivateKey, version)
	}

	var chainCode [32]byte
	copy(chainCode[:], payload[13:45])

	// BIP32 encodes a private key as a 33-byte field: a leading 0x00
	// padding byte followed by the real 32-byte scalar -- payload[45] is
	// that padding byte, never part of the key itself.
	if payload[45] != 0x00 {
		return [32]byte{}, nil, fmt.Errorf("%w: malformed private-key padding byte", ErrNotExtendedPrivateKey)
	}
	var keyBytes [32]byte
	copy(keyBytes[:], payload[46:78])

	priv := secp256k1.PrivKeyFromBytes(keyBytes[:])
	for i := range keyBytes {
		keyBytes[i] = 0
	}
	return chainCode, priv, nil
}

// parseXpubForCrossCheck parses just enough of an xpub (chain code +
// compressed public key) to validate it against a parsed xprv -- NOT a
// general-purpose xpub parser (this package has no business deriving
// addresses on the public side; that's depositwatcher's own job), only
// a same-node check.
func parseXpubForCrossCheck(s string) ([32]byte, [33]byte, error) {
	payload, err := bscBase58CheckDecode(s)
	if err != nil {
		return [32]byte{}, [33]byte{}, err
	}
	if len(payload) != 78 {
		return [32]byte{}, [33]byte{}, fmt.Errorf("payload is %d bytes, want 78", len(payload))
	}
	var version [4]byte
	copy(version[:], payload[0:4])
	if version != bscXpubVersion {
		return [32]byte{}, [33]byte{}, fmt.Errorf("unrecognized xpub version bytes %x", version)
	}
	var chainCode [32]byte
	copy(chainCode[:], payload[13:45])
	var pubKey [33]byte
	copy(pubKey[:], payload[45:78])
	return chainCode, pubKey, nil
}

const bscBase58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var bscBase58Index = func() [256]int8 {
	var idx [256]int8
	for i := range idx {
		idx[i] = -1
	}
	for i, c := range []byte(bscBase58Alphabet) {
		idx[c] = int8(i)
	}
	return idx
}()

func bscBase58Decode(s string) ([]byte, error) {
	num := new(big.Int)
	base := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		digit := bscBase58Index[s[i]]
		if digit < 0 {
			return nil, fmt.Errorf("kmssign: invalid base58 character %q at position %d", s[i], i)
		}
		num.Mul(num, base)
		num.Add(num, big.NewInt(int64(digit)))
	}
	decoded := num.Bytes()

	leadingZeros := 0
	for leadingZeros < len(s) && s[leadingZeros] == '1' {
		leadingZeros++
	}
	out := make([]byte, leadingZeros+len(decoded))
	copy(out[leadingZeros:], decoded)
	return out, nil
}

func bscBase58CheckDecode(s string) ([]byte, error) {
	decoded, err := bscBase58Decode(s)
	if err != nil {
		return nil, err
	}
	if len(decoded) < 4 {
		return nil, errors.New("kmssign: decoded value shorter than a checksum")
	}
	payload, checksum := decoded[:len(decoded)-4], decoded[len(decoded)-4:]
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	if !hmac.Equal(second[:4], checksum) {
		return nil, errors.New("kmssign: base58check checksum mismatch")
	}
	return payload, nil
}
