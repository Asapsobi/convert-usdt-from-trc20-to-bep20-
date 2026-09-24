// Package chain is the multi-provider RPC layer: every read this service
// does against BSC goes through a Pool, never a single client directly.
// Getting this wrong is invisible until the day one provider quietly
// diverges from the others -- see LatestFinalized and LogsAt for where
// that would actually show up.
package chain

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/ethclient"
)

// Provider is one configured RPC endpoint this package treats as an
// independent source of truth about chain state. Name is used in every
// error, log line, and health record -- pick something operator-
// recognizable (a vendor name, not a bare URL).
type Provider struct {
	Name   string
	Client *ethclient.Client
}

// Config controls a Pool's agreement and health-tracking thresholds.
type Config struct {
	// MinAgreement is how many providers must independently report the
	// same result before LatestFinalized or LogsAt trusts it. Defaults to
	// 2 if zero or negative -- this is invariant 5 from the C2 spec's
	// standing context ("No single RPC provider is trusted alone for a
	// finality decision"), not a tunable a deployment should routinely
	// lower.
	MinAgreement int

	// UnhealthyAfterConsecutiveFailures is how many consecutive rounds a
	// provider must fail or disagree before ProviderHealthSnapshot marks
	// it unhealthy. Defaults to 3 if zero or negative.
	UnhealthyAfterConsecutiveFailures int

	// AllowSingleProvider is an explicit, deliberate override of
	// invariant 5, for a deployment that has decided (operator's own
	// informed choice, not this package's default) to trust exactly one
	// RPC provider rather than get a second one. With exactly one
	// provider and this set, MinAgreement defaults to 1 instead of the
	// usual 2 -- there is no second provider left to agree with. False
	// (the default) preserves invariant 5 exactly as before: fewer than
	// 2 providers is always ErrTooFewProviders. See
	// cmd/watcherd/engine.go's own WATCHER_ALLOW_SINGLE_PROVIDER gate,
	// the same double-gate convention WATCHER_ALLOW_ASYNC_FINALITY
	// already uses for an equally deliberate, non-default posture.
	AllowSingleProvider bool
}

// ErrTooFewProviders is returned by NewPool for fewer than 2 providers,
// unless Config.AllowSingleProvider was explicitly set -- refusing to
// even construct a Pool by default, per the spec: "this isn't a
// runtime-degradable feature, it's invariant 5." A caller cannot obtain a
// Pool that would silently trust a single provider.
var ErrTooFewProviders = errors.New("chain: at least 2 providers are required (no single-provider finality) -- " +
	"set AllowSingleProvider to explicitly override")

// Pool fans a query out to every configured provider and reports a
// result only when enough of them agree. It never speaks for a single
// provider on its own.
type Pool struct {
	providers      []Provider
	minAgreement   int
	unhealthyAfter int

	mu     sync.Mutex
	health map[string]*health
}

// NewPool validates providers and cfg and returns a ready-to-use Pool, or
// an error if the configuration cannot possibly satisfy this package's
// own invariants: fewer than 2 providers, a MinAgreement no configured
// set of providers could reach, a duplicate or empty provider name, or a
// nil client. All of these are configuration mistakes worth failing loud
// on at construction, not discovering on the first call.
func NewPool(providers []Provider, cfg Config) (*Pool, error) {
	if len(providers) < 2 && !(cfg.AllowSingleProvider && len(providers) == 1) {
		return nil, ErrTooFewProviders
	}
	seen := make(map[string]bool, len(providers))
	for _, prov := range providers {
		if prov.Name == "" {
			return nil, errors.New("chain: every provider must have a non-empty Name")
		}
		if prov.Client == nil {
			return nil, fmt.Errorf("chain: provider %q has a nil Client", prov.Name)
		}
		if seen[prov.Name] {
			return nil, fmt.Errorf("chain: duplicate provider name %q", prov.Name)
		}
		seen[prov.Name] = true
	}

	minAgreement := cfg.MinAgreement
	if minAgreement <= 0 {
		minAgreement = 2
		if cfg.AllowSingleProvider && len(providers) == 1 {
			minAgreement = 1
		}
	}
	if minAgreement > len(providers) {
		return nil, fmt.Errorf("chain: MinAgreement (%d) exceeds the number of configured providers (%d)",
			minAgreement, len(providers))
	}

	unhealthyAfter := cfg.UnhealthyAfterConsecutiveFailures
	if unhealthyAfter <= 0 {
		unhealthyAfter = 3
	}

	p := &Pool{
		providers:      append([]Provider(nil), providers...), // defensive copy
		minAgreement:   minAgreement,
		unhealthyAfter: unhealthyAfter,
		health:         make(map[string]*health, len(providers)),
	}
	for _, prov := range providers {
		p.health[prov.Name] = &health{}
	}
	return p, nil
}
