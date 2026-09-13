package addresses

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrPrivateKeyMaterial is returned whenever an input looks like it
	// could be private key material -- an extended PRIVATE key ("xprv"),
	// a WIF-shaped string, or a BIP39-mnemonic-shaped word list. This is a
	// deliberate guard, not just an unused code path: this package must be
	// structurally incapable of ever handling one, and rejecting the shape
	// before any parsing is attempted is what makes an operator's
	// copy-paste mistake fail loudly and immediately instead of three
	// derivation steps later.
	ErrPrivateKeyMaterial = errors.New("addresses: input looks like private key material, refusing")

	// ErrHardenedIndex is returned for any index >= 2^31. Hardened BIP32
	// derivation requires the parent PRIVATE key by construction -- it is
	// not an optional feature this package chooses not to expose, it is
	// mathematically impossible from a public key alone. Rejecting it
	// here keeps the reason visible at this package's boundary instead of
	// surfacing as a generic curve-math error two calls deeper.
	ErrHardenedIndex = errors.New("addresses: hardened derivation requires a private key, which this package never holds")
)

const hardenedOffset = uint32(0x80000000)

// DeriveAddress derives the base58check TRON address at child index
// `index` under the BIP32 extended PUBLIC key `xpub`, using public-key-only
// ("neutered"/CKDpub) derivation -- see bip32.go for why that's implemented
// directly against decred/dcrd's secp256k1 primitives rather than via a
// general-purpose BIP32 library. Same (xpub, index) always yields the same
// address.
func DeriveAddress(xpub string, index uint32) (Address, error) {
	if index >= hardenedOffset {
		return "", fmt.Errorf("%w: index %d", ErrHardenedIndex, index)
	}
	if err := rejectPrivateKeyShaped(xpub); err != nil {
		return "", err
	}

	parent, err := parseXpub(xpub)
	if err != nil {
		return "", err
	}

	// TRON, like Ethereum/BSC, hashes the 64-byte uncompressed X||Y point,
	// never the 0x04 prefix byte the uncompressed serialization prepends
	// -- TRON forked its account/address model from Ethereum's, per
	// s1/internal/slots/tron.go's own doc comment.
	uncompressed, err := ckdPub(parent, index)
	if err != nil {
		return "", fmt.Errorf("addresses: deriving child at index %d: %w", index, err)
	}
	hash := keccak256(uncompressed[1:])
	return mustTronAddress(hash[len(hash)-20:]), nil
}

// rejectPrivateKeyShaped is a cheap, pre-parse shape check for the two
// forms of private key material parseXpub's structured decode would never
// see on its own: WIF, and a mnemonic. (An xprv IS caught by parseXpub
// itself, via its version bytes, after real base58check decoding -- this
// textual prefix check runs first anyway, so a mistake fails before any
// parsing is attempted at all, not just before derivation.)
func rejectPrivateKeyShaped(s string) error {
	trimmed := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(trimmed, "xprv"), strings.HasPrefix(trimmed, "tprv"):
		return fmt.Errorf("%w: extended PRIVATE key prefix", ErrPrivateKeyMaterial)
	case len(strings.Fields(trimmed)) >= 12:
		// A BIP39 mnemonic is a space-separated word list, conventionally
		// 12/15/18/21/24 words. This doesn't validate against the
		// wordlist -- that would mean bundling it, an entire category of
		// dependency this package has no other reason to carry -- it's a
		// shape check, deliberately cheap and deliberately over-inclusive.
		return fmt.Errorf("%w: looks like a space-separated word list (BIP39 mnemonic?)", ErrPrivateKeyMaterial)
	case looksLikeWIF(trimmed):
		return fmt.Errorf("%w: looks like a WIF private key", ErrPrivateKeyMaterial)
	}
	return nil
}

// looksLikeWIF is a shape heuristic, not a real base58check decode: a
// Bitcoin-mainnet WIF private key is base58check, 51 characters
// (uncompressed) or 52 (compressed), starting with '5', 'K', or 'L'
// respectively. Good enough to catch an obvious mistake before it reaches
// anything that parses it further.
func looksLikeWIF(s string) bool {
	if len(s) != 51 && len(s) != 52 {
		return false
	}
	switch s[0] {
	case '5', 'K', 'L':
		return true
	default:
		return false
	}
}
