// Package txbuild constructs unsigned TRC20 transfer transactions --
// the TRC20-direction forward leg's own transaction, for the
// TRC20->BEP20 relay flow's step 8 (per docs/03-build/
// model-f-relay-build-prompts.md's own happy flow).
//
// Copied from dispatcher/internal/txbuild/txbuild.go, NOT the whole
// package: only BuildTransfer/Digest, the single-transfer construction
// (pure, no network call, no signing) -- that file's own
// multisend.go (Sweep-tier batching, a not-yet-deployed contract) is
// explicitly out of scope here, per docs/02-architecture/
// model-f-relay-architecture.md §2's own note: "No sweep-tier batching
// ... a single relay order is always exactly one forward transfer."
// Duplicated rather than imported: separate Go modules, no shared
// internal package, same convention as every other service here. The
// one real change from dispatcher's own version: amount is this
// module's own money.Amount (which carries an Asset field, unlike
// dispatcher's single-asset money package) -- callers must pass a
// USDT_TRC20-denominated amount; this package does not itself re-check
// the asset (relayd's own orchestrate loop is what decides which
// forward-leg builder to call for a given direction).
package txbuild

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/abi"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"relayd/internal/money"
)

// USDTContractAddress is the one real, permanent USDT-TRC20 contract --
// duplicated from dispatcher/internal/txbuild's own verified constant.
const USDTContractAddress = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

// USDTOnChainDecimals is USDT-TRC20's own on-chain decimal precision --
// the same 6 as money.Amount's internal units, which is why
// buildCalldata encodes Units unscaled (unlike evmtx, whose BEP20 USDT
// has 18).
const USDTOnChainDecimals = 6

// transferMethod is USDT's (and every standard TRC20's) transfer method
// signature.
const transferMethod = "transfer(address,uint256)"

// defaultFeeLimit bounds the energy/bandwidth this transaction may
// burn -- 100 TRX in sun, matching dispatcher's own verified value.
const defaultFeeLimit = 100_000_000

var (
	// ErrInvalidAddress means fromAddress or recipientAddress failed
	// TRON's own base58check decoding.
	ErrInvalidAddress = errors.New("txbuild: invalid TRON address")

	// ErrNonPositiveAmount means amount was zero or negative -- never a
	// valid forward transfer.
	ErrNonPositiveAmount = errors.New("txbuild: amount must be positive")
)

// BlockReference is the recent chain state a caller resolved elsewhere
// (a single read-only node call, outside this package) -- identical
// shape and reasoning to dispatcher/internal/txbuild.BlockReference.
type BlockReference struct {
	BlockNumber int64
	BlockHash   [32]byte
	Timestamp   time.Time
	Expiration  time.Time
}

// BuildTransfer constructs an unsigned TRC20 transfer of amount USDT
// from fromAddress to recipientAddress, calling USDTContractAddress.
// Deterministic: identical arguments produce byte-identical output.
// fromAddress and recipientAddress are validated (TRON base58check)
// before anything else runs.
func BuildTransfer(fromAddress, recipientAddress string, amount money.Amount, ref BlockReference) ([]byte, error) {
	owner, err := address.Base58ToAddress(fromAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: fromAddress %q: %v", ErrInvalidAddress, fromAddress, err)
	}
	recipient, err := address.Base58ToAddress(recipientAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: recipientAddress %q: %v", ErrInvalidAddress, recipientAddress, err)
	}
	contract, err := address.Base58ToAddress(USDTContractAddress)
	if err != nil {
		return nil, fmt.Errorf("txbuild: internal error: USDTContractAddress is invalid: %w", err)
	}
	if amount.Units <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrNonPositiveAmount, amount.Units)
	}

	data, err := buildCalldata(recipient.String(), amount.Units)
	if err != nil {
		return nil, fmt.Errorf("txbuild: encoding transfer calldata: %w", err)
	}

	trigger := &core.TriggerSmartContract{
		OwnerAddress:    owner.Bytes(),
		ContractAddress: contract.Bytes(),
		Data:            data,
	}
	parameter, err := anypb.New(trigger)
	if err != nil {
		return nil, fmt.Errorf("txbuild: wrapping TriggerSmartContract: %w", err)
	}

	refBlockBytes := []byte{byte(ref.BlockNumber >> 8), byte(ref.BlockNumber)}
	refBlockHash := ref.BlockHash[8:16]

	raw := &core.TransactionRaw{
		RefBlockBytes: refBlockBytes,
		RefBlockHash:  refBlockHash,
		Expiration:    ref.Expiration.UnixMilli(),
		Contract: []*core.Transaction_Contract{
			{Type: core.Transaction_Contract_TriggerSmartContract, Parameter: parameter},
		},
		Timestamp: ref.Timestamp.UnixMilli(),
		FeeLimit:  defaultFeeLimit,
	}

	out, err := proto.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("txbuild: marshaling raw_data: %w", err)
	}
	return out, nil
}

// buildCalldata ABI-encodes transfer(address,uint256) via gotron-sdk's
// own abi.Pack -- see dispatcher/internal/txbuild's own identical
// function for the live-verified encoding details this mirrors exactly.
func buildCalldata(recipientBase58 string, amountUnits int64) ([]byte, error) {
	return abi.Pack(transferMethod, []abi.Param{
		{"address": recipientBase58},
		{"uint256": strconv.FormatInt(amountUnits, 10)},
	})
}

// ErrNotATransfer means an unsigned transaction is not a single, plain
// TRC20 transfer(address,uint256) call.
var ErrNotATransfer = errors.New("txbuild: not a TRC20 transfer")

// transferSelector is keccak256(transferMethod)[:4], the first 4 bytes
// buildCalldata's abi.Pack output always starts with.
var transferSelector = []byte{0xa9, 0x05, 0x9c, 0xbb}

// DecodedTransfer is what an unsigned TRC20 transfer actually does, read
// back from its own bytes. Addresses are base58check T-addresses.
type DecodedTransfer struct {
	Owner     string
	Token     string
	Recipient string
	Amount    *big.Int // raw on-chain units
}

// DecodeTransfer is BuildTransfer's inverse: it reads what unsignedTx
// will really do on-chain, so a caller can check the bytes about to be
// broadcast instead of trusting how they were built.
func DecodeTransfer(unsignedTx []byte) (DecodedTransfer, error) {
	raw := &core.TransactionRaw{}
	if err := proto.Unmarshal(unsignedTx, raw); err != nil {
		return DecodedTransfer{}, fmt.Errorf("%w: unmarshaling raw_data: %v", ErrNotATransfer, err)
	}
	if len(raw.Contract) != 1 || raw.Contract[0].Type != core.Transaction_Contract_TriggerSmartContract {
		return DecodedTransfer{}, fmt.Errorf("%w: want exactly one TriggerSmartContract", ErrNotATransfer)
	}
	trigger := &core.TriggerSmartContract{}
	if err := raw.Contract[0].Parameter.UnmarshalTo(trigger); err != nil {
		return DecodedTransfer{}, fmt.Errorf("%w: unwrapping TriggerSmartContract: %v", ErrNotATransfer, err)
	}
	if trigger.CallValue != 0 || trigger.CallTokenValue != 0 {
		return DecodedTransfer{}, fmt.Errorf("%w: also sends TRX or a TRC10 token", ErrNotATransfer)
	}
	data := trigger.Data
	if len(data) != 4+32+32 {
		return DecodedTransfer{}, fmt.Errorf("%w: calldata is %d bytes, want 68", ErrNotATransfer, len(data))
	}
	if string(data[:4]) != string(transferSelector) {
		return DecodedTransfer{}, fmt.Errorf("%w: method selector %x", ErrNotATransfer, data[:4])
	}
	for _, b := range data[4 : 4+12] {
		if b != 0 {
			return DecodedTransfer{}, fmt.Errorf("%w: recipient word is not a 20-byte address", ErrNotATransfer)
		}
	}
	recipient := append([]byte{address.TronBytePrefix}, data[4+12:4+32]...)
	return DecodedTransfer{
		Owner:     address.Address(trigger.OwnerAddress).String(),
		Token:     address.Address(trigger.ContractAddress).String(),
		Recipient: address.Address(recipient).String(),
		Amount:    new(big.Int).SetBytes(data[4+32 : 4+64]),
	}, nil
}

// Digest is SHA256 of unsignedTx (BuildTransfer's own return value) --
// both this transaction's own txID and the exact 32 bytes
// internal/signing's SigningService.Sign is asked to sign.
func Digest(unsignedTx []byte) [32]byte {
	return sha256.Sum256(unsignedTx)
}

// BuildTRXTransfer constructs an unsigned transfer of sun TRX from
// fromAddress to recipientAddress -- the treasury activating a new TRON
// deposit wallet (the first TRX an address receives creates its account)
// and funding the TRX its bandwidth burns.
func BuildTRXTransfer(fromAddress, recipientAddress string, sun int64, ref BlockReference) ([]byte, error) {
	owner, err := address.Base58ToAddress(fromAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: fromAddress %q: %v", ErrInvalidAddress, fromAddress, err)
	}
	to, err := address.Base58ToAddress(recipientAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: recipientAddress %q: %v", ErrInvalidAddress, recipientAddress, err)
	}
	if sun <= 0 {
		return nil, fmt.Errorf("%w: got %d sun", ErrNonPositiveAmount, sun)
	}
	parameter, err := anypb.New(&core.TransferContract{OwnerAddress: owner.Bytes(), ToAddress: to.Bytes(), Amount: sun})
	if err != nil {
		return nil, fmt.Errorf("txbuild: wrapping TransferContract: %w", err)
	}
	raw := &core.TransactionRaw{
		RefBlockBytes: []byte{byte(ref.BlockNumber >> 8), byte(ref.BlockNumber)},
		RefBlockHash:  ref.BlockHash[8:16],
		Expiration:    ref.Expiration.UnixMilli(),
		Contract:      []*core.Transaction_Contract{{Type: core.Transaction_Contract_TransferContract, Parameter: parameter}},
		Timestamp:     ref.Timestamp.UnixMilli(),
	}
	out, err := proto.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("txbuild: marshaling raw_data: %w", err)
	}
	return out, nil
}

// DecodedTRXTransfer is what an unsigned TRX transfer actually does.
type DecodedTRXTransfer struct {
	Owner     string
	Recipient string
	Sun       int64
}

// DecodeTRXTransfer reads back an unsigned transaction and refuses
// anything but exactly one plain TRX transfer.
func DecodeTRXTransfer(unsignedTx []byte) (DecodedTRXTransfer, error) {
	raw := &core.TransactionRaw{}
	if err := proto.Unmarshal(unsignedTx, raw); err != nil {
		return DecodedTRXTransfer{}, fmt.Errorf("%w: unmarshaling raw_data: %v", ErrNotATransfer, err)
	}
	if len(raw.Contract) != 1 || raw.Contract[0].Type != core.Transaction_Contract_TransferContract {
		return DecodedTRXTransfer{}, fmt.Errorf("%w: want exactly one TransferContract", ErrNotATransfer)
	}
	var tc core.TransferContract
	if err := raw.Contract[0].Parameter.UnmarshalTo(&tc); err != nil {
		return DecodedTRXTransfer{}, fmt.Errorf("%w: unwrapping TransferContract: %v", ErrNotATransfer, err)
	}
	return DecodedTRXTransfer{
		Owner: address.Address(tc.OwnerAddress).String(), Recipient: address.Address(tc.ToAddress).String(), Sun: tc.Amount,
	}, nil
}
