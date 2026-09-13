package chain

import (
	"math/big"
	"testing"
	"time"

	"tronwatcher/internal/money"
)

func TestParseTransferValue(t *testing.T) {
	tr := Transfer{TxID: "tx1", ValueRaw: big.NewInt(3_000123), BlockTimestamp: time.Now()}
	got, err := ParseTransferValue(tr)
	if err != nil {
		t.Fatalf("ParseTransferValue: %v", err)
	}
	if got != 3_000123 {
		t.Errorf("got %d, want 3000123", got)
	}
}

func TestClassifyAgainstOrder(t *testing.T) {
	dustFloor := DefaultDustFloor
	quoted := money.Amount(100_000000)

	tests := []struct {
		name   string
		amount money.Amount
		want   Classification
	}{
		{"zero", 0, ZeroValue},
		{"exact", 100_000000, Exact},
		{"overpay", 100_000001, Overpay},
		{"dust", 500000, Dust},
		{"underpay above dust", 50_000000, Underpay},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyAgainstOrder(tc.amount, quoted, dustFloor)
			if got != tc.want {
				t.Errorf("ClassifyAgainstOrder(%d, %d, %d) = %s, want %s", tc.amount, quoted, dustFloor, got, tc.want)
			}
		})
	}
}
