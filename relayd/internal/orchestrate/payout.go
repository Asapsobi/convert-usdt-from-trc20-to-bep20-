package orchestrate

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"relayd/internal/evmbroadcast"
	"relayd/internal/evmtx"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/tronbroadcast"
	"relayd/internal/txbuild"
)

// A leg is only marked complete once the vendor's payout is checked on the
// chain it pays customers on (product goals §6: "marked as completed once
// the required confirmation conditions are met"): the vendor's payout
// transaction must be final and must pay the customer's own destination
// address in USDT. The amount recorded as paid is what the chain shows.

// EVMPayoutReader reads the token transfers a BSC transaction made
// (*evmbroadcast.Client).
type EVMPayoutReader interface {
	TokenTransfers(ctx context.Context, txHash string) ([]evmbroadcast.TokenTransfer, bool, error)
}

// TRONPayoutReader reads the token transfers a TRON transaction made
// (*tronbroadcast.FinalityReader).
type TRONPayoutReader interface {
	TokenTransfers(ctx context.Context, txID string) ([]tronbroadcast.TokenTransfer, bool, error)
}

type payoutVerdict int

const (
	payoutUnchecked payoutVerdict = iota // no reader for that chain: the vendor's word stands
	payoutPending                        // not final (or not visible) yet
	payoutWrong                          // final, but doesn't pay the customer in USDT
	payoutPaid
)

// checkPayout looks up the vendor's payout txID for leg on the chain the
// customer is paid on, returning what it paid the customer's address.
func (o *Orchestrator) checkPayout(ctx context.Context, leg relay.Leg, txID string) (payoutVerdict, money.Amount, string, error) {
	asset := leg.AmountOutExpected.Asset
	var raw *big.Int
	var final bool
	var decimals int
	if leg.Direction == relay.BEP20ToTRC20 {
		if o.TRONPayouts == nil {
			return payoutUnchecked, money.Amount{}, "", nil
		}
		transfers, fin, err := o.TRONPayouts.TokenTransfers(ctx, txID)
		if err != nil {
			return 0, money.Amount{}, "", err
		}
		final, decimals, raw = fin, txbuild.USDTOnChainDecimals, new(big.Int)
		for _, t := range transfers {
			if t.Token == txbuild.USDTContractAddress && t.To == leg.DestinationAddress {
				raw.Add(raw, t.Amount)
			}
		}
	} else {
		if o.EVMPayouts == nil {
			return payoutUnchecked, money.Amount{}, "", nil
		}
		transfers, fin, err := o.EVMPayouts.TokenTransfers(ctx, txID)
		if err != nil {
			return 0, money.Amount{}, "", err
		}
		final, decimals, raw = fin, evmtx.USDTOnChainDecimals, new(big.Int)
		for _, t := range transfers {
			if strings.EqualFold(t.Token, evmtx.USDTContractAddress) && strings.EqualFold(t.To, leg.DestinationAddress) {
				raw.Add(raw, t.Amount)
			}
		}
	}
	if !final {
		return payoutPending, money.Amount{}, "", nil
	}
	if raw.Sign() == 0 {
		return payoutWrong, money.Amount{}, fmt.Sprintf("transaction %s is final but pays no USDT to the customer's address %s", txID, leg.DestinationAddress), nil
	}
	internal, err := asset.Decimals()
	if err != nil {
		return 0, money.Amount{}, "", err
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-internal)), nil)
	units := new(big.Int).Quo(raw, scale)
	if !units.IsInt64() {
		return payoutWrong, money.Amount{}, fmt.Sprintf("transaction %s pays an implausible amount (%s raw units)", txID, raw), nil
	}
	return payoutPaid, money.Amount{Asset: asset, Units: units.Int64()}, "", nil
}
