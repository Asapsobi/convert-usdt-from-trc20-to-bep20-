// Package watcherclient is relayd's path to either deposit watcher --
// depositwatcher/internal/httpapi (C2, BSC) and tronwatcher/internal/httpapi
// (C2', TRON) expose the IDENTICAL POST /v1/addresses / GET
// /v1/addresses/{order_id} contract (both mirror the same
// addressResponse shape), so one client, pointed at whichever base URL
// a relay leg's own direction needs, serves both -- no need for two
// near-duplicate packages the way an ad hoc port might produce.
package watcherclient

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

// Client calls one real, running deposit-watcher instance (either
// depositwatcher or tronwatcher), authenticating with a single bearer
// token.
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

// APIError is a structured error response from the watcher.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("watcherclient: watcher returned %d %s: %s", e.Status, e.Code, e.Message)
}

func decodeAPIError(status int, body []byte) *APIError {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(body, &envelope)
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

func (c *Client) do(ctx context.Context, method, path, idempotencyKey string, body any) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("watcherclient: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("watcherclient: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("watcherclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("watcherclient: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

// Address is the subset of the watcher's own addressResponse this
// client reads.
type Address struct {
	Address    string
	OrderID    int64
	ExternalID string
	CustomerID string
	Status     string
}

type addressResponse struct {
	Address    string `json:"address"`
	OrderID    int64  `json:"order_id"`
	ExternalID string `json:"external_id"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
}

type postAddressRequest struct {
	OrderID        int64     `json:"order_id"`
	ExternalID     string    `json:"external_id"`
	CustomerID     string    `json:"customer_id"`
	QuotedAt       time.Time `json:"quoted_at"`
	QuoteExpiresAt time.Time `json:"quote_expires_at"`
}

// AssignAddress calls POST /v1/addresses -- idempotent on order_id
// (the watcher's own guarantee), so a retried call after an uncertain
// outcome safely returns the same address rather than erroring or
// allocating a second one.
func (c *Client) AssignAddress(ctx context.Context, orderID int64, externalID, customerID string, quotedAt, quoteExpiresAt time.Time, idempotencyKey string) (Address, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/addresses", idempotencyKey, postAddressRequest{
		OrderID: orderID, ExternalID: externalID, CustomerID: customerID,
		QuotedAt: quotedAt, QuoteExpiresAt: quoteExpiresAt,
	})
	if err != nil {
		return Address{}, err
	}
	if status != http.StatusOK {
		return Address{}, decodeAPIError(status, body)
	}
	var resp addressResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Address{}, fmt.Errorf("watcherclient: decoding AssignAddress response: %w", err)
	}
	return Address{Address: resp.Address, OrderID: resp.OrderID, ExternalID: resp.ExternalID, CustomerID: resp.CustomerID, Status: resp.Status}, nil
}

// GetAddress calls GET /v1/addresses/{orderID}.
func (c *Client) GetAddress(ctx context.Context, orderID int64) (Address, error) {
	status, body, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/addresses/%d", orderID), "", nil)
	if err != nil {
		return Address{}, err
	}
	if status != http.StatusOK {
		return Address{}, decodeAPIError(status, body)
	}
	var resp addressResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Address{}, fmt.Errorf("watcherclient: decoding GetAddress response: %w", err)
	}
	return Address{Address: resp.Address, OrderID: resp.OrderID, ExternalID: resp.ExternalID, CustomerID: resp.CustomerID, Status: resp.Status}, nil
}
