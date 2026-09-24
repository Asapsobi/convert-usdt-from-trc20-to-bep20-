// This file is S1's own real client for Privy (privy.io) Server
// Wallets -- a genuine custody-as-a-service backend, replacing
// TronDepositKeys's own in-process BIP32 xprv (see that file's own
// PRODUCTION CAVEAT) with private key material that lives inside
// Privy's infrastructure instead of this process's memory.
//
// The exact request/response shapes below were NOT designed from
// Privy's own public API docs -- those left the one fact that matters
// most (does signing work for TRON at all) undocumented. They were
// empirically PROVEN against Privy's real, live, production API by
// cmd/privy-probe, this file's own direct predecessor (see that
// command's own top-of-file doc comment for the full investigation).
// Two real, load-bearing findings that doc-reading never would have
// surfaced, both confirmed by independently recovering a real signature
// and checking it against the wallet's own reported identity:
//   - POST /wallets/{id}/raw_sign requires its fields nested inside a
//     "params" object -- a flat body 400s with "Unrecognized key(s) in
//     object: 'hash'". Confirmed by hitting this exact error live.
//   - raw_sign returns EXACTLY 64 bytes: bare (r, s), never
//     DER-wrapped, never a trailing recovery byte -- confirmed by
//     decoding a real response and recovering the signer via both
//     candidate recovery codes.
package kmssign

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const privyBaseURL = "https://api.privy.io/v1"

// ErrPrivyMalformedResponse means a Privy API response could not be
// parsed the way the exact, previously-proven shape requires -- a real,
// serious condition (Privy's own API contract changed, or this
// process's own request was subtly wrong), never silently treated as
// "no signature."
var ErrPrivyMalformedResponse = fmt.Errorf("kmssign: malformed response from Privy")

// privyClient is S1's own thin HTTP layer over Privy's real API --
// mirrors cmd/privy-probe/main.go's own already-proven privyClient.do
// almost verbatim, trimmed of that command's defensive/exploratory
// parsing now that the real shapes are known, not guessed.
type privyClient struct {
	appID     string
	appSecret string
	http      *http.Client
	// baseURL defaults to privyBaseURL (real Privy) -- overridable only by
	// this package's own tests, pointed at an httptest.Server replaying
	// captured real-Privy response shapes instead.
	baseURL string
}

func newPrivyClient(appID, appSecret string) *privyClient {
	return &privyClient{appID: appID, appSecret: appSecret, http: &http.Client{Timeout: 20 * time.Second}, baseURL: privyBaseURL}
}

// do posts body to path, authenticated per Privy's own documented
// app-level auth (HTTP Basic: App ID as username, App secret as
// password, plus a privy-app-id header), and returns the raw response
// bytes -- every caller below parses only the specific fields its own
// already-proven shape guarantees, never more.
func (c *privyClient) do(ctx context.Context, method, path string, body any, idempotencyKey string) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("kmssign: privy: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("kmssign: privy: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.appID, c.appSecret)
	req.Header.Set("privy-app-id", c.appID)
	if idempotencyKey != "" {
		req.Header.Set("privy-idempotency-key", idempotencyKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("kmssign: privy: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("kmssign: privy: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

// PrivyAPIError is a structured error response from Privy.
type PrivyAPIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *PrivyAPIError) Error() string {
	return fmt.Sprintf("kmssign: privy returned HTTP %d %s: %s", e.Status, e.Code, e.Msg)
}

func decodePrivyAPIError(status int, body []byte) *PrivyAPIError {
	var envelope struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	return &PrivyAPIError{Status: status, Code: envelope.Code, Msg: envelope.Error}
}

// privyWalletResponse is the wallet-object shape BOTH POST /wallets
// (wallet creation) and GET /wallets/{id} (fetching an EXISTING wallet,
// used by getTronWallet below) return -- confirmed identical by a real,
// live GET /wallets/{id} call (see getTronWallet's own doc comment).
// createTronWallet and getTronWallet each apply their own validation
// over the same decoded shape rather than duplicating this struct.
type privyWalletResponse struct {
	ID        string `json:"id"`
	ChainType string `json:"chain_type"`
	Address   string `json:"address"`
	PublicKey string `json:"public_key"`
}

// createTronWallet calls POST /wallets with chain_type=tron, minting a
// real Privy wallet -- a real, harmless, effectively-zero-cost API call
// (confirmed by cmd/privy-probe: no funds are ever at risk from wallet
// creation alone). idempotencyKey should be deterministic per caller
// (see privytron.go's own Provision) so a retried call is safe.
func (c *privyClient) createTronWallet(ctx context.Context, externalID, idempotencyKey string) (walletID, address string, pubKey [33]byte, err error) {
	status, body, err := c.do(ctx, http.MethodPost, "/wallets", map[string]string{
		"chain_type":  "tron",
		"external_id": externalID,
	}, idempotencyKey)
	if err != nil {
		return "", "", [33]byte{}, err
	}
	if status < 200 || status >= 300 {
		return "", "", [33]byte{}, decodePrivyAPIError(status, body)
	}

	var wallet privyWalletResponse
	if err := json.Unmarshal(body, &wallet); err != nil {
		return "", "", [33]byte{}, fmt.Errorf("%w: decoding wallet creation response: %v", ErrPrivyMalformedResponse, err)
	}
	if wallet.ID == "" || wallet.Address == "" || wallet.PublicKey == "" {
		return "", "", [33]byte{}, fmt.Errorf("%w: wallet creation response missing id, address, or public_key", ErrPrivyMalformedResponse)
	}
	pubBytes, err := hex.DecodeString(trimHexPrefix(wallet.PublicKey))
	if err != nil || len(pubBytes) != 33 {
		return "", "", [33]byte{}, fmt.Errorf("%w: public_key %q is not a 33-byte compressed hex key", ErrPrivyMalformedResponse, wallet.PublicKey)
	}
	copy(pubKey[:], pubBytes)
	return wallet.ID, wallet.Address, pubKey, nil
}

// getTronWallet calls GET /wallets/{id} for an EXISTING wallet --
// confirmed live (by the operator, via cmd/privy-probe -get-wallet) to
// return the identical id/chain_type/address/public_key shape POST
// /wallets does. Used by PrivyKMSClient.GetPublicKey
// (privy_kms_client.go) to resolve a slot key's own already-created
// wallet -- never to create one; wallet creation for a slot key is a
// deliberate, human-run, out-of-band operational step (see
// privy_kms_client.go's own top-of-file doc comment). Rejects any
// chain_type other than "tron": only tron-typed wallets support
// raw_sign and report a public_key at all (both confirmed empirically
// this session) -- calling this against the wrong wallet type is a real
// configuration error, not something to coerce.
func (c *privyClient) getTronWallet(ctx context.Context, walletID string) ([33]byte, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/wallets/"+walletID, nil, "")
	if err != nil {
		return [33]byte{}, err
	}
	if status < 200 || status >= 300 {
		return [33]byte{}, decodePrivyAPIError(status, body)
	}

	var wallet privyWalletResponse
	if err := json.Unmarshal(body, &wallet); err != nil {
		return [33]byte{}, fmt.Errorf("%w: decoding wallet fetch response: %v", ErrPrivyMalformedResponse, err)
	}
	if wallet.ChainType != "tron" {
		return [33]byte{}, fmt.Errorf("kmssign: privy: wallet %s has chain_type %q, want \"tron\"", walletID, wallet.ChainType)
	}
	if wallet.PublicKey == "" {
		return [33]byte{}, fmt.Errorf("%w: wallet fetch response missing public_key", ErrPrivyMalformedResponse)
	}
	pubBytes, err := hex.DecodeString(trimHexPrefix(wallet.PublicKey))
	if err != nil || len(pubBytes) != 33 {
		return [33]byte{}, fmt.Errorf("%w: public_key %q is not a 33-byte compressed hex key", ErrPrivyMalformedResponse, wallet.PublicKey)
	}
	var pubKey [33]byte
	copy(pubKey[:], pubBytes)
	return pubKey, nil
}

// rawSignTron calls POST /wallets/{id}/raw_sign over digest -- the
// fields MUST be nested inside a "params" object (a flat body 400s,
// confirmed live). Returns the raw (r, s) -- exactly 64 bytes, never
// DER, never a trailing recovery byte (confirmed live); the caller
// (privytron.go's own Sign) is responsible for recovery-id matching via
// finishRecoverableSignatureRS.
func (c *privyClient) rawSignTron(ctx context.Context, walletID string, digest [32]byte, idempotencyKey string) (rBytes, sBytes [32]byte, err error) {
	status, body, err := c.do(ctx, http.MethodPost, "/wallets/"+walletID+"/raw_sign", map[string]any{
		"params": map[string]string{
			"hash": "0x" + hex.EncodeToString(digest[:]),
		},
	}, idempotencyKey)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	if status < 200 || status >= 300 {
		return [32]byte{}, [32]byte{}, decodePrivyAPIError(status, body)
	}

	var resp struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("%w: decoding raw_sign response: %v", ErrPrivyMalformedResponse, err)
	}
	sigBytes, err := hex.DecodeString(trimHexPrefix(resp.Data.Signature))
	if err != nil || len(sigBytes) != 64 {
		return [32]byte{}, [32]byte{}, fmt.Errorf("%w: raw_sign signature %q is not exactly 64 bytes of hex", ErrPrivyMalformedResponse, resp.Data.Signature)
	}
	copy(rBytes[:], sigBytes[0:32])
	copy(sBytes[:], sigBytes[32:64])
	return rBytes, sBytes, nil
}

// createEVMWallet calls POST /wallets with chain_type=ethereum, minting
// a real Privy wallet -- a real, harmless, effectively-zero-cost API
// call, same as createTronWallet. Returns ONLY (walletID, address, err),
// no pubKey: unlike a tron-typed wallet, an ethereum-typed wallet's own
// creation response never reports a public_key field at all (confirmed
// empirically by cmd/privy-probe, both at creation and via a separate
// GET /wallets/{id} call) -- privybsc.go's own Provision recovers the
// public key separately, via one throwaway secp256k1SignEVM call, rather
// than expecting one here.
func (c *privyClient) createEVMWallet(ctx context.Context, externalID, idempotencyKey string) (walletID, address string, err error) {
	status, body, err := c.do(ctx, http.MethodPost, "/wallets", map[string]string{
		"chain_type":  "ethereum",
		"external_id": externalID,
	}, idempotencyKey)
	if err != nil {
		return "", "", err
	}
	if status < 200 || status >= 300 {
		return "", "", decodePrivyAPIError(status, body)
	}

	var wallet privyWalletResponse
	if err := json.Unmarshal(body, &wallet); err != nil {
		return "", "", fmt.Errorf("%w: decoding wallet creation response: %v", ErrPrivyMalformedResponse, err)
	}
	if wallet.ID == "" || wallet.Address == "" {
		return "", "", fmt.Errorf("%w: wallet creation response missing id or address", ErrPrivyMalformedResponse)
	}
	return wallet.ID, wallet.Address, nil
}

// secp256k1SignEVM calls POST /wallets/{id}/rpc with method=secp256k1_sign
// -- the chain-scoped signing method ethereum-typed wallets require,
// confirmed live by cmd/privy-probe after raw_sign (TRON's own signing
// endpoint) was confirmed to reject ethereum wallets outright: HTTP 400,
// "ethereum wallets are not supported for this low-level signature
// endpoint." Returns the first 64 bytes (r, s) of what is confirmed
// EXACTLY 65 bytes total -- r||s||v, a trailing recovery byte raw_sign
// never includes. That byte is deliberately discarded here, never
// parsed: every caller of this method already independently
// brute-forces the recovery code (finishRecoverableSignatureRS,
// recoverPubKeyMatchingAddress) rather than trusting a vendor-reported
// v, the same "verify, don't trust" posture this whole file takes with
// every other field Privy reports.
func (c *privyClient) secp256k1SignEVM(ctx context.Context, walletID string, digest [32]byte, idempotencyKey string) (rBytes, sBytes [32]byte, err error) {
	status, body, err := c.do(ctx, http.MethodPost, "/wallets/"+walletID+"/rpc", map[string]any{
		"method": "secp256k1_sign",
		"params": map[string]string{
			"hash": "0x" + hex.EncodeToString(digest[:]),
		},
	}, idempotencyKey)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	if status < 200 || status >= 300 {
		return [32]byte{}, [32]byte{}, decodePrivyAPIError(status, body)
	}

	var resp struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("%w: decoding secp256k1_sign response: %v", ErrPrivyMalformedResponse, err)
	}
	sigBytes, err := hex.DecodeString(trimHexPrefix(resp.Data.Signature))
	if err != nil || len(sigBytes) != 65 {
		return [32]byte{}, [32]byte{}, fmt.Errorf("%w: secp256k1_sign signature %q is not exactly 65 bytes of hex", ErrPrivyMalformedResponse, resp.Data.Signature)
	}
	copy(rBytes[:], sigBytes[0:32])
	copy(sBytes[:], sigBytes[32:64])
	return rBytes, sBytes, nil
}

func trimHexPrefix(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}
