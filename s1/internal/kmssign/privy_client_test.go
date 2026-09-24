package kmssign

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestPrivyClient builds a privyClient pointed at ts instead of
// Privy's real API -- every test below replays a shape cmd/privy-probe
// captured from a real, live call (see privy_client.go's own top-of-file
// doc comment), so these tests need no real Privy credentials and never
// touch the network.
func newTestPrivyClient(ts *httptest.Server) *privyClient {
	return &privyClient{appID: "test-app-id", appSecret: "test-app-secret", http: ts.Client(), baseURL: ts.URL}
}

// TestPrivyClient_Do_SetsTheRealAuthShape confirms every request carries
// Basic Auth (App ID:secret), the privy-app-id header, and (when given)
// the idempotency-key header -- the exact shape this file's own doc
// comment says was proven against Privy's real API.
func TestPrivyClient_Do_SetsTheRealAuthShape(t *testing.T) {
	var gotUser, gotPass string
	var gotAuthOK bool
	var gotAppIDHeader, gotIdemKeyHeader string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotAuthOK = r.BasicAuth()
		gotAppIDHeader = r.Header.Get("privy-app-id")
		gotIdemKeyHeader = r.Header.Get("privy-idempotency-key")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "wallet-1", "address": "TFakeAddress", "public_key": "02" + strings.Repeat("00", 32),
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	if _, _, _, err := c.createTronWallet(context.Background(), "ext-id", "idem-123"); err != nil {
		t.Fatalf("createTronWallet: %v", err)
	}

	if !gotAuthOK || gotUser != "test-app-id" || gotPass != "test-app-secret" {
		t.Errorf("BasicAuth = (%q, %q, %v), want (test-app-id, test-app-secret, true)", gotUser, gotPass, gotAuthOK)
	}
	if gotAppIDHeader != "test-app-id" {
		t.Errorf("privy-app-id header = %q, want test-app-id", gotAppIDHeader)
	}
	if gotIdemKeyHeader != "idem-123" {
		t.Errorf("privy-idempotency-key header = %q, want idem-123", gotIdemKeyHeader)
	}
}

// TestPrivyClient_CreateTronWallet_ParsesARealShapedResponse replays the
// exact JSON shape a real POST /v1/wallets call returns for
// chain_type=tron: id/address/public_key, all top-level strings,
// public_key a 33-byte compressed hex key.
func TestPrivyClient_CreateTronWallet_ParsesARealShapedResponse(t *testing.T) {
	wantID := "wallet-abc123"
	wantAddress := "TFakeAddressForTestingOnlyNotReal11"
	wantPubKeyHex := "02" + strings.Repeat("ab", 32)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wallets" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body["chain_type"] != "tron" {
			t.Errorf("chain_type = %q, want tron", body["chain_type"])
		}
		if body["external_id"] != "ext-id" {
			t.Errorf("external_id = %q, want ext-id", body["external_id"])
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": wantID, "address": wantAddress, "public_key": wantPubKeyHex,
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	walletID, address, pubKey, err := c.createTronWallet(context.Background(), "ext-id", "idem-key")
	if err != nil {
		t.Fatalf("createTronWallet: %v", err)
	}
	if walletID != wantID {
		t.Errorf("walletID = %q, want %q", walletID, wantID)
	}
	if address != wantAddress {
		t.Errorf("address = %q, want %q", address, wantAddress)
	}
	wantPubKeyBytes, _ := hex.DecodeString(wantPubKeyHex)
	if !bytes.Equal(pubKey[:], wantPubKeyBytes) {
		t.Errorf("pubKey = %x, want %x", pubKey, wantPubKeyBytes)
	}
}

// TestPrivyClient_CreateTronWallet_MissingFieldIsATypedError confirms a
// response missing any of id/address/public_key is a hard,
// ErrPrivyMalformedResponse-wrapped error -- never silently accepted
// with a zero-value field, since a wallet record with a missing address
// or public key is unusable custody, not a partial success.
func TestPrivyClient_CreateTronWallet_MissingFieldIsATypedError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "wallet-1"}) // missing address, public_key
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, _, _, err := c.createTronWallet(context.Background(), "ext-id", "idem-key")
	if !errors.Is(err, ErrPrivyMalformedResponse) {
		t.Fatalf("createTronWallet() error = %v, want ErrPrivyMalformedResponse", err)
	}
}

// TestPrivyClient_RawSignTron_SendsNestedParamsAndParsesRealShapedResponse
// confirms the request body nests hash inside a "params" object (a real,
// empirically-proven Privy requirement -- a flat body 400s, see this
// test's own sibling error test below) and that a real 64-byte bare
// (r, s) response is split correctly, with no DER/recovery-byte
// assumptions.
func TestPrivyClient_RawSignTron_SendsNestedParamsAndParsesRealShapedResponse(t *testing.T) {
	digest := [32]byte{1, 2, 3, 4}
	wantR := bytes.Repeat([]byte{0xAA}, 32)
	wantS := bytes.Repeat([]byte{0xBB}, 32)
	wantSig := append(append([]byte{}, wantR...), wantS...)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wallets/wallet-1/raw_sign" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body struct {
			Params struct {
				Hash string `json:"hash"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		wantHash := "0x" + hex.EncodeToString(digest[:])
		if body.Params.Hash != wantHash {
			t.Errorf("params.hash = %q, want %q (hash must be nested inside a params object)", body.Params.Hash, wantHash)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"signature": hex.EncodeToString(wantSig)},
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	rBytes, sBytes, err := c.rawSignTron(context.Background(), "wallet-1", digest, "idem-key")
	if err != nil {
		t.Fatalf("rawSignTron: %v", err)
	}
	if !bytes.Equal(rBytes[:], wantR) {
		t.Errorf("r = %x, want %x", rBytes, wantR)
	}
	if !bytes.Equal(sBytes[:], wantS) {
		t.Errorf("s = %x, want %x", sBytes, wantS)
	}
}

// TestPrivyClient_RawSignTron_WrongLengthSignatureIsATypedError confirms
// a signature that isn't exactly 64 bytes (e.g. DER-wrapped, or carrying
// a trailing recovery byte) is rejected as malformed rather than
// silently truncated/padded.
func TestPrivyClient_RawSignTron_WrongLengthSignatureIsATypedError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"signature": "abcd"}})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, _, err := c.rawSignTron(context.Background(), "wallet-1", [32]byte{}, "idem-key")
	if !errors.Is(err, ErrPrivyMalformedResponse) {
		t.Fatalf("rawSignTron() error = %v, want ErrPrivyMalformedResponse", err)
	}
}

// TestPrivyClient_RawSignTron_SurfacesARealShapedAPIError replays the
// exact real HTTP 400 error Privy returned when this file's own
// predecessor (cmd/privy-probe) first sent an un-nested request body --
// {"error": "Unrecognized key(s) in object: 'hash'"} -- confirming
// decodePrivyAPIError parses Privy's real error envelope into a
// *PrivyAPIError a caller can inspect, not just a generic failure.
func TestPrivyClient_RawSignTron_SurfacesARealShapedAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "Unrecognized key(s) in object: 'hash'",
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, _, err := c.rawSignTron(context.Background(), "wallet-1", [32]byte{}, "idem-key")
	var apiErr *PrivyAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("rawSignTron() error = %v, want a *PrivyAPIError", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", apiErr.Status, http.StatusBadRequest)
	}
	if apiErr.Msg != "Unrecognized key(s) in object: 'hash'" {
		t.Errorf("Msg = %q, want the real captured message", apiErr.Msg)
	}
}

// TestPrivyClient_GetTronWallet_ParsesARealShapedResponse replays the
// exact shape a real, live GET /wallets/{id} call returns (confirmed by
// the operator via cmd/privy-probe -get-wallet against a real wallet):
// identical to POST /wallets' own creation response --
// id/chain_type/address/public_key, public_key a 33-byte compressed hex
// key.
func TestPrivyClient_GetTronWallet_ParsesARealShapedResponse(t *testing.T) {
	wantPubKeyHex := "02" + strings.Repeat("cd", 32)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/wallets/wallet-existing" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "wallet-existing", "chain_type": "tron",
			"address": "TExistingAddress", "public_key": wantPubKeyHex,
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	pubKey, err := c.getTronWallet(context.Background(), "wallet-existing")
	if err != nil {
		t.Fatalf("getTronWallet: %v", err)
	}
	wantPubKeyBytes, _ := hex.DecodeString(wantPubKeyHex)
	if !bytes.Equal(pubKey[:], wantPubKeyBytes) {
		t.Errorf("pubKey = %x, want %x", pubKey, wantPubKeyBytes)
	}
}

// TestPrivyClient_GetTronWallet_RejectsANonTronChainType confirms
// getTronWallet hard-errors on any chain_type other than "tron" --
// only tron-typed wallets support raw_sign and report a public_key at
// all, so silently accepting an ethereum-typed wallet here would produce
// a KMSClient that can never actually sign.
func TestPrivyClient_GetTronWallet_RejectsANonTronChainType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "wallet-eth", "chain_type": "ethereum", "address": "0xabc",
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, err := c.getTronWallet(context.Background(), "wallet-eth")
	if err == nil {
		t.Fatal("getTronWallet() for an ethereum-typed wallet: want an error, got nil")
	}
}

// TestPrivyClient_GetTronWallet_MissingPublicKeyIsATypedError confirms a
// tron-typed wallet response with no public_key is a hard,
// ErrPrivyMalformedResponse-wrapped error.
func TestPrivyClient_GetTronWallet_MissingPublicKeyIsATypedError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "wallet-1", "chain_type": "tron", "address": "TAddr",
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, err := c.getTronWallet(context.Background(), "wallet-1")
	if !errors.Is(err, ErrPrivyMalformedResponse) {
		t.Fatalf("getTronWallet() error = %v, want ErrPrivyMalformedResponse", err)
	}
}

// TestPrivyClient_GetTronWallet_SurfacesARealShapedAPIError confirms a
// non-2xx GET /wallets/{id} response (e.g. a wallet id that doesn't
// exist) decodes into a *PrivyAPIError, not a generic failure.
func TestPrivyClient_GetTronWallet_SurfacesARealShapedAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "wallet not found"})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, err := c.getTronWallet(context.Background(), "wallet-missing")
	var apiErr *PrivyAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("getTronWallet() error = %v, want a *PrivyAPIError", err)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", apiErr.Status, http.StatusNotFound)
	}
}

// TestPrivyClient_CreateEVMWallet_ParsesARealShapedResponse replays the
// exact JSON shape a real POST /v1/wallets call returns for
// chain_type=ethereum -- id/address only, deliberately NO public_key
// field (confirmed empirically live, both at creation and via a separate
// GET /wallets/{id} call -- see this file's own top-of-file doc comment
// and privybsc.go's).
func TestPrivyClient_CreateEVMWallet_ParsesARealShapedResponse(t *testing.T) {
	wantID := "wallet-evm-abc123"
	wantAddress := "0x46a83bCF7CBcafA728F2AD90d5752578770c7fcb"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wallets" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body["chain_type"] != "ethereum" {
			t.Errorf("chain_type = %q, want ethereum", body["chain_type"])
		}
		if body["external_id"] != "ext-id" {
			t.Errorf("external_id = %q, want ext-id", body["external_id"])
		}
		w.WriteHeader(http.StatusOK)
		// Deliberately no "public_key" field -- the real, confirmed shape.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": wantID, "address": wantAddress, "chain_type": "ethereum",
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	walletID, address, err := c.createEVMWallet(context.Background(), "ext-id", "idem-key")
	if err != nil {
		t.Fatalf("createEVMWallet: %v", err)
	}
	if walletID != wantID {
		t.Errorf("walletID = %q, want %q", walletID, wantID)
	}
	if address != wantAddress {
		t.Errorf("address = %q, want %q", address, wantAddress)
	}
}

// TestPrivyClient_CreateEVMWallet_MissingFieldIsATypedError confirms a
// response missing id or address is a hard, ErrPrivyMalformedResponse
// error -- unlike createTronWallet, a missing public_key must NOT be
// treated as an error here, since ethereum wallets never report one.
func TestPrivyClient_CreateEVMWallet_MissingFieldIsATypedError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "wallet-1"}) // missing address
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, _, err := c.createEVMWallet(context.Background(), "ext-id", "idem-key")
	if !errors.Is(err, ErrPrivyMalformedResponse) {
		t.Fatalf("createEVMWallet() error = %v, want ErrPrivyMalformedResponse", err)
	}
}

// TestPrivyClient_Secp256k1SignEVM_SendsMethodAndParsesRealShapedResponse
// confirms the request body carries method="secp256k1_sign" with hash
// nested inside params (the exact shape cmd/privy-probe's own
// probeSecp256k1SignRPC sent live), and that a real 65-byte (r||s||v)
// response has its trailing recovery byte discarded, keeping only r/s.
func TestPrivyClient_Secp256k1SignEVM_SendsMethodAndParsesRealShapedResponse(t *testing.T) {
	digest := [32]byte{1, 2, 3, 4}
	wantR := bytes.Repeat([]byte{0xAA}, 32)
	wantS := bytes.Repeat([]byte{0xBB}, 32)
	wantSig := append(append(append([]byte{}, wantR...), wantS...), 0x1b) // + trailing v byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wallets/wallet-1/rpc" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body struct {
			Method string `json:"method"`
			Params struct {
				Hash string `json:"hash"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.Method != "secp256k1_sign" {
			t.Errorf("method = %q, want secp256k1_sign", body.Method)
		}
		wantHash := "0x" + hex.EncodeToString(digest[:])
		if body.Params.Hash != wantHash {
			t.Errorf("params.hash = %q, want %q", body.Params.Hash, wantHash)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"encoding": "hex", "signature": "0x" + hex.EncodeToString(wantSig)},
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	rBytes, sBytes, err := c.secp256k1SignEVM(context.Background(), "wallet-1", digest, "idem-key")
	if err != nil {
		t.Fatalf("secp256k1SignEVM: %v", err)
	}
	if !bytes.Equal(rBytes[:], wantR) {
		t.Errorf("r = %x, want %x", rBytes, wantR)
	}
	if !bytes.Equal(sBytes[:], wantS) {
		t.Errorf("s = %x, want %x", sBytes, wantS)
	}
}

// TestPrivyClient_Secp256k1SignEVM_WrongLengthSignatureIsATypedError
// confirms a signature that isn't exactly 65 bytes (e.g. a bare 64-byte
// r||s with no v, the TRON raw_sign shape) is rejected as malformed
// rather than silently truncated/padded -- mirrors
// TestPrivyClient_RawSignTron_WrongLengthSignatureIsATypedError's own
// exact-length check, at 65 instead of 64.
func TestPrivyClient_Secp256k1SignEVM_WrongLengthSignatureIsATypedError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"signature": "0x" + strings.Repeat("aa", 64)}})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, _, err := c.secp256k1SignEVM(context.Background(), "wallet-1", [32]byte{}, "idem-key")
	if !errors.Is(err, ErrPrivyMalformedResponse) {
		t.Fatalf("secp256k1SignEVM() error = %v, want ErrPrivyMalformedResponse", err)
	}
}

// TestPrivyClient_Secp256k1SignEVM_SurfacesARealShapedAPIError replays
// the exact real HTTP 400 error Privy returned when raw_sign (not
// secp256k1_sign) was sent for an ethereum-typed wallet --
// {"code":"invalid_data","error":"ethereum wallets are not supported for
// this low-level signature endpoint."} -- confirming decodePrivyAPIError
// still parses Privy's real error envelope correctly for this endpoint.
func TestPrivyClient_Secp256k1SignEVM_SurfacesARealShapedAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "invalid_data", "error": "ethereum wallets are not supported for this low-level signature endpoint.",
		})
	}))
	defer ts.Close()

	c := newTestPrivyClient(ts)
	_, _, err := c.secp256k1SignEVM(context.Background(), "wallet-1", [32]byte{}, "idem-key")
	var apiErr *PrivyAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("secp256k1SignEVM() error = %v, want a *PrivyAPIError", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", apiErr.Status, http.StatusBadRequest)
	}
}
