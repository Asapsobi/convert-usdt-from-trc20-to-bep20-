package chain

import (
	"context"
	"errors"
	"fmt"
)

// ErrInsufficientProviders is returned by NewPool for fewer than 2
// configured providers -- mirroring depositwatcher/internal/chain's own
// requirement (invariant 5: never trust a single provider's own say-so
// for a real deposit), applied here to TRON's per-address REST scan
// instead of BSC's eth_getLogs.
var ErrInsufficientProviders = errors.New("chain: at least 2 providers are required")

// Pool cross-checks ScanTRC20Transfers results from 2+ independently
// configured Providers before trusting any of them.
type Pool struct {
	providers []*Provider
}

// NewPool returns a Pool wrapping providers. At least 2 are required.
func NewPool(providers []*Provider) (*Pool, error) {
	if len(providers) < 2 {
		return nil, fmt.Errorf("%w: got %d", ErrInsufficientProviders, len(providers))
	}
	return &Pool{providers: providers}, nil
}

// ScanAddress calls ScanTRC20Transfers on every configured provider and
// returns only the transfers every provider agrees on -- same (TxID,
// From, To, ValueRaw, BlockTimestamp), the direct analogue of
// depositwatcher/internal/chain.LogsAt's own logEqual/logsEqual
// cross-check. A transfer only one provider reports (a lagging indexer,
// or a provider fabricating/dropping data) is silently excluded, not
// reported as an error -- the same posture LogsAt takes: withhold
// trust rather than fail the whole scan for one disagreeing provider,
// since the disagreeing transfer (if real) will agree on a later tick
// once every provider has caught up.
func (p *Pool) ScanAddress(ctx context.Context, address, contractAddress string, minTimestampMs int64) ([]Transfer, error) {
	perProvider := make([][]Transfer, len(p.providers))
	for i, prov := range p.providers {
		transfers, err := prov.ScanTRC20Transfers(ctx, address, contractAddress, minTimestampMs)
		if err != nil {
			return nil, fmt.Errorf("chain: provider %s: %w", prov.Name(), err)
		}
		perProvider[i] = transfers
	}

	primary := perProvider[0]
	agreed := make([]Transfer, 0, len(primary))
	for _, t := range primary {
		agreedByAll := true
		for _, other := range perProvider[1:] {
			if !containsMatchingTransfer(other, t) {
				agreedByAll = false
				break
			}
		}
		if agreedByAll {
			agreed = append(agreed, t)
		}
	}
	return agreed, nil
}

func containsMatchingTransfer(transfers []Transfer, want Transfer) bool {
	for _, t := range transfers {
		if transfersEqual(t, want) {
			return true
		}
	}
	return false
}

// transfersEqual compares every field a real, agreeing provider must
// report identically for the same on-chain event. BlockTimestamp is
// compared at millisecond resolution (both sides already truncate to
// that via time.UnixMilli in fetchPage), never approximately.
func transfersEqual(a, b Transfer) bool {
	if a.TxID != b.TxID || a.From != b.From || a.To != b.To || a.ContractAddress != b.ContractAddress {
		return false
	}
	if !a.BlockTimestamp.Equal(b.BlockTimestamp) {
		return false
	}
	if (a.ValueRaw == nil) != (b.ValueRaw == nil) {
		return false
	}
	if a.ValueRaw != nil && a.ValueRaw.Cmp(b.ValueRaw) != 0 {
		return false
	}
	return true
}
