// Package tronaddr is the one place C6 validates that a string is a
// real TRON address before it's ever allowed to become a quote's own
// (and therefore, downstream, an order's own) destination.
//
// Model D is a fixed-direction corridor -- deposit BSC (BEP20), payout
// TRON (TRC20), never the reverse and never same-network -- and this is
// the backend's own enforcement of that invariant, independent of
// whatever a caller's own frontend does or doesn't check. See
// internal/quotes' own doc comment on exactly where this is called and
// why that's the correct, structurally-unbypassable boundary rather
// than a per-handler check.
//
// Real base58check decode + checksum validation, not a prefix/regex
// guess: this package wraps github.com/fbsobreira/gotron-sdk's own
// address.Base58ToAddress -- the exact same library
// dispatcher/internal/txbuild and relayd/internal/txbuild already use
// to validate a payout's own real on-chain recipient before building a
// transaction, verified live against real TRON (see txbuild.go's own
// doc comment). Reusing it here means this package's own notion of
// "valid" is the same one that will actually be asked to receive real
// money later in this exact corridor, not a second, independently-
// maintained definition that could quietly drift from it.
package tronaddr

import (
	"errors"
	"fmt"

	"github.com/fbsobreira/gotron-sdk/pkg/address"
)

// ErrInvalidAddress means addr is not a valid TRON base58check address
// -- wrong length, wrong version byte, bad checksum, not base58 at all,
// or empty. Every rejection reason collapses to this one sentinel
// (wrapped, with the underlying reason still in the error string for
// logs) rather than a menu of distinct error types: a caller only ever
// needs to know "this isn't valid," never which specific way it failed.
var ErrInvalidAddress = errors.New("tronaddr: not a valid TRON address")

// Validate returns nil if addr is a real, checksum-valid TRON address,
// ErrInvalidAddress otherwise. This is a real base58check decode, not a
// shape/prefix check -- a string that merely starts with 'T' and is the
// right length but has a corrupted checksum is rejected exactly the
// same as an EVM 0x... address, an empty string, or garbage.
func Validate(addr string) error {
	if addr == "" {
		return fmt.Errorf("%w: empty", ErrInvalidAddress)
	}
	if _, err := address.Base58ToAddress(addr); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}
	return nil
}
