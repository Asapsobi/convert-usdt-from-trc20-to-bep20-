package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"relayd/internal/alert"
	"relayd/internal/evmtx"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
)

// ErrPreflightFailed means a transfer was stopped right before broadcast
// because what was about to leave didn't match the leg, or the vendor's
// own live view of the order. Nothing was sent.
//
// Every check here reads the actual bytes about to be broadcast (and,
// for a forward, asks the vendor), never the values the builder was
// given -- each real money bug this codebase has shipped was a correct-
// looking input encoded or signed wrong: a transfer amount never scaled
// to the token's 18 decimals, a vendor order created for a different
// amount than was later sent, and a deposit address derived from a key
// S1 couldn't sign for.
var ErrPreflightFailed = errors.New("pre-broadcast check failed")

// transferIntent is what a leg is supposed to send, stated independently
// of how the transaction was built.
type transferIntent struct {
	sender    string
	recipient string
	amount    money.Amount
}

// checkEVMTransfer verifies signed does exactly what intent says. The
// sender is recovered from the signature because on EVM chains the
// signer IS the sender: a signature from the wrong key silently moves
// funds from a different address.
func checkEVMTransfer(signed *types.Transaction, intent transferIntent) error {
	decoded, err := evmtx.DecodeTransfer(signed)
	if err != nil {
		return err
	}
	var problems []string
	if decoded.Token != common.HexToAddress(evmtx.USDTContractAddress) {
		problems = append(problems, fmt.Sprintf("calls token %s, not USDT %s", decoded.Token.Hex(), evmtx.USDTContractAddress))
	}
	if !sameAddress(decoded.Recipient.Hex(), intent.recipient) {
		problems = append(problems, fmt.Sprintf("pays %s, want %s", decoded.Recipient.Hex(), intent.recipient))
	}
	if msg := amountMismatch(decoded.Amount, intent.amount, evmtx.USDTOnChainDecimals); msg != "" {
		problems = append(problems, msg)
	}
	from, err := evmtx.Sender(signed)
	if err != nil {
		problems = append(problems, err.Error())
	} else if !sameAddress(from.Hex(), intent.sender) {
		problems = append(problems, fmt.Sprintf("is signed by %s, want %s (signing key does not control the address holding the funds)", from.Hex(), intent.sender))
	}
	return joinProblems(problems)
}

// checkTRONTransfer verifies unsignedTx does exactly what intent says.
func checkTRONTransfer(unsignedTx []byte, intent transferIntent) error {
	decoded, err := txbuild.DecodeTransfer(unsignedTx)
	if err != nil {
		return err
	}
	var problems []string
	if decoded.Token != txbuild.USDTContractAddress {
		problems = append(problems, fmt.Sprintf("calls token %s, not USDT %s", decoded.Token, txbuild.USDTContractAddress))
	}
	if decoded.Owner != intent.sender {
		problems = append(problems, fmt.Sprintf("sends from %s, want %s", decoded.Owner, intent.sender))
	}
	if decoded.Recipient != intent.recipient {
		problems = append(problems, fmt.Sprintf("pays %s, want %s", decoded.Recipient, intent.recipient))
	}
	if msg := amountMismatch(decoded.Amount, intent.amount, txbuild.USDTOnChainDecimals); msg != "" {
		problems = append(problems, msg)
	}
	return joinProblems(problems)
}

// amountMismatch compares a raw on-chain amount against intended with
// its own scaling math, deliberately not reusing the builders' --
// a builder that forgets to scale (the real 2026-09-23 incident) must
// not be checked by the same code that forgot.
func amountMismatch(raw *big.Int, intended money.Amount, onChainDecimals int) string {
	internal, err := intended.Asset.Decimals()
	if err != nil {
		return err.Error()
	}
	if onChainDecimals < internal {
		return fmt.Sprintf("token has %d decimals, fewer than %s's own %d", onChainDecimals, intended.Asset, internal)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(onChainDecimals-internal)), nil)
	want := new(big.Int).Mul(big.NewInt(intended.Units), scale)
	if raw.Cmp(want) != 0 {
		return fmt.Sprintf("moves %s raw units, want %s (%s %s)", raw, want, fmtAmount(intended), intended.Asset)
	}
	return ""
}

// checkVendorOrder confirms, against the vendor itself, that its order
// is still waiting for exactly this deposit and will pay the customer.
// Sending into an order that already expired or already received funds
// (e.g. relayd restarted after an earlier broadcast it never recorded)
// strands the money with the vendor. A failed lookup is transient: wait
// for the next tick rather than send blind.
func (o *Orchestrator) checkVendorOrder(ctx context.Context, leg relay.Leg, intent transferIntent) error {
	if leg.UpstreamOrderID == nil {
		return o.refuseBroadcast(ctx, leg, "forward", errors.New("leg has no upstream order recorded"))
	}
	vendorOrder, err := o.vendorOrder(ctx, leg)
	if err != nil {
		return fmt.Errorf("confirming upstream order %s before sending: %w", *leg.UpstreamOrderID, err)
	}

	var problems []string
	if vendorOrder.Status != upstream.StatusAwaitingDeposit {
		problems = append(problems, fmt.Sprintf("vendor order is %s, not awaiting a deposit", vendorOrder.Status))
	}
	if !sameAddress(vendorOrder.DepositAddress, intent.recipient) {
		problems = append(problems, fmt.Sprintf("vendor expects the deposit at %s, not %s", vendorOrder.DepositAddress, intent.recipient))
	}
	if vendorOrder.AmountIn != intent.amount {
		problems = append(problems, fmt.Sprintf("vendor expects %s %s, not %s %s",
			fmtAmount(vendorOrder.AmountIn), vendorOrder.AmountIn.Asset, fmtAmount(intent.amount), intent.amount.Asset))
	}
	// A vendor that doesn't report a payout address can't be checked on
	// it; one that does must be paying this leg's own customer.
	if vendorOrder.DestinationAddress != "" && !sameAddress(vendorOrder.DestinationAddress, leg.DestinationAddress) {
		problems = append(problems, fmt.Sprintf("vendor will pay out to %s, not the customer's %s", vendorOrder.DestinationAddress, leg.DestinationAddress))
	}
	if err := joinProblems(problems); err != nil {
		return o.refuseBroadcast(ctx, leg, "forward", err)
	}
	return nil
}

// refuseBroadcast records a failed pre-broadcast check: one critical
// alert per distinct failure (not one per tick -- the leg stays where it
// is and this runs again every tick), then an error the caller returns
// without broadcasting.
func (o *Orchestrator) refuseBroadcast(ctx context.Context, leg relay.Leg, kind string, cause error) error {
	o.alertOnce(ctx, leg.ExternalID, "relay_leg_preflight_failed", alert.SeverityCritical,
		fmt.Sprintf("relay leg %s: %s transfer NOT sent -- %v", leg.ExternalID, kind, cause))
	return fmt.Errorf("%w: %s transfer: %v", ErrPreflightFailed, kind, cause)
}

// sameAddress compares two addresses of the same chain: EVM hex
// case-insensitively (checksum casing is presentation only), anything
// else -- TRON base58check -- exactly.
func sameAddress(a, b string) bool {
	if common.IsHexAddress(a) && common.IsHexAddress(b) {
		return common.HexToAddress(a) == common.HexToAddress(b)
	}
	return a == b
}

func fmtAmount(a money.Amount) string {
	s, err := money.Format(a)
	if err != nil {
		return fmt.Sprintf("%d units", a.Units)
	}
	return s
}

func joinProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}
