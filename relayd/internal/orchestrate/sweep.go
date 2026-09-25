package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"relayd/internal/alert"
	"relayd/internal/evmtx"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/sweeps"
	"relayd/internal/transfers"
	"relayd/internal/txbuild"
)

// Sweeping: a deposit wallet keeps our profit from every order it served
// -- only the rest is forwarded. Moving it one order at a time would cost
// a transfer (on TRON, a rental of energy) per order, so it accumulates,
// and relayd moves it to the treasury in one transfer once a wallet's
// unswept profit reaches the administrator's minimum and no order is
// using the wallet.
//
// Only what the books say is ours ever leaves: the profit recorded on the
// wallet's settled legs, less what earlier sweeps took (internal/sweeps).
// Anything else a wallet holds -- a payment nobody ordered, a late
// deposit, a case waiting on an operator -- stays where it is. A wallet
// whose on-chain balance can't cover its recorded profit is not swept:
// the books and the chain disagree, and an operator is alerted instead.
//
// A sweep is an ordinary transfer (transfer.go): signed by the wallet's
// own key, its gas or energy provisioned first, one transaction open per
// wallet, recorded before it is signed. Once it confirms, the ledger
// moves the amount from asset:relay:commission_wallet to
// asset:relay:treasury.

const (
	// sweepGiveUpAfter: a sweep that still has no transaction that could
	// land this long after it started is given up; its amount becomes
	// sweepable again.
	sweepGiveUpAfter = 6 * time.Hour
	// sweepRetryAfter keeps a wallet whose last sweep was given up out of
	// sweeping for a while, so a persisting problem alerts once a day
	// instead of failing every interval.
	sweepRetryAfter = 24 * time.Hour
)

func sweepJob(id int64) string { return fmt.Sprintf("sweep:%d", id) }

func sweepTransfer(s sweeps.Sweep) transferRequest {
	index := s.DepositIndex
	return transferRequest{job: sweepJob(s.ID), purpose: transfers.Sweep, chain: s.Chain, from: s.FromAddress,
		signer: signer{depositIndex: &index}, to: s.ToAddress, amount: s.Amount}
}

// sweepDestination is where swept profit goes on chain.
func (o *Orchestrator) sweepDestination(chain transfers.Chain) string {
	if chain == transfers.BSC {
		if o.Cfg.SweepToBSC != "" {
			return o.Cfg.SweepToBSC
		}
		return o.Cfg.SlotEVMAddress
	}
	if o.Cfg.SweepToTRON != "" {
		return o.Cfg.SweepToTRON
	}
	return o.Cfg.SlotAddress
}

func treasuryAccountCode(asset money.Asset) string {
	return fmt.Sprintf("asset:relay:treasury:%s", asset)
}

// RequestSweepScan makes the next tick look for wallets to sweep instead
// of waiting out the rest of the interval.
func (o *Orchestrator) RequestSweepScan() {
	o.mu.Lock()
	o.lastSweepScan = time.Time{}
	o.mu.Unlock()
}

// advanceSweeps drives every sweep in flight toward its end.
func (o *Orchestrator) advanceSweeps(ctx context.Context) error {
	if o.Sweeps == nil {
		return nil
	}
	pending, err := o.Sweeps.ListPending(ctx)
	if err != nil {
		return err
	}
	for _, s := range pending {
		if err := o.advanceSweep(ctx, s); err != nil {
			slog.Error("orchestrate: advancing a sweep failed, will retry next tick", "sweep_id", s.ID, "from", s.FromAddress, "error", err)
		}
	}
	return nil
}

func (o *Orchestrator) advanceSweep(ctx context.Context, s sweeps.Sweep) error {
	done, err := o.driveTransfer(ctx, sweepTransfer(s))
	if err != nil || done == nil {
		if giveUpErr := o.giveUpStuckSweep(ctx, s, err); giveUpErr != nil {
			return errors.Join(err, giveUpErr)
		}
		return err
	}
	entryID, err := o.bookSweep(ctx, s, *done.TxHash)
	if err != nil {
		return fmt.Errorf("booking confirmed sweep %d (%s): %w", s.ID, *done.TxHash, err)
	}
	if err := o.Sweeps.MarkConfirmed(ctx, s.ID, *done.TxHash, entryID); err != nil {
		return err
	}
	slog.Info("orchestrate: profit swept to the treasury", "sweep_id", s.ID, "chain", s.Chain,
		"from", s.FromAddress, "to", s.ToAddress, "amount", fmtAmount(s.Amount), "tx_hash", *done.TxHash)
	return nil
}

// giveUpStuckSweep ends a sweep that can't get a transaction onto the
// chain -- after maxFailedAttempts on-chain failures, or sweepGiveUpAfter
// without one -- but never while a signed transaction of it might still
// land: only the chain resolves that.
func (o *Orchestrator) giveUpStuckSweep(ctx context.Context, s sweeps.Sweep, cause error) error {
	job := sweepJob(s.ID)
	open, hasOpen, err := o.Transfers.Open(ctx, job, transfers.Sweep)
	if err != nil {
		return err
	}
	if hasOpen && open.Status.MayLand() {
		return nil
	}
	failed, err := o.Transfers.CountFailed(ctx, job, transfers.Sweep)
	if err != nil {
		return err
	}
	if failed < maxFailedAttempts && time.Since(s.CreatedAt) < sweepGiveUpAfter {
		return nil
	}
	if hasOpen {
		if err := o.Transfers.MarkAbandoned(ctx, open.ID, "the sweep was given up before this was signed"); err != nil {
			return err
		}
	}
	reason := fmt.Sprintf("no transaction confirmed after %s and %d on-chain failures", time.Since(s.CreatedAt).Round(time.Minute), failed)
	if cause != nil {
		reason += ": " + cause.Error()
	}
	if err := o.Sweeps.MarkFailed(ctx, s.ID, reason); err != nil {
		return err
	}
	o.fire(ctx, alert.Alert{Severity: alert.SeverityWarning, ExternalID: job, Reason: "sweep_failed",
		Detail: fmt.Sprintf("sweep %d of %s %s from %s was given up (nothing moved; retried after %s): %s",
			s.ID, fmtAmount(s.Amount), s.Amount.Asset, s.FromAddress, sweepRetryAfter, reason)})
	return nil
}

// bookSweep records a confirmed sweep in the ledger: the profit leaves
// the deposit wallets and reaches the treasury.
func (o *Orchestrator) bookSweep(ctx context.Context, s sweeps.Sweep, txHash string) (int64, error) {
	asset := string(s.Amount.Asset)
	treasury, wallets := treasuryAccountCode(s.Amount.Asset), commissionWalletAccountCode(s.Amount.Asset)
	if err := o.Ledger.EnsureAccount(ctx, treasury, ledgerclient.AccountAsset, asset, "relayd:ensure-account:"+treasury); err != nil {
		return 0, fmt.Errorf("ensuring %s exists: %w", treasury, err)
	}
	neg, err := s.Amount.Neg()
	if err != nil {
		return 0, err
	}
	return o.Ledger.PostEntry(ctx, "relay_sweep", time.Now().UTC(), []ledgerclient.EntryLine{
		{AccountCode: treasury, Asset: asset, Amount: s.Amount},
		{AccountCode: wallets, Asset: asset, Amount: neg},
	}, map[string]any{
		"sweep_id": s.ID, "tx_hash": txHash, "from_address": s.FromAddress, "to_address": s.ToAddress,
	}, fmt.Sprintf("relayd:sweep:%d", s.ID))
}

// startSweeps, once per the administrator's interval, starts a sweep of
// every idle wallet holding at least the minimum of unswept profit.
func (o *Orchestrator) startSweeps(ctx context.Context) error {
	if o.Sweeps == nil {
		return nil
	}
	settings, err := o.Sweeps.Settings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	now := time.Now()
	o.mu.Lock()
	due := now.Sub(o.lastSweepScan) >= settings.Interval
	if due {
		o.lastSweepScan = now
	}
	o.mu.Unlock()
	if !due {
		return nil
	}

	wallets, err := o.Sweeps.Wallets(ctx)
	if err != nil {
		return err
	}
	for _, w := range wallets {
		if err := o.startSweep(ctx, w, settings, now); err != nil {
			slog.Error("orchestrate: starting a sweep failed, will retry next interval", "wallet", w.Address, "error", err)
		}
	}
	return nil
}

func (o *Orchestrator) startSweep(ctx context.Context, w sweeps.Wallet, settings sweeps.Settings, now time.Time) error {
	unswept := w.Unswept()
	switch {
	case unswept.Units <= 0 || unswept.Units < settings.MinAmount[unswept.Asset]:
		return nil
	case w.Busy || w.Pending:
		return nil // an order is using it, or a sweep already is
	case w.LastFailedAt != nil && now.Sub(*w.LastFailedAt) < sweepRetryAfter:
		return nil
	}
	alertKey := "sweep:" + w.Address
	if w.DepositIndex == nil {
		o.alertOnce(ctx, alertKey, "sweep_wallet_key_unknown", alert.SeverityWarning,
			fmt.Sprintf("deposit wallet %s holds %s %s of unswept profit, but its legs don't agree on the key that controls it (conflicting: %v) -- not sweeping it",
				w.Address, fmtAmount(unswept), unswept.Asset, w.IndexConflict))
		return nil
	}
	if _, busy, err := o.Transfers.OpenFrom(ctx, w.Address); err != nil || busy {
		return err
	}

	have, decimals, err := o.tokenBalance(ctx, w.Chain, w.Address)
	if err != nil {
		return fmt.Errorf("reading %s's USDT balance: %w", w.Address, err)
	}
	need, err := onChainUnits(unswept, decimals)
	if err != nil {
		return err
	}
	if have.Cmp(need) < 0 {
		o.alertOnce(ctx, alertKey, "sweep_balance_short", alert.SeverityCritical,
			fmt.Sprintf("deposit wallet %s should still hold %s %s of our profit, but holds only %s raw units on-chain -- the books and the chain disagree; not sweeping it until an operator investigates",
				w.Address, fmtAmount(unswept), unswept.Asset, have))
		return nil
	}

	to := o.sweepDestination(w.Chain)
	if to == "" {
		return fmt.Errorf("no sweep destination is configured for %s", w.Chain)
	}
	s, err := o.Sweeps.Create(ctx, sweeps.Sweep{Chain: w.Chain, FromAddress: w.Address, DepositIndex: *w.DepositIndex,
		ToAddress: to, Amount: unswept})
	if errors.Is(err, sweeps.ErrPending) {
		return nil
	}
	if err != nil {
		return err
	}
	slog.Info("orchestrate: sweeping profit to the treasury", "sweep_id", s.ID, "chain", s.Chain,
		"from", s.FromAddress, "to", s.ToAddress, "amount", fmtAmount(s.Amount))
	return o.advanceSweep(ctx, s)
}

// tokenBalance is holder's USDT balance on chain, in raw on-chain units,
// with the token's on-chain decimals.
func (o *Orchestrator) tokenBalance(ctx context.Context, chain transfers.Chain, holder string) (*big.Int, int, error) {
	if chain == transfers.BSC {
		have, err := o.EVMChain.TokenBalance(ctx, holder)
		return have, evmtx.USDTOnChainDecimals, err
	}
	have, err := o.Chain.TokenBalance(ctx, holder)
	return have, txbuild.USDTOnChainDecimals, err
}
