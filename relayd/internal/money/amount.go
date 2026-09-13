package money

import (
	"errors"
	"fmt"
	"math"
)

var (
	// ErrZeroValueAmount is returned whenever an operation is given the
	// zero-value Amount (empty Asset). There is no such thing as an
	// amount with no currency, so this is always a caller bug, never a
	// valid zero.
	ErrZeroValueAmount = errors.New("money: zero-value amount")
	ErrAssetMismatch   = errors.New("money: asset mismatch")
	ErrOverflow        = errors.New("money: overflow")
)

// Amount is a signed quantity of one Asset, expressed in that asset's
// minor units (e.g. for a 6-decimal asset, Units is the amount * 1e6).
type Amount struct {
	Asset Asset
	Units int64
}

func (a Amount) validate() error {
	if a.Asset == "" {
		return ErrZeroValueAmount
	}
	if !a.Asset.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownAsset, string(a.Asset))
	}
	return nil
}

// Add returns a+b. Both operands must carry the same, known asset, and
// the result must fit in an int64, or an error is returned. Never
// panics, never wraps silently on overflow.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := a.validate(); err != nil {
		return Amount{}, err
	}
	if err := b.validate(); err != nil {
		return Amount{}, err
	}
	if a.Asset != b.Asset {
		return Amount{}, fmt.Errorf("%w: %s vs %s", ErrAssetMismatch, a.Asset, b.Asset)
	}
	sum, ok := checkedAddInt64(a.Units, b.Units)
	if !ok {
		return Amount{}, fmt.Errorf("%w: %d + %d", ErrOverflow, a.Units, b.Units)
	}
	return Amount{Asset: a.Asset, Units: sum}, nil
}

// Sub returns a-b. Same asset and overflow rules as Add. relayd's own
// forward-leg amount (build-prompts step 8: "(received - take)") is the
// concrete caller of this.
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := a.validate(); err != nil {
		return Amount{}, err
	}
	if err := b.validate(); err != nil {
		return Amount{}, err
	}
	if a.Asset != b.Asset {
		return Amount{}, fmt.Errorf("%w: %s vs %s", ErrAssetMismatch, a.Asset, b.Asset)
	}
	diff, ok := checkedSubInt64(a.Units, b.Units)
	if !ok {
		return Amount{}, fmt.Errorf("%w: %d - %d", ErrOverflow, a.Units, b.Units)
	}
	return Amount{Asset: a.Asset, Units: diff}, nil
}

// Neg returns -a. Errors on the zero-value Amount, an unknown asset, or
// the one Units value (math.MinInt64) whose negation does not fit in an
// int64.
func (a Amount) Neg() (Amount, error) {
	if err := a.validate(); err != nil {
		return Amount{}, err
	}
	if a.Units == math.MinInt64 {
		return Amount{}, fmt.Errorf("%w: negate %d", ErrOverflow, a.Units)
	}
	return Amount{Asset: a.Asset, Units: -a.Units}, nil
}

// checkedAddInt64 adds a and b, reporting ok=false rather than wrapping
// if the mathematical result does not fit in an int64.
func checkedAddInt64(a, b int64) (sum int64, ok bool) {
	sum = a + b
	// Overflow is only possible when both operands share a sign, and in
	// that case the result must retain that sign too.
	if b > 0 && sum < a {
		return 0, false
	}
	if b < 0 && sum > a {
		return 0, false
	}
	return sum, true
}

// checkedSubInt64 subtracts b from a, reporting ok=false rather than
// wrapping if the mathematical result does not fit in an int64.
// Standard two's-complement check: overflow is only possible when a and
// b have different signs, and in that case the result must keep a's
// sign.
func checkedSubInt64(a, b int64) (diff int64, ok bool) {
	diff = a - b
	if (a^b) < 0 && (a^diff) < 0 {
		return 0, false
	}
	return diff, true
}
