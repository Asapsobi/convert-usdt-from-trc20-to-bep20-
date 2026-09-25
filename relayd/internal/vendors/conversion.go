package vendors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"relayd/internal/money"
	"relayd/internal/upstream"
)

// ConversionRouter is an upstream.SwapProvider over every configured
// conversion vendor: each request goes to the vendor the administrator's
// strategy prefers among those enabled and healthy, and fails over to the
// next one when it can't be served.
type ConversionRouter struct {
	store   *Store
	vendors map[string]upstream.SwapProvider
}

// NewConversionRouter registers every vendor in providers (keyed by the
// name its orders report as ProviderName) and routes across them.
func NewConversionRouter(ctx context.Context, store *Store, providers map[string]upstream.SwapProvider) (*ConversionRouter, error) {
	if len(providers) == 0 {
		return nil, errors.New("vendors: no conversion vendor is configured")
	}
	for name := range providers {
		if err := store.Register(ctx, Conversion, name); err != nil {
			return nil, err
		}
	}
	return &ConversionRouter{store: store, vendors: providers}, nil
}

var _ upstream.SwapProvider = (*ConversionRouter)(nil)

// isOutage reports whether err means the vendor itself is unhealthy (a
// transport failure, a 5xx, an unreadable answer) rather than refusing
// this one request -- only an outage takes a vendor out of the pool.
func isOutage(err error) bool {
	var apiErr *upstream.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code >= 500 && apiErr.Code < 600
	}
	return true
}

func (r *ConversionRouter) record(ctx context.Context, name string, err error) {
	var storeErr error
	switch {
	case err == nil:
		storeErr = r.store.RecordSuccess(ctx, Conversion, name)
	case isOutage(err):
		slog.Warn("vendors: conversion vendor failed, taking it out of the pool for a while", "vendor", name, "error", err)
		storeErr = r.store.RecordFailure(ctx, Conversion, name, err)
	}
	if storeErr != nil {
		slog.Error("vendors: recording vendor health failed", "vendor", name, "error", storeErr)
	}
}

func (r *ConversionRouter) candidates(ctx context.Context) ([]Vendor, error) {
	configured := make(map[string]bool, len(r.vendors))
	for name := range r.vendors {
		configured[name] = true
	}
	usable, err := r.store.usable(ctx, Conversion, configured)
	if err != nil {
		return nil, err
	}
	if len(usable) == 0 {
		return nil, errors.New("vendors: no conversion vendor is enabled and healthy right now")
	}
	return usable, nil
}

type rankedQuote struct {
	vendor Vendor
	quote  upstream.Quote
}

// rank orders the usable vendors for pair/amountIn: by the payout each
// quotes (best_rate) or by the administrator's priority (priority).
// Vendors that fail to quote are left out, and their health recorded.
func (r *ConversionRouter) rank(ctx context.Context, pair upstream.Pair, amountIn money.Amount) ([]rankedQuote, []string, error) {
	cands, err := r.candidates(ctx)
	if err != nil {
		return nil, nil, err
	}
	strategy, err := r.store.Strategy(ctx, Conversion)
	if err != nil {
		return nil, nil, err
	}
	results := make([]rankedQuote, len(cands))
	errs := make([]error, len(cands))
	var wg sync.WaitGroup
	for i, v := range cands {
		wg.Add(1)
		go func(i int, v Vendor) {
			defer wg.Done()
			q, err := r.vendors[v.Name].Quote(ctx, pair, amountIn)
			results[i], errs[i] = rankedQuote{vendor: v, quote: q}, err
		}(i, v)
	}
	wg.Wait()

	var ok []rankedQuote
	var failures []string
	for i, res := range results {
		r.record(ctx, res.vendor.Name, errs[i])
		if errs[i] != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", res.vendor.Name, errs[i]))
			continue
		}
		ok = append(ok, res)
	}
	if strategy == BestRate {
		sort.SliceStable(ok, func(a, b int) bool { return ok[a].quote.AmountOut.Units > ok[b].quote.AmountOut.Units })
	}
	return ok, failures, nil
}

// Quote returns the preferred vendor's quote.
func (r *ConversionRouter) Quote(ctx context.Context, pair upstream.Pair, amountIn money.Amount) (upstream.Quote, error) {
	ranked, failures, err := r.rank(ctx, pair, amountIn)
	if err != nil {
		return upstream.Quote{}, err
	}
	if len(ranked) == 0 {
		return upstream.Quote{}, fmt.Errorf("%w: %s", upstream.ErrAllProvidersFailed, strings.Join(failures, "; "))
	}
	return ranked[0].quote, nil
}

// CreateOrder creates the order with the preferred vendor, failing over
// down the ranking until one accepts it.
func (r *ConversionRouter) CreateOrder(ctx context.Context, pair upstream.Pair, amountIn money.Amount, destinationAddress string) (upstream.SwapOrder, error) {
	ranked, failures, err := r.rank(ctx, pair, amountIn)
	if err != nil {
		return upstream.SwapOrder{}, err
	}
	for _, c := range ranked {
		order, err := r.vendors[c.vendor.Name].CreateOrder(ctx, pair, amountIn, destinationAddress)
		r.record(ctx, c.vendor.Name, err)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", c.vendor.Name, err))
			continue
		}
		if order.ProviderName == "" {
			order.ProviderName = c.vendor.Name
		}
		return order, nil
	}
	return upstream.SwapOrder{}, fmt.Errorf("%w: %s", upstream.ErrAllProvidersFailed, strings.Join(failures, "; "))
}

// GetOrderFrom asks the named vendor -- the one the order was created
// with -- for its status. Works whether or not that vendor is currently
// enabled or healthy: an existing order is always followed to the end.
func (r *ConversionRouter) GetOrderFrom(ctx context.Context, provider, providerOrderID string) (upstream.SwapOrder, error) {
	v, ok := r.vendors[provider]
	if !ok {
		return upstream.SwapOrder{}, fmt.Errorf("vendors: no conversion vendor %q is configured (order %s)", provider, providerOrderID)
	}
	order, err := v.GetOrder(ctx, providerOrderID)
	r.record(ctx, provider, err)
	return order, err
}

// GetOrder finds an order when its vendor isn't known: the first
// configured vendor that knows the id answers.
func (r *ConversionRouter) GetOrder(ctx context.Context, providerOrderID string) (upstream.SwapOrder, error) {
	var errs []string
	for name, v := range r.vendors {
		order, err := v.GetOrder(ctx, providerOrderID)
		if err == nil {
			return order, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", name, err))
	}
	return upstream.SwapOrder{}, fmt.Errorf("vendors: no conversion vendor knows order %s: %s", providerOrderID, strings.Join(errs, "; "))
}

// SelfCheck runs every configured vendor's own startup self-check (valid
// credentials, known currency codes) -- a misconfigured vendor fails
// relayd's startup rather than its first real order.
func (r *ConversionRouter) SelfCheck(ctx context.Context) error {
	for name, v := range r.vendors {
		if c, ok := v.(upstream.SelfChecker); ok {
			if err := c.SelfCheck(ctx); err != nil {
				return fmt.Errorf("conversion vendor %s: %w", name, err)
			}
		}
	}
	return nil
}
