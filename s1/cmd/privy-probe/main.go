// Command privy-probe is a standalone diagnostic tool, not part of S1's
// own production surface -- the same "verify a real vendor's real
// behavior before designing around it, never guess from docs alone"
// convention relayd/cmd/sideshift-probe and relayd/cmd/fixedfloat-probe
// already established this session, applied to Privy (privy.io) Server
// Wallets as a candidate real custody backend for S1's own per-order
// deposit-signing keys (see internal/kmssign/trondeposit.go's own
// top-of-file doc comment for the gap this is evaluating a fix for).
//
// The one fact this tool exists to answer, because it is not documented
// anywhere in Privy's own public docs: does POST /v1/wallets/{id}/raw_sign
// -- the "sign this digest I already computed" primitive S1 actually
// needs -- work against a wallet whose chain_type is "tron", or is TRON
// signing restricted to the separate tron_signTransaction method (which
// would require S1 to hand Privy a fully structured transaction instead
// of a bare digest, a materially different and larger integration).
//
// SAFETY: this tool creates two real Privy server wallets (a real,
// harmless, effectively-zero-cost API call -- no money moves, no funds
// are ever at risk) and signs a fixed, printed, reproducible test digest
// with each. It never sends, funds, or moves anything, and never will.
//
// Usage:
//
//	export PRIVY_APP_ID=...
//	export PRIVY_APP_SECRET=...
//	go run ./cmd/privy-probe
//
// A second mode, -get-wallet <id>, answers a DIFFERENT question: now that
// S1's per-order TRON deposit keys are Privy-backed (see
// internal/kmssign/privytron.go), the next gap is S1's own SLOT keys --
// the main signing keys, still on an in-process fake KMS
// (internal/kmssign/provider.go's own KMSClient). Those keys are few,
// long-lived, and provisioned once by an operator, not minted per-order
// -- so a real KMSClient backend needs to fetch an EXISTING wallet's
// public key by id (GET /wallets/{id}) without re-creating it, a
// different real-API question raw_sign/secp256k1_sign above never
// answered. Purely read-only, real, and effectively free:
//
//	go run ./cmd/privy-probe -get-wallet <wallet-id>
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

const privyBaseURL = "https://api.privy.io/v1"

// compactSigRecoveryBase/compactSigCompressedFlag mirror
// internal/kmssign/wrapper.go's own identically-named constants --
// duplicated here (this is a standalone diagnostic tool, deliberately
// not importing the production kmssign package) rather than shared,
// matching this repo's own established "duplicate, don't share across a
// boundary" convention.
const (
	compactSigRecoveryBase   = 27
	compactSigCompressedFlag = 4
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "privy-probe:", err)
		os.Exit(1)
	}
}

func run() error {
	getWalletID := flag.String("get-wallet", "", "if set, only fetch this EXISTING wallet by id (GET /wallets/{id}) and print the raw response -- skips the create+sign probe entirely (see this file's own top-of-file doc comment for why)")
	flag.Parse()

	appID := os.Getenv("PRIVY_APP_ID")
	appSecret := os.Getenv("PRIVY_APP_SECRET")
	if appID == "" || appSecret == "" {
		return fmt.Errorf("PRIVY_APP_ID and PRIVY_APP_SECRET must both be set (never pass these as flags -- they'd end up in your shell history)")
	}
	client := &privyClient{appID: appID, appSecret: appSecret, http: &http.Client{Timeout: 20 * time.Second}}

	if *getWalletID != "" {
		return probeGetWallet(client, *getWalletID)
	}

	testDigest := sha256.Sum256([]byte("s1-privy-probe test digest -- fixed and reproducible"))
	fmt.Printf("Test digest (sha256, fixed/reproducible): %x\n", testDigest)

	results := make(map[string]string)
	for _, chainType := range []string{"tron", "ethereum"} {
		fmt.Println()
		fmt.Println(strings.Repeat("=", 72))
		fmt.Printf("chain_type = %s\n", chainType)
		fmt.Println(strings.Repeat("=", 72))
		results[chainType] = probeChain(client, chainType, testDigest)
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("SUMMARY")
	for _, chainType := range []string{"tron", "ethereum"} {
		fmt.Printf("  %-9s %s\n", chainType+":", results[chainType])
	}
	fmt.Println(strings.Repeat("=", 72))
	return nil
}

type privyClient struct {
	appID     string
	appSecret string
	http      *http.Client
}

// do posts body (or nil for a GET-shaped call with no body) to path,
// authenticated per Privy's own documented app-level auth (HTTP Basic:
// App ID as username, App secret as password, plus a privy-app-id
// header), and returns the raw response bytes verbatim -- unparsed, so
// every field a real response carries is visible, not just the ones a
// hypothetical production integration would end up mapping.
func (c *privyClient) do(ctx context.Context, method, path string, body any, idempotencyKey string) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, privyBaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.appID, c.appSecret)
	req.Header.Set("privy-app-id", c.appID)
	req.Header.Set("User-Agent", "s1-privy-probe/1.0 (+diagnostic tool, not a browser)")
	if idempotencyKey != "" {
		req.Header.Set("privy-idempotency-key", idempotencyKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

func prettyPrint(label string, body []byte) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Printf("--- %s (raw, not valid JSON) ---\n%s\n", label, body)
		return
	}
	pretty, _ := json.MarshalIndent(v, "", "  ")
	fmt.Printf("--- %s ---\n%s\n", label, pretty)
}

// probeGetWallet fetches an EXISTING wallet by id via GET /wallets/{id}
// -- the one remaining fact needed before S1's own slot keys (the main
// signing keys, not just per-order deposit keys) can move onto Privy:
// given only a wallet id an operator already has, can its public key be
// fetched WITHOUT re-creating the wallet or re-deriving anything?
// Purely read-only, real, and effectively free -- no money moves, no
// funds are ever at risk.
func probeGetWallet(c *privyClient, walletID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	status, body, err := c.do(ctx, http.MethodGet, "/wallets/"+walletID, nil, "")
	if err != nil {
		return fmt.Errorf("GET /wallets/%s failed: %w", walletID, err)
	}
	prettyPrint(fmt.Sprintf("GET /wallets/%s (HTTP %d)", walletID, status), body)
	if status < 200 || status >= 300 {
		return fmt.Errorf("GET /wallets/%s failed with HTTP %d -- see response above", walletID, status)
	}

	var wallet map[string]any
	if err := json.Unmarshal(body, &wallet); err != nil {
		return fmt.Errorf("could not parse response: %w", err)
	}
	fmt.Println()
	fmt.Printf("id:         %v\n", wallet["id"])
	fmt.Printf("chain_type: %v\n", wallet["chain_type"])
	fmt.Printf("address:    %v\n", wallet["address"])
	fmt.Printf("public_key: %v\n", firstNonEmptyString(wallet, "public_key", "publicKey"))
	return nil
}

// probeChain runs the full create-wallet + raw_sign + recover flow for
// one chain_type and returns a one-line human-readable verdict.
func probeChain(c *privyClient, chainType string, digest [32]byte) string {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	externalID := fmt.Sprintf("s1-privy-probe-%s-%d", chainType, time.Now().UnixNano())
	fmt.Printf("Creating a real %s wallet (external_id=%s)...\n\n", chainType, externalID)

	createIdemKey := "s1-privy-probe-create-" + externalID
	status, body, err := c.do(ctx, http.MethodPost, "/wallets", map[string]string{
		"chain_type":  chainType,
		"external_id": externalID,
	}, createIdemKey)
	if err != nil {
		return fmt.Sprintf("wallet creation request failed: %v", err)
	}
	prettyPrint(fmt.Sprintf("POST /wallets (HTTP %d)", status), body)
	if status < 200 || status >= 300 {
		return fmt.Sprintf("wallet creation failed with HTTP %d -- see response above", status)
	}

	var wallet map[string]any
	if err := json.Unmarshal(body, &wallet); err != nil {
		return fmt.Sprintf("could not parse wallet creation response: %v", err)
	}
	walletID, _ := wallet["id"].(string)
	if walletID == "" {
		return "wallet creation response has no usable \"id\" field -- see raw response above"
	}
	reportedPubKeyRaw := firstNonEmptyString(wallet, "public_key", "publicKey")
	address := firstNonEmptyString(wallet, "address")
	fmt.Printf("\nwallet id: %s\naddress:   %s\npublic_key (as reported): %s\n", walletID, address, reportedPubKeyRaw)

	fmt.Println()
	fmt.Printf("Requesting raw_sign over the fixed test digest (%x)...\n\n", digest)

	signIdemKey := "s1-privy-probe-sign-" + externalID
	status, body, err = c.do(ctx, http.MethodPost, "/wallets/"+walletID+"/raw_sign", map[string]any{
		"params": map[string]string{
			"hash": "0x" + hex.EncodeToString(digest[:]),
		},
	}, signIdemKey)
	if err != nil {
		return fmt.Sprintf("raw_sign request failed: %v", err)
	}
	prettyPrint(fmt.Sprintf("POST /wallets/%s/raw_sign (HTTP %d)", walletID, status), body)
	if status < 200 || status >= 300 {
		rawSignErr := fmt.Sprintf("raw_sign fails: HTTP %d", status)
		if chainType == "ethereum" {
			fmt.Println()
			fmt.Println("raw_sign failed for ethereum -- trying the chain-scoped secp256k1_sign RPC method instead...")
			fmt.Println()
			return rawSignErr + "; " + probeSecp256k1SignRPC(c, walletID, digest, address, externalID)
		}
		return rawSignErr
	}

	var signResp map[string]any
	sigRaw := ""
	if err := json.Unmarshal(body, &signResp); err == nil {
		sigRaw = firstNonEmptyString(signResp, "signature", "data")
		if sigRaw == "" {
			if data, ok := signResp["data"].(map[string]any); ok {
				sigRaw = firstNonEmptyString(data, "signature")
			}
		}
	}
	if sigRaw == "" {
		return "raw_sign works: HTTP " + strconv.Itoa(status) + ", but no recognizable \"signature\" field -- compare raw response above by hand"
	}

	sigBytes, decErr := decodeFlexible(sigRaw)
	if decErr != nil || len(sigBytes) < 64 {
		return fmt.Sprintf("raw_sign works: HTTP %d, but signature field %q could not be decoded to >=64 bytes (%v) -- compare by hand", status, sigRaw, decErr)
	}

	verdict := recoverAndCompare(sigBytes, digest, reportedPubKeyRaw)
	return fmt.Sprintf("raw_sign works: HTTP %d, signature=%d bytes, recovery=%s", status, len(sigBytes), verdict)
}

// probeSecp256k1SignRPC tries Privy's chain-scoped secp256k1_sign RPC
// method (POST /wallets/{id}/rpc, method="secp256k1_sign") -- the
// endpoint Privy's own docs namespace under "ethereum" specifically,
// evaluated here as the candidate replacement now that raw_sign is
// confirmed NOT to work for ethereum-typed wallets. Ethereum wallet
// creation responses carry no public_key field (confirmed empirically
// against a real wallet), so verification here derives the recovered
// signature's own EIP-55 EVM address (the same Keccak256-based
// technique s1/internal/slots/evm.go's own deriveEVMAddress already
// uses in production) and compares it against the wallet's own reported
// address instead of comparing raw public keys.
func probeSecp256k1SignRPC(c *privyClient, walletID string, digest [32]byte, reportedAddress, externalID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	idemKey := "s1-privy-probe-secp256k1sign-" + externalID
	status, body, err := c.do(ctx, http.MethodPost, "/wallets/"+walletID+"/rpc", map[string]any{
		"method": "secp256k1_sign",
		"params": map[string]string{
			"hash": "0x" + hex.EncodeToString(digest[:]),
		},
	}, idemKey)
	if err != nil {
		return fmt.Sprintf("secp256k1_sign request failed: %v", err)
	}
	prettyPrint(fmt.Sprintf("POST /wallets/%s/rpc (secp256k1_sign) (HTTP %d)", walletID, status), body)
	if status < 200 || status >= 300 {
		return fmt.Sprintf("secp256k1_sign also fails: HTTP %d", status)
	}

	var resp map[string]any
	sigRaw := ""
	if err := json.Unmarshal(body, &resp); err == nil {
		sigRaw = firstNonEmptyString(resp, "signature", "data")
		if sigRaw == "" {
			if data, ok := resp["data"].(map[string]any); ok {
				sigRaw = firstNonEmptyString(data, "signature")
			}
		}
	}
	if sigRaw == "" {
		return "secp256k1_sign works: HTTP " + strconv.Itoa(status) + ", but no recognizable \"signature\" field -- compare raw response above by hand"
	}

	sigBytes, decErr := decodeFlexible(sigRaw)
	if decErr != nil || len(sigBytes) < 64 {
		return fmt.Sprintf("secp256k1_sign works: HTTP %d, but signature field %q could not be decoded to >=64 bytes (%v) -- compare by hand", status, sigRaw, decErr)
	}

	verdict := recoverAndCompareEVMAddress(sigBytes, digest, reportedAddress)
	return fmt.Sprintf("secp256k1_sign works: HTTP %d, signature=%d bytes, recovery=%s", status, len(sigBytes), verdict)
}

// recoverAndCompareEVMAddress mirrors recoverAndCompare's own recovery
// brute force, but verifies against an EVM address (the only identifier
// an ethereum-typed wallet's own creation response reports) rather than
// a raw public key.
func recoverAndCompareEVMAddress(sig []byte, digest [32]byte, reportedAddress string) string {
	var rBytes, sBytes [32]byte
	copy(rBytes[:], sig[0:32])
	copy(sBytes[:], sig[32:64])

	for recoveryCode := byte(0); recoveryCode <= 1; recoveryCode++ {
		compact := make([]byte, 65)
		compact[0] = compactSigRecoveryBase + recoveryCode + compactSigCompressedFlag
		copy(compact[1:33], rBytes[:])
		copy(compact[33:65], sBytes[:])

		recovered, _, err := ecdsa.RecoverCompact(compact, digest[:])
		if err != nil {
			continue
		}
		recoveredAddr := deriveEVMAddress(recovered)
		if strings.EqualFold(recoveredAddr, reportedAddress) {
			return "MATCH (recovered signer's own EVM address equals the wallet's own reported address)"
		}
	}
	return fmt.Sprintf("MISMATCH or could not recover a candidate matching reported address %q -- compare raw response by hand", reportedAddress)
}

// deriveEVMAddress mirrors s1/internal/slots/evm.go's own production
// implementation exactly (Keccak256 of the 64-byte uncompressed point,
// last 20 bytes, 0x-prefixed hex -- lowercase, not EIP-55 checksummed,
// since this tool only needs a case-insensitive compare against
// whatever case Privy itself reports).
func deriveEVMAddress(pub *secp256k1.PublicKey) string {
	uncompressed := pub.SerializeUncompressed()
	h := sha3.NewLegacyKeccak256()
	h.Write(uncompressed[1:])
	hash := h.Sum(nil)
	return "0x" + hex.EncodeToString(hash[len(hash)-20:])
}

// recoverAndCompare brute-forces both possible recovery codes against
// the first 64 bytes of sig (r||s) -- the exact technique
// internal/kmssign/wrapper.go's own finishRecoverableSignature already
// uses in production, duplicated here since this is a standalone tool --
// and reports whether either recovered public key matches whatever
// public key the wallet-creation response itself claimed.
func recoverAndCompare(sig []byte, digest [32]byte, reportedPubKeyRaw string) string {
	var rBytes, sBytes [32]byte
	copy(rBytes[:], sig[0:32])
	copy(sBytes[:], sig[32:64])

	var expected *secp256k1.PublicKey
	if reportedPubKeyRaw != "" {
		if pubBytes, err := decodeFlexible(reportedPubKeyRaw); err == nil {
			expected, _ = secp256k1.ParsePubKey(pubBytes)
		}
	}

	var recoveredAny *secp256k1.PublicKey
	matched := false
	for recoveryCode := byte(0); recoveryCode <= 1; recoveryCode++ {
		compact := make([]byte, 65)
		compact[0] = compactSigRecoveryBase + recoveryCode + compactSigCompressedFlag
		copy(compact[1:33], rBytes[:])
		copy(compact[33:65], sBytes[:])

		recovered, _, err := ecdsa.RecoverCompact(compact, digest[:])
		if err != nil {
			continue
		}
		recoveredAny = recovered
		if expected != nil && recovered.IsEqual(expected) {
			matched = true
			break
		}
	}

	switch {
	case matched:
		return "MATCH (recovered signer equals the wallet's own reported public key)"
	case expected == nil:
		if recoveredAny != nil {
			return fmt.Sprintf("could not parse a public key from the wallet response to compare against -- recovered candidate pubkey: %x (compare by hand)", recoveredAny.SerializeCompressed())
		}
		return "could not parse a public key from the wallet response, and recovery itself failed for both candidate codes -- compare raw response by hand"
	default:
		return "MISMATCH (recovered a valid signer, but it does not equal the wallet's own reported public key -- investigate before trusting this signature shape)"
	}
}

// decodeFlexible tries hex (with or without a 0x prefix) then base64 --
// Privy's own docs show 0x-prefixed hex for digests/addresses, but the
// exact encoding of a raw_sign response's own signature/public_key
// fields is one of the things this tool exists to observe, not assume.
func decodeFlexible(s string) ([]byte, error) {
	trimmed := strings.TrimPrefix(s, "0x")
	if b, err := hex.DecodeString(trimmed); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("value %q is neither valid hex nor valid base64", s)
}

func firstNonEmptyString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
