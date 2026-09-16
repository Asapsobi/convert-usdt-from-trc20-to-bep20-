package finality

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"depositwatcher/internal/money"
)

func testHash(b byte) common.Hash {
	var h common.Hash
	h[31] = b
	return h
}

func agreeingObs(height uint64) ProviderObservation {
	return ProviderObservation{
		Height: height, BlockHash: testHash(0xAA), Present: true,
		OrderID: 42, Amount: money.Amount(3000_000000),
	}
}

// ---------------------------------------------------------------------
// Happy path: quorum across different ticks (calls), never simultaneous.
// ---------------------------------------------------------------------

func TestAsyncConfirm_ProviderLag_EventuallyConfirms(t *testing.T) {
	c := NewCandidateConfirmations(2)

	// Tick 1: only A has caught up.
	status := c.RecordConfirmed("A", agreeingObs(1000))
	if status != AsyncPending {
		t.Fatalf("after 1 of 2 required confirmations, status = %s, want PENDING", status)
	}

	// Ticks 2-3: B still hasn't reached the height -- caller simply
	// doesn't call RecordConfirmed for B at all (see ProviderObservation's
	// own doc comment). Status must remain unchanged.
	if got := c.Status(); got != AsyncPending {
		t.Fatalf("status without any new observation = %s, want PENDING (unchanged)", got)
	}

	// Tick 4 (an arbitrary number of ticks later -- observation TIMES
	// never need to match): B finally confirms, agreeing exactly with A.
	status = c.RecordConfirmed("B", agreeingObs(1000))
	if status != AsyncQuorumReached {
		t.Fatalf("after 2 of 2 required confirmations, status = %s, want QUORUM_REACHED", status)
	}
}

func TestAsyncConfirm_QuorumAcrossDifferentTicks_ProvidersListedCorrectly(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("A", agreeingObs(1000))
	c.RecordConfirmed("B", agreeingObs(1000))

	got := c.ConfirmedProviders()
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("ConfirmedProviders() = %v, want [A B]", got)
	}
}

// ---------------------------------------------------------------------
// The hash/facts-binding invariant -- the sharpest requirement.
// ---------------------------------------------------------------------

// TestAsyncConfirm_SameHeightDifferentHash_NeverOutvoted proves the core
// invariant this whole redesign exists for: two providers independently
// reaching the SAME height is never enough on its own -- if they
// disagree on the block hash at that height, it's a hard, terminal
// DISAGREEMENT_ALERT, even if a THIRD provider later agrees with the
// FIRST one (majority must never silently win).
func TestAsyncConfirm_SameHeightDifferentHash_NeverOutvoted(t *testing.T) {
	c := NewCandidateConfirmations(2)

	obsA := agreeingObs(1000)
	obsB := agreeingObs(1000)
	obsB.BlockHash = testHash(0xBB) // same height, DIFFERENT hash

	status := c.RecordConfirmed("A", obsA)
	if status != AsyncPending {
		t.Fatalf("after A alone, status = %s, want PENDING", status)
	}
	status = c.RecordConfirmed("B", obsB)
	if status != AsyncDisagreementAlert {
		t.Fatalf("after B disagrees with A on block hash, status = %s, want DISAGREEMENT_ALERT", status)
	}

	// A third provider agreeing with A must NOT clear the disagreement
	// or let a 2-of-3 "majority" quietly finalize.
	status = c.RecordConfirmed("C", agreeingObs(1000))
	if status != AsyncDisagreementAlert {
		t.Fatalf("after a 3rd provider agrees with the ORIGINAL majority, status = %s, want DISAGREEMENT_ALERT (never outvoted)", status)
	}
	if note := c.DisagreementNote(); note == "" {
		t.Error("expected a non-empty DisagreementNote once disagreement is recorded")
	}
}

// TestAsyncConfirm_SameHeightSameHashDifferentFacts_Contradicted proves
// the OTHER half of the binding requirement: even identical block
// hashes don't save an observation that disagrees on the DECODED
// deposit facts (defense in depth -- a provider could report a correct
// hash while returning fabricated/wrong log content).
func TestAsyncConfirm_SameHeightSameHashDifferentFacts_Contradicted(t *testing.T) {
	c := NewCandidateConfirmations(2)

	obsA := agreeingObs(1000)
	obsB := agreeingObs(1000)
	obsB.Amount = money.Amount(999_000000) // same height, same hash, DIFFERENT amount

	c.RecordConfirmed("A", obsA)
	status := c.RecordConfirmed("B", obsB)
	if status != AsyncDisagreementAlert {
		t.Fatalf("after B disagrees with A on decoded amount (same hash), status = %s, want DISAGREEMENT_ALERT", status)
	}
}

// absentObs is agreeingObs's own absent counterpart -- same height and
// block hash (so it never triggers the hash-disagreement path on its
// own), just Present=false.
func absentObs(height uint64) ProviderObservation {
	return ProviderObservation{Height: height, BlockHash: testHash(0xAA), Present: false}
}

// TestAsyncConfirm_SingleAbsentProvider_IsPendingNotContradicted is the
// corrected behavior this whole revision exists for: a LONE provider
// reporting its own successful-but-empty re-query must NEVER be treated
// as a chain-history contradiction on its own -- a provider's own
// finality-tag tracking can legitimately outpace its own log indexer
// (see this package's own top-of-file doc comment, and
// finality.go's existing "reorged out before finality -- routine,
// silently dropped" precedent). One absence alone is just... one
// absence alone: PENDING, not ALERT, not NOT_FOUND (which itself needs
// minAgreement corroboration -- see the dedicated test below).
func TestAsyncConfirm_SingleAbsentProvider_IsPendingNotContradicted(t *testing.T) {
	c := NewCandidateConfirmations(2)
	status := c.RecordConfirmed("A", absentObs(1000))
	if status != AsyncPending {
		t.Fatalf("a single ABSENT observation: status = %s, want PENDING", status)
	}
	if note := c.DisagreementNote(); note != "" {
		t.Errorf("expected no disagreement from a single absence, got note: %q", note)
	}
}

// TestAsyncConfirm_PresentAndAbsent_SameHash_NotContradiction is rule
// 3/6, directly: one provider found the deposit, another (same block
// hash -- so NOT a hash-level disagreement) didn't. This must not be
// treated as a contradiction -- it's exactly the "indexer lag on one
// provider" case.
func TestAsyncConfirm_PresentAndAbsent_SameHash_NotContradiction(t *testing.T) {
	c := NewCandidateConfirmations(2)
	status := c.RecordConfirmed("A", agreeingObs(1000))
	if status != AsyncPending {
		t.Fatalf("after A (present) alone, status = %s, want PENDING", status)
	}
	status = c.RecordConfirmed("B", absentObs(1000)) // same hash (0xAA), absent
	if status != AsyncPending {
		t.Fatalf("present + absent at the SAME hash: status = %s, want PENDING (not a contradiction)", status)
	}
	if note := c.DisagreementNote(); note != "" {
		t.Errorf("expected no disagreement note, got: %q", note)
	}
}

// TestAsyncConfirm_PresentQuorumNotBlockedByStaleAbsentProvider proves
// the sharpest half of rule 6: a real PRESENT quorum must form even
// while a DIFFERENT provider sits recorded as absent (same hash) --
// the stale provider's own absence must never prevent legitimate
// providers from reaching quorum.
func TestAsyncConfirm_PresentQuorumNotBlockedByStaleAbsentProvider(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("stale", absentObs(1000)) // reports absent first, same hash
	c.RecordConfirmed("A", agreeingObs(1000))
	status := c.RecordConfirmed("B", agreeingObs(1000))
	if status != AsyncQuorumReached {
		t.Fatalf("2 agreeing PRESENT providers despite 1 stale ABSENT provider (same hash): status = %s, want QUORUM_REACHED", status)
	}
	// The stale provider's own later self-correction (indexer catches
	// up) must also be reflected cleanly -- it simply moves from absent
	// to present, agreeing with the rest.
	status = c.RecordConfirmed("stale", agreeingObs(1000))
	if status != AsyncQuorumReached {
		t.Fatalf("after the stale provider self-corrects and agrees: status = %s, want QUORUM_REACHED (still)", status)
	}
	if got := c.AbsentProviders(); len(got) != 0 {
		t.Errorf("AbsentProviders() = %v, want empty after the stale provider moved to present", got)
	}
}

// TestAsyncConfirm_AbsentQuorum_ReachesNotFound is rule 7's positive
// case: enough INDEPENDENT, successful absent observations (same hash,
// so not itself a disagreement) reach the SAME minAgreement bar
// QuorumReached uses, symmetrically, on the negative side.
func TestAsyncConfirm_AbsentQuorum_ReachesNotFound(t *testing.T) {
	c := NewCandidateConfirmations(2)
	status := c.RecordConfirmed("A", absentObs(1000))
	if status != AsyncPending {
		t.Fatalf("1 of 2 required absent confirmations: status = %s, want PENDING", status)
	}
	status = c.RecordConfirmed("B", absentObs(1000))
	if status != AsyncNotFound {
		t.Fatalf("2 of 2 required absent confirmations: status = %s, want ASYNC_NOT_FOUND", status)
	}
}

// TestAsyncConfirm_AbsentBelowMinAgreement_NeverReachesNotFound is rule
// 7's negative case, stated explicitly and exhaustively for a 3-required
// setup: fewer than minAgreement absent confirmations must never tip
// into ASYNC_NOT_FOUND, no matter how many "not yet checked" providers
// there are around it.
func TestAsyncConfirm_AbsentBelowMinAgreement_NeverReachesNotFound(t *testing.T) {
	c := NewCandidateConfirmations(3)
	c.RecordConfirmed("A", absentObs(1000))
	status := c.RecordConfirmed("B", absentObs(1000))
	if status != AsyncPending {
		t.Fatalf("2 of 3 required absent confirmations: status = %s, want PENDING (not ASYNC_NOT_FOUND)", status)
	}
}

// TestAsyncConfirm_RPCFailure_ProducesNoVote documents (rule 1) what a
// transport-level failure looks like at this layer: it simply never
// becomes a call to RecordConfirmed at all. There is nothing to assert
// beyond "the candidate's status is exactly what it was before the
// failed attempt" -- shown here by interleaving successful confirmations
// with skipped ones (standing in for failed attempts a real caller would
// have skipped) and confirming the outcome only reflects the successful
// calls.
func TestAsyncConfirm_RPCFailure_ProducesNoVote(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("A", agreeingObs(1000))
	// Simulated failed attempts for B on ticks 2 and 3: nothing called.
	if got := c.Status(); got != AsyncPending {
		t.Fatalf("after A alone (B's attempts failed, never recorded): status = %s, want PENDING", got)
	}
	if got := c.ConfirmedProviders(); len(got) != 1 {
		t.Fatalf("ConfirmedProviders() = %v, want exactly [A] (B's failures must never appear)", got)
	}
	// B finally succeeds on tick 4.
	status := c.RecordConfirmed("B", agreeingObs(1000))
	if status != AsyncQuorumReached {
		t.Fatalf("after B's first SUCCESSFUL attempt: status = %s, want QUORUM_REACHED (no penalty for the earlier failures)", status)
	}
}

// TestAsyncConfirm_ReorgBeforeFinalityCompatibility mirrors today's
// existing, shipped precedent (finality.go's readyCandidates +
// TestCheckFinality_ReorgedOutBeforeFinality_SilentlyDropped) under
// Design B: a candidate that every configured provider, once each
// independently reaches finality, agrees is genuinely absent reaches
// ASYNC_NOT_FOUND -- the async analogue of "routine, silently dropped",
// never DISAGREEMENT_ALERT. A caller (Phase 2) is expected to drop such
// a candidate quietly, exactly like today, not alert a human.
func TestAsyncConfirm_ReorgBeforeFinalityCompatibility(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("A", absentObs(1000))
	status := c.RecordConfirmed("B", absentObs(1000))
	if status != AsyncNotFound {
		t.Fatalf("every provider agrees the deposit never became final: status = %s, want ASYNC_NOT_FOUND (today's own routine-drop case, async)", status)
	}
	if status == AsyncDisagreementAlert {
		t.Fatal("a routine pre-final reorg must never surface as a human-alert-worthy disagreement")
	}
}

// ---------------------------------------------------------------------
// Adversarial: stale/silent providers, outage+recovery, re-confirmation.
// ---------------------------------------------------------------------

func TestAsyncConfirm_StaleProvider_NoConfirmationUntilFreshSuccess(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("A", agreeingObs(1000))
	// B never calls RecordConfirmed at all across many "ticks" -- there
	// is nothing to simulate beyond simply not calling it; status must
	// stay PENDING indefinitely, not time out into any other state on
	// its own (a stale-pending ALERT is a separate, additive concern --
	// see the approved plan's own §9 -- not something this type decides).
	for i := 0; i < 50; i++ {
		if got := c.Status(); got != AsyncPending {
			t.Fatalf("iteration %d: status = %s, want PENDING (still waiting on a silent provider)", i, got)
		}
	}
}

func TestAsyncConfirm_ProviderOutageThenRecovery_ConfirmsOnReturn(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("A", agreeingObs(1000))
	// B "errors" for a while: caller simply never calls RecordConfirmed
	// for B (an error is never a call, per ProviderObservation's own doc
	// comment) -- then B recovers and confirms, agreeing with A.
	status := c.RecordConfirmed("B", agreeingObs(1000))
	if status != AsyncQuorumReached {
		t.Fatalf("after B recovers and agrees, status = %s, want QUORUM_REACHED (no penalty for the earlier gap)", status)
	}
}

// TestAsyncConfirm_ProviderReconfirmsItself_NeverADisagreement covers a
// provider calling RecordConfirmed twice for the SAME candidate (e.g. a
// later tick re-verifying, or a caller retrying defensively) with an
// identical observation both times -- must never be treated as a
// disagreement against itself.
func TestAsyncConfirm_ProviderReconfirmsItself_NeverADisagreement(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("A", agreeingObs(1000))
	status := c.RecordConfirmed("A", agreeingObs(1000)) // same provider, same observation, again
	if status != AsyncPending {
		t.Fatalf("A re-confirming identically with itself: status = %s, want PENDING (still only 1 distinct provider)", status)
	}
	if got := c.ConfirmedProviders(); len(got) != 1 {
		t.Fatalf("ConfirmedProviders() = %v, want exactly 1 entry (re-confirmation must not double-count)", got)
	}
}

// TestAsyncConfirm_MoreThanMinAgreementProviders_StillQuorumReached
// covers a 3rd, agreeing provider arriving after quorum was already
// reached with 2 -- status stays QUORUM_REACHED, not some different
// "over-confirmed" state a caller would have to special-case.
func TestAsyncConfirm_MoreThanMinAgreementProviders_StillQuorumReached(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("A", agreeingObs(1000))
	c.RecordConfirmed("B", agreeingObs(1000))
	status := c.RecordConfirmed("C", agreeingObs(1000))
	if status != AsyncQuorumReached {
		t.Fatalf("a 3rd agreeing provider: status = %s, want QUORUM_REACHED", status)
	}
}

// TestAsyncConfirm_MinAgreementOne_SingleProviderNeverSufficientByDefault
// documents that MinAgreement is clamped to at least 1 (never 0 or
// negative) but does NOT itself forbid a deployment from configuring
// MinAgreement=1 -- that would be a real single-provider-trust
// deployment mistake, exactly the kind chain.Pool's own NewPool already
// guards against at a higher level (chain/pool.go's own
// ErrTooFewProviders / MinAgreement-exceeds-provider-count checks). This
// type only enforces internal consistency of whatever minAgreement it's
// given; the "never trust one provider alone" POLICY is enforced by
// whatever Phase-2 wiring constructs it, mirroring chain.Pool's own
// layering (Pool refuses fewer than 2 providers; a caller could still,
// in principle, misconfigure MinAgreement=1 against 2 providers -- that
// misconfiguration is out of scope for this type to prevent).
func TestAsyncConfirm_MinAgreementClampedToAtLeastOne(t *testing.T) {
	c := NewCandidateConfirmations(0)
	status := c.RecordConfirmed("A", agreeingObs(1000))
	if status != AsyncQuorumReached {
		t.Fatalf("MinAgreement=0 clamped to 1, status after 1 confirmation = %s, want QUORUM_REACHED", status)
	}
}

// ---------------------------------------------------------------------
// Adversarial: a provider "lying" is indistinguishable, at this layer,
// from an honest provider that genuinely observed something different
// -- both surface identically as a disagreement. That IS the safety
// property: this layer cannot know who is right, only that two
// providers disagree, and it must never guess.
// ---------------------------------------------------------------------

func TestAsyncConfirm_ProviderLiesAboutHash_CaughtAsDisagreement(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("honest1", agreeingObs(1000))
	lying := agreeingObs(1000)
	lying.BlockHash = testHash(0xFF) // fabricated
	status := c.RecordConfirmed("liar", lying)
	if status != AsyncDisagreementAlert {
		t.Fatalf("a fabricated block hash: status = %s, want DISAGREEMENT_ALERT", status)
	}
	// A second honest provider agreeing with the first must still not
	// clear it -- 2 honest vs 1 liar is still "never outvoted."
	status = c.RecordConfirmed("honest2", agreeingObs(1000))
	if status != AsyncDisagreementAlert {
		t.Fatalf("2 honest providers vs 1 liar: status = %s, want DISAGREEMENT_ALERT (majority must not silently win)", status)
	}
}

func TestAsyncConfirm_ProviderLiesAboutDepositFacts_CaughtAsDisagreement(t *testing.T) {
	c := NewCandidateConfirmations(2)
	c.RecordConfirmed("honest1", agreeingObs(1000))
	lying := agreeingObs(1000)
	lying.OrderID = 999999 // fabricated order id
	status := c.RecordConfirmed("liar", lying)
	if status != AsyncDisagreementAlert {
		t.Fatalf("a fabricated order id: status = %s, want DISAGREEMENT_ALERT", status)
	}
}

// ---------------------------------------------------------------------
// ProviderHeightMonitor -- the monotonicity guard.
// ---------------------------------------------------------------------

func TestProviderHeightMonitor_AcceptsMonotonicIncreases(t *testing.T) {
	m := NewProviderHeightMonitor()
	if ok := m.Check("A", 1000); !ok {
		t.Fatal("first-ever reading for a provider must always be accepted")
	}
	if ok := m.Check("A", 1005); !ok {
		t.Fatal("a strictly higher reading must be accepted")
	}
	if ok := m.Check("A", 1005); !ok {
		t.Fatal("an EQUAL reading (no progress this tick) must be accepted, not treated as a regression")
	}
	hwm, seen := m.HighWaterMark("A")
	if !seen || hwm != 1005 {
		t.Fatalf("HighWaterMark = (%d, %v), want (1005, true)", hwm, seen)
	}
}

func TestProviderHeightMonitor_RejectsRegression(t *testing.T) {
	m := NewProviderHeightMonitor()
	m.Check("A", 1000)
	if ok := m.Check("A", 999); ok {
		t.Fatal("a LOWER reading than this provider's own prior high-water mark must be rejected")
	}
	// The bad reading must not have lowered the stored high-water mark.
	hwm, _ := m.HighWaterMark("A")
	if hwm != 1000 {
		t.Fatalf("high-water mark after a rejected regression = %d, want unchanged at 1000", hwm)
	}
	// And the bar for the NEXT check is still the real prior max, not
	// the rejected lower value.
	if ok := m.Check("A", 999); ok {
		t.Fatal("a second identical regression must also be rejected, not accepted because the first one 'moved the bar'")
	}
}

func TestProviderHeightMonitor_TracksProvidersIndependently(t *testing.T) {
	m := NewProviderHeightMonitor()
	m.Check("A", 5000)
	// B has never been seen -- a low first reading for B must not be
	// judged against A's own high-water mark.
	if ok := m.Check("B", 100); !ok {
		t.Fatal("provider B's own first reading must be independent of provider A's own high-water mark")
	}
}
