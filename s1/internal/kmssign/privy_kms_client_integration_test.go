//go:build integration

// Requires real Privy credentials (PRIVY_APP_ID/PRIVY_APP_SECRET) --
// skips (not fails) when unset, since this test spends a real, though
// harmless and effectively-zero-cost, Privy API call (minting one
// throwaway wallet). Run manually, with the operator's own credentials
// in their own terminal -- never by Claude (see this repository's own
// standing credential-handling rule). No Postgres needed: PrivyKMSClient
// holds no database state (see its own top-of-file doc comment).
package kmssign

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestPrivyKMSClient_RealPrivy_GetPublicKeyThenSignRoundTrips mirrors
// TestPrivyTronDepositKeys_RealPrivy_ProvisionThenSignRoundTrips: proves
// the real HTTP integration still works end to end through the ACTUAL
// production code path (Wrapper -> PrivyKMSClient -> privyClient),
// never a hand-rolled probe request. Creates one real, throwaway wallet
// via createTronWallet (test-only use here -- production code never
// creates a slot wallet, see privy_kms_client.go's own doc comment),
// then exercises PrivyKMSClient exactly as cmd/seed-slot-key and cmd/s1d
// would: GetPublicKey by id, then Sign, independently recovering the
// signature to prove it corresponds to that same public key.
func TestPrivyKMSClient_RealPrivy_GetPublicKeyThenSignRoundTrips(t *testing.T) {
	appID := os.Getenv("PRIVY_APP_ID")
	appSecret := os.Getenv("PRIVY_APP_SECRET")
	if appID == "" || appSecret == "" {
		t.Skip("PRIVY_APP_ID/PRIVY_APP_SECRET not set; skipping real-Privy integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw := newPrivyClient(appID, appSecret)
	externalID := fmt.Sprintf("s1-privy-kms-client-test-%d", time.Now().UnixNano())
	walletID, createdAddress, createdPubKey, err := raw.createTronWallet(ctx, externalID, "s1-privy-kms-client-test-create-"+externalID)
	if err != nil {
		t.Fatalf("creating a throwaway real Privy wallet: %v", err)
	}
	t.Logf("created real throwaway Privy TRON wallet %s at address %s for this test", walletID, createdAddress)

	kmsClient := NewPrivyKMSClient(appID, appSecret)
	w := NewWrapper(kmsClient)

	pubKey, err := w.GetPublicKey(ctx, walletID)
	if err != nil {
		t.Fatalf("GetPublicKey (real Privy): %v", err)
	}
	if pubKey != createdPubKey {
		t.Fatalf("GetPublicKey() = %x, want the same key the wallet was created with (%x)", pubKey, createdPubKey)
	}

	digest := [32]byte{0xBE, 0xEF, 0xCA, 0xFE, 1, 2, 3, 4, 5, 6}
	sig, err := w.Sign(ctx, walletID, digest, pubKey)
	if err != nil {
		t.Fatalf("Sign (real Privy): %v", err)
	}
	if !verifyRecoverableSignature(t, sig, digest, pubKey) {
		t.Fatal("real-Privy Sign (via PrivyKMSClient/Wrapper) produced a signature that does not independently recover to the wallet's own public key")
	}
}
