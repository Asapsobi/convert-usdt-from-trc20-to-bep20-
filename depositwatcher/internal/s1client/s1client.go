// Package s1client is depositwatcher's own path to S1's real-custody BSC
// deposit-key provisioning endpoint (POST /v1/bsc-deposit-keys) --
// internal/addresses.Assign's Privy-backed alternative to deriving an
// address locally from an xpub (see that package's own Provisioner
// interface and provisioner variable). Mirrors
// tronwatcher/internal/s1client's own identical shape (itself mirroring
// relayd/internal/watcherclient's), duplicated rather than shared across
// the module boundary per this codebase's own convention.
package s1client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls one real, running S1 instance, authenticating with a
// single bearer token scoped to S1's own Provisioning auth (see
// s1/internal/httpapi/auth.go's own ProvisioningAuthConfigFromEnv) --
// never S1's C5 or approver token, which this deployment has no reason
// to hold.
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
		http:    &http.Client{Timeout: 20 * time.Second},
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
	return fmt.Sprintf("s1client: s1 returned %d %s: %s", e.Status, e.Code, e.Message)
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
			return 0, nil, fmt.Errorf("s1client: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("s1client: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("s1client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("s1client: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

type postBSCDepositKeyRequest struct {
	DepositIndex uint32 `json:"deposit_index"`
}

type bscDepositKeyResponse struct {
	DepositIndex uint32 `json:"deposit_index"`
	Address      string `json:"address"`
}

// ProvisionBSCDepositKey calls POST /v1/bsc-deposit-keys, satisfying
// depositwatcher/internal/addresses.Provisioner. Idempotent on index
// (S1's own guarantee, purely from deposit_index -- no idempotency-key
// header needed here, unlike watcherclient's own AssignAddress).
func (c *Client) ProvisionBSCDepositKey(ctx context.Context, index uint32) (string, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/bsc-deposit-keys", postBSCDepositKeyRequest{DepositIndex: index})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", decodeAPIError(status, body)
	}
	var resp bscDepositKeyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("s1client: decoding ProvisionBSCDepositKey response: %w", err)
	}
	return resp.Address, nil
}
