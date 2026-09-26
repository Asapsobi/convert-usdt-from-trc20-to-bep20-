package orchestrate

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	tronaddress "github.com/fbsobreira/gotron-sdk/pkg/address"

	"relayd/internal/alert"
	"relayd/internal/transfers"
	"relayd/internal/txbuild"
)

// tronTransferBandwidth is the bandwidth one USDT transfer uses -- its
// signed size, about 345 bytes -- with headroom. When a wallet's free
// daily bandwidth runs out, TRON burns TRX for it instead (1000 sun per
// byte, ~0.35 TRX a transfer).
const tronTransferBandwidth = 400

// tronTreasuryReserveSun is what the TRON treasury keeps on top of a TRX
// top-up's amount: sending TRX to a never-activated address burns TRON's
// 1 TRX account-creation fee, plus 0.1 TRX when the sender lacks staked
// bandwidth for an account-creating transfer.
const tronTreasuryReserveSun = 1_100_000

// tronAdapter sends transfers on TRON.
type tronAdapter struct{ o *Orchestrator }

func (t tronAdapter) build(ctx context.Context, req transferRequest, failedBefore int) (transfers.Attempt, error) {
	res, err := t.o.Chain.AccountResources(ctx, req.from)
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("reading %s's TRON resources: %w", req.from, err)
	}

	if req.native() {
		// The treasury sending TRX: it only needs the TRX itself.
		if need := req.amount.Units + tronTreasuryReserveSun; !res.Exists || res.BalanceSun < need {
			detail := fmt.Sprintf("the TRON treasury %s needs at least %d sun of TRX to send %s's %s, holds %d -- top it up",
				req.from, need, req.job, req.kind(), res.BalanceSun)
			t.o.alertOnce(ctx, "treasury:TRON", "treasury_needs_trx", alert.SeverityCritical, detail)
			return transfers.Attempt{}, errors.New(detail)
		}
		return t.buildWith(ctx, req, func(ref txbuild.BlockReference) ([]byte, error) {
			return txbuild.BuildTRXTransfer(req.from, req.to, req.amount.Units, ref)
		})
	}

	have, err := t.o.Chain.TokenBalance(ctx, req.from)
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("reading %s's USDT balance: %w", req.from, err)
	}
	if err := t.o.requireBalance(ctx, req, have, txbuild.USDTOnChainDecimals); err != nil {
		return transfers.Attempt{}, err
	}
	// A wallet that only ever received USDT has no TRON account yet, and
	// can neither receive rented energy nor pay for bandwidth: the first
	// TRX it receives activates it.
	if !res.Exists {
		return transfers.Attempt{}, t.o.topUp(ctx, req, transfers.TRXTopUp, big.NewInt(0))
	}
	raw, err := onChainUnits(req.amount, txbuild.USDTOnChainDecimals)
	if err != nil {
		return transfers.Attempt{}, err
	}
	// The exact energy this transfer needs -- about 65k when the recipient
	// already holds USDT, about 130k when it never has -- measured by
	// simulating it on the node, not assumed.
	needEnergy, err := t.o.Chain.EstimateTransferEnergy(ctx, req.from, req.to, raw)
	if err != nil {
		return transfers.Attempt{}, err
	}
	if res.Energy < needEnergy {
		return transfers.Attempt{}, t.o.rentEnergy(ctx, req, needEnergy-res.Energy)
	}
	if res.Bandwidth < tronTransferBandwidth && res.BalanceSun < tronTransferBandwidth*1000 {
		return transfers.Attempt{}, t.o.topUp(ctx, req, transfers.TRXTopUp, big.NewInt(tronTransferBandwidth*1000-res.BalanceSun))
	}
	return t.buildWith(ctx, req, func(ref txbuild.BlockReference) ([]byte, error) {
		return txbuild.BuildTransfer(req.from, req.to, req.amount, ref)
	})
}

func (t tronAdapter) buildWith(ctx context.Context, req transferRequest, build func(txbuild.BlockReference) ([]byte, error)) (transfers.Attempt, error) {
	ref, err := t.o.Chain.CurrentBlockReference(ctx)
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("resolving a current TRON block reference: %w", err)
	}
	unsigned, err := build(ref)
	if err != nil {
		return transfers.Attempt{}, fmt.Errorf("building the unsigned %s transfer: %w", req.kind(), err)
	}
	expires := ref.Expiration.UTC()
	return transfers.Attempt{
		ExternalID: req.job, Purpose: req.purpose, Chain: transfers.TRON,
		FromAddress: req.from, ToAddress: req.to, Amount: req.amount,
		UnsignedTx: unsigned, Digest: txbuild.Digest(unsigned), TronExpiresAt: &expires,
	}, nil
}

func (t tronAdapter) unsendable(ctx context.Context, a transfers.Attempt, now time.Time) (bool, string, error) {
	if now.Add(tronSendMargin).After(*a.TronExpiresAt) {
		return true, fmt.Sprintf("expired at %s before it was signed and sent", a.TronExpiresAt.Format(time.RFC3339)), nil
	}
	return false, "", nil
}

func (t tronAdapter) verify(req transferRequest, a transfers.Attempt, sig [65]byte) (string, error) {
	if req.native() {
		d, err := txbuild.DecodeTRXTransfer(a.UnsignedTx)
		if err != nil {
			return "", err
		}
		var problems []string
		if d.Owner != req.from {
			problems = append(problems, fmt.Sprintf("sends from %s, want %s", d.Owner, req.from))
		}
		if d.Recipient != req.to {
			problems = append(problems, fmt.Sprintf("pays %s, want %s", d.Recipient, req.to))
		}
		if d.Sun != req.amount.Units {
			problems = append(problems, fmt.Sprintf("moves %d sun, want %d", d.Sun, req.amount.Units))
		}
		if err := joinProblems(problems); err != nil {
			return "", err
		}
	} else if err := checkTRONTransfer(a.UnsignedTx, req.intent()); err != nil {
		return "", err
	}
	if err := checkTRONSigner(a.Digest, sig, req.from); err != nil {
		return "", err
	}
	return hex.EncodeToString(a.Digest[:]), nil
}

// checkTRONSigner confirms sig was made by the key that controls sender
// -- the TRON counterpart of the EVM sender check in checkEVMTransfer.
// A node would reject a mismatch anyway; this names the problem before
// anything leaves the process.
func checkTRONSigner(digest [32]byte, sig [65]byte, sender string) error {
	recoverable := sig
	if recoverable[64] >= 27 {
		recoverable[64] -= 27
	}
	pub, err := crypto.SigToPub(digest[:], recoverable[:])
	if err != nil {
		return fmt.Errorf("signature does not recover to a public key: %v", err)
	}
	if signer := tronaddress.PubkeyToAddress(*pub).String(); signer != sender {
		return fmt.Errorf("is signed by %s, want %s (signing key does not control the address holding the funds)", signer, sender)
	}
	return nil
}

func (t tronAdapter) send(ctx context.Context, a transfers.Attempt) error {
	if time.Now().After(*a.TronExpiresAt) {
		return fmt.Errorf("not re-sent: expired at %s", a.TronExpiresAt.Format(time.RFC3339))
	}
	_, err := t.o.Chain.BroadcastSigned(ctx, a.UnsignedTx, *a.Signature)
	return err
}

func (t tronAdapter) outcome(ctx context.Context, a transfers.Attempt, now time.Time) (sentOutcome, error) {
	final, success, reason, err := t.o.Finality.CheckExecution(ctx, *a.TxHash)
	if err != nil {
		return sentOutcome{}, err
	}
	switch {
	case final && success:
		return sentOutcome{state: outcomeConfirmed}, nil
	case final && reason != "":
		return sentOutcome{state: outcomeFailed, reason: reason}, nil
	case final:
		// Solidified, but the receipt carries no verdict. Never guess
		// "failed" -- a wrong guess rebuilds a transfer that landed.
		return sentOutcome{state: outcomePending}, nil
	}
	if now.After(a.TronExpiresAt.Add(tronDropGrace)) {
		return sentOutcome{state: outcomeDropped,
			reason: fmt.Sprintf("expired at %s and never appeared on the solidified chain", a.TronExpiresAt.Format(time.RFC3339))}, nil
	}
	return sentOutcome{state: outcomePending}, nil
}
