//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. Covers Assign's own Privy-backed branch (see
// store.go's own Provisioner interface and provisioner variable) against
// a local, in-memory fake -- never a real S1/Privy round trip, which is
// covered instead by s1/internal/kmssign's own real-Privy integration
// test, run separately with the operator's own credentials. Direct port
// of tronwatcher/internal/addresses's own identically-shaped test
// (separate modules, no shared internal package -- this repo's own
// established cross-module-boundary convention).
package addresses_test

import (
	"context"
	"fmt"
	"testing"

	"depositwatcher/internal/addresses"
)

// fakeProvisioner is a deterministic, in-memory addresses.Provisioner --
// this package's own local fake for testing Assign's Privy-backed branch
// without a real S1 round trip. Duplicated rather than shared with s1's
// own FakeSigningService.ProvisionBSCDepositKey, which satisfies a
// different interface in a separate module -- this codebase's own
// established "duplicate, don't share" convention.
type fakeProvisioner struct {
	calls int
}

func (f *fakeProvisioner) ProvisionBSCDepositKey(ctx context.Context, index uint32) (string, error) {
	f.calls++
	return fmt.Sprintf("0xFakeProvisioned%d", index), nil
}

// TestAssign_UsesProvisionerWhenConfigured confirms Assign calls out to
// the configured Provisioner (S1, in production) for a NEW order, rather
// than deriving an address locally, and that a retried Assign for the
// SAME order short-circuits on the existing row without provisioning a
// second time.
func TestAssign_UsesProvisionerWhenConfigured(t *testing.T) {
	// testPool also configures a local xpub; the Provisioner branch takes
	// priority when both are set (see Assign's own doc comment) -- a real
	// deployment never configures both (cmd/watcherd's own fail-loud
	// startup check), but Assign itself doesn't need xpub unset to prove
	// its own dispatch here.
	pool := testPool(t)
	fake := &fakeProvisioner{}
	addresses.ConfigureS1Provisioning(fake)
	t.Cleanup(func() { addresses.ConfigureS1Provisioning(nil) })

	ctx := context.Background()
	orderID := uniqueOrderID()
	quotedAt, quoteExpiresAt := fixedTimes()

	addr, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("provisioner ProvisionBSCDepositKey calls = %d, want 1", fake.calls)
	}

	addr2, err := addresses.Assign(ctx, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1", quotedAt, quoteExpiresAt)
	if err != nil {
		t.Fatalf("Assign (retried): %v", err)
	}
	if addr2 != addr {
		t.Fatalf("retried Assign returned a different address: %s vs %s", addr, addr2)
	}
	if fake.calls != 1 {
		t.Fatalf("provisioner ProvisionBSCDepositKey calls after retried Assign = %d, want still 1 (retried Assign must not re-provision)", fake.calls)
	}
}
