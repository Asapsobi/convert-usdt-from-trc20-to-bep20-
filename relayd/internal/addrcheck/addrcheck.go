// Package addrcheck validates the addresses customers give us, before an
// order exists. A payout address that is malformed, on the wrong network,
// or mistyped is caught here -- not after the customer has paid, when the
// only way out is a refund.
package addrcheck

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	tronaddress "github.com/fbsobreira/gotron-sdk/pkg/address"
)

// ErrInvalid means an address is not a valid address on the network it
// was given for.
var ErrInvalid = errors.New("invalid address")

// TRON checks a base58check TRON mainnet address (T..., 21 bytes with the
// 0x41 prefix and a matching checksum).
func TRON(addr string) error {
	if !strings.HasPrefix(addr, "T") {
		return fmt.Errorf("%w: %q is not a TRON address (they start with T)", ErrInvalid, addr)
	}
	if _, err := tronaddress.Base58ToAddress(addr); err != nil {
		return fmt.Errorf("%w: %q is not a valid TRON address: %v", ErrInvalid, addr, err)
	}
	return nil
}

// EVM checks a 0x-prefixed 20-byte hex address. A mixed-case address must
// match its EIP-55 checksum (that is what the mixed case is for: a single
// mistyped character fails it). The zero address is rejected.
func EVM(addr string) error {
	if !strings.HasPrefix(addr, "0x") || !common.IsHexAddress(addr) {
		return fmt.Errorf("%w: %q is not a BSC address (0x followed by 40 hex characters)", ErrInvalid, addr)
	}
	a := common.HexToAddress(addr)
	if a == (common.Address{}) {
		return fmt.Errorf("%w: the zero address can't receive funds", ErrInvalid)
	}
	body := addr[2:]
	if body != strings.ToLower(body) && body != strings.ToUpper(body) && a.Hex() != addr {
		return fmt.Errorf("%w: %q fails its checksum -- check it for a typo", ErrInvalid, addr)
	}
	return nil
}
