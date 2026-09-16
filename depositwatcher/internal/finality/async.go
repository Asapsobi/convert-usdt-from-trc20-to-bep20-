// Design B (Step 2 of the C2 finality remediation): asynchronous,
// per-provider deposit confirmation. This file is deliberately a pure
// decision core with no chain/RPC/database dependency of its own --
// Phase 1 of the approved implementation plan. A later phase wires a
// real per-provider query layer (chain.Pool's own new single-provider
// primitives) that PRODUCES the ProviderObservation values this file
// consumes; nothing here makes a network call, and nothing here is
// wired into the real Tracker/CheckFinality path yet -- Design A
// (chain.Pool.LatestFinalized + LogsAt, both already N-of-M as of Step
// 1) remains the only path actually used by watcherd until a later
// phase's own integration/replay/shadow validation passes.
//
// The core invariant: a candidate reaches QUORUM_REACHED only when at
// least minAgreement DISTINCT providers have each independently
// reported the exact SAME target block (height AND block hash) and the
// exact SAME verified deposit facts (order id, amount) for that
// candidate. A provider reporting "my own finalized height has reached
// the target height" is never, by itself, sufficient -- see
// ProviderObservation's own doc comment for why there is no way to even
// construct one without also supplying what that same provider found
// when it looked.
//
// A single provider failing to find the deposit is explicitly NOT
// treated as evidence of a chain-history contradiction (see this
// package's own reconsideration of that exact question, and
// finality.go's own existing, shipped precedent at readyCandidates/
// "reorged out before finality reached it -- the routine, expected
// case"): a provider's finality-tag tracking can legitimately outpace
// its own log indexer, a mundane operational reality, not a statement
// about canonical history. Absence is tracked on its own, symmetric
// path (ASYNC_NOT_FOUND) requiring the SAME minAgreement corroboration
// presence does -- a lone absent provider can neither drop a real
// deposit nor block a real PRESENT quorum formed by others.
//
// What IS always a genuine, terminal disagreement (CONTRADICTED,
// candidate-permanent, never later cleared or outvoted): two providers
// reporting DIFFERENT block hashes for the same target height (block
// header lookups are simple, single, foundational queries -- not the
// kind of thing that plausibly lags the way log indexing can, and an
// already-final height's own hash is supposed to be permanently
// settled); or two providers agreeing on the SAME hash but decoding
// DIFFERENT deposit facts from their own log queries at that block
// (since matching hash cryptographically pins the true content, at
// least one of them is returning corrupted or wrong data). Presence
// vs. absence AT THE SAME HASH is deliberately excluded from this list
// -- see RecordConfirmed's own doc comment.
package finality

import (
	"fmt"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"

	"depositwatcher/internal/money"
)

// AsyncCandidateStatus is one candidate's own current status under
// Design B.
type AsyncCandidateStatus string

const (
	// AsyncPending means neither a PRESENT nor an ABSENT quorum has been
	// reached yet, and no disagreement has been detected -- the
	// ordinary, expected state while providers are still catching up at
	// their own pace.
	AsyncPending AsyncCandidateStatus = "PENDING"
	// AsyncQuorumReached means at least minAgreement distinct providers
	// have independently reported the exact same present observation --
	// the caller should now proceed to OnFinal, exactly as today's
	// synchronous design does the moment LatestFinalized+LogsAt agree.
	// Takes priority over AsyncNotFound if, in principle, both
	// conditions were ever simultaneously true -- a real PRESENT quorum
	// must never be prevented by any number of absent reports that
	// don't themselves contradict it (see RecordConfirmed).
	AsyncQuorumReached AsyncCandidateStatus = "QUORUM_REACHED"
	// AsyncNotFound means at least minAgreement distinct providers have
	// each independently and successfully confirmed the deposit is
	// absent at the target block (own finality check passed, own
	// content re-query executed successfully, found nothing) -- the
	// same corroboration bar QuorumReached requires, on the symmetric
	// negative case. A single absent provider alone can never reach
	// this. Mirrors today's existing "reorged out before finality --
	// routine, silently dropped" precedent (finality.go), just reached
	// asynchronously and per-provider instead of via one joint round.
	AsyncNotFound AsyncCandidateStatus = "ASYNC_NOT_FOUND"
	// AsyncDisagreementAlert means a genuine cross-provider contradiction
	// was detected -- see the package doc comment for exactly which
	// cases qualify. Terminal: never cleared by later agreement from
	// other providers, never resolved by outvoting. A caller must alert
	// a human and must never call OnFinal for a candidate in this state.
	AsyncDisagreementAlert AsyncCandidateStatus = "DISAGREEMENT_ALERT"
)

// ProviderObservation is what a caller reports after independently
// checking ONE provider for ONE candidate, having already confirmed
// BOTH of the following against that same provider, in the same
// attempt: (1) that provider's own eth_getBlockByNumber("finalized")
// height has reached or passed the candidate's target height, and (2)
// that SAME provider's own eth_getLogs re-query at exactly that height
// was executed successfully (regardless of whether it found the
// deposit -- see Present). There is deliberately no field or
// constructor for "past the height, didn't check the content, or the
// check itself errored" -- a caller that hit a transport error on
// either check, or hasn't yet passed check (1), must not call
// RecordConfirmed at all for this attempt (leave the candidate as-is
// and retry that provider next attempt), the same posture
// chain.Pool.recordFailure already takes for an ordinary transient
// error: no vote, no contradiction, just retried later.
type ProviderObservation struct {
	// Height is the target height this observation is about -- always
	// the candidate's own Height; a caller never has a choice here, it's
	// carried on the type only so the observation is fully
	// self-describing for logging/tests.
	Height uint64
	// BlockHash is what THIS provider's own eth_getBlockByNumber(Height,
	// false) returned for that height, independently of what any other
	// provider reported. Compared across EVERY other provider's own
	// observation, present or absent alike -- a differing hash at the
	// same height is always a hard disagreement (see package doc).
	BlockHash common.Hash
	// Present is false when this provider's own successful re-query at
	// Height did not find the candidate's own deposit log -- a valid,
	// ordinary ABSENT observation (see package doc comment for why this
	// is not automatically treated as a contradiction), never "hasn't
	// happened yet" (that case is a plain non-call, above).
	Present bool
	// OrderID/Amount are the deposit facts THIS provider's own re-query
	// decoded from the log, when Present -- meaningless (and ignored)
	// when Present is false.
	OrderID int64
	Amount  money.Amount
}

// hashKey is the target-block identity alone (height + hash) -- what
// EVERY observation, present or absent, must agree on with every other
// provider's own observation before either counts as corroborating
// evidence of anything (rule 4: a differing block hash at the same
// height is always terminal, regardless of presence).
type hashKey struct {
	Height    uint64
	BlockHash common.Hash
}

func (o ProviderObservation) hashKey() hashKey {
	return hashKey{Height: o.Height, BlockHash: o.BlockHash}
}

// factsKey is a PRESENT observation's full identity: the target block
// plus the decoded deposit facts found there. Two PRESENT observations
// must match on this exactly to corroborate each other (rule 5); it is
// never computed for an absent observation (absence has no facts).
type factsKey struct {
	hashKey
	OrderID int64
	Amount  money.Amount
}

func (o ProviderObservation) factsKey() factsKey {
	return factsKey{hashKey: o.hashKey(), OrderID: o.OrderID, Amount: o.Amount}
}

// CandidateConfirmations tracks every provider's own confirmed
// observation for exactly one candidate and enforces the invariants
// documented at the top of this file. One instance per pending
// candidate.
type CandidateConfirmations struct {
	mu           sync.Mutex
	minAgreement int

	// presentByProvider / absentByProvider hold every provider's own
	// most recent CONSISTENT observation of each kind -- a provider
	// appears in at most one of the two at a time (RecordConfirmed moves
	// it between them if its own view changes, e.g. an indexer catching
	// up from absent to present on a later attempt).
	presentByProvider map[string]factsKey
	absentByProvider  map[string]hashKey

	disagreement     bool
	disagreementNote string
}

// NewCandidateConfirmations starts tracking one candidate fresh.
// minAgreement is the SAME chain.Config.MinAgreement value Design A's
// own chain.Pool already uses (exposed since Step 1 via
// WATCHER_MIN_AGREEMENT) -- Design B introduces no separate quorum-size
// configuration, and uses the identical number for both the PRESENT and
// ABSENT corroboration bars.
func NewCandidateConfirmations(minAgreement int) *CandidateConfirmations {
	if minAgreement < 1 {
		minAgreement = 1
	}
	return &CandidateConfirmations{
		minAgreement:      minAgreement,
		presentByProvider: make(map[string]factsKey),
		absentByProvider:  make(map[string]hashKey),
	}
}

// RecordConfirmed records providerName's own successful, definitive
// observation and returns the candidate's status immediately afterward.
//
// Hash-level consistency is checked FIRST, against every OTHER
// provider's own most recently recorded observation of EITHER kind
// (present or absent) -- a differing block hash for the candidate's own
// target height is always a hard, terminal disagreement (rule 4),
// regardless of which side found the deposit present.
//
// Only once hash-consistency holds does presence matter: a PRESENT
// observation is then checked for facts-level agreement against every
// other PRESENT observation only (rule 5) -- absence carries no facts
// to compare. An ABSENT observation, once hash-consistent, is recorded
// purely on its own symmetric track (absentByProvider) and can NEVER by
// itself contradict or block a PRESENT observation that agrees with it
// on hash (rule 3/6) -- it only ever contributes toward AsyncNotFound,
// which itself requires the same minAgreement corroboration
// AsyncQuorumReached does (rule 7), and which AsyncQuorumReached always
// takes priority over the moment real PRESENT corroboration exists
// (statusLocked).
func (c *CandidateConfirmations) RecordConfirmed(providerName string, obs ProviderObservation) AsyncCandidateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	newHK := obs.hashKey()

	// Hash-level consistency, checked against every other provider's own
	// most recent recorded observation of EITHER kind.
	for name, existing := range c.presentByProvider {
		if name != providerName && existing.hashKey != newHK {
			return c.recordDisagreement(providerName, obs, fmt.Sprintf(
				"provider %q reports block hash %s at height %d, but provider %q (PRESENT) previously reported hash %s",
				providerName, newHK.BlockHash, newHK.Height, name, existing.BlockHash))
		}
	}
	for name, existing := range c.absentByProvider {
		if name != providerName && existing != newHK {
			return c.recordDisagreement(providerName, obs, fmt.Sprintf(
				"provider %q reports block hash %s at height %d, but provider %q (ABSENT) previously reported hash %s",
				providerName, newHK.BlockHash, newHK.Height, name, existing.BlockHash))
		}
	}

	if !obs.Present {
		delete(c.presentByProvider, providerName) // this provider's own view is now absent -- keep only its latest
		c.absentByProvider[providerName] = newHK
		return c.statusLocked()
	}

	newFK := obs.factsKey()
	for name, existing := range c.presentByProvider {
		if name != providerName && existing != newFK {
			return c.recordDisagreement(providerName, obs, fmt.Sprintf(
				"provider %q reports deposit facts %+v, but provider %q previously reported %+v for the same block",
				providerName, newFK, name, existing))
		}
	}
	delete(c.absentByProvider, providerName) // this provider's own view just flipped from absent to present
	c.presentByProvider[providerName] = newFK
	return c.statusLocked()
}

// recordDisagreement marks the candidate permanently disagreement-alert
// (unless it already was, in which case the note is left as the FIRST
// disagreement ever recorded, not overwritten by a later one), still
// records the triggering observation itself (useful for a post-mortem/
// alert payload), and returns AsyncDisagreementAlert. Must be called
// with c.mu already held.
func (c *CandidateConfirmations) recordDisagreement(providerName string, obs ProviderObservation, note string) AsyncCandidateStatus {
	if !c.disagreement {
		c.disagreement = true
		c.disagreementNote = note
	}
	if obs.Present {
		delete(c.absentByProvider, providerName)
		c.presentByProvider[providerName] = obs.factsKey()
	} else {
		delete(c.presentByProvider, providerName)
		c.absentByProvider[providerName] = obs.hashKey()
	}
	return AsyncDisagreementAlert
}

// Status returns the candidate's current status without recording
// anything new.
func (c *CandidateConfirmations) Status() AsyncCandidateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

func (c *CandidateConfirmations) statusLocked() AsyncCandidateStatus {
	if c.disagreement {
		return AsyncDisagreementAlert
	}
	if len(c.presentByProvider) >= c.minAgreement {
		return AsyncQuorumReached
	}
	if len(c.absentByProvider) >= c.minAgreement {
		return AsyncNotFound
	}
	return AsyncPending
}

// DisagreementNote returns the human-readable reason a candidate is in
// AsyncDisagreementAlert -- empty string if it isn't. For alerting.
func (c *CandidateConfirmations) DisagreementNote() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disagreementNote
}

// ConfirmedProviders returns the names of every provider currently
// counted toward a PRESENT quorum, sorted for deterministic
// assertions/logging.
func (c *CandidateConfirmations) ConfirmedProviders() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sortedKeys(c.presentByProvider)
}

// AbsentProviders returns the names of every provider currently counted
// toward an ABSENT quorum, sorted -- for observability (a candidate
// sitting with, say, 1-of-2 required absent confirmations is worth
// surfacing distinctly from ordinary PENDING, per the approved plan's
// own §9) and tests.
func (c *CandidateConfirmations) AbsentProviders() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sortedKeys(c.absentByProvider)
}

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ProviderHeightMonitor tracks, across ALL candidates, the highest
// finalized height ever legitimately observed from each provider --
// BEP-126 finality is supposed to be monotonically non-decreasing, so a
// provider reporting a LOWER height than its own prior high-water mark
// is a real anomaly (a buggy or unreliable node), worth flagging on its
// own, distinct from an ordinary transient error. This is observability
// bookkeeping, not a safety gate: CandidateConfirmations' own
// disagreement logic above never depends on it -- a regressed reading is
// simply excluded from use (treated the same as any other failed
// attempt) by whatever Phase-2 caller checks this before ever
// constructing a ProviderObservation, not silently trusted.
type ProviderHeightMonitor struct {
	mu  sync.Mutex
	max map[string]uint64
}

// NewProviderHeightMonitor returns an empty monitor.
func NewProviderHeightMonitor() *ProviderHeightMonitor {
	return &ProviderHeightMonitor{max: make(map[string]uint64)}
}

// Check reports whether height is a legitimate (monotonically
// non-decreasing) reading for providerName, and records it as the new
// high-water mark if so. A regression (ok == false) leaves the stored
// high-water mark unchanged -- the bad reading is not allowed to lower
// the bar for the NEXT check either.
func (m *ProviderHeightMonitor) Check(providerName string, height uint64) (ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prior, seen := m.max[providerName]
	if seen && height < prior {
		return false
	}
	if !seen || height > prior {
		m.max[providerName] = height
	}
	return true
}

// HighWaterMark returns the highest height ever legitimately observed
// from providerName, and whether anything has been observed at all.
func (m *ProviderHeightMonitor) HighWaterMark(providerName string) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.max[providerName]
	return h, ok
}
