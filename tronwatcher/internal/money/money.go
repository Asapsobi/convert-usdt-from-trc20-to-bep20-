// Package money is tronwatcher's minimal money primitive. This service
// only ever handles one asset -- USDT_TRC20, watched deposits into it,
// never anything else -- so unlike C1's own money.Amount
// (ledger/internal/money), there is no Asset field here, mirroring
// depositwatcher/internal/money's own reasoning exactly (duplicated, not
// shared: separate Go modules, no common internal package, same
// convention as every other service in this repo).
//
// Unlike depositwatcher, this package's FromOnChainUnits is close to a
// no-op: USDT-TRC20 (contract TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t, per
// dispatcher/internal/txbuild/txbuild.go's own verified constant) uses 6
// decimals on-chain, identical to this ledger-facing precision -- unlike
// BEP20's surprising 18. The conversion is kept anyway, for symmetry and
// in case that ever needs to change, rather than assumed away.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// ErrInvalidDecimal is returned by ParseDecimal for anything that isn't a
// plain decimal string within this package's own precision.
var ErrInvalidDecimal = errors.New("money: invalid decimal string")

// Decimals is this service's minor-unit precision for USDT_TRC20,
// matching C1's money.Asset.Decimals() for that asset exactly.
const Decimals = 6

// Amount is a quantity of USDT_TRC20 in the ledger's minor units (see
// Decimals): Amount(1) is one millionth of one USDT, Amount(1_000000) is
// 1.000000 USDT.
type Amount int64

// FromOnChainUnits converts raw -- an amount already known to be
// expressed in onChainDecimals fractional digits -- into this package's
// Decimals-place minor-unit convention. raw must be non-negative (a
// TRC20 transfer's amount is a uint256; a caller passing anything else
// has a bug upstream) and onChainDecimals must be at least Decimals.
//
// Fractional precision below Decimals is truncated, never rounded.
// What IS an error is a value that, even after truncation, does not fit
// in an int64 -- returned rather than silently wrapped, the same
// discipline C1's own money package uses for overflow.
func FromOnChainUnits(raw *big.Int, onChainDecimals int) (Amount, error) {
	if raw == nil {
		return 0, fmt.Errorf("money: nil raw amount")
	}
	if raw.Sign() < 0 {
		return 0, fmt.Errorf("money: negative on-chain amount %s", raw)
	}
	if onChainDecimals < Decimals {
		return 0, fmt.Errorf("money: onChainDecimals %d is finer than this package's own %d-decimal floor",
			onChainDecimals, Decimals)
	}

	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(onChainDecimals-Decimals)), nil)
	scaled := new(big.Int).Quo(raw, divisor)
	if !scaled.IsInt64() {
		return 0, fmt.Errorf("money: on-chain amount %s overflows int64 minor units", raw)
	}
	return Amount(scaled.Int64()), nil
}

// ParseDecimal parses a plain decimal string ("3000.000000", "-0.5",
// "12") into an Amount, the inverse of Format. A string with more
// fractional digits than Decimals is rejected outright, matching every
// other component's own money.ParseDecimal discipline exactly (mirrored,
// not shared).
func ParseDecimal(s string) (Amount, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty string", ErrInvalidDecimal)
	}

	neg := false
	rest := s
	switch rest[0] {
	case '-':
		neg = true
		rest = rest[1:]
	case '+':
		rest = rest[1:]
	}
	if rest == "" {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}

	intPart, fracPart, hasDot := strings.Cut(rest, ".")
	if hasDot && strings.Contains(fracPart, ".") {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if intPart == "" {
		intPart = "0"
	}
	if !isDigits(intPart) || (fracPart != "" && !isDigits(fracPart)) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if len(fracPart) > Decimals {
		return 0, fmt.Errorf("%w: %q has more than %d decimal places", ErrInvalidDecimal, s, Decimals)
	}

	digits := intPart + fracPart + strings.Repeat("0", Decimals-len(fracPart))
	units, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %v", ErrInvalidDecimal, s, err)
	}
	if neg {
		units = -units
	}
	return Amount(units), nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Format renders a as a plain decimal string with exactly Decimals
// fractional digits (e.g. Amount(3000_000000).Format() -> "3000.000000"),
// never a JSON number, never routed through a float here either. a may
// be negative; FormatInt handles the math.MinInt64 edge case correctly,
// unlike negating a first would.
func (a Amount) Format() string {
	neg := a < 0
	digits := strconv.FormatInt(int64(a), 10)
	if neg {
		digits = digits[1:]
	}
	for len(digits) < Decimals+1 {
		digits = "0" + digits
	}
	intPart, fracPart := digits[:len(digits)-Decimals], digits[len(digits)-Decimals:]
	out := intPart + "." + fracPart
	if neg {
		out = "-" + out
	}
	return out
}
