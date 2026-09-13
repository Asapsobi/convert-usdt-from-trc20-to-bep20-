// Package money implements the minor-unit integer money primitive used
// throughout relayd's write path. No floating point, ever -- mirrored
// from ledger/internal/money (not shared: separate Go modules, no
// common internal package, same convention as every other service in
// this repo), because relayd's own amounts cross two different assets
// within a single relay leg (AmountIn on one chain, AmountOut on the
// other), the same reason C1's own money.Amount carries an Asset field
// instead of assuming one asset the way C4's single-asset money package
// does.
package money

import (
	"errors"
	"fmt"
)

// Asset is a closed set of the currencies relayd knows how to hold or
// quote. Deliberately a defined type over string, not a bare string, so
// a typo cannot silently pass as a valid asset anywhere in this module.
type Asset string

const (
	USDT_BEP20 Asset = "USDT_BEP20"
	USDT_TRC20 Asset = "USDT_TRC20"
	TRX        Asset = "TRX"
	BNB        Asset = "BNB"
)

var ErrUnknownAsset = errors.New("money: unknown asset")

// Decimals returns the number of minor-unit decimal places for the
// asset, or ErrUnknownAsset if the asset is not one of the closed set
// above. BNB is truncated to 9 decimals (gwei-equivalent), matching
// ledger/internal/money's own reasoning: BNB only ever appears here as
// a BSC gas expense, so sub-gwei precision is deliberately discarded
// rather than modeled.
func (a Asset) Decimals() (int, error) {
	switch a {
	case USDT_BEP20, USDT_TRC20, TRX:
		return 6, nil
	case BNB:
		return 9, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownAsset, string(a))
	}
}

// Valid reports whether a is one of the closed set of known assets.
func (a Asset) Valid() bool {
	_, err := a.Decimals()
	return err == nil
}
