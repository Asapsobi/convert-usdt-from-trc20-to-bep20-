package chain

import (
	"fmt"

	"tronwatcher/internal/money"
)

// USDTTRC20ContractDecimals is this token's OWN on-chain decimals(),
// NOT this service's ledger-facing money.Decimals -- kept as an explicit
// constant for the same reason depositwatcher/internal/chain's own
// usdtBEP20ContractDecimals is, even though for USDT-TRC20 the two
// numbers happen to be equal (6), unlike BEP20's surprising 18. Real
// contract: TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t, the same address
// dispatcher/internal/txbuild/txbuild.go already verified and uses for
// the payout side.
const USDTTRC20ContractDecimals = 6

// USDTTRC20ContractAddress is the real, verified USDT-TRC20 contract on
// TRON mainnet -- duplicated from dispatcher/internal/txbuild/txbuild.go's
// own constant (separate Go modules, no shared internal package, same
// convention as every other service here).
const USDTTRC20ContractAddress = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

// ParseTransferValue rescales a Transfer's raw on-chain value into this
// service's ledger-facing money.Amount. Unlike
// depositwatcher/internal/chain.ParseTransferLog, there is no log shape
// to validate here -- Provider.ScanTRC20Transfers already returns
// structured, TronGrid-parsed fields, not a raw log this package would
// otherwise have to decode itself.
func ParseTransferValue(t Transfer) (money.Amount, error) {
	amount, err := money.FromOnChainUnits(t.ValueRaw, USDTTRC20ContractDecimals)
	if err != nil {
		return 0, fmt.Errorf("chain: parsing transfer amount for %s: %w", t.TxID, err)
	}
	return amount, nil
}

// Classification is a candidate deposit's outcome against what its order
// quoted, decided purely from amounts -- identical in shape and meaning
// to depositwatcher/internal/chain.Classification (mirrored, not
// shared).
type Classification int

const (
	Exact     Classification = iota // matches the order's quoted amount exactly
	Overpay                         // more than quoted
	Underpay                        // less than quoted, at or above the dust floor
	Dust                            // nonzero but below the dust floor
	ZeroValue                       // a Transfer with amount 0 (legal on-chain, meaningless here)
)

func (c Classification) String() string {
	switch c {
	case Exact:
		return "Exact"
	case Overpay:
		return "Overpay"
	case Underpay:
		return "Underpay"
	case Dust:
		return "Dust"
	case ZeroValue:
		return "ZeroValue"
	default:
		return fmt.Sprintf("Classification(%d)", int(c))
	}
}

// DefaultDustFloor mirrors depositwatcher/internal/chain's own example
// nonzero-but-negligible threshold.
const DefaultDustFloor = money.Amount(1_000000) // 1.000000 USDT_TRC20

// ClassifyAgainstOrder compares amount against quoted -- the order's own
// amount_in -- using dustFloor as the nonzero-but-negligible threshold.
// Identical logic to depositwatcher/internal/chain.ClassifyAgainstOrder
// (mirrored, not shared): this comparison is purely about money, not
// about which chain the deposit arrived on.
func ClassifyAgainstOrder(amount, quoted, dustFloor money.Amount) Classification {
	switch {
	case amount == 0:
		return ZeroValue
	case amount == quoted:
		return Exact
	case amount > quoted:
		return Overpay
	case amount < dustFloor:
		return Dust
	default:
		return Underpay
	}
}
