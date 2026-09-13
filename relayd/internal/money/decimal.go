package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrInvalidDecimal  = errors.New("money: invalid decimal string")
	ErrTooManyDecimals = errors.New("money: too many decimal places for asset")
)

// ParseDecimal parses a plain decimal string ("2990.700000", "-0.5",
// "12") into an Amount for the given asset. It never rounds: a string
// with more fractional digits than the asset's Decimals() allows is
// rejected outright rather than truncated. Scientific notation, empty
// strings, and anything containing characters other than an optional
// leading sign, digits, and at most one '.' are rejected.
func ParseDecimal(s string, asset Asset) (Amount, error) {
	decimals, err := asset.Decimals()
	if err != nil {
		return Amount{}, err
	}
	if s == "" {
		return Amount{}, fmt.Errorf("%w: empty string", ErrInvalidDecimal)
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
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}

	intPart, fracPart, hasDot := strings.Cut(rest, ".")
	if hasDot && strings.Contains(fracPart, ".") {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if intPart == "" && fracPart == "" {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if intPart == "" {
		intPart = "0"
	}
	if !isDigits(intPart) {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if fracPart != "" && !isDigits(fracPart) {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if len(fracPart) > decimals {
		return Amount{}, fmt.Errorf("%w: %q has %d decimal place(s), %s allows %d",
			ErrTooManyDecimals, s, len(fracPart), asset, decimals)
	}

	digits := intPart + fracPart + strings.Repeat("0", decimals-len(fracPart))
	// digits has no sign and only ever grows the string ParseInt sees, so
	// a value that doesn't fit int64 surfaces as a strconv range error,
	// which we report as our own overflow error rather than leaking
	// strconv's.
	units, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q: %v", ErrOverflow, s, err)
	}
	if neg {
		negUnits, err := (Amount{Asset: asset, Units: units}).Neg()
		if err != nil {
			return Amount{}, fmt.Errorf("%w: %q", ErrOverflow, s)
		}
		units = negUnits.Units
	}

	return Amount{Asset: asset, Units: units}, nil
}

// Format renders a as a plain decimal string with exactly
// asset.Decimals() fractional digits, e.g.
// Amount{USDT_TRC20, 2990700000} -> "2990.700000".
func Format(a Amount) (string, error) {
	if err := a.validate(); err != nil {
		return "", err
	}
	decimals, _ := a.Asset.Decimals()

	neg := a.Units < 0
	// strconv.FormatInt handles math.MinInt64 correctly (unlike negating
	// it ourselves would); we then do the decimal-point placement on the
	// resulting string, never on the int64 value itself.
	digits := strconv.FormatInt(a.Units, 10)
	if neg {
		digits = digits[1:]
	}
	for len(digits) < decimals+1 {
		digits = "0" + digits
	}

	intPart := digits[:len(digits)-decimals]
	fracPart := digits[len(digits)-decimals:]

	var out string
	if decimals > 0 {
		out = intPart + "." + fracPart
	} else {
		out = intPart
	}
	if neg {
		out = "-" + out
	}
	return out, nil
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
