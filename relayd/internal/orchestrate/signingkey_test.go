package orchestrate

import (
	"testing"

	"relayd/internal/transfers"
)

// The same top-up sent from two treasuries has the same EVM digest (the
// sender isn't part of it), so S1's idempotency key must tell them apart.
func TestSigningKey_TreasurySlotIsPartOfTheKey(t *testing.T) {
	digest := [32]byte{1, 2, 3}
	one := transferRequest{job: "gas_topup:leg:FORWARD:0", purpose: transfers.GasTopUp, signer: signer{slot: true, slotID: 1}}
	two := one
	two.signer.slotID = 2
	if one.signingKey(digest) == two.signingKey(digest) {
		t.Fatalf("two treasuries share a signing key: %s", one.signingKey(digest))
	}
	forward := transferRequest{job: "leg", purpose: transfers.Forward}
	if got := forward.signingKey(digest); got != "relayd:sign:leg:0102030000000000000000000000000000000000000000000000000000000000" {
		t.Fatalf("a forward's key must not change, got %s", got)
	}
}
