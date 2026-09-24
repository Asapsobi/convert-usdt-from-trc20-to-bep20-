//go:build integration

// Requires a real, reachable Postgres 16 instance (S1_TEST_DATABASE_URL);
// run via `make test-integration`. Exercises PrivyTronDepositKeys's own
// real-Postgres registry (get-or-create/race behavior) against a FAKE
// Privy HTTP server (httptest), never real Privy -- see
// privy_client_test.go's own HTTP-shape tests for real-shape coverage of
// privyClient in isolation, and privytron_integration_test.go (gated
// separately, additionally skip-if-unset on real Privy credentials) for
// a real-Privy round trip.
package kmssign

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"s1/internal/db"
)

func privyTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("S1_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("S1_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `TRUNCATE s1_privy_deposit_wallets`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

// fakePrivyServer stands in for Privy's real API, returning a REAL,
// independently-verifiable keypair/signature (via the fake KMS client
// this package already uses for its own non-Privy tests) rather than
// arbitrary bytes -- so a test can prove Sign's own recovery-id matching
// works end to end, not just that HTTP round-tripped successfully. One
// fake wallet (fixed keypair) backs every wallet-creation call, since
// these tests only care about registry behavior (which row exists for
// which index), not about distinguishing multiple real keys.
type fakePrivyServer struct {
	srv            *httptest.Server
	createCalls    int
	mu             sync.Mutex
	walletCounter  int
	kms            *FakeKMSClient
	fixedPubKeyHex string
}

func newFakePrivyServer(t *testing.T) *fakePrivyServer {
	t.Helper()
	kms := NewFakeKMSClient(99)
	derPub, err := kms.GetPublicKey(context.Background(), "privy-fake-wallet")
	if err != nil {
		t.Fatalf("fake KMS GetPublicKey: %v", err)
	}
	pubKey, err := parseDERPublicKey(derPub)
	if err != nil {
		t.Fatalf("parseDERPublicKey: %v", err)
	}

	f := &fakePrivyServer{kms: kms, fixedPubKeyHex: hex.EncodeToString(pubKey[:])}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePrivyServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/wallets":
		f.mu.Lock()
		f.createCalls++
		f.walletCounter++
		walletID := hexWalletID(f.walletCounter)
		f.mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": walletID, "address": "T" + walletID, "public_key": f.fixedPubKeyHex,
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/raw_sign"):
		var body struct {
			Params struct {
				Hash string `json:"hash"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		digestBytes, err := hex.DecodeString(trimHexPrefix(body.Params.Hash))
		if err != nil || len(digestBytes) != 32 {
			http.Error(w, "bad digest", http.StatusBadRequest)
			return
		}
		var digest [32]byte
		copy(digest[:], digestBytes)

		der, err := f.kms.Sign(context.Background(), "privy-fake-wallet", digest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sig, err := ecdsa.ParseDERSignature(der)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rVal := sig.R()
		sVal := sig.S()
		var rBytes, sBytes [32]byte
		rVal.PutBytesUnchecked(rBytes[:])
		sVal.PutBytesUnchecked(sBytes[:])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"signature": hex.EncodeToString(append(rBytes[:], sBytes[:]...))},
		})
	default:
		http.NotFound(w, r)
	}
}

func hexWalletID(n int) string {
	return "wallet-" + hex.EncodeToString([]byte{byte(n >> 8), byte(n)})
}

func (f *fakePrivyServer) newKeys(pool *db.Pool) *PrivyTronDepositKeys {
	return &PrivyTronDepositKeys{
		client: &privyClient{appID: "test", appSecret: "test", http: f.srv.Client(), baseURL: f.srv.URL},
		pool:   pool,
	}
}

func TestPrivyTronDepositKeys_Provision_IsIdempotentAndNeverMintsASecondWallet(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyServer(t)
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

func TestPrivyTronDepositKeys_Provision_DifferentIndicesGetDifferentWallets(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyServer(t)
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

func TestPrivyTronDepositKeys_PublicKeyAndSign_UnprovisionedIndexReturnsTypedError(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyServer(t)
	keys := f.newKeys(pool)
	ctx := context.Background()

	if _, err := keys.PublicKey(ctx, 7); !errors.Is(err, ErrTronDepositKeyNotProvisioned) {
		t.Errorf("PublicKey() error = %v, want ErrTronDepositKeyNotProvisioned", err)
	}
	if _, err := keys.Sign(ctx, 7, [32]byte{1}, [33]byte{2}); !errors.Is(err, ErrTronDepositKeyNotProvisioned) {
		t.Errorf("Sign() error = %v, want ErrTronDepositKeyNotProvisioned", err)
	}
}

// TestPrivyTronDepositKeys_ProvisionThenSignProducesAnIndependentlyVerifiableSignature
// is this package's own end-to-end proof for the Privy-backed path,
// mirroring TestWrapper_SignProducesAnIndependentlyVerifiableSignature's
// own discipline: never trust "Sign returned no error" alone, always
// independently recover and compare.
func TestPrivyTronDepositKeys_ProvisionThenSignProducesAnIndependentlyVerifiableSignature(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyServer(t)
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

// TestPrivyTronDepositKeys_Provision_ConcurrentRaceProducesOneRow mirrors
// tronwatcher's own TestAssign_ConcurrentRaceProducesOneRowNoWastedIndex
// -- this package's own approved plan names the real, bounded cost this
// proves stays bounded: under Privy, each of the losing concurrent
// Provision calls also mints a real (harmless, orphaned) Privy wallet
// before losing the DB race, accepted as a trade-off rather than
// engineered around.
func TestPrivyTronDepositKeys_Provision_ConcurrentRaceProducesOneRow(t *testing.T) {
	pool := privyTestPool(t)
	f := newFakePrivyServer(t)
	keys := f.newKeys(pool)
	ctx := context.Background()

	const n = 10
	results := make([]PrivyTronKey, n)
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM s1_privy_deposit_wallets WHERE chain_type = 'tron' AND deposit_index = 100`).Scan(&rowCount); err != nil {
		t.Fatalf("counting rows for index 100: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("row count for index 100 = %d, want exactly 1 despite %d concurrent Provision calls", rowCount, n)
	}
}
