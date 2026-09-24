package chain

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"depositwatcher/internal/money"
)

// TokenDecimals is this token's OWN on-chain decimals(), NOT
// this service's ledger-facing money.Decimals. Verified live against the
// real contract on BSC mainnet (0x55d398326f99059fF775485246999027B3197955,
// Binance-Peg BSC-USD) via a direct eth_call to decimals() on 2026-09-05,
// which returned 18 -- not the 6 a reader familiar with Ethereum mainnet's
// real USDT might assume, and not copy-pasted from the build spec's own
// citation, per that spec's explicit instruction to verify independently.
// Every raw Transfer amount this package sees is in these 18-decimal units
// until ParseTransferLog rescales it.
const TokenDecimals = 18

// transferEventTopic is keccak256("Transfer(address,address,uint256)"),
// the ERC20 Transfer event's signature hash -- computed here, not
// hardcoded, and cross-checked in classify_test.go against the widely
// published value for this exact event signature.
var transferEventTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// ErrWrongToken means a log does not have the shape of a
// Transfer(address,address,uint256) event: wrong topic count, wrong topic0,
// or a non-32-byte data payload. In the real pipeline this should be
// unreachable -- LogsAt's own eth_getLogs query already filters by contract
// address and this exact topic before a log ever reaches here -- but a
// provider is untrusted input, not a guarantee (see the C2 build spec's
// WrongToken classification: "should be near-impossible given the filter,
// but classify defensively").
var ErrWrongToken = errors.New("chain: log does not match the Transfer(address,address,uint256) shape")

// ParseTransferLog decodes log as an ERC20 Transfer event and rescales its
// raw on-chain amount into this service's ledger-facing money.Amount. It
// does not check log.Address against the USDT BEP20 contract -- that
// filtering is LogsAt's job, done once at the eth_getLogs query level for
// every log in a batch, not re-done per log here.
func ParseTransferLog(log types.Log) (from, to common.Address, amount money.Amount, err error) {
	if len(log.Topics) != 3 || log.Topics[0] != transferEventTopic {
		return common.Address{}, common.Address{}, 0, fmt.Errorf("%w: log %s:%d",
			ErrWrongToken, log.TxHash, log.Index)
	}
	if len(log.Data) != 32 {
		return common.Address{}, common.Address{}, 0, fmt.Errorf("%w: log %s:%d has non-standard data length %d",
			ErrWrongToken, log.TxHash, log.Index, len(log.Data))
	}

	from = common.BytesToAddress(log.Topics[1].Bytes())
	to = common.BytesToAddress(log.Topics[2].Bytes())

	raw := new(big.Int).SetBytes(log.Data)
	amount, err = money.FromOnChainUnits(raw, TokenDecimals)
	if err != nil {
		return common.Address{}, common.Address{}, 0, fmt.Errorf("chain: parsing transfer amount for log %s:%d: %w",
			log.TxHash, log.Index, err)
	}
	return from, to, amount, nil
}

// Classification is a candidate deposit's outcome against what its order
// quoted, decided purely from amounts. It does not cover the shape-level
// defensive case (ParseTransferLog's ErrWrongToken) -- a log that fails to
// parse never reaches ClassifyAgainstOrder at all, so there is no
// WrongToken value returned from here; a caller that catches ErrWrongToken
// tags the anomaly itself.
type Classification int

const (
	Exact     Classification = iota // matches the order's quoted amount exactly
	Overpay                         // more than quoted
	Underpay                        // less than quoted, at or above the dust floor
	Dust                            // nonzero but below the dust floor
	ZeroValue                       // Transfer event with amount 0 (legal on-chain, meaningless here)
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

// DefaultDustFloor is the build spec's own example nonzero-but-negligible
// threshold ("< $1 equiv"), made concrete for callers that don't need
// anything different.
const DefaultDustFloor = money.Amount(1_000000) // 1.000000 USDT_BEP20

// ClassifyAgainstOrder compares amount (already parsed by ParseTransferLog)
// against quoted -- the order's own amount_in -- using dustFloor as the
// nonzero-but-negligible threshold.
//
// This takes quoted as a money.Amount directly rather than a WatchedAddress
// (the build spec's own pseudocode signature for this function). C2.1's
// watched_addresses table deliberately stores no amount at all -- "no
// ledger of money," per the build spec's WHAT C2 IS NOT section -- and
// C2.9's inbound Assign contract likewise never receives one from whoever
// creates the order. The quoted amount has to come from wherever C2
// actually learns it: C1's own order record, via GET
// /v1/orders/{external_id} (see the build spec's "Discovering what to
// watch for"; C2.7's ledgerclient is what will fetch it in practice) --
// never from WatchedAddress, which has no field to hold it.
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
