package kmssign

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// fakePrivyKMSServer stands in for Privy's real API for PrivyKMSClient's
// own two calls -- GET /wallets/{id} and POST /wallets/{id}/raw_sign --
// backed by a REAL, independently-verifiable keypair (via FakeKMSClient,
// the same technique privytron_registry_integration_test.go's own
// fakePrivyServer uses), never arbitrary bytes. Deliberately NOT reused
// from that file: this one needs no wallet-CREATION endpoint at all
// (PrivyKMSClient never creates a wallet, see its own doc comment), and
// keeping this test plain (no integration build tag, no Postgres) means
// it runs under ordinary `go test ./...`.
type fakePrivyKMSServer struct {
	srv       *httptest.Server
	walletID  string
	chainType string
	kms       *FakeKMSClient
	pubKeyHex string
}

func newFakePrivyKMSServer(t *testing.T, walletID string) *fakePrivyKMSServer {
	t.Helper()
	kms := NewFakeKMSClient(123)
	derPub, err := kms.GetPublicKey(context.Background(), walletID)
	if err != nil {
		t.Fatalf("fake KMS GetPublicKey: %v", err)
	}
	pubKey, err := parseDERPublicKey(derPub)
	if err != nil {
		t.Fatalf("parseDERPublicKey: %v", err)
	}

	f := &fakePrivyKMSServer{walletID: walletID, chainType: "tron", kms: kms, pubKeyHex: hex.EncodeToString(pubKey[:])}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePrivyKMSServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/wallets/"+f.walletID:
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": f.walletID, "chain_type": f.chainType, "address": "T" + f.walletID, "public_key": f.pubKeyHex,
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

		der, err := f.kms.Sign(context.Background(), f.walletID, digest)
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

func (f *fakePrivyKMSServer) newClient() *PrivyKMSClient {
	return &PrivyKMSClient{client: &privyClient{appID: "test", appSecret: "test", http: f.srv.Client(), baseURL: f.srv.URL}}
}

// TestWrapper_OverPrivyKMSClient_SignProducesAnIndependentlyVerifiableSignature
// is TestWrapper_SignProducesAnIndependentlyVerifiableSignature's own
// PrivyKMSClient counterpart: proves Wrapper's existing, UNCHANGED
// machinery (DER parsing, low-s normalization, recovery-id brute force)
// works correctly against this new backend end to end -- GetPublicKey
// then Sign through a real *Wrapper, never touching PrivyKMSClient's
// internals directly.
func TestWrapper_OverPrivyKMSClient_SignProducesAnIndependentlyVerifiableSignature(t *testing.T) {
	ctx := context.Background()
	f := newFakePrivyKMSServer(t, "slot-wallet-1")
	w := NewWrapper(f.newClient())

	pubKey, err := w.GetPublicKey(ctx, "slot-wallet-1")
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}

	seenV := map[byte]bool{}
	for i := 0; i < 40; i++ {
		digest := testDigest(i)
		sig, err := w.Sign(ctx, "slot-wallet-1", digest, pubKey)
		if err != nil {
			t.Fatalf("Sign (digest %d): %v", i, err)
		}
		seenV[sig[64]] = true
		if !verifyRecoverableSignature(t, sig, digest, pubKey) {
			t.Fatalf("Sign (digest %d): produced signature does not independently recover to the expected public key", i)
		}
	}
	if !seenV[0] || !seenV[1] {
		t.Fatalf("across 40 signatures, only saw recovery codes %v -- want both 0 and 1 to appear", keysOf(seenV))
	}
}

// TestWrapper_OverPrivyKMSClient_RejectsAWrongKeyID confirms GetPublicKey
// for an unknown wallet id surfaces the fake server's 404 as an error,
// not a zero-value success.
func TestWrapper_OverPrivyKMSClient_RejectsAWrongKeyID(t *testing.T) {
	f := newFakePrivyKMSServer(t, "slot-wallet-1")
	w := NewWrapper(f.newClient())

	if _, err := w.GetPublicKey(context.Background(), "some-other-wallet-id"); err == nil {
		t.Fatal("GetPublicKey() for an unregistered wallet id: want an error, got nil")
	}
}

func testDigest(i int) [32]byte {
	var d [32]byte
	copy(d[:], []byte{byte(i), byte(i >> 8), 1, 2, 3})
	return d
}
