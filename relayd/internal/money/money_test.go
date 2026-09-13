package money

import "testing"

func TestParseDecimalAndFormatRoundTrip(t *testing.T) {
	cases := []struct {
		in    string
		asset Asset
	}{
		{"2990.700000", USDT_TRC20},
		{"-0.5", USDT_BEP20},
		{"12", TRX},
		{"0.000000001", BNB},
	}
	for _, c := range cases {
		amt, err := ParseDecimal(c.in, c.asset)
		if err != nil {
			t.Fatalf("ParseDecimal(%q, %s): %v", c.in, c.asset, err)
		}
		out, err := Format(amt)
		if err != nil {
			t.Fatalf("Format(%v): %v", amt, err)
		}
		back, err := ParseDecimal(out, c.asset)
		if err != nil {
			t.Fatalf("re-parsing %q: %v", out, err)
		}
		if back != amt {
			t.Errorf("round trip mismatch for %q: got %v, want %v", c.in, back, amt)
		}
	}
}

func TestParseDecimalTooManyDecimals(t *testing.T) {
	if _, err := ParseDecimal("1.1234567", USDT_TRC20); err == nil {
		t.Fatal("expected ErrTooManyDecimals for 7 fractional digits on a 6-decimal asset")
	}
}

func TestParseDecimalInvalid(t *testing.T) {
	for _, s := range []string{"", "abc", "1.2.3", "1e10"} {
		if _, err := ParseDecimal(s, USDT_TRC20); err == nil {
			t.Errorf("ParseDecimal(%q): expected error, got none", s)
		}
	}
}

func TestAmountSubForForwardLeg(t *testing.T) {
	received := Amount{Asset: USDT_TRC20, Units: 100_000000}
	commission := Amount{Asset: USDT_TRC20, Units: 250000}
	forward, err := received.Sub(commission)
	if err != nil {
		t.Fatalf("Sub: %v", err)
	}
	want := Amount{Asset: USDT_TRC20, Units: 99_750000}
	if forward != want {
		t.Errorf("got %v, want %v", forward, want)
	}
}

func TestAmountAssetMismatch(t *testing.T) {
	a := Amount{Asset: USDT_TRC20, Units: 1}
	b := Amount{Asset: USDT_BEP20, Units: 1}
	if _, err := a.Add(b); err == nil {
		t.Fatal("expected ErrAssetMismatch adding two different assets")
	}
	if _, err := a.Sub(b); err == nil {
		t.Fatal("expected ErrAssetMismatch subtracting two different assets")
	}
}
