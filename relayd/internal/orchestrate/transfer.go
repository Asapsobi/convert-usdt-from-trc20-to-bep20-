package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"relayd/internal/alert"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/transfers"
)

// This file gets one transfer onto the chain exactly once, on either
// chain, surviving a restart at any point: a leg's forward to the vendor
// or its refund to the depositor, a treasury gas or TRX top-up, or a
// sweep to treasury.
//
// Every attempt is recorded in internal/transfers before it is signed,
// and its signature is recorded before it is sent. A restart therefore
// resumes the exact transaction that might already be in flight. A new
// transaction is only built once every earlier one has provably
// confirmed, failed on-chain (no funds moved), or been dropped (can
// never land), and only one transaction is ever open per sending address.
// Before anything is built, the sender's balance and network resources
// are checked -- and provisioned when short (resources.go): relayd never
// signs a transfer the sending wallet can't pay for.

const (
	// maxFailedAttempts caps how often one transfer is rebuilt after
	// executing and failing on-chain. Each failure costs gas or energy,
	// and a failure that keeps repeating is not going to fix itself.
	maxFailedAttempts = 3

	// rebroadcastEvery is how often a sent transaction the chain hasn't
	// picked up yet is sent again. Re-sending the same signed bytes is
	// harmless: at most one copy can ever land.
	rebroadcastEvery = 30 * time.Second

	// sentAttemptStuckAfter raises a one-time alert for a sent
	// transaction that still hasn't resolved.
	sentAttemptStuckAfter = 10 * time.Minute

	// evmDropGrace is how long the chain must sit past an EVM attempt's
	// nonce, with no receipt for the attempt itself, before it is
	// declared dropped. Covers an RPC provider that answers the nonce
	// and receipt queries from differently-synced nodes.
	evmDropGrace = 3 * time.Minute

	// tronDropGrace is how long past its expiration a TRON attempt must
	// stay off the solidified chain before it is declared dropped.
	// Solidification trails inclusion by about a minute.
	tronDropGrace = 5 * time.Minute

	// tronSendMargin: a BUILT TRON attempt this close to its expiration
	// is not signed and sent -- it is rebuilt with a fresh block
	// reference instead.
	tronSendMargin = 15 * time.Second
)

// maxGasPriceWei refuses to send when the node suggests a gas price no
// healthy BSC node would (BSC runs well under 1 gwei). A misbehaving RPC
// could otherwise spend a deposit address's whole BNB balance on one
// transfer.
var maxGasPriceWei = big.NewInt(20_000_000_000) // 20 gwei

// assetBNB and assetTRX mark a native transfer: its amount is in wei
// (BNB) or sun (TRX), not USDT minor units.
const (
	assetBNB money.Asset = "BNB"
	assetTRX money.Asset = "TRX"
)

// signer is which key signs for a transfer's sender.
type signer struct {
	depositIndex *uint32 // a deposit wallet's own key
	slot         bool    // the treasury key (S1 slot Config.SlotID)
}

// transferRequest is one transfer relayd must get onto the chain exactly
// once.
type transferRequest struct {
	job     string // groups a transfer's attempts: a leg's external id, or a treasury/sweep job key
	purpose transfers.Purpose
	chain   transfers.Chain
	from    string
	signer  signer
	to      string
	amount  money.Amount // USDT for token transfers; wei (assetBNB) or sun (assetTRX) for native ones
	leg     *relay.Leg   // the leg a forward or refund belongs to; nil otherwise
}

// legTransfer is a forward or refund from leg's own deposit wallet,
// signed with that wallet's own key.
func legTransfer(leg relay.Leg, purpose transfers.Purpose, to string, amount money.Amount) transferRequest {
	chain := transfers.TRON
	if leg.Direction == relay.BEP20ToTRC20 {
		chain = transfers.BSC
	}
	return transferRequest{job: leg.ExternalID, purpose: purpose, chain: chain, from: leg.DepositAddress,
		signer: signer{depositIndex: leg.DepositDerivationIndex}, to: to, amount: amount, leg: &leg}
}

func (r transferRequest) native() bool {
	return r.amount.Asset == assetBNB || r.amount.Asset == assetTRX
}

func (r transferRequest) kind() string {
	switch r.purpose {
	case transfers.Forward:
		return "forward"
	case transfers.Refund:
		return "refund"
	}
	return strings.ToLower(string(r.purpose))
}

func (r transferRequest) intent() transferIntent {
	return transferIntent{sender: r.from, recipient: r.to, amount: r.amount}
}

// signingKey is S1's idempotency key for one attempt's signature. Scoped
// to the digest: a rebuilt transaction must get a fresh signature, never
// a replay of one over different bytes.
func (r transferRequest) signingKey(digest [32]byte) string {
	switch r.purpose {
	case transfers.Forward:
		return fmt.Sprintf("relayd:sign:%s:%x", r.job, digest)
	case transfers.Refund:
		return fmt.Sprintf("relayd:refund-sign:%s:%x", r.job, digest)
	}
	return fmt.Sprintf("relayd:%s-sign:%s:%x", r.kind(), r.job, digest)
}

// estimatedUSD is what S1's approval threshold sees. Native top-ups are a
// few cents of BNB or TRX.
func (r transferRequest) estimatedUSD() float64 {
	if r.native() {
		return 1
	}
	return estimatedUSDFor(r.amount)
}

type outcomeState int

const (
	outcomePending outcomeState = iota
	outcomeConfirmed
	outcomeFailed  // executed on-chain and failed: no funds moved
	outcomeDropped // can provably never land
)

type sentOutcome struct {
	state  outcomeState
	reason string
}

// errNotReady means a transfer can't be built yet because its sender is
// still being provisioned (a gas or TRX top-up, an energy rental). Not a
// failure: the same transfer is retried next tick.
type errNotReady struct{ reason string }

func (e errNotReady) Error() string { return e.reason }

func notReady(format string, args ...any) error { return errNotReady{fmt.Sprintf(format, args...)} }

// chainAdapter is what driveTransfer needs from one chain.
type chainAdapter interface {
	// build checks (and when short, provisions) what the sender needs to
	// pay for req, then builds the unsigned transaction (not yet
	// persisted). failedBefore counts this transfer's earlier on-chain
	// failures.
	build(ctx context.Context, req transferRequest, failedBefore int) (transfers.Attempt, error)
	// unsendable reports whether a BUILT attempt can no longer be signed
	// and sent, and why.
	unsendable(ctx context.Context, a transfers.Attempt, now time.Time) (bool, string, error)
	// verify decodes the exact transaction a would broadcast with sig,
	// checks it against req, and returns its hash.
	verify(req transferRequest, a transfers.Attempt, sig [65]byte) (string, error)
	send(ctx context.Context, a transfers.Attempt) error
	// outcome reports where a sent attempt stands on-chain.
	outcome(ctx context.Context, a transfers.Attempt, now time.Time) (sentOutcome, error)
}

func (o *Orchestrator) adapterFor(chain transfers.Chain) (chainAdapter, error) {
	switch chain {
	case transfers.BSC:
		return bscAdapter{o}, nil
	case transfers.TRON:
		return tronAdapter{o}, nil
	}
	return nil, fmt.Errorf("unknown chain %q", chain)
}

// driveTransfer advances req by one step and returns the confirmed
// attempt once the transfer has landed (nil while still in progress).
func (o *Orchestrator) driveTransfer(ctx context.Context, req transferRequest) (*transfers.Attempt, error) {
	if req.signer.depositIndex == nil && !req.signer.slot {
		return nil, fmt.Errorf("%s %s has no signing key: a deposit wallet transfer needs its derivation index -- "+
			"every relay leg gets one at Create from the real AssignAddress response; this is a data-integrity bug, not a transient condition",
			req.kind(), req.job)
	}
	ad, err := o.adapterFor(req.chain)
	if err != nil {
		return nil, err
	}

	if done, ok, err := o.Transfers.Confirmed(ctx, req.job, req.purpose); err != nil {
		return nil, err
	} else if ok {
		return &done, nil
	}

	a, ok, err := o.Transfers.Open(ctx, req.job, req.purpose)
	if err != nil {
		return nil, err
	}
	if ok && a.Status.MayLand() {
		return o.trackSent(ctx, ad, req, a)
	}
	if ok && (a.ToAddress != req.to || a.Amount != req.amount || a.FromAddress != req.from) {
		// Never signed, so nothing can land -- but it no longer matches
		// what this job needs to send. Rebuild from scratch.
		return nil, o.Transfers.MarkAbandoned(ctx, a.ID, fmt.Sprintf("transfer changed to %s %s -> %s before it was signed",
			fmtAmount(req.amount), req.amount.Asset, req.to))
	}
	if !ok {
		if other, busy, err := o.Transfers.OpenFrom(ctx, req.from); err != nil {
			return nil, err
		} else if busy && (other.ExternalID != req.job || other.Purpose != req.purpose) {
			o.waiting(req, fmt.Sprintf("%s is still sending %s for %s", req.from, other.Purpose, other.ExternalID))
			return nil, nil
		}
		failed, err := o.Transfers.CountFailed(ctx, req.job, req.purpose)
		if err != nil {
			return nil, err
		}
		if failed >= maxFailedAttempts {
			detail := fmt.Sprintf("%s: %s transfer failed on-chain %d times -- no longer retrying automatically; "+
				"investigate the failed attempts before doing anything by hand", req.job, req.kind(), failed)
			o.alertOnce(ctx, req.job, "relay_leg_transfer_gave_up", alert.SeverityCritical, detail)
			return nil, errors.New(detail)
		}
		built, err := ad.build(ctx, req, failed)
		var wait errNotReady
		if errors.As(err, &wait) {
			o.waiting(req, wait.reason)
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if a, err = o.Transfers.Create(ctx, built); err != nil {
			return nil, fmt.Errorf("recording the unsigned %s transfer: %w", req.kind(), err)
		}
	}
	return nil, o.signAndSend(ctx, ad, req, a)
}

// waiting logs that a transfer is waiting on something outside it --
// once per distinct reason, not every tick.
func (o *Orchestrator) waiting(req transferRequest, reason string) {
	key := "waiting|" + req.job + "|" + string(req.purpose)
	o.mu.Lock()
	same := o.preflightAlerted[key] == reason
	o.preflightAlerted[key] = reason
	o.mu.Unlock()
	if !same {
		slog.Info("orchestrate: transfer waiting", "job", req.job, "purpose", req.purpose, "reason", reason)
	}
}

// requestSignature asks S1 to sign digest with req's key.
func (o *Orchestrator) requestSignature(ctx context.Context, req transferRequest, digest [32]byte) (signing.SigningRequest, error) {
	key, usd := req.signingKey(digest), req.estimatedUSD()
	switch {
	case req.signer.slot:
		return o.Signing.RequestSignature(ctx, o.Cfg.SlotID, digest, usd, key)
	case req.chain == transfers.BSC:
		return o.Signing.RequestDepositSweepSignature(ctx, *req.signer.depositIndex, digest, usd, key)
	default:
		return o.Signing.RequestTronDepositSweepSignature(ctx, *req.signer.depositIndex, digest, usd, key)
	}
}

// signAndSend takes a BUILT attempt through signing to its first send.
// The signature is recorded before the send: from that moment the
// attempt may land, and it is only ever resolved by what the chain says.
func (o *Orchestrator) signAndSend(ctx context.Context, ad chainAdapter, req transferRequest, a transfers.Attempt) error {
	if gone, why, err := ad.unsendable(ctx, a, time.Now()); err != nil {
		return fmt.Errorf("checking the unsigned %s transfer is still sendable: %w", req.kind(), err)
	} else if gone {
		return o.Transfers.MarkAbandoned(ctx, a.ID, why)
	}

	if req.leg != nil && req.signer.depositIndex != nil {
		if err := o.reverifyDepositAddress(ctx, *req.leg); err != nil {
			return err
		}
	}
	sigReq, err := o.requestSignature(ctx, req, a.Digest)
	if err != nil {
		return fmt.Errorf("requesting %s signature: %w", req.kind(), err)
	}
	switch sigReq.Status {
	case signing.StatusPending:
		slog.Info("orchestrate: signature still pending approval, resuming next tick", "job", req.job, "purpose", req.purpose)
		return nil
	case signing.StatusRejected:
		if err := o.Transfers.MarkAbandoned(ctx, a.ID, "signature request was rejected"); err != nil {
			return err
		}
		return fmt.Errorf("%s signature request was rejected", req.kind())
	case signing.StatusSigned:
	default:
		return fmt.Errorf("unexpected signing status %q", sigReq.Status)
	}

	txHash, err := ad.verify(req, a, sigReq.SignedTx)
	if err != nil {
		if req.leg != nil {
			return o.refuseBroadcast(ctx, *req.leg, req.kind(), err)
		}
		o.alertOnce(ctx, req.job, "relay_leg_preflight_failed", alert.SeverityCritical,
			fmt.Sprintf("%s: %s transfer NOT sent -- %v", req.job, req.kind(), err))
		return fmt.Errorf("%w: %s transfer: %v", ErrPreflightFailed, req.kind(), err)
	}
	if req.purpose == transfers.Forward && req.leg != nil {
		if err := o.checkVendorOrder(ctx, *req.leg, req.intent()); err != nil {
			return err
		}
	}

	if err := o.Transfers.MarkSigned(ctx, a.ID, sigReq.SignedTx, txHash); err != nil {
		return fmt.Errorf("recording the signed %s transfer: %w", req.kind(), err)
	}
	sig := sigReq.SignedTx
	a.Status, a.Signature, a.TxHash = transfers.StatusSigned, &sig, &txHash
	slog.Info("orchestrate: transfer signed and recorded, sending",
		"job", req.job, "purpose", req.purpose, "tx_hash", txHash)
	return o.sendAttempt(ctx, ad, req, a)
}

// sendAttempt broadcasts a signed attempt and records the result.
func (o *Orchestrator) sendAttempt(ctx context.Context, ad chainAdapter, req transferRequest, a transfers.Attempt) error {
	sendErr := ad.send(ctx, a)
	if sendErr != nil && alreadyOnChain(sendErr) {
		sendErr = nil // the network already has it; its outcome decides from here
	}
	if err := o.Transfers.RecordBroadcast(ctx, a.ID, sendErr); err != nil {
		return err
	}
	if sendErr != nil {
		if strings.Contains(strings.ToLower(sendErr.Error()), "insufficient funds") {
			o.alertOnce(ctx, req.job, "relay_leg_needs_gas", alert.SeverityCritical,
				fmt.Sprintf("relay leg %s: %s transfer %s can't be sent -- %s has too little for its fee: %v",
					req.job, req.kind(), *a.TxHash, a.FromAddress, sendErr))
		}
		return fmt.Errorf("sending %s transfer %s (it stays recorded and is re-sent): %w", req.kind(), *a.TxHash, sendErr)
	}
	slog.Info("orchestrate: transfer sent, awaiting its on-chain result",
		"external_id", req.job, "purpose", req.purpose, "tx_hash", *a.TxHash)
	return nil
}

// trackSent follows a SIGNED or BROADCAST attempt until the chain
// settles it.
func (o *Orchestrator) trackSent(ctx context.Context, ad chainAdapter, req transferRequest, a transfers.Attempt) (*transfers.Attempt, error) {
	now := time.Now()
	out, err := ad.outcome(ctx, a, now)
	if err != nil {
		return nil, fmt.Errorf("checking %s transfer %s: %w", req.kind(), *a.TxHash, err)
	}
	ext := req.job

	switch out.state {
	case outcomeConfirmed:
		if err := o.Transfers.MarkConfirmed(ctx, a.ID); err != nil {
			return nil, err
		}
		a.Status = transfers.StatusConfirmed
		slog.Info("orchestrate: transfer confirmed on-chain", "external_id", ext, "purpose", req.purpose, "tx_hash", *a.TxHash)
		return &a, nil

	case outcomeFailed:
		if err := o.Transfers.MarkFailed(ctx, a.ID, out.reason); err != nil {
			return nil, err
		}
		detail := fmt.Sprintf("leg %s: %s transfer %s was included on-chain but its execution failed: %s -- "+
			"funds did NOT move; a new transaction will be built", ext, req.kind(), *a.TxHash, out.reason)
		o.fire(ctx, alert.Alert{Severity: alert.SeverityCritical, ExternalID: ext,
			Reason: fmt.Sprintf("relay_leg_%s_execution_failed", req.kind()), Detail: detail})
		return nil, errors.New(detail)

	case outcomeDropped:
		if err := o.Transfers.MarkDropped(ctx, a.ID, out.reason); err != nil {
			return nil, err
		}
		o.fire(ctx, alert.Alert{Severity: alert.SeverityWarning, ExternalID: ext, Reason: "relay_leg_transfer_dropped",
			Detail: fmt.Sprintf("leg %s: %s transfer %s will never land (%s) -- a new transaction will be built",
				ext, req.kind(), *a.TxHash, out.reason)})
		return nil, nil
	}

	if now.Sub(a.CreatedAt) > sentAttemptStuckAfter {
		if fired, err := o.Transfers.MarkStuckAlerted(ctx, a.ID); err == nil && fired {
			o.fire(ctx, alert.Alert{Severity: alert.SeverityWarning, ExternalID: ext, Reason: "relay_leg_transfer_stuck",
				Detail: fmt.Sprintf("leg %s: %s transfer %s was signed %s ago and still hasn't landed (last send error: %s)",
					ext, req.kind(), *a.TxHash, now.Sub(a.CreatedAt).Round(time.Second), valueOrEmpty(a.LastBroadcastError))})
		}
	}
	if a.LastBroadcastAt == nil || now.Sub(*a.LastBroadcastAt) >= rebroadcastEvery {
		return nil, o.sendAttempt(ctx, ad, req, a)
	}
	return nil, nil
}

// alreadyOnChain reports whether a send error only means the network
// already has this transaction, or already mined a transaction with its
// nonce -- neither is a failure to act on. Whether it was ours that
// landed is the outcome check's call.
func alreadyOnChain(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"already known", "known transaction", "nonce too low", "dup_transaction_error"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// onChainUnits scales amount to the token's raw on-chain units.
func onChainUnits(amount money.Amount, onChainDecimals int) (*big.Int, error) {
	internal, err := amount.Asset.Decimals()
	if err != nil {
		return nil, err
	}
	if onChainDecimals < internal {
		return nil, fmt.Errorf("token has %d decimals, fewer than %s's own %d", onChainDecimals, amount.Asset, internal)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(onChainDecimals-internal)), nil)
	return new(big.Int).Mul(big.NewInt(amount.Units), scale), nil
}

// requireBalance refuses to build a token transfer the sender can't pay.
func (o *Orchestrator) requireBalance(ctx context.Context, req transferRequest, have *big.Int, onChainDecimals int) error {
	need, err := onChainUnits(req.amount, onChainDecimals)
	if err != nil {
		return err
	}
	if have.Cmp(need) >= 0 {
		return nil
	}
	detail := fmt.Sprintf("%s: %s transfer NOT built -- %s holds %s raw units, the transfer needs %s",
		req.job, req.kind(), req.from, have, need)
	o.alertOnce(ctx, req.job, "relay_leg_insufficient_funds", alert.SeverityCritical, detail)
	return errors.New(detail)
}

// alertOnce fires an alert for (externalID, reason) unless the exact same
// detail was already fired -- so a condition that persists across ticks
// alerts once, and alerts again only when it changes.
func (o *Orchestrator) alertOnce(ctx context.Context, externalID, reason string, severity alert.Severity, detail string) {
	key := externalID + "|" + reason
	o.mu.Lock()
	already := o.preflightAlerted[key] == detail
	o.preflightAlerted[key] = detail
	o.mu.Unlock()
	if !already {
		o.fire(ctx, alert.Alert{Severity: severity, ExternalID: externalID, Reason: reason, Detail: detail})
	}
}

// fire delivers a, logging (never returning) a delivery failure: the
// alert channel being down must not block the money path.
func (o *Orchestrator) fire(ctx context.Context, a alert.Alert) {
	if err := o.Alert.Fire(ctx, a); err != nil {
		slog.Error("orchestrate: firing an alert itself failed", "external_id", a.ExternalID, "reason", a.Reason, "error", err)
	}
}
