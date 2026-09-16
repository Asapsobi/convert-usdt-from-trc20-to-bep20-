// Single-provider primitives -- Phase 2 of the C2 finality remediation
// (Design B). Every method here queries exactly ONE named provider,
// with no agreement/quorum logic of its own -- that's deliberate:
// internal/finality's own async decision engine (async.go,
// async_driver.go) is what combines these into a safety decision, and
// it must never be handed anything already averaged, agreed, or
// otherwise pre-combined across providers. LatestFinalized/LogsAt
// (finality.go, logs.go) remain the joint, synchronous, N-of-M primary
// path (Design A) -- these are new, additive, single-provider siblings,
// not replacements.
package chain

import (
	"context"
	"fmt"
	"math/big"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ProviderNames returns every configured provider's own name, in
// configuration order -- for a caller (the async driver) that needs to
// iterate providers individually rather than through a joint Pool
// operation.
func (p *Pool) ProviderNames() []string {
	names := make([]string, len(p.providers))
	for i, prov := range p.providers {
		names[i] = prov.Name
	}
	return names
}

func (p *Pool) providerByName(name string) (Provider, bool) {
	for _, prov := range p.providers {
		if prov.Name == name {
			return prov, true
		}
	}
	return Provider{}, false
}

// errUnknownProvider is returned by every method below when providerName
// doesn't match any provider this Pool was constructed with -- a caller
// bug (e.g. a stale name from a config that changed), not a transport
// error, so it's named distinctly rather than folded into an ordinary
// RPC failure.
func errUnknownProvider(name string) error {
	return fmt.Errorf("chain: no configured provider named %q", name)
}

// FinalizedFrom queries providerName ALONE for its own current
// "finalized" height and hash -- no agreement with any other provider,
// no quorum. Exactly the single-provider primitive Design B's own
// per-provider confirmation loop needs; LatestFinalized (finality.go)
// remains the joint N-of-M version every other caller should keep
// using.
func (p *Pool) FinalizedFrom(ctx context.Context, providerName string) (height uint64, hash common.Hash, err error) {
	prov, ok := p.providerByName(providerName)
	if !ok {
		return 0, common.Hash{}, errUnknownProvider(providerName)
	}
	return queryFinalized(ctx, prov)
}

// BlockHashAt queries providerName ALONE for the block hash it reports
// at a SPECIFIC historical height -- genuinely new capability: every
// other query in this package targets the "finalized" sentinel height,
// never an arbitrary numeric one. This is what lets Design B's own
// per-provider confirmation independently re-derive "what does THIS
// provider say block H's own hash is," the first half of the
// height+hash binding internal/finality's own ProviderObservation
// requires.
func (p *Pool) BlockHashAt(ctx context.Context, providerName string, height uint64) (common.Hash, error) {
	prov, ok := p.providerByName(providerName)
	if !ok {
		return common.Hash{}, errUnknownProvider(providerName)
	}
	var result finalizedBlockRPC
	err := prov.Client.Client().CallContext(ctx, &result, "eth_getBlockByNumber", fmt.Sprintf("0x%x", height), false)
	if err != nil {
		return common.Hash{}, fmt.Errorf("%s: eth_getBlockByNumber(%d): %w", prov.Name, height, err)
	}
	if result.Hash == "" {
		return common.Hash{}, fmt.Errorf("%s: eth_getBlockByNumber(%d) returned an empty block -- height may not exist on this provider's own chain view",
			prov.Name, height)
	}
	return common.HexToHash(result.Hash), nil
}

// LogsAtFrom queries providerName ALONE for logs in [fromBlock,
// toBlock] -- no cross-provider agreement, no quorum. The exact same
// underlying FilterLogs call LogsAt's own per-provider goroutines
// already make (logs.go), factored out here so it can be called for
// one specific, named provider directly instead of only ever in
// parallel across every configured one.
func (p *Pool) LogsAtFrom(ctx context.Context, providerName string, fromBlock, toBlock uint64, contractAddress common.Address, topics [][]common.Hash) ([]types.Log, error) {
	prov, ok := p.providerByName(providerName)
	if !ok {
		return nil, errUnknownProvider(providerName)
	}
	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
		Addresses: []common.Address{contractAddress},
		Topics:    topics,
	}
	logs, err := prov.Client.FilterLogs(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%s: eth_getLogs: %w", prov.Name, err)
	}
	return logs, nil
}
