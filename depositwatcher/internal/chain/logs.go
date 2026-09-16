package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ErrLogMismatch means fewer than minAgreement providers returned
// identical logs for the identical query -- a hard error, per this
// chunk's spec: "never silently resolved by trust the primary." Wrapped
// alongside ErrNoAgreement/ErrAmbiguousAgreement (whichever
// resolveAgreement actually returned) so an existing caller checking for
// either of those two (e.g. internal/replay/scenarios.go's own
// disagreement scenario) keeps working unchanged, while a caller that
// specifically checks for ErrLogMismatch (none in this codebase today)
// still can.
var ErrLogMismatch = errors.New("chain: providers returned different logs for the same query")

// logsKey is LogsAt's own agreement key -- one provider's full,
// order-normalized log set for the query, reduced to a single
// comparable string (resolveAgreement's groups map needs a comparable
// key type, and []types.Log isn't one).
type logsKey string

// logsAgreementKey renders logs into logsKey via the exact same
// consensus-meaningful fields the original two-provider LogsAt always
// compared, after sorting into the same canonical (block, tx index, log
// index) order a well-formed provider's response should already be in
// (normalizing anyway avoids a false mismatch from two otherwise-correct
// providers that merely differ in literal response ordering).
// Deliberately excludes BlockHash and Removed: two providers observing
// the exact same log around a benign, momentary reorg could disagree on
// those fields even when the log's own content -- what actually happened
// -- is identical and correct; comparing the fields that define the
// event itself, not each provider's view of exactly when/how it was
// included, is the right level of strictness for this cross-check.
func logsAgreementKey(logs []types.Log) logsKey {
	sorted := append([]types.Log(nil), logs...)
	sortLogs(sorted)
	var b strings.Builder
	for _, l := range sorted {
		fmt.Fprintf(&b, "%s|%v|%x|%s|%d|%d|%d;",
			l.Address.Hex(), l.Topics, l.Data, l.TxHash.Hex(), l.BlockNumber, l.TxIndex, l.Index)
	}
	return logsKey(b.String())
}

// LogsAt fetches logs in [fromBlock, toBlock] for contractAddress
// matching topics from every configured provider and requires at least
// minAgreement of them to report byte-identical results before
// returning -- generalized from an earlier fixed providers[0]/
// providers[1] pair to the same N-provider quorum LatestFinalized itself
// uses (resolveAgreement, agreement.go), so a 3rd+ configured provider
// actually helps THIS check too, not just the finalized-tag check.
// With exactly 2 providers configured and the package's own default
// minAgreement=2, this reduces to exactly the original "both must agree
// or hard fail" behavior, unchanged. At this system's real volume (~4
// deposits/hour peak, per component-map.md) checking everything rather
// than sampling costs nothing, so that's what this does rather than the
// sampling this chunk's own spec allows as an alternative.
func (p *Pool) LogsAt(ctx context.Context, fromBlock, toBlock uint64, contractAddress common.Address, topics [][]common.Hash) ([]types.Log, error) {
	if len(p.providers) < 2 {
		// NewPool already refuses fewer than 2 providers, so this is
		// unreachable in practice.
		return nil, ErrTooFewProviders
	}

	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
		Addresses: []common.Address{contractAddress},
		Topics:    topics,
	}

	type providerLogs struct {
		name string
		logs []types.Log
		err  error
	}
	raw := make([]providerLogs, len(p.providers))
	var wg sync.WaitGroup
	for i, prov := range p.providers {
		wg.Add(1)
		go func(i int, prov Provider) {
			defer wg.Done()
			logs, err := prov.Client.FilterLogs(ctx, query)
			raw[i] = providerLogs{name: prov.Name, logs: logs, err: err}
		}(i, prov)
	}
	wg.Wait()

	byName := make(map[string][]types.Log, len(raw))
	results := make([]namedResult[logsKey], len(raw))
	for i, r := range raw {
		if r.err != nil {
			results[i] = namedResult[logsKey]{name: r.name, err: fmt.Errorf("chain: provider %s: %w", r.name, r.err)}
			continue
		}
		byName[r.name] = r.logs
		results[i] = namedResult[logsKey]{name: r.name, key: logsAgreementKey(r.logs)}
	}

	_, succeeded, failed, err := resolveAgreement(results, p.minAgreement)
	for _, n := range succeeded {
		p.recordSuccess(n)
	}
	for n, e := range failed {
		p.recordFailure(n, e)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w (blocks [%d,%d])", ErrLogMismatch, err, fromBlock, toBlock)
	}
	// byName[succeeded[0]] is any winning provider's own log slice --
	// every provider in succeeded reported an identical set by
	// construction (resolveAgreement's own grouping), so which one is
	// returned doesn't matter.
	return byName[succeeded[0]], nil
}

func sortLogs(logs []types.Log) {
	sort.Slice(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		if logs[i].TxIndex != logs[j].TxIndex {
			return logs[i].TxIndex < logs[j].TxIndex
		}
		return logs[i].Index < logs[j].Index
	})
}
