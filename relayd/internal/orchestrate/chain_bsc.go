package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"relayd/internal/alert"
	"relayd/internal/evmbroadcast"
	"relayd/internal/evmtx"
	"relayd/internal/transfers"
)

// bscAdapter sends transfers on BSC.
type bscAdapter struct{ o *Orchestrator }

func (b bscAdapter) build(ctx context.Context, req transferRequest, failedBefore int) (transfers.Attempt, error) {
	gasPrice, err := b.o.EVMChain.SuggestGasPrice(ctx)
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("suggesting a BSC gas price: %w", err)
	}
	if gasPrice.Cmp(maxGasPriceWei) > 0 {
		detail := fmt.Sprintf("%s: %s transfer NOT built -- the BSC node suggests a gas price of %s wei, above the %s wei ceiling",
			req.job, req.kind(), gasPrice, maxGasPriceWei)
		b.o.alertOnce(ctx, req.job, "relay_leg_gas_price_too_high", alert.SeverityCritical, detail)
		return transfers.Attempt{}, errors.New(detail)
	}
	bnb, err := b.o.EVMChain.NativeBalance(ctx, req.from)
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("reading %s's BNB balance: %w", req.from, err)
	}

	var gasLimit uint64
	if req.native() {
		gasLimit = evmtx.NativeTransferGasLimit
	} else {
		have, err := b.o.EVMChain.TokenBalance(ctx, req.from)
		if err != nil {
			return transfers.Attempt{}, fmt.Errorf("reading %s's USDT balance: %w", req.from, err)
		}
		if err := b.o.requireBalance(ctx, req, have, evmtx.USDTOnChainDecimals); err != nil {
			return transfers.Attempt{}, err
		}
		if gasLimit = b.o.Cfg.EVMGasLimit; gasLimit == 0 {
			gasLimit = evmtx.DefaultGasLimit
		}
	}
	fee := new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(gasLimit))
	need := new(big.Int).Set(fee)
	if req.native() {
		need.Add(need, big.NewInt(req.amount.Units))
	}
	if bnb.Cmp(need) < 0 {
		if req.signer.slot {
			detail := fmt.Sprintf("the BSC treasury %s needs at least %s wei of BNB to send %s's %s, holds %s -- top it up",
				req.from, need, req.job, req.kind(), bnb)
			b.o.alertOnce(ctx, "treasury:BSC", "treasury_needs_bnb", alert.SeverityCritical, detail)
			return transfers.Attempt{}, errors.New(detail)
		}
		// Scenario B: the deposit wallet can't pay its own gas -- fund it
		// from the treasury, then carry on.
		return transfers.Attempt{}, b.o.topUp(ctx, req, transfers.GasTopUp, new(big.Int).Sub(need, bnb))
	}

	nonce, err := b.o.EVMChain.CurrentNonce(ctx, req.from)
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("resolving a BSC nonce: %w", err)
	}
	var tx *types.Transaction
	var digest [32]byte
	if req.native() {
		tx, digest, err = evmtx.BuildNativeTransfer(req.to, big.NewInt(req.amount.Units), nonce, gasPrice)
	} else {
		tx, digest, err = evmtx.BuildTransfer(req.to, req.amount, evmtx.TxParams{Nonce: nonce, GasPrice: gasPrice, GasLimit: b.o.Cfg.EVMGasLimit})
	}
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("building the unsigned %s transfer: %w", req.kind(), err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("encoding the unsigned %s transfer: %w", req.kind(), err)
	}
	return transfers.Attempt{
		ExternalID: req.job, Purpose: req.purpose, Chain: transfers.BSC,
		FromAddress: req.from, ToAddress: req.to, Amount: req.amount,
		UnsignedTx: raw, Digest: digest, EVMNonce: &nonce,
	}, nil
}

func (b bscAdapter) unsendable(ctx context.Context, a transfers.Attempt, now time.Time) (bool, string, error) {
	confirmed, err := b.o.EVMChain.ConfirmedNonce(ctx, a.FromAddress)
	if err != nil {
		return false, "", err
	}
	if confirmed > *a.EVMNonce {
		return true, fmt.Sprintf("nonce %d was used by another transaction before this one was signed", *a.EVMNonce), nil
	}
	return false, "", nil
}

func (b bscAdapter) signed(a transfers.Attempt, sig [65]byte) (*types.Transaction, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(a.UnsignedTx); err != nil {
		return nil, fmt.Errorf("decoding recorded transaction: %w", err)
	}
	return evmtx.WithSignature(tx, sig)
}

func (b bscAdapter) verify(req transferRequest, a transfers.Attempt, sig [65]byte) (string, error) {
	signed, err := b.signed(a, sig)
	if err != nil {
		return "", err
	}
	if req.native() {
		err = checkEVMNativeTransfer(signed, req.intent())
	} else {
		err = checkEVMTransfer(signed, req.intent())
	}
	if err != nil {
		return "", err
	}
	return signed.Hash().Hex(), nil
}

// checkEVMNativeTransfer verifies signed is exactly a plain BNB transfer
// of intent.amount wei from intent.sender to intent.recipient.
func checkEVMNativeTransfer(signed *types.Transaction, intent transferIntent) error {
	var problems []string
	if signed.To() == nil || !sameAddress(signed.To().Hex(), intent.recipient) {
		problems = append(problems, fmt.Sprintf("pays %v, want %s", signed.To(), intent.recipient))
	}
	if signed.Value().Cmp(big.NewInt(intent.amount.Units)) != 0 {
		problems = append(problems, fmt.Sprintf("sends %s wei, want %d", signed.Value(), intent.amount.Units))
	}
	if len(signed.Data()) != 0 {
		problems = append(problems, "carries call data -- a plain BNB transfer carries none")
	}
	if signed.Gas() != evmtx.NativeTransferGasLimit {
		problems = append(problems, fmt.Sprintf("has a gas limit of %d, want %d", signed.Gas(), evmtx.NativeTransferGasLimit))
	}
	from, err := evmtx.Sender(signed)
	if err != nil {
		problems = append(problems, err.Error())
	} else if from != common.HexToAddress(intent.sender) {
		problems = append(problems, fmt.Sprintf("is signed by %s, want %s", from.Hex(), intent.sender))
	}
	return joinProblems(problems)
}

func (b bscAdapter) send(ctx context.Context, a transfers.Attempt) error {
	signed, err := b.signed(a, *a.Signature)
	if err != nil {
		return err
	}
	_, err = b.o.EVMChain.Broadcast(ctx, signed)
	return err
}

func (b bscAdapter) outcome(ctx context.Context, a transfers.Attempt, now time.Time) (sentOutcome, error) {
	final, err := b.o.EVMFinality.IsFinal(ctx, *a.TxHash)
	if err != nil {
		if errors.Is(err, evmbroadcast.ErrReverted) {
			return sentOutcome{state: outcomeFailed, reason: err.Error()}, nil
		}
		return sentOutcome{}, err
	}
	if final {
		return sentOutcome{state: outcomeConfirmed}, nil
	}

	// Not final yet. It is only dropped if some other mined transaction
	// used its nonce and it has no receipt of its own -- sustained for
	// evmDropGrace, so one lagging node can't trigger a rebuild.
	mined, err := b.o.EVMChain.TransactionMined(ctx, *a.TxHash)
	if err != nil {
		return sentOutcome{}, err
	}
	nonceUsed := false
	if !mined {
		confirmed, err := b.o.EVMChain.ConfirmedNonce(ctx, a.FromAddress)
		if err != nil {
			return sentOutcome{}, err
		}
		nonceUsed = confirmed > *a.EVMNonce
	}
	switch {
	case !nonceUsed:
		if a.NonceConsumedSince != nil {
			return sentOutcome{}, b.o.Transfers.SetNonceConsumedSince(ctx, a.ID, nil)
		}
	case a.NonceConsumedSince == nil:
		return sentOutcome{}, b.o.Transfers.SetNonceConsumedSince(ctx, a.ID, &now)
	case now.Sub(*a.NonceConsumedSince) >= evmDropGrace:
		return sentOutcome{state: outcomeDropped,
			reason: fmt.Sprintf("nonce %d was used by another mined transaction", *a.EVMNonce)}, nil
	}
	return sentOutcome{state: outcomePending}, nil
}
