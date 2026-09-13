// Package ledgerclient is tronwatcher's only path to C1: every HTTP
// call this service makes to the ledger core goes through here.
// Mirrors depositwatcher/internal/ledgerclient's own contract closely
// (separate Go modules, no shared internal package, same convention as
// every other service here) -- the one real difference is which
// account the deposit is credited into: a relay suspense account
// (asset:relay:leg:<order_id>, per docs/02-architecture/
// model-f-relay-architecture.md §5), not a treasury account, since this
// service watches TRON deposits for the zero-float relay product line,
// not the pre-funded corridor.
package ledgerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"tronwatcher/internal/finality"
	"tronwatcher/internal/money"
)

// Client calls one C1 (ledger) instance, authenticating with a single
// bearer token -- service-to-service auth, per C1's own AuthConfig.
type Client struct {
	baseURL string
	token   string
	http    *http.Client

	// Metrics is optional -- nil means no metrics are recorded, never a
	// panic.
	Metrics MetricsRecorder
}

// MetricsRecorder mirrors depositwatcher/internal/ledgerclient's own
// interface.
type MetricsRecorder interface {
	ReportedToLedger(resultCode string)
}

func (c *Client) recordReport(resultCode string) {
	if c.Metrics != nil {
		c.Metrics.ReportedToLedger(resultCode)
	}
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

// Order is the subset of C1's order resource this client actually
// reads.
type Order struct {
	ExternalID string `json:"external_id"`
	CustomerID string `json:"customer_id"`
	State      string `json:"state"`
	AmountIn   string `json:"amount_in"` // decimal string, always USDT_TRC20 -- see money.ParseDecimal
	Version    int32  `json:"version"`
}

// QuotedAmount returns externalID's order's quoted amount_in as this
// service's own money.Amount.
func (c *Client) QuotedAmount(ctx context.Context, externalID string) (money.Amount, error) {
	order, err := c.GetOrder(ctx, externalID)
	if err != nil {
		return 0, fmt.Errorf("ledgerclient: quoted amount for %s: %w", externalID, err)
	}
	amount, err := money.ParseDecimal(order.AmountIn)
	if err != nil {
		return 0, fmt.Errorf("ledgerclient: quoted amount for %s: parsing %q: %w", externalID, order.AmountIn, err)
	}
	return amount, nil
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

// APIError is a structured error response from C1.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ledgerclient: C1 returned %d %s: %s", e.Status, e.Code, e.Message)
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
			return 0, nil, fmt.Errorf("ledgerclient: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("ledgerclient: building request: %w", err)
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
		return 0, nil, fmt.Errorf("ledgerclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("ledgerclient: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

// GetOrder fetches GET /v1/orders/{externalID}.
func (c *Client) GetOrder(ctx context.Context, externalID string) (Order, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, decodeAPIError(status, body)
	}
	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding order response for %s: %w", externalID, err)
	}
	return order, nil
}

// AccountType is C1's closed set of account types, mirrored here rather
// than imported -- HTTP is the only boundary.
type AccountType string

const (
	AccountAsset     AccountType = "ASSET"
	AccountLiability AccountType = "LIABILITY"
)

type postAccountRequest struct {
	Code  string `json:"code"`
	Type  string `json:"type"`
	Asset string `json:"asset"`
}

// EnsureAccount calls POST /v1/accounts, C1's idempotent-on-code account
// creation endpoint.
func (c *Client) EnsureAccount(ctx context.Context, code string, accountType AccountType, asset string, idempotencyKey string) error {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/accounts", idempotencyKey, postAccountRequest{
		Code: code, Type: string(accountType), Asset: asset,
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return decodeAPIError(status, body)
	}
	return nil
}

// ReportDepositFinal posts the one write this service ever makes to
// credit a deposit: POST /v1/orders/{external_id}/transitions to
// funded, with the deposit-final entry inline. Unlike
// depositwatcher/internal/ledgerclient's own reportDepositFinal, this
// credits asset:relay:leg:<order_id> -- a short-lived suspense account
// relayd closes once it forwards the funds onward, per
// docs/02-architecture/model-f-relay-architecture.md §5 -- not a
// treasury account.
func (c *Client) ReportDepositFinal(ctx context.Context, candidate finality.Candidate) error {
	return c.reportDepositFinal(ctx, candidate, true)
}

func (c *Client) reportDepositFinal(ctx context.Context, candidate finality.Candidate, allowVersionRetry bool) error {
	order, err := c.GetOrder(ctx, candidate.ExternalID)
	if err != nil {
		c.recordReport("get_order_failed")
		return fmt.Errorf("ledgerclient: deposit_final for %s: fetching current version: %w", candidate.ExternalID, err)
	}

	idempotencyKey := finality.DepositFinalIdempotencyKey(candidate.TxID)
	occurredAt := candidate.BlockTimestamp.UTC().Format(time.RFC3339)
	relayLegAccount := fmt.Sprintf("asset:relay:leg:%d", candidate.OrderID)
	customerAccount := "liability:customer:" + candidate.CustomerID + ":USDT_TRC20"

	if err := c.EnsureAccount(ctx, relayLegAccount, AccountAsset, "USDT_TRC20", idempotencyKey+":ensure-relay-leg"); err != nil {
		c.recordReport("ensure_account_failed")
		return fmt.Errorf("ledgerclient: deposit_final for %s: ensuring %s exists: %w", candidate.ExternalID, relayLegAccount, err)
	}
	if err := c.EnsureAccount(ctx, customerAccount, AccountLiability, "USDT_TRC20", idempotencyKey+":ensure-customer"); err != nil {
		c.recordReport("ensure_account_failed")
		return fmt.Errorf("ledgerclient: deposit_final for %s: ensuring %s exists: %w", candidate.ExternalID, customerAccount, err)
	}

	reqBody := map[string]any{
		"to_state":         "funded",
		"expected_version": order.Version,
		"reason":           "trc20_relay_deposit_final",
		"occurred_at":      occurredAt,
		"entry": map[string]any{
			"entry_type":  "deposit_final",
			"occurred_at": occurredAt,
			"lines": []map[string]any{
				{"account_code": relayLegAccount, "asset": "USDT_TRC20", "amount": candidate.Amount.Format()},
				{"account_code": customerAccount, "asset": "USDT_TRC20", "amount": (-candidate.Amount).Format()},
			},
		},
	}
	if candidate.SenderAddress != "" {
		reqBody["sender_address"] = candidate.SenderAddress
	}

	status, body, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/orders/%s/transitions", candidate.ExternalID), idempotencyKey, reqBody)
	if err != nil {
		c.recordReport("network_error")
		return fmt.Errorf("ledgerclient: deposit_final for %s: %w", candidate.ExternalID, err)
	}
	if status == http.StatusOK {
		c.recordReport("ok")
		return nil
	}

	apiErr := decodeAPIError(status, body)
	c.recordReport(apiErr.Code)
	switch apiErr.Code {
	case "version_conflict":
		if !allowVersionRetry {
			return fmt.Errorf("ledgerclient: deposit_final for %s: version_conflict persisted after one retry: %w",
				candidate.ExternalID, apiErr)
		}
		slog.Warn("ledgerclient: deposit_final hit version_conflict, retrying once with a fresh version",
			"external_id", candidate.ExternalID)
		return c.reportDepositFinal(ctx, candidate, false)
	case "system_halted":
		slog.Info("ledgerclient: deposit_final deferred -- the ledger is halted, will retry",
			"external_id", candidate.ExternalID)
		return apiErr
	case "illegal_transition":
		return fmt.Errorf("ledgerclient: deposit_final for %s: order no longer in quoted -- hand off to orphaned-deposit capture: %w: %w: %w",
			candidate.ExternalID, finality.ErrPermanentFailure, finality.ErrOrphanedDeposit, apiErr)
	case "idempotency_conflict":
		slog.Error("ledgerclient: P1 BUG ALERT -- deposit_final idempotency_conflict: this service's own key construction produced the same key for two different payloads",
			"external_id", candidate.ExternalID, "idempotency_key", idempotencyKey)
		return fmt.Errorf("%w: %w", finality.ErrPermanentFailure, apiErr)
	default:
		return apiErr
	}
}
