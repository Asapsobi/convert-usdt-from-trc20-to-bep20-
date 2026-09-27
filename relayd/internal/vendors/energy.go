package vendors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"relayd/internal/energy"
)

// EnergyVendor is one TRON energy rental vendor.
type EnergyVendor interface {
	// MinUnits is the smallest rental the vendor accepts.
	MinUnits() int64
	// QuoteSun is what renting units of energy for one delegation costs.
	QuoteSun(ctx context.Context, units int64) (int64, error)
	// Rent delegates units of energy to target, paid from our account
	// with the vendor.
	Rent(ctx context.Context, target string, units int64) (Rental, error)
}

// EnergyAccountReader is an energy vendor that can report our prepaid
// account with it.
type EnergyAccountReader interface {
	Account(ctx context.Context) (EnergyAccount, error)
}

// EnergyAccount is our prepaid account with an energy vendor: rentals are
// paid from its balance.
type EnergyAccount struct {
	BalanceSun   int64
	TopUpAddress string // TRX sent here tops the balance up
}

// Rental is a vendor's accepted rental.
type Rental struct {
	OrderID string
	CostSun int64
}

// EnergyRejection marks a vendor refusing a rental (a bad address, an
// amount it doesn't sell) rather than being unavailable -- the next vendor
// is tried, but this one stays in the pool.
type EnergyRejection struct{ Err error }

func (e *EnergyRejection) Error() string { return e.Err.Error() }
func (e *EnergyRejection) Unwrap() error { return e.Err }

// EnergyRouter rents energy from the cheapest (or highest-priority)
// enabled, healthy vendor, failing over down the list. It implements the
// same Reserve call relayd's TRON transfers already use.
type EnergyRouter struct {
	store   *Store
	vendors map[string]EnergyVendor
}

// NewEnergyRouter registers every vendor in vs (keyed by name) and routes
// rentals across them.
func NewEnergyRouter(ctx context.Context, store *Store, vs map[string]EnergyVendor) (*EnergyRouter, error) {
	if len(vs) == 0 {
		return nil, errors.New("vendors: no energy vendor is configured")
	}
	for name := range vs {
		if err := store.Register(ctx, Energy, name); err != nil {
			return nil, err
		}
	}
	return &EnergyRouter{store: store, vendors: vs}, nil
}

func (r *EnergyRouter) record(ctx context.Context, name string, err error) {
	var rejection *EnergyRejection
	var storeErr error
	switch {
	case err == nil:
		storeErr = r.store.RecordSuccess(ctx, Energy, name)
	case !errors.As(err, &rejection):
		slog.Warn("vendors: energy vendor failed, taking it out of the pool for a while", "vendor", name, "error", err)
		storeErr = r.store.RecordFailure(ctx, Energy, name, err)
	}
	if storeErr != nil {
		slog.Error("vendors: recording vendor health failed", "vendor", name, "error", storeErr)
	}
}

type energyCandidate struct {
	name  string
	units int64
	cost  int64
}

// Reserve rents units of energy for target -- the exact shortfall, raised
// only to a vendor's own minimum. externalID, tier, and deadline are part
// of the shared EnergyClient shape and unused here: a direct rental needs
// no order and settles within the call. idempotencyKey is recorded by the
// caller (resource_rentals) before this is called.
func (r *EnergyRouter) Reserve(ctx context.Context, externalID, target string, units int64, tier string,
	deadline time.Time, idempotencyKey string) (energy.Reservation, error) {
	configured := make(map[string]bool, len(r.vendors))
	for name := range r.vendors {
		configured[name] = true
	}
	usable, err := r.store.usable(ctx, Energy, configured)
	if err != nil {
		return energy.Reservation{}, err
	}
	if len(usable) == 0 {
		return energy.Reservation{}, errors.New("vendors: no energy vendor is enabled and healthy right now")
	}
	strategy, err := r.store.Strategy(ctx, Energy)
	if err != nil {
		return energy.Reservation{}, err
	}

	var failures []string
	var cands []energyCandidate
	for _, v := range usable {
		c := energyCandidate{name: v.Name, units: max(units, r.vendors[v.Name].MinUnits())}
		if strategy == Cheapest {
			cost, err := r.vendors[v.Name].QuoteSun(ctx, c.units)
			if err != nil {
				r.record(ctx, v.Name, err)
				failures = append(failures, fmt.Sprintf("%s: %v", v.Name, err))
				continue
			}
			c.cost = cost
		}
		cands = append(cands, c)
	}
	if strategy == Cheapest {
		sort.SliceStable(cands, func(a, b int) bool { return cands[a].cost < cands[b].cost })
	}

	for _, c := range cands {
		rental, err := r.vendors[c.name].Rent(ctx, target, c.units)
		r.record(ctx, c.name, err)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", c.name, err))
			continue
		}
		vendor := c.name
		cost := fmt.Sprintf("%d.%06d", rental.CostSun/1_000_000, rental.CostSun%1_000_000)
		now := time.Now().UTC()
		return energy.Reservation{
			ExternalID: externalID, TargetAddress: target, EnergyUnits: c.units, Tier: tier,
			Status: "CONFIRMED", Vendor: &vendor, CostTRX: &cost, ConfirmedAt: &now, CreatedAt: now,
		}, nil
	}
	return energy.Reservation{}, fmt.Errorf("vendors: every energy vendor failed this rental: %s", strings.Join(failures, "; "))
}

// EnergyQuote is one energy vendor's live price for an amount of energy.
type EnergyQuote struct {
	Vendor  string
	Units   int64
	CostSun int64
	Err     error
}

// Account reads our prepaid account with the vendor called name. ok is
// false when that vendor can't report one.
func (r *EnergyRouter) Account(ctx context.Context, name string) (acct EnergyAccount, ok bool, err error) {
	reader, ok := r.vendors[name].(EnergyAccountReader)
	if !ok {
		return EnergyAccount{}, false, nil
	}
	acct, err = reader.Account(ctx)
	return acct, true, err
}

// QuoteEach asks every configured energy vendor for its price on units
// (raised to each vendor's minimum): the administrator's pricing view.
func (r *EnergyRouter) QuoteEach(ctx context.Context, units int64) []EnergyQuote {
	out := make([]EnergyQuote, 0, len(r.vendors))
	for name, v := range r.vendors {
		n := max(units, v.MinUnits())
		cost, err := v.QuoteSun(ctx, n)
		out = append(out, EnergyQuote{Vendor: name, Units: n, CostSun: cost, Err: err})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Vendor < out[b].Vendor })
	return out
}
