package sweeps

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"relayd/internal/money"
)

func TestSettings_RoundTripsAsDecimalUSDT(t *testing.T) {
	in := Settings{Enabled: true, Interval: 90 * time.Minute, MinAmount: map[money.Asset]int64{
		money.USDT_BEP20: 12_500000, money.USDT_TRC20: 50_000000,
	}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"enabled":true,"interval_minutes":90,"min_amount":{"USDT_BEP20":"12.500000","USDT_TRC20":"50.000000"}}`
	if string(raw) != want {
		t.Fatalf("got %s, want %s", raw, want)
	}
	var out Settings
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Enabled != in.Enabled || out.Interval != in.Interval || out.MinAmount[money.USDT_BEP20] != 12_500000 || out.MinAmount[money.USDT_TRC20] != 50_000000 {
		t.Fatalf("round trip changed the settings: %+v", out)
	}
}

func TestSettings_Validate(t *testing.T) {
	good := Defaults()
	if err := good.Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	for name, s := range map[string]Settings{
		"interval too short": {Interval: time.Minute, MinAmount: good.MinAmount},
		"no TRON minimum":    {Interval: time.Hour, MinAmount: map[money.Asset]int64{money.USDT_BEP20: 1}},
		"zero minimum":       {Interval: time.Hour, MinAmount: map[money.Asset]int64{money.USDT_BEP20: 0, money.USDT_TRC20: 1}},
	} {
		if err := s.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
	var s Settings
	if err := json.Unmarshal([]byte(`{"enabled":true,"interval_minutes":60,"min_amount":{"BTC":"1"}}`), &s); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown asset must be rejected, got %v", err)
	}
}
