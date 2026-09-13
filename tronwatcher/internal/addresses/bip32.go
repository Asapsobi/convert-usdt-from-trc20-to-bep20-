package addresses

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// This file implements exactly as much of BIP32 as this package needs --
// parsing an extended PUBLIC key and deriving one non-hardened child public
// key from it (CKDpub) -- directly against decred/dcrd's secp256k1
// primitives, rather than depending on a general-purpose BIP32 library.
// Copied verbatim from depositwatcher/internal/addresses/bip32.go (not
// shared: separate Go modules, no common internal package, same
// convention as every other service in this repo) -- the curve math
// here is chain-agnostic: TRON uses the same secp256k1 curve as BSC, so
// nothing in this file needed to change for the TRON watcher.
//
// That's a deliberate choice, not the path of least resistance. The
// obvious library, github.com/tyler-smith/go-bip32, works, but its
// public-derivation path pulls in github.com/FactomProject/basen and
// github.com/FactomProject/btcutilecc for base58 and curve math
// respectively -- both last touched around 2015, neither actively
// maintained. That's an acceptable risk for very little; it is not an
// acceptable risk for the code that derives the addresses real deposits
// land on. decred/dcrd/dcrec/secp256k1/v4 is already a transitive
// dependency (via btcec/v2, used in derive.go for the reverse direction:
// parsing an already-derived compressed public key back out), is actively
// maintained, and exposes point/scalar arithmetic as plain exported types
// -- ModNScalar, JacobianPoint -- with no "private key" concept wrapping
// them at all, which is a better fit for this package's constraints than
// any higher-level library that would.
//
// go-bip32 is still a dependency of this module, but ONLY of
// derive_test.go, as a fixture generator for a real, valid extended public
// key to feed into the code below -- never of anything that ships.

// xpubVersion is the version-byte prefix for a mainnet BIP32 extended
// PUBLIC key ("xpub..."). The corresponding private version (0x0488ADE4,
// "xprv...") is checked for and explicitly rejected, never parsed further.
var xpubVersion = [4]byte{0x04, 0x88, 0xB2, 0x1E}
var xprvVersion = [4]byte{0x04, 0x88, 0xAD, 0xE4}

var (
	ErrNotExtendedPublicKey = errors.New("addresses: not a mainnet BIP32 extended public key (xpub)")
	ErrInvalidChecksum      = errors.New("addresses: base58check checksum mismatch")
	ErrChildOverflow        = errors.New("addresses: derived child scalar exceeds the curve order (astronomically rare; retry with a different index)")
)

// extendedPubKey is the subset of an xpub's 78-byte payload this package
// needs: the 32-byte chain code and the 33-byte compressed public key.
// depth, parent fingerprint, and child number are parsed for validation
// (a malformed length is still a malformed key) but never used, since this
// package derives an address, not a re-serializable child extended key.
type extendedPubKey struct {
	chainCode [32]byte
	pubKey    [33]byte
}

// parseXpub decodes a base58check-encoded extended key string, verifies
// the checksum, and returns its chain code and compressed public key --
// but ONLY if the version bytes say "xpub". An "xprv" is rejected here
// too, as a second, structural check independent of derive.go's textual
// prefix guard: that guard runs before this function is ever called, but
// a load-bearing rejection should never depend on only one layer.
func parseXpub(s string) (extendedPubKey, error) {
	payload, err := base58CheckDecode(s)
	if err != nil {
		return extendedPubKey{}, err
	}
	// version(4) + depth(1) + parent fingerprint(4) + child number(4) +
	// chain code(32) + key(33) = 78 bytes, exactly, for every BIP32
	// extended key regardless of depth.
	if len(payload) != 78 {
		return extendedPubKey{}, fmt.Errorf("%w: payload is %d bytes, want 78", ErrNotExtendedPublicKey, len(payload))
	}

	var version [4]byte
	copy(version[:], payload[0:4])
	switch version {
	case xpubVersion:
		// fall through
	case xprvVersion:
		return extendedPubKey{}, fmt.Errorf("%w: version bytes are the PRIVATE key prefix (xprv)", ErrPrivateKeyMaterial)
	default:
		return extendedPubKey{}, fmt.Errorf("%w: unrecognized version bytes %x", ErrNotExtendedPublicKey, version)
	}

	var out extendedPubKey
	copy(out.chainCode[:], payload[13:45])
	copy(out.pubKey[:], payload[45:78])
	return out, nil
}

// ckdPub implements BIP32's public-parent-to-public-child derivation
// (CKDpub) for a single non-hardened index. Callers (derive.go) are
// responsible for rejecting a hardened index before this is reached --
// this function has no way to serve one regardless, since that requires
// the parent private key by construction, not a missing feature.
func ckdPub(parent extendedPubKey, index uint32) ([]byte, error) {
	var idxBytes [4]byte
	binary.BigEndian.PutUint32(idxBytes[:], index)

	mac := hmac.New(sha512.New, parent.chainCode[:])
	mac.Write(parent.pubKey[:])
	mac.Write(idxBytes[:])
	i := mac.Sum(nil) // 64 bytes: I_L (tweak scalar) || I_R (child chain code, unused here)

	var il secp256k1.ModNScalar
	if overflow := il.SetByteSlice(i[:32]); overflow {
		return nil, ErrChildOverflow
	}

	var tweak secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(&il, &tweak)

	parentPub, err := secp256k1.ParsePubKey(parent.pubKey[:])
	if err != nil {
		return nil, fmt.Errorf("addresses: parsing parent public key: %w", err)
	}
	var parentPoint secp256k1.JacobianPoint
	parentPub.AsJacobian(&parentPoint)

	var childPoint secp256k1.JacobianPoint
	secp256k1.AddNonConst(&parentPoint, &tweak, &childPoint)
	childPoint.ToAffine()

	childPub := secp256k1.NewPublicKey(&childPoint.X, &childPoint.Y)
	return childPub.SerializeUncompressed(), nil
}

// --- base58check, self-contained: stdlib math/big + crypto/sha256 only ---

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var base58Index = func() [256]int8 {
	var idx [256]int8
	for i := range idx {
		idx[i] = -1
	}
	for i, c := range []byte(base58Alphabet) {
		idx[c] = int8(i)
	}
	return idx
}()

// base58Decode converts a base58 string to bytes, preserving leading-zero
// semantics: each leading '1' in the input (base58's encoding of a zero
// byte) becomes one leading 0x00 byte in the output, exactly as base58
// requires for a fixed-width payload like an extended key to round-trip.
func base58Decode(s string) ([]byte, error) {
	num := new(big.Int)
	base := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		digit := base58Index[s[i]]
		if digit < 0 {
			return nil, fmt.Errorf("addresses: invalid base58 character %q at position %d", s[i], i)
		}
		num.Mul(num, base)
		num.Add(num, big.NewInt(int64(digit)))
	}

	decoded := num.Bytes() // big-endian, no leading zeros

	leadingZeros := 0
	for leadingZeros < len(s) && s[leadingZeros] == '1' {
		leadingZeros++
	}

	out := make([]byte, leadingZeros+len(decoded))
	copy(out[leadingZeros:], decoded)
	return out, nil
}

// base58CheckDecode decodes s and verifies its trailing 4-byte checksum
// (the first 4 bytes of SHA256(SHA256(payload))) before returning the
// payload with the checksum stripped. Any mismatch -- wrong checksum,
// too-short input -- is ErrInvalidChecksum or a decode error, never a
// silently-truncated or best-effort result.
func base58CheckDecode(s string) ([]byte, error) {
	decoded, err := base58Decode(s)
	if err != nil {
		return nil, err
	}
	if len(decoded) < 4 {
		return nil, fmt.Errorf("%w: decoded value shorter than a checksum", ErrInvalidChecksum)
	}
	payload, checksum := decoded[:len(decoded)-4], decoded[len(decoded)-4:]

	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	if !hmacEqual(second[:4], checksum) {
		return nil, ErrInvalidChecksum
	}
	return payload, nil
}

// hmacEqual is a constant-time byte-slice comparison. A checksum
// comparison isn't defending a secret the way an HMAC verification is,
// but there is no upside to a variable-time compare here either, and
// hmac.Equal is already imported transitively -- using it is simpler than
// justifying why bytes.Equal would be fine this one time.
func hmacEqual(a, b []byte) bool {
	return hmac.Equal(a, b)
}
