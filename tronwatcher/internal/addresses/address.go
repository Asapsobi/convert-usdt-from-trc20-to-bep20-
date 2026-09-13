// Package addresses derives watch-only TRON (TRC20) deposit addresses
// from an extended PUBLIC key. It never holds, derives, or accepts a
// private key or seed phrase -- see the guards in derive.go and the
// dependency-scan test in no_signing_test.go, both of which exist
// specifically to make that a structural property of this package, not
// a promise about how it happens to be used today.
//
// This package is depositwatcher/internal/addresses's TRON sibling: the
// BIP32 CKDpub math in bip32.go is copied unchanged (TRON uses the same
// secp256k1 curve as BSC), and only the final address-encoding step
// differs -- base58check with TRON's own 0x41 version byte instead of
// EIP-55 checksummed hex, mirroring s1/internal/slots/tron.go's own
// deriveTronAddress (that file derives a slot's own signing address the
// same way; this package derives a watch-only deposit address with no
// signing capability at all).
package addresses

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"
)

// tronAddressVersion is TRON's own address-version byte, prepended to
// the 20-byte hash before base58check encoding -- the direct TRON
// analogue of the xpub/xprv version-byte pair bip32.go already checks
// for.
const tronAddressVersion = 0x41

// Address is a base58check-encoded TRON address, e.g. "TR7NHq...".
// Always produced by toTronAddress; never assembled by hand elsewhere in
// this package, so there is exactly one place the encoding can be gotten
// wrong.
type Address string

// keccak256 is the ONLY cryptographic primitive this package uses beyond
// elliptic-curve point arithmetic (isolated in derive.go). sha3.LegacyKeccak256
// is a pure hash function with no signing capability of any kind -- unlike
// the secp256k1 library derive.go has to depend on for point decompression,
// there is no tension here to document.
func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// toTronAddress renders a 20-byte hash as a base58check TRON address:
// prefix with tronAddressVersion, append a 4-byte double-SHA256
// checksum, base58-encode. Mirrors s1/internal/slots/tron.go's own
// deriveTronAddress encoding step exactly (duplicated, not shared --
// separate Go modules, same convention as every other service here).
func toTronAddress(addr20 []byte) Address {
	payload := append([]byte{tronAddressVersion}, addr20...)
	checksum := doubleSHA256(payload)[:4]
	full := append(append([]byte{}, payload...), checksum...)
	return Address(base58Encode(full))
}

func doubleSHA256(b []byte) []byte {
	first := sha256.Sum256(b)
	second := sha256.Sum256(first[:])
	return second[:]
}

func base58Encode(b []byte) string {
	leadingZeros := 0
	for _, c := range b {
		if c != 0 {
			break
		}
		leadingZeros++
	}

	num := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)

	var out []byte
	for num.Cmp(zero) > 0 {
		num.DivMod(num, base, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for i := 0; i < leadingZeros; i++ {
		out = append(out, base58Alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// Valid reports whether addr is a well-formed, correctly-checksummed
// TRON address: base58check-decodable, 21-byte payload (1 version byte +
// 20-byte hash), version byte 0x41. Unlike EIP-55's mixed-case checksum
// (depositwatcher/internal/addresses's own ValidChecksum), TRON's
// checksum lives entirely inside base58check itself -- there is no
// separate case-sensitivity convention to also verify.
func Valid(addr Address) bool {
	s := string(addr)
	if s == "" || !strings.HasPrefix(s, "T") {
		return false
	}
	payload, err := base58CheckDecode(s)
	if err != nil {
		return false
	}
	if len(payload) != 21 {
		return false
	}
	return payload[0] == tronAddressVersion
}

// mustTronAddress panics on a length mismatch, which would be a bug in
// this package's own caller (derive.go always passes exactly 20 bytes),
// never a reachable error from untrusted input.
func mustTronAddress(addr20 []byte) Address {
	if len(addr20) != 20 {
		panic(fmt.Sprintf("addresses: internal error: expected 20 bytes, got %d", len(addr20)))
	}
	return toTronAddress(addr20)
}
