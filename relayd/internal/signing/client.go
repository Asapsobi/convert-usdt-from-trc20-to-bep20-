// Package signing is the only package in this module that ever calls
// S1's SigningService -- no private key, seed, or anything key-derived
// appears anywhere in this module's memory or logs; every signature
// comes from a real S1 instance over HTTP. See dependency_test.go, which
// enforces that mechanically, mirroring
// dispatcher/internal/signing's own identical boundary.
package signing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Status mirrors s1/internal/requests.Status -- duplicated, not shared
// (separate Go modules, no common internal package).
type Status string

const (
	StatusPending  Status = "PENDING"
	StatusSigned   Status = "SIGNED"
	StatusRejected Status = "REJECTED"
)

// SigningRequest mirrors S1's own JSON response shape for a signing
// request.
type SigningRequest struct {
	ID        int64
	Status    Status
	SignedTx  [65]byte // valid only when Status == StatusSigned
	CreatedAt time.Time
}

// Client calls one real, running S1 instance, authenticating with a
// single bearer token (S1's own C5-scoped auth).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for baseURL, authenticating every call with
// token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

// APIError is a structured error response from S1.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("signing: S1 returned %d %s: %s", e.Status, e.Code, e.Message)
}

func decodeAPIError(status int, body []byte) *APIError {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(body, &envelope)
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

func (c *Client) do(ctx context.Context, method, path string, body any) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("signing: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("signing: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("signing: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("signing: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

type signingRequestResponse struct {
	ID        int64     `json:"id"`
	Status    string    `json:"status"`
	SignedTx  *string   `json:"signed_tx"`
	CreatedAt time.Time `json:"created_at"`
}

func (r signingRequestResponse) toSigningRequest() (SigningRequest, error) {
	out := SigningRequest{ID: r.ID, Status: Status(r.Status), CreatedAt: r.CreatedAt}
	if r.SignedTx != nil {
		b, err := hex.DecodeString(*r.SignedTx)
		if err != nil {
			return SigningRequest{}, fmt.Errorf("signing: decoding signed_tx hex: %w", err)
		}
		if len(b) != 65 {
			return SigningRequest{}, fmt.Errorf("signing: signed_tx is %d bytes, want 65", len(b))
		}
		copy(out.SignedTx[:], b)
	}
	return out, nil
}

type postSigningRequestBody struct {
	SlotID         int     `json:"slot_id"`
	Digest         string  `json:"digest"`
	EstimatedUSD   float64 `json:"estimated_usd"`
	IdempotencyKey string  `json:"idempotency_key"`
}

// RequestSignature calls POST /v1/signing-requests -- request/poll,
// never blocking past S1's own immediate response (SIGNED for an
// auto-approved request, PENDING otherwise), matching
// dispatcher/internal/signing's identical discipline: relayd's own
// orchestrate loop returns a "still pending" sentinel and lets the
// outer tick retry, never blocks inside this call.
func (c *Client) RequestSignature(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (SigningRequest, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/signing-requests", postSigningRequestBody{
		SlotID: slotID, Digest: hex.EncodeToString(digest[:]), EstimatedUSD: estimatedUSD, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return SigningRequest{}, err
	}
	if status != http.StatusCreated {
		return SigningRequest{}, decodeAPIError(status, body)
	}
	var resp signingRequestResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return SigningRequest{}, fmt.Errorf("signing: decoding RequestSignature response: %w", err)
	}
	return resp.toSigningRequest()
}

// GetSignature calls GET /v1/signing-requests/{id}.
func (c *Client) GetSignature(ctx context.Context, id int64) (SigningRequest, error) {
	status, body, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/signing-requests/%d", id), nil)
	if err != nil {
		return SigningRequest{}, err
	}
	if status != http.StatusOK {
		return SigningRequest{}, decodeAPIError(status, body)
	}
	var resp signingRequestResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return SigningRequest{}, fmt.Errorf("signing: decoding GetSignature response: %w", err)
	}
	return resp.toSigningRequest()
}

// SlotAddress calls GET /v1/slots/{id}/address.
func (c *Client) SlotAddress(ctx context.Context, slotID int) (string, error) {
	status, body, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/slots/%d/address", slotID), nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", decodeAPIError(status, body)
	}
	var resp struct {
		TronAddress string `json:"tron_address"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("signing: decoding SlotAddress response: %w", err)
	}
	return resp.TronAddress, nil
}

// EVMAddress calls GET /v1/slots/{id}/evm-address -- the SAME slot key
// SlotAddress reports a TRON address for, in its EVM (BSC included)
// format. Needed for relayd's own BEP20->TRC20 relay direction (see
// s1/internal/slots/evm.go's own doc comment).
func (c *Client) EVMAddress(ctx context.Context, slotID int) (string, error) {
	status, body, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/slots/%d/evm-address", slotID), nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", decodeAPIError(status, body)
	}
	var resp struct {
		EVMAddress string `json:"evm_address"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("signing: decoding EVMAddress response: %w", err)
	}
	return resp.EVMAddress, nil
}
