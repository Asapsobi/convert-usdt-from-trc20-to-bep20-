package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"time"

	"relayd/internal/alert"
	"relayd/internal/money"
	"relayd/internal/transfers"
)

// Provisioning a deposit wallet before it sends: BNB for gas on BSC; on
// TRON, activation and bandwidth (TRX) and energy (rented). Each returns
// errNotReady while it waits, so the transfer it serves is retried next
// tick rather than built before its sender can pay for it.

const (
	// defaultGasTopUpWei is the least BNB a gas top-up sends: 0.0005 BNB
	// pays for roughly fifty USDT transfers at BSC's usual gas price, so a
	// pooled wallet needs topping up rarely, not before every transfer.
	defaultGasTopUpWei = 500_000_000_000_000
	// defaultTRXTopUpSun is the least TRX a TRON top-up sends: 2 TRX
	// activates a new wallet and pays for several transfers' bandwidth.
	defaultTRXTopUpSun = 2_000_000
	// maxTopUpRounds caps top-ups for one transfer; needing more means
	// something is wrong (a runaway gas price, a wallet draining).
	maxTopUpRounds = 3
	// topUpSettle is how long after a top-up confirms its effect is
	// awaited before another round is considered -- a lagging node can
	// report the old balance for a moment.
	topUpSettle = 2 * time.Minute

	// maxEnergyRentals caps rentals for one transfer.
	maxEnergyRentals = 5
	// rentalPendingTimeout: a rental still PENDING this long has no
	// recorded outcome (relayd stopped mid-request).
	rentalPendingTimeout = 2 * time.Minute
	// rentalArrivalWait is how long rented energy is given to show up on
	// the wallet before it is rented again.
	rentalArrivalWait = 2 * time.Minute
)

// staleUnsignedTopUp is how long an unsigned top-up may sit before it is
// abandoned: its wallet was provisioned some other way, or no longer
// needs it -- and an open attempt blocks the treasury's other sends.
const staleUnsignedTopUp = 10 * time.Minute

// trackTopUps follows every open treasury top-up to its on-chain outcome.
// A top-up is normally driven by the transfer it pays for, but that
// transfer stops asking once its wallet can pay -- without this phase its
// top-up would stay open forever, blocking every later treasury send.
func (o *Orchestrator) trackTopUps(ctx context.Context) error {
	open, err := o.Transfers.ListOpen(ctx, transfers.GasTopUp, transfers.TRXTopUp)
	if err != nil {
		return err
	}
	for _, a := range open {
		if !a.Status.MayLand() {
			if time.Since(a.CreatedAt) > staleUnsignedTopUp {
				if err := o.Transfers.MarkAbandoned(ctx, a.ID, "no longer needed before it was signed"); err != nil {
					slog.Error("orchestrate: abandoning a stale top-up failed", "job", a.ExternalID, "error", err)
				}
			}
			continue
		}
		ad, err := o.adapterFor(a.Chain)
		if err != nil {
			return err
		}
		req := transferRequest{job: a.ExternalID, purpose: a.Purpose, chain: a.Chain, from: a.FromAddress,
			signer: signer{slot: true}, to: a.ToAddress, amount: a.Amount}
		if _, err := o.trackSent(ctx, ad, req, a); err != nil {
			slog.Error("orchestrate: tracking a treasury top-up failed, will retry next tick", "job", a.ExternalID, "error", err)
		}
	}
	return nil
}

// treasuryAddress is where top-ups come from on chain: the S1 slot's
// own address in that chain's encoding.
func (o *Orchestrator) treasuryAddress(chain transfers.Chain) string {
	if chain == transfers.BSC {
		return o.Cfg.SlotEVMAddress
	}
	return o.Cfg.SlotAddress
}

// topUp funds parent's sending wallet from the treasury: BNB for gas on
// BSC, TRX on TRON (activating a new wallet). shortfall is what is
// missing; at least the configured minimum is sent, so a pooled wallet is
// topped up rarely. Always returns an error: errNotReady while the top-up
// is under way, a real error when it can't be done.
func (o *Orchestrator) topUp(ctx context.Context, parent transferRequest, purpose transfers.Purpose, shortfall *big.Int) error {
	treasury := o.treasuryAddress(parent.chain)
	if treasury == "" {
		return fmt.Errorf("%s needs a %s from the treasury, but no treasury address is configured", parent.from, purpose)
	}
	prefix := fmt.Sprintf("%s:%s:%s:", strings.ToLower(string(purpose)), parent.job, parent.purpose)
	rounds, err := o.Transfers.CountConfirmedWithPrefix(ctx, prefix, purpose)
	if err != nil {
		return err
	}
	if rounds > 0 {
		if last, ok, err := o.Transfers.LastConfirmedWithPrefix(ctx, prefix, purpose); err != nil {
			return err
		} else if ok && time.Since(last) < topUpSettle {
			return notReady("a %s to %s just landed; waiting for its balance to show", purpose, parent.from)
		}
	}
	if rounds >= maxTopUpRounds {
		detail := fmt.Sprintf("%s: %s's %s still can't be paid for after %d top-ups from the treasury -- investigate",
			parent.job, parent.from, parent.kind(), rounds)
		o.alertOnce(ctx, parent.job, "relay_leg_topup_gave_up", alert.SeverityCritical, detail)
		return errors.New(detail)
	}

	var amount money.Amount
	switch purpose {
	case transfers.GasTopUp:
		units := new(big.Int).Mul(shortfall, big.NewInt(2))
		if min := big.NewInt(o.gasTopUpWei()); units.Cmp(min) < 0 {
			units = min
		}
		amount = money.Amount{Asset: assetBNB, Units: units.Int64()}
	case transfers.TRXTopUp:
		units := shortfall.Int64()
		if min := o.trxTopUpSun(); units < min {
			units = min
		}
		amount = money.Amount{Asset: assetTRX, Units: units}
	default:
		return fmt.Errorf("%s is not a top-up", purpose)
	}

	req := transferRequest{
		job: prefix + strconv.Itoa(rounds), purpose: purpose, chain: parent.chain,
		from: treasury, signer: signer{slot: true}, to: parent.from, amount: amount,
	}
	done, err := o.driveTransfer(ctx, req)
	if err != nil {
		return fmt.Errorf("topping up %s from the treasury: %w", parent.from, err)
	}
	if done != nil {
		return notReady("a %s to %s landed; checking its balance again", purpose, parent.from)
	}
	return notReady("waiting for a %s of %d %s from the treasury to %s", purpose, amount.Units, amount.Asset, parent.from)
}

func (o *Orchestrator) gasTopUpWei() int64 {
	if o.Cfg.GasTopUpWei > 0 {
		return o.Cfg.GasTopUpWei
	}
	return defaultGasTopUpWei
}

func (o *Orchestrator) trxTopUpSun() int64 {
	if o.Cfg.TRXTopUpSun > 0 {
		return o.Cfg.TRXTopUpSun
	}
	return defaultTRXTopUpSun
}

// rentEnergy rents the energy req's sender is short of -- exactly the
// shortfall, never a fixed amount. Each rental is recorded before the
// vendor is asked, so a restart mid-request is seen (and waited out)
// instead of buying twice. Always returns an error: errNotReady while
// rented energy is on its way, a real error when renting failed.
func (o *Orchestrator) rentEnergy(ctx context.Context, req transferRequest, shortfall int64) error {
	latest, n, err := o.Transfers.LatestRental(ctx, req.job, req.purpose, "ENERGY")
	if err != nil {
		return err
	}
	if n > 0 {
		switch latest.Status {
		case "PENDING":
			if time.Since(latest.CreatedAt) < rentalPendingTimeout {
				return notReady("an energy rental for %s is in progress", req.from)
			}
			if err := o.Transfers.FinishRental(ctx, latest.ID, false, nil, nil,
				errors.New("no outcome recorded -- relayd stopped mid-request")); err != nil {
				return err
			}
		case "CONFIRMED":
			if time.Since(latest.UpdatedAt) < rentalArrivalWait {
				return notReady("waiting for %d rented energy to reach %s", latest.Units, req.from)
			}
		}
	}
	if n >= maxEnergyRentals {
		detail := fmt.Sprintf("%s: %s still lacks energy after %d rentals -- investigate before renting more", req.job, req.from, n)
		o.alertOnce(ctx, req.job, "relay_leg_energy_gave_up", alert.SeverityCritical, detail)
		return errors.New(detail)
	}
	if o.Energy == nil {
		return fmt.Errorf("%s needs %d energy but no energy provider is configured", req.from, shortfall)
	}

	rental, err := o.Transfers.CreateRental(ctx, transfers.Rental{
		Job: req.job, Purpose: req.purpose, Address: req.from, Resource: "ENERGY", Units: shortfall,
		IdempotencyKey: fmt.Sprintf("relayd:energy:%s:%s:%d", req.job, req.purpose, n),
	})
	if err != nil {
		return err
	}
	res, err := o.Energy.Reserve(ctx, req.job, req.from, shortfall, "STANDARD",
		time.Now().Add(defaultEnergyDeadlineWindow), rental.IdempotencyKey)
	if err == nil && res.Status != "CONFIRMED" {
		err = fmt.Errorf("the energy reservation ended %s, not CONFIRMED", res.Status)
	}
	if err != nil {
		if finishErr := o.Transfers.FinishRental(ctx, rental.ID, false, nil, nil, err); finishErr != nil {
			slog.Error("orchestrate: recording a failed rental failed", "job", req.job, "error", finishErr)
		}
		return fmt.Errorf("renting %d energy for %s: %w", shortfall, req.from, err)
	}
	cost := trxToSun(res.CostTRX)
	if err := o.Transfers.FinishRental(ctx, rental.ID, true, res.Vendor, cost, nil); err != nil {
		return err
	}
	slog.Info("orchestrate: rented energy", "job", req.job, "address", req.from, "units", shortfall,
		"vendor", valueOrEmpty(res.Vendor), "cost_trx", valueOrEmpty(res.CostTRX))
	return notReady("rented %d energy for %s; waiting for it to arrive", shortfall, req.from)
}

// trxToSun parses a decimal TRX amount ("3.25") into sun.
func trxToSun(trx *string) *int64 {
	if trx == nil {
		return nil
	}
	r, ok := new(big.Rat).SetString(*trx)
	if !ok {
		return nil
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	sun := new(big.Int).Quo(r.Num(), r.Denom()).Int64()
	return &sun
}
