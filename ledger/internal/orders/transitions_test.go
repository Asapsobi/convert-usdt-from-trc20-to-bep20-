package orders

import "testing"

// TestTransitionTableHasExactlyTwelvePairs is a cheap, DB-free sanity
// check on the production table's own shape. It is not the acceptance
// test -- that is TestFullStateCrossProduct in the integration suite,
// which drives real Transition calls against an independently
// hand-copied version of the spec's table, and is what actually proves
// this map matches the spec rather than just itself.
func TestTransitionTableHasExactlyTwelvePairs(t *testing.T) {
	if len(transitionTable) != 12 {
		t.Fatalf("transitionTable has %d entries, want 12", len(transitionTable))
	}
	for p := range transitionTable {
		if !p.From.Valid() {
			t.Errorf("transitionTable key has invalid From state %q", p.From)
		}
		if !p.To.Valid() {
			t.Errorf("transitionTable key has invalid To state %q", p.To)
		}
		if p.From.Terminal() {
			t.Errorf("transitionTable has an outgoing pair from terminal state %q: %s -> %s", p.From, p.From, p.To)
		}
	}
}

func TestNoSelfTransitions(t *testing.T) {
	for p := range transitionTable {
		if p.From == p.To {
			t.Errorf("transitionTable contains a self-transition: %s -> %s", p.From, p.To)
		}
	}
}
