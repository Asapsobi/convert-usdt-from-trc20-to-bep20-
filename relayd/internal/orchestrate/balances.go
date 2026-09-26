package orchestrate

import (
	"context"
	"fmt"
	"math/big"

	"relayd/internal/evmtx"
	"relayd/internal/transfers"
	"relayd/internal/txbuild"
)

// Balance is what one address holds on-chain right now.
type Balance struct {
	USDTRaw      *big.Int // raw token units
	USDTDecimals int      // the token's on-chain decimals
	NativeRaw    *big.Int // wei (BSC) or sun (TRON)
	NativeName   string   // "BNB" or "TRX"
	// Energy and Bandwidth are a TRON address's available resources.
	Energy, Bandwidth int64
}

// BalanceOf reads address's USDT and native balance on chain -- the
// administrator's view of every deposit wallet and treasury.
func (o *Orchestrator) BalanceOf(ctx context.Context, chain transfers.Chain, address string) (Balance, error) {
	switch chain {
	case transfers.BSC:
		usdt, err := o.EVMChain.TokenBalance(ctx, address)
		if err != nil {
			return Balance{}, fmt.Errorf("reading %s's USDT: %w", address, err)
		}
		bnb, err := o.EVMChain.NativeBalance(ctx, address)
		if err != nil {
			return Balance{}, fmt.Errorf("reading %s's BNB: %w", address, err)
		}
		return Balance{USDTRaw: usdt, USDTDecimals: evmtx.USDTOnChainDecimals, NativeRaw: bnb, NativeName: "BNB"}, nil
	case transfers.TRON:
		usdt, err := o.Chain.TokenBalance(ctx, address)
		if err != nil {
			return Balance{}, fmt.Errorf("reading %s's USDT: %w", address, err)
		}
		res, err := o.Chain.AccountResources(ctx, address)
		if err != nil {
			return Balance{}, fmt.Errorf("reading %s's TRX and resources: %w", address, err)
		}
		return Balance{USDTRaw: usdt, USDTDecimals: txbuild.USDTOnChainDecimals, NativeRaw: big.NewInt(res.BalanceSun),
			NativeName: "TRX", Energy: res.Energy, Bandwidth: res.Bandwidth}, nil
	}
	return Balance{}, fmt.Errorf("unknown chain %q", chain)
}

// TreasuryAddress is where top-ups come from on chain.
func (o *Orchestrator) TreasuryAddress(chain transfers.Chain) string { return o.treasuryAddress(chain) }

// SweepDestination is where swept profit goes on chain.
func (o *Orchestrator) SweepDestination(chain transfers.Chain) string {
	return o.sweepDestination(chain)
}
