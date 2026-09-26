package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"dispatcher/internal/ledgerclient"
)

// Only DIRECT and STANDARD orders are the dispatcher's to pay. A RELAY
// order is relayd's: paying it here as well would pay its customer twice.
func TestDispatchableTier(t *testing.T) {
	for tier, want := range map[string]bool{"DIRECT": true, "STANDARD": true, "RELAY": false, "SWEEP": false, "": false} {
		if got := DispatchableTier(tier); got != want {
			t.Errorf("DispatchableTier(%q) = %v, want %v", tier, got, want)
		}
	}
}

// EnterDispatching refuses a foreign tier before touching the ledger --
// the dispatcher here has no ledger client at all.
func TestEnterDispatching_RefusesARelayOrder(t *testing.T) {
	d := &Dispatcher{}
	_, err := d.EnterDispatching(context.Background(), ledgerclient.Order{ExternalID: "relay-web-1", Tier: "RELAY"}, 1, time.Now())
	if !errors.Is(err, ErrNotDispatchable) {
		t.Fatalf("expected ErrNotDispatchable, got %v", err)
	}
}
