//go:build integration

// Requires a real, reachable Postgres 16 instance (S1_TEST_DATABASE_URL)
// AND real Privy credentials (PRIVY_APP_ID/PRIVY_APP_SECRET) -- skips
// (not fails) when either is unset, since this test spends a real,
// though harmless and effectively-zero-cost, Privy API call (minting a
// real wallet). Run manually, with the operator's own credentials in
// their own terminal -- never by Claude (see this repository's own
// standing credential-handling rule). Mirrors cmd/privy-probe/main.go's
// own proof (Provision then Sign, independently recover and verify) but
// through the ACTUAL production code path this time, not a hand-rolled
// probe request. See privytron_registry_integration_test.go for the
// registry's own get-or-create/race coverage against a fake Privy
// server -- this file's only job is proving the real HTTP integration
// still works end to end.
package kmssign

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func TestPrivyTronDepositKeys_RealPrivy_ProvisionThenSignRoundTrips(t *testing.T) {
	pool := privyTestPool(t)
	appID := os.Getenv("PRIVY_APP_ID")
	appSecret := os.Getenv("PRIVY_APP_SECRET")
	if appID == "" || appSecret == "" {
		t.Skip("PRIVY_APP_ID/PRIVY_APP_SECRET not set; skipping real-Privy integration test")
	}

	keys := NewPrivyTronDepositKeys(pool, appID, appSecret)
	// A large, fixed index unlikely to collide with any other test or
	// manual run against the same Privy app -- deterministic, so a
	// retried run of this exact test is itself idempotent against Privy
	// (see Provision's own doc comment).
	const index = 900_000_001

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	provisioned, err := keys.Provision(ctx, index)
	if err != nil {
		t.Fatalf("Provision (real Privy): %v", err)
	}
	if provisioned.Address == "" || provisioned.PrivyWalletID == "" {
		t.Fatalf("Provision returned an incomplete result: %+v", provisioned)
	}
	t.Logf("provisioned real Privy TRON wallet %s at address %s for index %d", provisioned.PrivyWalletID, provisioned.Address, index)

	pubKey, err := keys.PublicKey(ctx, index)
	if err != nil {
		t.Fatalf("PublicKey (real Privy): %v", err)
	}
	if pubKey != provisioned.PublicKey {
		t.Fatalf("PublicKey() = %x, want the same key Provision recorded (%x)", pubKey, provisioned.PublicKey)
	}

	digest := [32]byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 2, 3, 4, 5, 6, 7, 8}
	sig, err := keys.Sign(ctx, index, digest, pubKey)
	if err != nil {
		t.Fatalf("Sign (real Privy): %v", err)
	}

	compact := make([]byte, 65)
	compact[0] = compactSigRecoveryBase + sig[64] + compactSigCompressedFlag
	copy(compact[1:33], sig[0:32])
	copy(compact[33:65], sig[32:64])
	recovered, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		t.Fatalf("independently recovering signer from Sign's own real-Privy output: %v", err)
	}
	expected, err := secp256k1.ParsePubKey(pubKey[:])
	if err != nil {
		t.Fatalf("parsing expected pub key: %v", err)
	}
	if !recovered.IsEqual(expected) {
		t.Fatal("real-Privy Sign produced a signature that does not independently recover to the provisioned wallet's own public key")
	}

	// Re-provisioning the same index must be a safe, idempotent no-op
	// against real Privy too -- the same guarantee
	// TestPrivyTronDepositKeys_Provision_IsIdempotentAndNeverMintsASecondWallet
	// proves against a fake server, re-verified here against the real API.
	reProvisioned, err := keys.Provision(ctx, index)
	if err != nil {
		t.Fatalf("Provision (re-provision, real Privy): %v", err)
	}
	if reProvisioned != provisioned {
		t.Fatalf("re-provisioning index %d returned a different wallet: first=%+v second=%+v", index, provisioned, reProvisioned)
	}
}
