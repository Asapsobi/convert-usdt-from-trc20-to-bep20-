//go:build integration

// Requires a real, reachable Postgres 16 instance (S1_TEST_DATABASE_URL)
// AND real Privy credentials (PRIVY_APP_ID/PRIVY_APP_SECRET) -- skips
// (not fails) when either is unset, since this test spends two real,
// though harmless and effectively-zero-cost, Privy API calls (minting a
// real wallet, then one throwaway recovery signature). Run manually, with
// the operator's own credentials in their own terminal -- never by
// Claude (see this repository's own standing credential-handling rule).
// Mirrors privytron_integration_test.go's own proof (Provision then
// Sign, independently recover and verify) through the ACTUAL production
// code path, plus the one real fact TRON's own version never needed to
// check: that the recovered public key Provision records really does
// derive to the same EVM address Privy itself reports for the wallet.
// See privybsc_registry_integration_test.go for the registry's own
// get-or-create/race/recovery-rejection coverage against a fake Privy
// server -- this file's only job is proving the real HTTP integration
// still works end to end.
package kmssign

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func TestPrivyBSCDepositKeys_RealPrivy_ProvisionThenSignRoundTrips(t *testing.T) {
	pool := privyTestPool(t)
	appID := os.Getenv("PRIVY_APP_ID")
	appSecret := os.Getenv("PRIVY_APP_SECRET")
	if appID == "" || appSecret == "" {
		t.Skip("PRIVY_APP_ID/PRIVY_APP_SECRET not set; skipping real-Privy integration test")
	}

	keys := NewPrivyBSCDepositKeys(pool, appID, appSecret)
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
	if provisioned.PublicKey == ([33]byte{}) {
		t.Fatalf("Provision recorded an all-zero public key -- the recovery step (see privybsc.go) should have populated a real one or failed outright")
	}
	t.Logf("provisioned real Privy BSC wallet %s at address %s for index %d, recovered public key %x",
		provisioned.PrivyWalletID, provisioned.Address, index, provisioned.PublicKey)

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

	// The one fact TRON's own version of this test never needed to check:
	// the recovered/stored public key must itself derive to the SAME EVM
	// address Privy reported for this wallet at creation time -- proving
	// Provision's own recovery step (privybsc.go) verified something
	// real, not just internally self-consistent.
	derivedAddress := deriveEVMAddressForRecovery(recovered)
	if !strings.EqualFold(derivedAddress, provisioned.Address) {
		t.Fatalf("recovered public key derives to EVM address %s, want it to match Privy's own reported address %s", derivedAddress, provisioned.Address)
	}

	// Re-provisioning the same index must be a safe, idempotent no-op
	// against real Privy too -- the same guarantee
	// TestPrivyBSCDepositKeys_Provision_IsIdempotentAndNeverMintsASecondWallet
	// proves against a fake server, re-verified here against the real API.
	reProvisioned, err := keys.Provision(ctx, index)
	if err != nil {
		t.Fatalf("Provision (re-provision, real Privy): %v", err)
	}
	if reProvisioned != provisioned {
		t.Fatalf("re-provisioning index %d returned a different wallet: first=%+v second=%+v", index, provisioned, reProvisioned)
	}
}
