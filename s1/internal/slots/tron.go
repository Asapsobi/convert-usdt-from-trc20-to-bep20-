package slots

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/crypto/sha3"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// tronAddressVersion is TRON's own address-version byte (0x41), prepended
// to the 20-byte hash before base58check encoding -- the direct TRON
// analogue of Bitcoin's 0x00 mainnet version byte, and of the xpub/xprv
// version-byte pair depositwatcher/internal/addresses/bip32.go already
// checks for BSC's own extended keys.
const tronAddressVersion = 0x41

// DeriveTronAddress computes the base58check TRON address for a
// compressed secp256k1 public key -- exported, no database access, so a
// caller that already has a public key in hand (e.g. a TRON deposit-key
// signature's own recovered/derived pubkey) can derive the address
// without a second round trip through this package's own Get call.
// Mirrors DeriveEVMAddress's own already-precedented shape (evm.go).
func DeriveTronAddress(compressed [33]byte) (string, error) {
	pub, err := secp256k1.ParsePubKey(compressed[:])
	if err != nil {
		return "", fmt.Errorf("slots: parsing public key: %w", err)
	}
	return deriveTronAddress(pub), nil
}

// deriveTronAddress computes the base58check TRON address for a
// secp256k1 public key: Keccak256 of the 64-byte uncompressed point
// (X||Y, never the 0x04 prefix byte -- the same convention
// depositwatcher/internal/addresses/derive.go already uses for EVM
// addresses, since TRON forked its account/address model from Ethereum's),
// last 20 bytes, prefixed with tronAddressVersion, base58check-encoded.
func deriveTronAddress(pub *secp256k1.PublicKey) string {
	uncompressed := pub.SerializeUncompressed() // 0x04 || X(32) || Y(32)
	hash := keccak256(uncompressed[1:])
	payload := append([]byte{tronAddressVersion}, hash[len(hash)-20:]...)
	return base58CheckEncode(payload)
}

func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// --- base58check encode, self-contained: stdlib math/big + crypto/sha256
// only -- the encode-direction counterpart to
// depositwatcher/internal/addresses/bip32.go's own base58check DECODE
// (that package only ever parses an xpub string; this one only ever
// produces a TRON address string, so the two directions live in their
// own modules rather than one shared implementation neither fully
// needs). Same alphabet as Bitcoin's and TRON's own -- TRON addresses
// are base58check over the identical alphabet, just a different version
// byte and payload length.

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var errEmptyPayload = errors.New("slots: base58check encode called with an empty payload")

func base58CheckEncode(payload []byte) string {
	if len(payload) == 0 {
		panic(fmt.Errorf("%w", errEmptyPayload)) // unreachable given this file's own callers, guarded loudly rather than silently returning ""
	}
	checksum := doubleSHA256(payload)[:4]
	full := append(append([]byte{}, payload...), checksum...)
	return base58Encode(full)
}

func doubleSHA256(b []byte) []byte {
	first := sha256.Sum256(b)
	second := sha256.Sum256(first[:])
	return second[:]
}

func base58Encode(b []byte) string {
	// Count leading zero bytes -- each becomes one leading '1' in the
	// output, exactly mirroring how base58Decode in
	// depositwatcher/internal/addresses/bip32.go turns a leading '1'
	// back into a 0x00 byte.
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
	// out was built least-significant-digit-first; reverse it.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
