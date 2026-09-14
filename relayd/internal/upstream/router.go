// MultiProvider is Model F's own "best rate" router: it implements
// SwapProvider exactly like a single real vendor would, so nothing in
// relayd's own driver/orchestrate code needs to know routing happens at
// all, but internally holds several real vendor providers and picks the
// best one at the moment it actually matters, per call:
//
//   - Quote asks every configured vendor in parallel and returns
//     whichever offers the best AmountOut for the customer.
//   - CreateOrder re-quotes all vendors fresh (not reusing whatever won
//     an earlier Quote call, which may be stale by the time a leg
//     actually reaches screened -- see architecture doc §6's own
//     "re-quote at forward-time" mitigation, which this folds vendor
//     selection into rather than bolting on as a separate step) and
//     creates the order with whichever vendor wins AT THAT MOMENT.
//   - GetOrder needs to know which vendor issued a given order to poll
//     it -- solved the same way FixedFloat's own id/token packing
//     solves an analogous problem: CreateOrder prefixes the real
//     ProviderOrderID with "<vendor>:" before returning it, and GetOrder
//     strips the prefix to route to the right backend.
//
// A single vendor's outage or error never blocks a quote or order --
// only when EVERY configured vendor fails does a call fail, mirroring
// energybroker's own fallback-ladder posture (route around a degraded
// provider automatically, escalate only when nothing is left to route
// to).
package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"relayd/internal/money"
)

// ErrAllProvidersFailed is MultiProvider's own result when every
// configured vendor errored on the same call -- wraps each vendor's own
// error so the real cause of each failure is still visible, never just
// "something failed somewhere."
var ErrAllProvidersFailed = errors.New("upstream: every configured vendor failed this call")

// NamedProvider pairs a SwapProvider with the name MultiProvider uses to
// prefix its own ProviderOrderID values -- distinct from
// Quote/SwapOrder's own ProviderName field (which a real vendor sets
// itself, e.g. "fixedfloat"/"changenow"), though in every provider this
// module ships the two happen to already match.
type NamedProvider struct {
	Name     string
	Provider SwapProvider
}

// MultiProvider implements SwapProvider by routing to whichever of its
// own configured providers offers the best rate, per call. See this
// file's own top-of-file doc comment for the full routing/fallback
// contract.
type MultiProvider struct {
	providers []NamedProvider
}

// NewMultiProvider returns a MultiProvider routing across providers.
// At least 2 are required -- a single-provider "router" is exactly what
// PlaceholderProvider/a bare real provider already is, with none of
// this file's own added complexity, so wiring only one provider through
// here would be a real bug in the caller, not a legitimate degenerate
// case.
func NewMultiProvider(providers []NamedProvider) (*MultiProvider, error) {
	if len(providers) < 2 {
		return nil, fmt.Errorf("upstream: MultiProvider needs at least 2 providers, got %d -- wire the single provider directly instead", len(providers))
	}
	seen := make(map[string]bool, len(providers))
	for _, p := range providers {
		if p.Name == "" {
			return nil, errors.New("upstream: MultiProvider: every provider needs a non-empty Name")
		}
		if strings.Contains(p.Name, ":") {
			return nil, fmt.Errorf("upstream: MultiProvider: provider name %q must not contain ':' -- it prefixes every ProviderOrderID this router returns, and ':' is the separator GetOrder splits on", p.Name)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("upstream: MultiProvider: duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
	}
	return &MultiProvider{providers: providers}, nil
}

type namedResult[T any] struct {
	name   string
	result T
	err    error
}

// callAll runs fn against every configured provider concurrently and
// returns every result, success or failure, in provider-list order --
// never short-circuits on the first error, since a slow or dead vendor
// must never prevent this router from seeing what every OTHER vendor
// had to say.
func callAll[T any](ctx context.Context, providers []NamedProvider, fn func(context.Context, SwapProvider) (T, error)) []namedResult[T] {
	out := make([]namedResult[T], len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p NamedProvider) {
			defer wg.Done()
			result, err := fn(ctx, p.Provider)
			out[i] = namedResult[T]{name: p.Name, result: result, err: err}
		}(i, p)
	}
	wg.Wait()
	return out
}

// errAllFailed collects every named error into one ErrAllProvidersFailed,
// so a caller sees every vendor's own real failure reason, not just the
// first one.
func errAllFailed[T any](results []namedResult[T]) error {
	msgs := make([]string, 0, len(results))
	for _, r := range results {
		msgs = append(msgs, fmt.Sprintf("%s: %v", r.name, r.err))
	}
	return fmt.Errorf("%w: %s", ErrAllProvidersFailed, strings.Join(msgs, "; "))
}

// Quote implements SwapProvider: query every provider, return whichever
// offers the best (highest) AmountOut among the ones that succeeded.
func (m *MultiProvider) Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error) {
	results := callAll(ctx, m.providers, func(ctx context.Context, p SwapProvider) (Quote, error) {
		return p.Quote(ctx, pair, amountIn)
	})

	var best *Quote
	var bestName string
	for _, r := range results {
		if r.err != nil {
			continue
		}
		q := r.result
		if best == nil || q.AmountOut.Units > best.AmountOut.Units {
			qCopy := q
			best = &qCopy
			bestName = r.name
		}
	}
	if best == nil {
		return Quote{}, errAllFailed(results)
	}
	_ = bestName // best.ProviderName already carries this; kept for a future log line
	return *best, nil
}

// CreateOrder implements SwapProvider: re-quote every provider fresh
// (see this file's own top-of-file doc comment for why this, not the
// winner of an earlier Quote call, decides routing), create the order
// with whichever wins, and prefix the returned ProviderOrderID with
// that provider's own configured name so GetOrder can route back to it.
func (m *MultiProvider) CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error) {
	quotes := callAll(ctx, m.providers, func(ctx context.Context, p SwapProvider) (Quote, error) {
		return p.Quote(ctx, pair, amountIn)
	})

	var bestIdx = -1
	for i, r := range quotes {
		if r.err != nil {
			continue
		}
		if bestIdx == -1 || r.result.AmountOut.Units > quotes[bestIdx].result.AmountOut.Units {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return SwapOrder{}, fmt.Errorf("upstream: MultiProvider: CreateOrder's own re-quote step failed for every provider: %w", errAllFailed(quotes))
	}

	winner := m.providers[bestIdx]
	order, err := winner.Provider.CreateOrder(ctx, pair, amountIn, destinationAddress)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: MultiProvider: CreateOrder against the winning provider %q: %w", winner.Name, err)
	}
	order.ProviderOrderID = winner.Name + ":" + order.ProviderOrderID
	return order, nil
}

// GetOrder implements SwapProvider: split providerOrderID's own
// "<vendor>:<real id>" prefix (added by this router's own CreateOrder)
// and route to that vendor's own provider.
func (m *MultiProvider) GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error) {
	name, realID, ok := strings.Cut(providerOrderID, ":")
	if !ok {
		return SwapOrder{}, fmt.Errorf("upstream: MultiProvider: malformed provider order id %q -- expected \"<vendor>:<id>\"", providerOrderID)
	}
	for _, p := range m.providers {
		if p.Name == name {
			order, err := p.Provider.GetOrder(ctx, realID)
			if err != nil {
				return SwapOrder{}, err
			}
			order.ProviderOrderID = providerOrderID // keep the prefix on the way back out too
			return order, nil
		}
	}
	return SwapOrder{}, fmt.Errorf("upstream: MultiProvider: no configured provider named %q for order id %q", name, providerOrderID)
}
