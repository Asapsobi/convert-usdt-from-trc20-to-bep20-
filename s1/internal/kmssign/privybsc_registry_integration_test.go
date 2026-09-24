//go:build integration

// Requires a real, reachable Postgres 16 instance (S1_TEST_DATABASE_URL);
// run via `make test-integration`. Exercises PrivyBSCDepositKeys's own
// real-Postgres registry (get-or-create/race behavior, AND the
// public-key-recovery step privytron.go's own TRON path never needed --
// see privybsc.go's own top-of-file doc comment) against a FAKE Privy
// HTTP server (httptest), never real Privy -- see privy_client_test.go's
// own HTTP-shape tests for real-shape coverage of privyClient in
// isolation, and privybsc_integration_test.go (gated separately,
// additionally skip-if-unset on real Privy credentials) for a real-Privy
// round trip. Reuses privyTestPool (privytron_registry_integration_test.go,
// same package) rather than redefining it.
package kmssign

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"s1/internal/db"
)

// fakePrivyEVMServer stands in for Privy's real API for chain_type=
// ethereum wallets. Unlike fakePrivyServer (the TRON-direction sibling,
// same package), it CANNOT reuse one fixed keypair for every wallet: the
// real recovery step (privybsc.go's own Provision) verifies the
// recovered public key's own derived EVM address against the wallet's
// reported address, so a synthetic/decoupled address the way the TRON
// fake uses ("T" + a counter, paired with an unrelated fixed pubkey)
// would make every real Provision call here fail that check. Each
// wallet-creation call therefore mints a genuinely fresh keypair and
// reports that key's own real, correctly-derived address -- signing
// requests are then dispatched to the SAME key by wallet id, so
// recovery genuinely succeeds, exercising Provision's real verification
// logic rather than a shortcut around it.
type fakePrivyEVMServer struct {
	mu            sync.Mutex
	srv           *httptest.Server
	createCalls   int
	walletCounter int
	keys          map[string]*secp256k1.PrivateKey
	// forceMismatchWalletID, when set, makes the /rpc handler sign with a
	// DIFFERENT, unrelated key for that one wallet id -- the one knob
	// TestPrivyBSCDepositKeys_Provision_RecoveryMismatchIsRejected needs.
	forceMismatchWalletID string
}

func newFakePrivyEVMServer(t *testing.T) *fakePrivyEVMServer {
	t.Helper()
	f := &fakePrivyEVMServer{keys: make(map[string]*secp256k1.PrivateKey)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePrivyEVMServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/wallets":
		priv, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.createCalls++
		f.walletCounter++
		walletID := fmt.Sprintf("evm-wallet-%d", f.walletCounter)
		f.keys[walletID] = priv
		f.mu.Unlock()

		address := deriveEVMAddressForRecovery(priv.PubKey())
		w.WriteHeader(http.StatusOK)
		// Deliberately no "public_key" field -- the real, confirmed shape.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": walletID, "address": address, "chain_type": "ethereum",
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rpc"):
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 3 || parts[0] != "wallets" {
			http.NotFound(w, r)
			return
		}
		walletID := parts[1]

		var body struct {
			Method string `json:"method"`
			Params struct {
				Hash string `json:"hash"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Method != "secp256k1_sign" {
			http.Error(w, "unsupported method", http.StatusBadRequest)
			return
		}
		digestBytes, err := hex.DecodeString(trimHexPrefix(body.Params.Hash))
		if err != nil || len(digestBytes) != 32 {
			http.Error(w, "bad digest", http.StatusBadRequest)
			return
		}
		var digest [32]byte
		copy(digest[:], digestBytes)

		f.mu.Lock()
		priv, ok := f.keys[walletID]
		mismatch := walletID == f.forceMismatchWalletID
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if mismatch {
			wrongPriv, err := secp256k1.GeneratePrivateKey()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			priv = wrongPriv
		}

		sig := ecdsa.Sign(priv, digest[:])
		rVal := sig.R()
		sVal := sig.S()
		var rBytes, sBytes [32]byte
		rVal.PutBytesUnchecked(rBytes[:])
		sVal.PutBytesUnchecked(sBytes[:])
		full := append(append(append([]byte{}, rBytes[:]...), sBytes[:]...), 0x1b) // fake trailing v -- discarded by secp256k1SignEVM either way

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"encoding": "hex", "signature": "0x" + hex.EncodeToString(full)},
		})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePrivyEVMServer) newKeys(pool *db.Pool) *PrivyBSCDepositKeys {
	return &PrivyBSCDepositKeys{
		client: &privyClient{appID: "test", appSecret: "test", http: f.srv.Client(), baseURL: f.srv.URL},
		pool:   pool,
	}
}

func TestPrivyBSCDepositKeys_Provision_IsIdempotentAndNeverMintsASecondWallet(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyEVMServer(t)
	keys := f.newKeys(pool)
	ctx := context.Background()

	first, err := keys.Provision(ctx, 42)
	if err != nil {
		t.Fatalf("Provision (first): %v", err)
	}
	second, err := keys.Provision(ctx, 42)
	if err != nil {
		t.Fatalf("Provision (second, retried): %v", err)
	}
	if first != second {
		t.Fatalf("Provision returned different results for a retried call: first=%+v second=%+v", first, second)
	}

	f.mu.Lock()
	calls := f.createCalls
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("wallet-creation calls = %d, want exactly 1 (retried Provision must not mint a second wallet)", calls)
	}
}

func TestPrivyBSCDepositKeys_Provision_DifferentIndicesGetDifferentWallets(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyEVMServer(t)
	keys := f.newKeys(pool)
	ctx := context.Background()

	a, err := keys.Provision(ctx, 1)
	if err != nil {
		t.Fatalf("Provision(1): %v", err)
	}
	b, err := keys.Provision(ctx, 2)
	if err != nil {
		t.Fatalf("Provision(2): %v", err)
	}
	if a.PrivyWalletID == b.PrivyWalletID || a.Address == b.Address {
		t.Fatalf("indices 1 and 2 got the same wallet: %+v vs %+v", a, b)
	}
}

func TestPrivyBSCDepositKeys_PublicKeyAndSign_UnprovisionedIndexReturnsTypedError(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyEVMServer(t)
	keys := f.newKeys(pool)
	ctx := context.Background()

	if _, err := keys.PublicKey(ctx, 7); !errors.Is(err, ErrBSCDepositKeyNotProvisioned) {
		t.Errorf("PublicKey() error = %v, want ErrBSCDepositKeyNotProvisioned", err)
	}
	if _, err := keys.Sign(ctx, 7, [32]byte{1}, [33]byte{2}); !errors.Is(err, ErrBSCDepositKeyNotProvisioned) {
		t.Errorf("Sign() error = %v, want ErrBSCDepositKeyNotProvisioned", err)
	}
}

// TestPrivyBSCDepositKeys_ProvisionThenSignProducesAnIndependentlyVerifiableSignature
// mirrors TestPrivyTronDepositKeys_ProvisionThenSignProducesAnIndependentlyVerifiableSignature
// exactly: never trust "Sign returned no error" alone, always
// independently recover and compare -- here against the recovered
// PUBLIC KEY Provision's own recovery step already verified, proving
// that verified key is really usable for real signing afterward, not
// just self-consistent at provisioning time.
func TestPrivyBSCDepositKeys_ProvisionThenSignProducesAnIndependentlyVerifiableSignature(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyEVMServer(t)
	keys := f.newKeys(pool)
	ctx := context.Background()

	provisioned, err := keys.Provision(ctx, 5)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	pubKey, err := keys.PublicKey(ctx, 5)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if pubKey != provisioned.PublicKey {
		t.Fatalf("PublicKey() = %x, want the same key Provision recorded (%x)", pubKey, provisioned.PublicKey)
	}

	digest := [32]byte{9, 8, 7, 6, 5}
	sig, err := keys.Sign(ctx, 5, digest, pubKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	compact := make([]byte, 65)
	compact[0] = compactSigRecoveryBase + sig[64] + compactSigCompressedFlag
	copy(compact[1:33], sig[0:32])
	copy(compact[33:65], sig[32:64])
	recovered, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		t.Fatalf("independently recovering signer from Sign's own output: %v", err)
	}
	expected, err := secp256k1.ParsePubKey(pubKey[:])
	if err != nil {
		t.Fatalf("parsing expected pub key: %v", err)
	}
	if !recovered.IsEqual(expected) {
		t.Fatal("Sign produced a signature that does not independently recover to the provisioned wallet's own public key")
	}
}

// TestPrivyBSCDepositKeys_Provision_RecoveryMismatchIsRejected proves
// Provision's own verification step actually rejects a real, well-formed
// signature that simply doesn't recover to the wallet's own reported
// address -- and, critically, that nothing is left behind in the
// registry when it does (unverified custody must never be recorded).
func TestPrivyBSCDepositKeys_Provision_RecoveryMismatchIsRejected(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyEVMServer(t)

	// Prime one wallet, then flip on mismatch for the NEXT one this
	// server creates (index 200's own wallet id, which Provision hasn't
	// been asked for yet -- so this exercises the recovery call
	// Provision itself makes, not a pre-existing row).
	f.mu.Lock()
	f.forceMismatchWalletID = fmt.Sprintf("evm-wallet-%d", f.walletCounter+1)
	f.mu.Unlock()

	keys := f.newKeys(pool)
	ctx := context.Background()

	if _, err := keys.Provision(ctx, 200); !errors.Is(err, ErrBSCDepositPublicKeyRecoveryFailed) {
		t.Fatalf("Provision() error = %v, want ErrBSCDepositPublicKeyRecoveryFailed", err)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM s1_privy_deposit_wallets WHERE chain_type = 'ethereum' AND deposit_index = 200`).Scan(&rowCount); err != nil {
		t.Fatalf("counting rows for index 200: %v", err)
	}
	if rowCount != 0 {
		t.Fatalf("row count for index 200 = %d, want exactly 0 -- a failed recovery must never record custody", rowCount)
	}
}

// TestPrivyBSCDepositKeys_Provision_ConcurrentRaceProducesOneRow mirrors
// TestPrivyTronDepositKeys_Provision_ConcurrentRaceProducesOneRow -- see
// this package's own approved plan for the real, accepted cost this
// proves stays bounded: under this design, each losing concurrent
// Provision call wastes a wallet creation AND a throwaway recovery sign,
// not just a wallet creation.
func TestPrivyBSCDepositKeys_Provision_ConcurrentRaceProducesOneRow(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyEVMServer(t)
	keys := f.newKeys(pool)
	ctx := context.Background()

	const n = 10
	results := make([]PrivyBSCKey, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = keys.Provision(ctx, 100)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Provision goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatalf("goroutine %d got a different winner than goroutine 0: %+v vs %+v", i, results[i], results[0])
		}
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM s1_privy_deposit_wallets WHERE chain_type = 'ethereum' AND deposit_index = 100`).Scan(&rowCount); err != nil {
		t.Fatalf("counting rows for index 100: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("row count for index 100 = %d, want exactly 1 despite %d concurrent Provision calls", rowCount, n)
	}
}
