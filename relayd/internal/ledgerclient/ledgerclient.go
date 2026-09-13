// Package ledgerclient is relayd's only path to C1: every HTTP call
// this service makes to the ledger core goes through here. Mirrors
// dispatcher/internal/ledgerclient's own Client/do/APIError/classify
// skeleton (separate Go modules, no shared internal package, same
// convention as every other service here).
//
// Unlike every sibling ledgerclient, this one's Order carries assets
// that can genuinely differ per order (RELAY orders are bidirectional --
// see docs/02-architecture/model-f-relay-architecture.md §5 and
// ledger/internal/orders/store.go's own relay_direction column) -- so
// AmountIn/AmountOut are parsed against C1's own explicit
// amount_in_asset/amount_out_asset response fields, never an assumed
// fixed pairing the way dispatcher's/gateway's own clients can get away
// with.
package ledgerclient

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

	"relayd/internal/money"
)

// Client calls one C1 (ledger) instance, authenticating with a single
// bearer token.
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

// Sentinel errors matched against APIError.Code -- C1's own real,
// stable error codes (ledger/internal/httpapi/errors.go).
var (
	ErrOrderNotFound     = errors.New("ledgerclient: no such order")
	ErrIllegalTransition = errors.New("ledgerclient: no such transition is legal from the order's current state")
	ErrVersionConflict   = errors.New("ledgerclient: expected_version did not match the order's current version")
	ErrSystemHalted      = errors.New("ledgerclient: the ledger is halted")
	ErrAlreadyReversed   = errors.New("ledgerclient: that entry has already been reversed")
	ErrEntryNotFound     = errors.New("ledgerclient: no such journal entry")
)

var codeToSentinel = map[string]error{
	"order_not_found":    ErrOrderNotFound,
	"illegal_transition": ErrIllegalTransition,
	"version_conflict":   ErrVersionConflict,
	"system_halted":      ErrSystemHalted,
	"already_reversed":   ErrAlreadyReversed,
	"entry_not_found":    ErrEntryNotFound,
}

// classify wraps apiErr with whichever sentinel its Code matches.
func classify(apiErr *APIError) error {
	if sentinel, ok := codeToSentinel[apiErr.Code]; ok {
		return fmt.Errorf("%w: %w", sentinel, apiErr)
	}
	return apiErr
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

// Order is the subset of C1's order resource this client reads.
type Order struct {
	ID               int64
	ExternalID       string
	CustomerID       string
	Tier             string
	State            string
	AmountIn         money.Amount
	AmountOut        money.Amount
	FeeUnits         money.Amount
	NetworkFeeUnits  money.Amount
	RecipientAddress string
	Version          int32
}

type orderResponse struct {
	ID               int64  `json:"id"`
	ExternalID       string `json:"external_id"`
	CustomerID       string `json:"customer_id"`
	Tier             string `json:"tier"`
	State            string `json:"state"`
	AmountIn         string `json:"amount_in"`
	AmountOut        string `json:"amount_out"`
	FeeUnits         string `json:"fee_units"`
	NetworkFeeUnits  string `json:"network_fee_units"`
	RecipientAddress string `json:"recipient_address"`
	Version          int32  `json:"version"`
	AmountInAsset    string `json:"amount_in_asset"`
	AmountOutAsset   string `json:"amount_out_asset"`
}

func (r orderResponse) toOrder() (Order, error) {
	inAsset := money.Asset(r.AmountInAsset)
	outAsset := money.Asset(r.AmountOutAsset)

	amountIn, err := money.ParseDecimal(r.AmountIn, inAsset)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: parsing amount_in: %w", err)
	}
	amountOut, err := money.ParseDecimal(r.AmountOut, outAsset)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: parsing amount_out: %w", err)
	}
	// fee_units/network_fee_units are in amount_in's asset for RELAY
	// orders, not amount_out's -- C1 withholds relayd's own commission
	// from the deposited (in) asset before forwarding, since the
	// upstream vendor pays the customer's own destination wallet
	// directly with 100% of its own conversion output. See
	// ledger/internal/orders/store.go's CreateParams.validate own doc
	// comment for the full reasoning (found via this exact client
	// hitting a real ledgerd in internal/orchestrate's own integration
	// test, not assumed).
	feeAsset := outAsset
	if r.Tier == "RELAY" {
		feeAsset = inAsset
	}
	feeUnits, err := money.ParseDecimal(r.FeeUnits, feeAsset)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: parsing fee_units: %w", err)
	}
	networkFeeUnits, err := money.ParseDecimal(r.NetworkFeeUnits, feeAsset)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: parsing network_fee_units: %w", err)
	}
	return Order{
		ID: r.ID, ExternalID: r.ExternalID, CustomerID: r.CustomerID, Tier: r.Tier, State: r.State,
		AmountIn: amountIn, AmountOut: amountOut, FeeUnits: feeUnits, NetworkFeeUnits: networkFeeUnits,
		RecipientAddress: r.RecipientAddress, Version: r.Version,
	}, nil
}

// GetOrder fetches GET /v1/orders/{externalID}.
func (c *Client) GetOrder(ctx context.Context, externalID string) (Order, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, classify(decodeAPIError(status, body))
	}
	var resp orderResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding order response for %s: %w", externalID, err)
	}
	return resp.toOrder()
}

type postOrderRequest struct {
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
	AmountInAsset    string    `json:"amount_in_asset"`
	AmountOutAsset   string    `json:"amount_out_asset"`
}

// CreateOrder calls POST /v1/orders with tier=RELAY, always sending
// amount_in_asset/amount_out_asset explicitly -- C1's own
// ledger/internal/httpapi.postOrder requires both for tier=RELAY, per
// that handler's own doc comment: RELAY is bidirectional, so there is
// no fixed convention for C1 to assume the way there is for
// DIRECT/STANDARD/SWEEP. idempotencyKey should be stable per relay leg
// (e.g. derived from the leg's own external_id), so a retried
// CreateOrder call after an uncertain outcome is safe.
func (c *Client) CreateOrder(ctx context.Context, externalID, customerID string, amountIn, amountOut, feeUnits, networkFeeUnits money.Amount,
	recipientAddress string, quotedAt, quoteExpiresAt time.Time, idempotencyKey string) (Order, error) {
	amountInStr, err := money.Format(amountIn)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: formatting amount_in: %w", err)
	}
	amountOutStr, err := money.Format(amountOut)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: formatting amount_out: %w", err)
	}
	feeUnitsStr, err := money.Format(feeUnits)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: formatting fee_units: %w", err)
	}
	networkFeeUnitsStr, err := money.Format(networkFeeUnits)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: formatting network_fee_units: %w", err)
	}

	body := postOrderRequest{
		ExternalID: externalID, CustomerID: customerID, Tier: "RELAY",
		AmountIn: amountInStr, AmountOut: amountOutStr, FeeUnits: feeUnitsStr, NetworkFeeUnits: networkFeeUnitsStr,
		RecipientAddress: recipientAddress, QuotedAt: quotedAt, QuoteExpiresAt: quoteExpiresAt,
		AmountInAsset: string(amountIn.Asset), AmountOutAsset: string(amountOut.Asset),
	}
	status, respBody, err := c.do(ctx, http.MethodPost, "/v1/orders", idempotencyKey, body)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusCreated {
		return Order{}, classify(decodeAPIError(status, respBody))
	}
	var resp orderResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding create-order response for %s: %w", externalID, err)
	}
	return resp.toOrder()
}

// AccountType is C1's closed set of account types, mirrored here rather
// than imported -- HTTP is the only boundary.
type AccountType string

const (
	AccountAsset     AccountType = "ASSET"
	AccountLiability AccountType = "LIABILITY"
	AccountRevenue   AccountType = "REVENUE"
)

type postAccountRequest struct {
	Code  string `json:"code"`
	Type  string `json:"type"`
	Asset string `json:"asset"`
}

// EnsureAccount calls POST /v1/accounts, C1's idempotent-on-code account
// creation endpoint -- the same pattern C5's slot accounts and
// tronwatcher's own relay-leg suspense accounts already use.
func (c *Client) EnsureAccount(ctx context.Context, code string, accountType AccountType, asset string, idempotencyKey string) error {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/accounts", idempotencyKey, postAccountRequest{
		Code: code, Type: string(accountType), Asset: asset,
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return classify(decodeAPIError(status, body))
	}
	return nil
}
