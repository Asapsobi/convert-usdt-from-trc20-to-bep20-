// Package energy is relayd's only path to C4: every HTTP call this
// service makes to the energy broker (for the TRC20 forward leg's
// energy reservation) goes through here. Mirrors
// dispatcher/internal/energy's own Client verbatim (separate Go
// modules, no shared internal package) -- this client's own shape has
// no Sweep-tier-specific logic to strip, since C4's reservation
// endpoint itself is already order-scoped, not batch-scoped.
package energy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Reservation mirrors C4's own real JSON response shape for
// POST/GET .../reservations.
type Reservation struct {
	ID            int64
	ExternalID    string
	OrderID       int64
	TargetAddress string
	EnergyUnits   int64
	Tier          string
	Status        string // "PENDING" | "CONFIRMED" | "FAILED"
	Vendor        *string
	CostTRX       *string
	ConfirmedAt   *time.Time
	Deadline      time.Time
	CreatedAt     time.Time
	FastPath      *bool
}

// Client calls one real, running C4 (energy broker) instance,
// authenticating with a single bearer token.
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

// APIError is a structured error response from C4.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("energy: C4 returned %d %s: %s", e.Status, e.Code, e.Message)
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
			return 0, nil, fmt.Errorf("energy: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("energy: building request: %w", err)
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
		return 0, nil, fmt.Errorf("energy: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("energy: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

type reservationResponse struct {
	ID            int64      `json:"id"`
	ExternalID    string     `json:"external_id"`
	OrderID       int64      `json:"order_id"`
	TargetAddress string     `json:"target_address"`
	EnergyUnits   int64      `json:"energy_units"`
	Tier          string     `json:"tier"`
	Status        string     `json:"status"`
	Vendor        *string    `json:"vendor"`
	CostTRX       *string    `json:"cost_trx"`
	ConfirmedAt   *time.Time `json:"confirmed_at"`
	Deadline      time.Time  `json:"deadline"`
	CreatedAt     time.Time  `json:"created_at"`
	FastPath      *bool      `json:"fast_path"`
}

func (r reservationResponse) toReservation() Reservation {
	return Reservation{
		ID: r.ID, ExternalID: r.ExternalID, OrderID: r.OrderID, TargetAddress: r.TargetAddress,
		EnergyUnits: r.EnergyUnits, Tier: r.Tier, Status: r.Status, Vendor: r.Vendor, CostTRX: r.CostTRX,
		ConfirmedAt: r.ConfirmedAt, Deadline: r.Deadline, CreatedAt: r.CreatedAt, FastPath: r.FastPath,
	}
}

type postReservationBody struct {
	ExternalID    string    `json:"external_id"`
	TargetAddress string    `json:"target_address"`
	EnergyUnits   int64     `json:"energy_units"`
	Tier          string    `json:"tier"`
	Deadline      time.Time `json:"deadline"`
}

// Reserve calls POST /v1/reservations. C4's own Create is synchronous --
// it never returns while still actually PENDING.
func (c *Client) Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (Reservation, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/reservations", idempotencyKey, postReservationBody{
		ExternalID: externalID, TargetAddress: targetAddress, EnergyUnits: units, Tier: tier, Deadline: deadline,
	})
	if err != nil {
		return Reservation{}, err
	}
	if status != http.StatusCreated {
		return Reservation{}, decodeAPIError(status, body)
	}
	var resp reservationResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Reservation{}, fmt.Errorf("energy: decoding Reserve response: %w", err)
	}
	return resp.toReservation(), nil
}

// ErrReservationTimeout is Poll's own result when deadline elapses
// before the reservation reaches a terminal status.
var ErrReservationTimeout = errors.New("energy: polling for reservation status timed out")

// Poll calls GET /v1/reservations/{id} until CONFIRMED, FAILED, or
// deadline elapses -- exists for the rare case a caller only has a
// reservation id (e.g. after a crash-and-resume).
func (c *Client) Poll(ctx context.Context, reservationID int64, deadline time.Time) (Reservation, error) {
	const pollInterval = 500 * time.Millisecond
	for {
		res, err := c.get(ctx, reservationID)
		if err != nil {
			return Reservation{}, err
		}
		if res.Status == "CONFIRMED" || res.Status == "FAILED" {
			return res, nil
		}
		if time.Now().After(deadline) {
			return Reservation{}, ErrReservationTimeout
		}
		select {
		case <-ctx.Done():
			return Reservation{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (c *Client) get(ctx context.Context, reservationID int64) (Reservation, error) {
	status, body, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/reservations/%d", reservationID), "", nil)
	if err != nil {
		return Reservation{}, err
	}
	if status != http.StatusOK {
		return Reservation{}, decodeAPIError(status, body)
	}
	var resp reservationResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Reservation{}, fmt.Errorf("energy: decoding Poll response: %w", err)
	}
	return resp.toReservation(), nil
}
