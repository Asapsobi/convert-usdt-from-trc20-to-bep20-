package chain

import "fmt"

// namedResult is one provider's own outcome for a single synchronous
// agreement round, keyed on any comparable type K -- LatestFinalized
// uses (height,hash) (finality.go), LogsAt uses a normalized log-set key
// (logs.go). Factored out here so both share ONE agreement algorithm
// instead of two that could silently drift apart.
type namedResult[K comparable] struct {
	name string
	key  K
	err  error
}

// resolveAgreement is LatestFinalized's own original algorithm
// (unchanged in behavior), generalized to any comparable key type: the
// largest group of providers reporting an identical key wins, provided
// it reaches minAgreement; two groups tied at the top is ambiguous;
// nothing reaching minAgreement is no-agreement. A provider is recorded
// as having succeeded this round only if it's in the winning group;
// every other respondent (wrong answer or a transport error) is
// recorded as failed, with a reason -- callers do the actual
// Pool.recordSuccess/recordFailure calls (this function has no Pool
// receiver, so it can't take Pool.mu itself), using the two returned
// maps.
//
// With exactly 2 results and minAgreement=2 (this package's own default,
// pool.go's NewPool), this reduces to exactly "both must agree or hard
// fail" -- the same outcome the original two-provider-only LatestFinalized
// and LogsAt implementations already produced, by construction: with only
// two respondents, the only way to reach a group of size >= 2 is for both
// to report the identical key.
func resolveAgreement[K comparable](results []namedResult[K], minAgreement int) (winner K, succeeded []string, failed map[string]error, err error) {
	groups := make(map[K][]string) // key -> provider names reporting it
	failed = make(map[string]error)
	for _, r := range results {
		if r.err != nil {
			failed[r.name] = r.err
			continue
		}
		groups[r.key] = append(groups[r.key], r.name)
	}

	// Find the largest group that reaches the agreement threshold, and
	// detect a tie at the top among groups that do.
	winnerCount := 0
	ambiguous := false
	for k, names := range groups {
		if len(names) < minAgreement {
			continue // doesn't reach the threshold at all -- not a contender
		}
		switch {
		case winnerCount == 0:
			winner, winnerCount = k, len(names)
		case len(names) == winnerCount:
			ambiguous = true
		case len(names) > winnerCount:
			// A strictly larger group supersedes an earlier smaller one
			// that had also crossed the threshold -- not itself
			// ambiguous, since one group is unambiguously the largest.
			winner, winnerCount = k, len(names)
			ambiguous = false
		}
	}

	// A successfully-responding provider is only penalized for THIS round
	// if there was a genuine disagreement to be on the wrong side of --
	// i.e., at least two DISTINCT answers came back among the providers
	// that responded at all (len(groups) > 1). A lone group whose members
	// all agree with each other is never penalized merely for falling
	// short of minAgreement because other providers errored: those
	// providers' own failures were already recorded above, and the
	// survivors did nothing wrong.
	switch {
	case len(groups) > 1:
		winningNames := make(map[string]bool)
		if winnerCount > 0 && !ambiguous {
			for _, n := range groups[winner] {
				winningNames[n] = true
			}
		}
		for k, names := range groups {
			for _, n := range names {
				if winningNames[n] {
					succeeded = append(succeeded, n)
					continue
				}
				failed[n] = fmt.Errorf("reported %v, which was not this round's agreed result", k)
			}
		}
	case winnerCount > 0:
		// Exactly one distinct answer, and it reached the threshold: no
		// disagreement occurred, every respondent succeeded.
		succeeded = append(succeeded, groups[winner]...)
		// The remaining case -- exactly one group, below threshold -- has
		// nothing further to record here: its members agreed with each
		// other and simply didn't have enough company this round, which
		// is not a fault of theirs to be penalized for.
	}

	if ambiguous {
		return winner, succeeded, failed, fmt.Errorf("%w: need >= %d providers to agree, but multiple different results each had that many",
			ErrAmbiguousAgreement, minAgreement)
	}
	if winnerCount == 0 {
		return winner, succeeded, failed, fmt.Errorf("%w: need %d, best group had fewer (of %d providers queried)",
			ErrNoAgreement, minAgreement, len(results))
	}
	return winner, succeeded, failed, nil
}
