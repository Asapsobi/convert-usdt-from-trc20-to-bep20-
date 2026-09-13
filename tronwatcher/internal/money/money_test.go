package money

import (
	"math/big"
	"strings"
	"testing"
)

func TestFromOnChainUnits_TruncatesBelowLedgerPrecision(t *testing.T) {
	tests := []struct {
		name string
		raw  string // decimal string, avoids int64-literal overflow for 18-decimal values
		want Amount
	}{
		{"exact, no fractional remainder", "3000000000000000000", 3_000000},         // 3 USDT (10^18 raw units per whole token)
		{"truncates the 12 extra on-chain digits", "3000123456789012345", 3_000123}, // truncated, not rounded up
		{"one wei is far below ledger precision", "1", 0},
		{"largest value that truncates to zero", "999999999999", 0},     // < 1e12 raw units == 0 minor units
		{"smallest value that survives truncation", "1000000000000", 1}, // == 1e12 raw units == 1 minor unit
		{"zero", "0", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, ok := new(big.Int).SetString(tc.raw, 10)
			if !ok {
				t.Fatalf("bad fixture: %q is not a valid decimal", tc.raw)
			}
			got, err := FromOnChainUnits(raw, 18)
			if err != nil {
				t.Fatalf("FromOnChainUnits(%s, 18): unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("FromOnChainUnits(%s, 18) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestFromOnChainUnits_TRC20SixDecimalsIsANoOpRescale exercises the real
// on-chain decimals this service actually sees (USDT-TRC20 is 6
// decimals, unlike BEP20's surprising 18) -- onChainDecimals ==
// Decimals means the divisor is 10^0 == 1, a pure pass-through.
func TestFromOnChainUnits_TRC20SixDecimalsIsANoOpRescale(t *testing.T) {
	raw := big.NewInt(3_000123)
	got, err := FromOnChainUnits(raw, 6)
	if err != nil {
		t.Fatalf("FromOnChainUnits(3000123, 6): unexpected error: %v", err)
	}
	if got != 3_000123 {
		t.Errorf("FromOnChainUnits(3000123, 6) = %d, want 3000123", got)
	}
}

func TestFromOnChainUnits_RejectsNegative(t *testing.T) {
	_, err := FromOnChainUnits(big.NewInt(-1), 18)
	if err == nil {
		t.Fatal("expected an error for a negative raw amount, got nil")
	}
}

func TestFromOnChainUnits_RejectsNil(t *testing.T) {
	_, err := FromOnChainUnits(nil, 18)
	if err == nil {
		t.Fatal("expected an error for a nil raw amount, got nil")
	}
}

func TestFromOnChainUnits_RejectsOnChainDecimalsBelowLedgerFloor(t *testing.T) {
	_, err := FromOnChainUnits(big.NewInt(100), 3)
	if err == nil {
		t.Fatal("expected an error when onChainDecimals is below this package's own Decimals, got nil")
	}
}

func TestFromOnChainUnits_OverflowsInt64(t *testing.T) {
	// int64 max is ~9.22e18; a raw 18-decimal value of 1e25 scales down to
	// 1e13 minor units, which fits -- so push well past int64 max even
	// after the /1e12 rescale: 1e31 raw -> 1e19 minor units, over the
	// int64 ceiling of ~9.22e18.
	huge, ok := new(big.Int).SetString(strings.Repeat("9", 31), 10)
	if !ok {
		t.Fatal("bad fixture")
	}
	_, err := FromOnChainUnits(huge, 18)
	if err == nil {
		t.Fatal("expected an overflow error, got nil")
	}
}

func TestAmount_Format(t *testing.T) {
	tests := []struct {
		amount Amount
		want   string
	}{
		{3000_000000, "3000.000000"},
		{-3000_000000, "-3000.000000"},
		{1, "0.000001"},
		{-1, "-0.000001"},
		{0, "0.000000"},
	}
	for _, tc := range tests {
		if got := tc.amount.Format(); got != tc.want {
			t.Errorf("Amount(%d).Format() = %q, want %q", tc.amount, got, tc.want)
		}
	}
}

func TestParseDecimal_RoundTripsWithFormat(t *testing.T) {
	amounts := []Amount{0, 1, -1, 3000_000000, -3000_000000, 999999_999999}
	for _, a := range amounts {
		got, err := ParseDecimal(a.Format())
		if err != nil {
			t.Fatalf("ParseDecimal(%q): %v", a.Format(), err)
		}
		if got != a {
			t.Errorf("ParseDecimal(Format(%d)) = %d, want %d", a, got, a)
		}
	}
}

func TestParseDecimal_Table(t *testing.T) {
	tests := []struct {
		in      string
		want    Amount
		wantErr bool
	}{
		{"3000.000000", 3000_000000, false},
		{"3000", 3000_000000, false},
		{"-0.5", -500000, false},
		{"0.000001", 1, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1.2.3", 0, true},
		{"1.0000001", 0, true}, // more than Decimals fractional digits
	}
	for _, tc := range tests {
		got, err := ParseDecimal(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDecimal(%q): expected an error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDecimal(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDecimal(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
