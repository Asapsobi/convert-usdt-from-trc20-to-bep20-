// Package evmtx constructs unsigned BEP20 (ERC20-shaped) transfer
// transactions -- the BEP20-direction forward leg's own transaction,
// for the BEP20->TRC20 relay flow (docs/03-build/model-f-relay-build-prompts.md's
// own "BEP20 -> TRC20 is the mirror" happy-flow note). The direct
// sibling of internal/txbuild (which does the identical job for the
// TRC20 direction): pure construction only, no signing, no broadcast,
// no network call of any kind -- a caller resolves nonce/gas price
// elsewhere (a real node call) and supplies them here.
//
// This is genuinely new code, not a port: no service in this repo has
// ever built or broadcast a BEP20 transfer before now --
// depositwatcher only WATCHES BSC, it never sends -- flagged as the one
// real gap in "everything already exists to reuse" when this plan was
// written (docs/02-architecture/model-f-relay-architecture.md's own
// review this session). Built directly against go-ethereum's own
// battle-tested transaction/signing primitives (the same library
// depositwatcher already depends on for watching), not hand-rolled RLP
// encoding.
//
// The signing handoff to S1 needs no S1-side change beyond exposing an
// EVM-format address (s1/internal/slots/evm.go): go-ethereum's own
// types.EIP155Signer.SignatureValues already expects a raw 0/1 recovery
// byte as the signature's 65th byte and does the v=recid+35+2*chainID
// conversion internally -- exactly the shape
// s1/internal/kmssign.Wrapper.Sign already returns (that function's own
// doc comment calls it "TRON/Ethereum-shaped" for precisely this
// reason). Nothing about S1's own signing mechanics needed to change.
package evmtx

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"relayd/internal/money"
)

// USDTContractAddress is the real, verified Binance-Peg BSC-USD
// (USDT_BEP20) mainnet contract -- duplicated from
// depositwatcher/cmd/watcherd/engine.go's own verified constant
// (defaultContractAddress there), confirmed independently against
// BscScan per that file's own doc comment.
const USDTContractAddress = "0x55d398326f99059fF775485246999027B3197955"

// BSCChainID is BSC mainnet's own EIP-155 chain id -- required for
// transaction replay protection; a signature valid on one EVM chain
// must not be replayable on another.
const BSCChainID = 56

// DefaultGasLimit bounds the gas a standard ERC20 transfer call needs --
// real transfers against this exact contract typically use well under
// this; headroom against Tether's own blacklist-check branch inside the
// contract (the same real cost txbuild.go's own defaultFeeLimit budgets
// for on the TRON side), not a tuned-to-the-wei minimum.
const DefaultGasLimit = 100_000

var (
	// ErrInvalidAddress means recipientAddress failed EVM address
	// parsing (not 20 bytes / not valid hex).
	ErrInvalidAddress = errors.New("evmtx: invalid EVM address")

	// ErrNonPositiveAmount means amount was zero or negative.
	ErrNonPositiveAmount = errors.New("evmtx: amount must be positive")

	// ErrNilGasPrice means TxParams.GasPrice was nil -- a caller bug,
	// never a reachable "zero gas price" real value (a real node's own
	// eth_gasPrice never returns nil).
	ErrNilGasPrice = errors.New("evmtx: gas price must not be nil")
)

// TxParams is the chain state a caller resolved elsewhere (a real
// node's own eth_getTransactionCount / eth_gasPrice calls, outside this
// package) and supplies here so BuildTransfer never has to make one
// itself -- the direct analogue of txbuild.BlockReference.
type TxParams struct {
	Nonce    uint64
	GasPrice *big.Int // wei
	GasLimit uint64   // DefaultGasLimit if zero
}

// transferMethodID is keccak256("transfer(address,uint256)")[:4] --
// ERC20's (and BEP20's, identical ABI) standard transfer selector,
// 0xa9059cbb, the same well-known value
// dispatcher/internal/txbuild.go's own transferMethod resolves to via
// abi.Pack for the TRC20 side (TRC20 mirrors ERC20's ABI exactly).
var transferMethodID = crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]

// BuildTransfer constructs an unsigned, legacy (type-0), EIP-155-signed
// BEP20 transfer of amount USDT from nothing in particular (the sender
// is never named in the transaction itself -- an EVM transaction's
// sender is recovered FROM its own signature, never stated) to
// recipientAddress, calling USDTContractAddress. Returns the
// transaction and the digest (EIP-155 signing hash) relayd must ask S1
// to sign -- the direct analogue of txbuild.BuildTransfer + txbuild.Digest,
// returned together here since go-ethereum's own API computes both from
// one types.Transaction rather than as two independent byte-slice
// operations.
func BuildTransfer(recipientAddress string, amount money.Amount, params TxParams) (tx *types.Transaction, digest [32]byte, err error) {
	if !common.IsHexAddress(recipientAddress) {
		return nil, [32]byte{}, fmt.Errorf("%w: recipientAddress %q", ErrInvalidAddress, recipientAddress)
	}
	if amount.Units <= 0 {
		return nil, [32]byte{}, fmt.Errorf("%w: got %d", ErrNonPositiveAmount, amount.Units)
	}
	if params.GasPrice == nil {
		return nil, [32]byte{}, ErrNilGasPrice
	}
	gasLimit := params.GasLimit
	if gasLimit == 0 {
		gasLimit = DefaultGasLimit
	}

	recipient := common.HexToAddress(recipientAddress)
	contract := common.HexToAddress(USDTContractAddress)
	data := buildCalldata(recipient, amount)

	unsigned := types.NewTransaction(params.Nonce, contract, big.NewInt(0), gasLimit, params.GasPrice, data)
	signer := types.NewEIP155Signer(big.NewInt(BSCChainID))
	digest = signer.Hash(unsigned)
	return unsigned, digest, nil
}

// buildCalldata ABI-encodes transfer(address,uint256): 4-byte selector,
// the recipient left-padded to 32 bytes, the amount left-padded to 32
// bytes -- the same encoding rule
// dispatcher/internal/txbuild.go's own buildCalldata comment describes
// (TRC20 mirrors ERC20's ABI exactly), applied here directly rather
// than through a parsed-ABI helper, since a single fixed method
// signature needs no general-purpose ABI machinery.
func buildCalldata(recipient common.Address, amount money.Amount) []byte {
	data := make([]byte, 4+32+32)
	copy(data[0:4], transferMethodID)
	copy(data[4+12:4+32], recipient.Bytes()) // last 20 of 32 bytes, per EVM's own address-as-word convention
	amountBytes := new(big.Int).SetInt64(amount.Units).Bytes()
	copy(data[4+64-len(amountBytes):4+64], amountBytes)
	return data
}

// WithSignature reconstructs the final, broadcast-ready signed
// transaction from tx (BuildTransfer's own first return value) and
// sig, S1's own 65-byte r||s||v signature (v a raw 0/1 recovery byte --
// s1/internal/kmssign.Wrapper.Sign's own contract, needing no
// conversion: go-ethereum's EIP155Signer.SignatureValues does the
// v=recid+35+2*chainID math internally from exactly this input shape).
func WithSignature(tx *types.Transaction, sig [65]byte) (*types.Transaction, error) {
	signer := types.NewEIP155Signer(big.NewInt(BSCChainID))
	signed, err := tx.WithSignature(signer, sig[:])
	if err != nil {
		return nil, fmt.Errorf("evmtx: applying signature: %w", err)
	}
	return signed, nil
}

// TxHash returns signed's own transaction hash -- what a caller polls
// a real node's finality endpoint with, the EVM analogue of
// txbuild.Digest's own txID role on the TRON side (though here it is
// computed over the SIGNED transaction, per Ethereum's own convention,
// not over raw_data before signing the way TRON's txID is).
func TxHash(signed *types.Transaction) [32]byte {
	return signed.Hash()
}

// MarshalForBroadcast returns signed's own RLP-encoded bytes, ready to
// submit via eth_sendRawTransaction.
func MarshalForBroadcast(signed *types.Transaction) ([]byte, error) {
	raw, err := signed.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("evmtx: encoding signed transaction: %w", err)
	}
	return raw, nil
}
