package slots

import (
	"encoding/hex"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// DeriveEVMAddress computes the EIP-55 checksum-encoded EVM address
// (BSC included) for a compressed secp256k1 public key -- exported, no
// database access, so a caller that already has a SlotKey.PublicKey in
// hand (e.g. cmd/s1d's own slotKeyGetterAdapter) can derive the EVM
// address without a second round trip through Store.EVMAddress's own
// Get call. See deriveEVMAddress's own doc comment for the derivation
// itself.
func DeriveEVMAddress(compressed [33]byte) (string, error) {
	pub, err := secp256k1.ParsePubKey(compressed[:])
	if err != nil {
		return "", fmt.Errorf("slots: parsing public key: %w", err)
	}
	return deriveEVMAddress(pub), nil
}

// deriveEVMAddress computes the EIP-55 checksum-encoded EVM address
// (BSC included -- BSC is EVM-compatible and uses the identical address
// format) for a secp256k1 public key: Keccak256 of the 64-byte
// uncompressed point (X||Y, never the 0x04 prefix byte), last 20 bytes,
// EIP-55 mixed-case checksum encoded.
//
// This is the SAME secp256k1 key deriveTronAddress (tron.go, same
// package) already derives a TRON address from -- one key, two valid
// address encodings on two different chains, not a second key or a new
// custody model. Added for Model F's own BEP20->TRC20 relay direction
// (docs/02-architecture/model-f-relay-architecture.md's own S1 entry:
// "Same custody model, smaller blast radius -- signs only the
// forward-leg transfer to the upstream platform"), which needs relayd
// to know an existing slot's own EVM-format address the same way it
// already knows its TRON one. Mirrors
// depositwatcher/internal/addresses/address.go's own toChecksumAddress
// exactly (duplicated, not shared: separate Go modules, no common
// internal package, same convention as every cross-service boundary in
// this repo).
func deriveEVMAddress(pub *secp256k1.PublicKey) string {
	uncompressed := pub.SerializeUncompressed() // 0x04 || X(32) || Y(32)
	hash := keccak256(uncompressed[1:])
	return string(toChecksumAddress(hash[len(hash)-20:]))
}

// toChecksumAddress renders a 20-byte address per EIP-55: each hex
// letter (a-f) in the lowercase address is uppercased if the
// corresponding nibble of keccak256(lowercase hex string) is >= 8, else
// left lowercase.
func toChecksumAddress(addr20 []byte) evmAddress {
	lower := hex.EncodeToString(addr20)
	hash := hex.EncodeToString(keccak256([]byte(lower)))

	out := make([]byte, len(lower))
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if c >= '0' && c <= '9' {
			out[i] = c
			continue
		}
		if hexNibble(hash[i]) >= 8 {
			out[i] = c - 'a' + 'A'
		} else {
			out[i] = c
		}
	}
	return evmAddress("0x" + string(out))
}

// evmAddress is a private type purely so toChecksumAddress's own return
// value can't be accidentally treated as a TRON Address elsewhere in
// this package -- converted to a plain string at deriveEVMAddress's own
// boundary, since that's all this package's own callers (requests.Store)
// need.
type evmAddress string

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return -1
	}
}
