package pricing

import (
	"encoding/json"
	"errors"
	"testing"

	"relayd/internal/money"
)

func TestProfit_RoundsDownAndHonorsTheFloor(t *testing.T) {
	c := Config{ProfitBPS: 25, MinProfit: 0, MinAmountIn: 1, MaxAmountIn: 10}
	cases := map[int64]int64{
		2_000000:              5000,               // 0.25% of 2 USDT
		1_999999:              4999,               // rounds down, never up
		100_000000:            250000,             // 0.25 USDT
		1:                     0,                  // too small for any profit
		9_000_000_000_000_000: 22_500_000_000_000, // no overflow near the int64 limit
	}
	for received, want := range cases {
		if got := c.Profit(received); got != want {
			t.Errorf("Profit(%d) = %d, want %d", received, got, want)
		}
	}
	c.MinProfit = 500000 // 0.50 USDT floor
	if got := c.Profit(2_000000); got != 500000 {
		t.Errorf("expected the 0.50 floor to apply, got %d", got)
	}
}

func TestSplit_RefusesToForwardNothing(t *testing.T) {
	c := Config{ProfitBPS: 25, MinProfit: 1_000000, MinAmountIn: 2_000000, MaxAmountIn: 10_000000}
	if _, _, ok := c.Split(money.Amount{Asset: money.USDT_BEP20, Units: 1_000000}); ok {
		t.Fatal("a deposit equal to the profit floor left nothing to forward but was accepted")
	}
	profit, forward, ok := c.Split(money.Amount{Asset: money.USDT_BEP20, Units: 5_000000})
	if !ok || profit.Units+forward.Units != 5_000000 || forward.Asset != money.USDT_BEP20 {
		t.Fatalf("split must cover the deposit exactly in its own asset, got %v + %v", profit, forward)
	}
}

func TestValidate(t *testing.T) {
	good := Config{ProfitBPS: 25, MinProfit: 0, MinAmountIn: 5_000000, MaxAmountIn: 10_000_000000}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, c := range map[string]Config{
		"negative bps":      {ProfitBPS: -1, MinAmountIn: 1, MaxAmountIn: 2},
		"100% profit":       {ProfitBPS: 10_000, MinAmountIn: 1, MaxAmountIn: 2},
		"zero minimum":      {ProfitBPS: 25, MinAmountIn: 0, MaxAmountIn: 2},
		"max below min":     {ProfitBPS: 25, MinAmountIn: 5, MaxAmountIn: 2},
		"floor eats orders": {ProfitBPS: 25, MinProfit: 5, MinAmountIn: 5, MaxAmountIn: 9},
	} {
		if err := c.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
}

func TestJSONRoundTripUsesDecimalStrings(t *testing.T) {
	c := Config{ProfitBPS: 30, MinProfit: 500000, MinAmountIn: 5_000000, MaxAmountIn: 10_000_000000}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"profit_bps":30,"min_profit":"0.500000","min_amount_in":"5.000000","max_amount_in":"10000.000000"}` {
		t.Fatalf("unexpected JSON %s", raw)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil || back != c {
		t.Fatalf("round trip: got %+v, %v", back, err)
	}
}
