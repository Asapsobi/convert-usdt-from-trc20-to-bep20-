package chain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

var (
	// ErrNoAgreement means fewer than minAgreement providers reported the
	// same (height, hash) this round -- whether because they disagreed,
	// errored, or timed out. A hard error, never a degraded success on
	// whatever happened to be the largest group.
	ErrNoAgreement = errors.New("chain: fewer than the required number of providers agree on the finalized block")

	// ErrAmbiguousAgreement means two or more DIFFERENT (height, hash)
	// pairs each independently reached minAgreement -- an even split
	// across a genuine network disagreement. Picking either one over the
	// other would be exactly the "silently resolved by trusting one
	// side" behavior this package exists to refuse.
	ErrAmbiguousAgreement = errors.New("chain: multiple groups of providers reached agreement on DIFFERENT finalized blocks")
)

// finalizedBlockRPC is the subset of eth_getBlockByNumber's response this
// package needs.
type finalizedBlockRPC struct {
	Number string `json:"number"`
	Hash   string `json:"hash"`
}

type providerResult struct {
	name   string
	height uint64
	hash   common.Hash
	err    error
}

// finalizedKey is LatestFinalized's own agreement key -- a provider's
// reported (height, hash) pair, per BEP-126's finalized tag.
type finalizedKey struct {
	height uint64
	hash   common.Hash
}

// LatestFinalized returns the block height and hash that at least
// minAgreement configured providers independently report as the chain's
// current "finalized" block, per BSC's BEP-126 fast-finality consensus
// (see the C2 build spec's "Read this second" section for why this is
// the primary finality signal here, not a fixed confirmation depth).
//
// A provider that times out, errors, or reports a different (height,
// hash) than the agreeing group is excluded from the agreement count --
// never silently counted as agreeing, and never used as a tie-breaker.
// The actual grouping/winner-selection algorithm lives in
// resolveAgreement (agreement.go), shared with LogsAt's own identical
// synchronous-round agreement check -- one algorithm, not two that could
// drift apart.
func (p *Pool) LatestFinalized(ctx context.Context) (height uint64, agreedHash common.Hash, err error) {
	raw := p.queryFinalizedFromAllProviders(ctx)
	results := make([]namedResult[finalizedKey], len(raw))
	for i, r := range raw {
		results[i] = namedResult[finalizedKey]{name: r.name, key: finalizedKey{r.height, r.hash}, err: r.err}
	}

	winner, succeeded, failed, err := resolveAgreement(results, p.minAgreement)
	for _, n := range succeeded {
		p.recordSuccess(n)
	}
	for n, e := range failed {
		p.recordFailure(n, e)
	}
	if err != nil {
		return 0, common.Hash{}, err
	}
	return winner.height, winner.hash, nil
}

func (p *Pool) queryFinalizedFromAllProviders(ctx context.Context) []providerResult {
	results := make([]providerResult, len(p.providers))
	var wg sync.WaitGroup
	for i, prov := range p.providers {
		wg.Add(1)
		go func(i int, prov Provider) {
			defer wg.Done()
			height, hash, err := queryFinalized(ctx, prov)
			results[i] = providerResult{name: prov.Name, height: height, hash: hash, err: err}
		}(i, prov)
	}
	wg.Wait()
	return results
}

// queryFinalized calls eth_getBlockByNumber("finalized", false) via the
// raw RPC client rather than a typed ethclient helper. The "finalized"
// block tag is a string sentinel per the post-Merge JSON-RPC convention
// BEP-126 reuses -- calling the method by name directly, exactly as this
// chunk's own spec names it, avoids depending on whether ethclient's
// higher-level helpers in a given go-ethereum version accept that
// sentinel the way we'd expect.
func queryFinalized(ctx context.Context, prov Provider) (uint64, common.Hash, error) {
	var result finalizedBlockRPC
	err := prov.Client.Client().CallContext(ctx, &result, "eth_getBlockByNumber", "finalized", false)
	if err != nil {
		return 0, common.Hash{}, fmt.Errorf("%s: eth_getBlockByNumber(finalized): %w", prov.Name, err)
	}
	if result.Number == "" || result.Hash == "" {
		return 0, common.Hash{}, fmt.Errorf("%s: eth_getBlockByNumber(finalized) returned an empty block "+
			"-- this provider may not support the finalized tag", prov.Name)
	}
	height, err := hexToUint64(result.Number)
	if err != nil {
		return 0, common.Hash{}, fmt.Errorf("%s: parsing block number %q: %w", prov.Name, result.Number, err)
	}
	return height, common.HexToHash(result.Hash), nil
}

func hexToUint64(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}
