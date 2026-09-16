// Package ledgerclient is C2's only path to C1: every HTTP call this
// service makes to the ledger core goes through here, built against the
// exact contract in the C2 build spec's §A -- never constructed ad hoc
// anywhere else in this codebase.
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

	"depositwatcher/internal/finality"
	"depositwatcher/internal/money"
)

// Client calls one C1 (ledger) instance, authenticating with a single
// bearer token -- service-to-service auth, per C1's own AuthConfig; there
// is no per-customer scoping here.
type Client struct {
	baseURL string
	token   string
	http    *http.Client

	// Metrics is optional -- nil means no metrics are recorded, never a
	// panic. Set directly after New; C2.9's httpapi.Metrics implements
	// this to drive reports_to_ledger_total{code}.
	Metrics MetricsRecorder
}

// MetricsRecorder is how ReportDepositFinal reports the one counter
// C2.9's build spec names that only this call site knows the moment
// of: the result code of a deposit-final report attempt. Behind an
// interface, optional, for the same reason every other pluggable
// dependency in this service is: testable without a metrics library.
type MetricsRecorder interface {
	ReportedToLedger(resultCode string)
}

func (c *Client) recordReport(resultCode string) {
	if c.Metrics != nil {
		c.Metrics.ReportedToLedger(resultCode)
	}
}

// New returns a Client for baseURL (e.g. "http://localhost:8080"),
// authenticating every call with token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Order is the subset of C1's order resource this client actually reads.
type Order struct {
	ExternalID string `json:"external_id"`
	CustomerID string `json:"customer_id"`
	State      string `json:"state"`
	AmountIn   string `json:"amount_in"` // decimal string, always USDT_BEP20 -- see money.ParseDecimal
	Version    int32  `json:"version"`
	// Tier is C1's own order tier ("DIRECT"/"STANDARD"/"SWEEP"/"RELAY",
	// ledger/internal/orders/order.go's own Tier type) -- read here only
	// to pick reportDepositFinal's own deposit account code (see that
	// method's own doc comment); nothing else in this client cares which
	// tier an order is.
	Tier string `json:"tier"`
}

// relayTierDepositAccountCode mirrors relayd/internal/orchestrate's own
// unexported relayLegAccountCode(orderID) exactly (same format string,
// same OrderID) -- duplicated, not shared (separate Go modules, no
// common internal package between depositwatcher and relayd), but MUST
// stay byte-for-byte identical: this is the one account name both C2 and
// relayd's own screened->dispatching entry have to agree on, or a RELAY
// leg's deposit lands in an account relayd never looks at.
const relayTierName = "RELAY"

func relayTierDepositAccountCode(orderID int64) string {
	return fmt.Sprintf("asset:relay:leg:%d", orderID)
}

// QuotedAmount returns externalID's order's quoted amount_in as this
// service's own money.Amount -- the one fact the candidate pipeline
// (C2.10) needs from C1 that it has no local copy of, since C2.1's
// watched_addresses deliberately stores no amount at all ("no ledger of
// money," per the build spec). Parsed via money.ParseDecimal, never a
// float at any point between C1's response and classification.
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

// APIError is a structured error response from C1, preserved so a caller
// can inspect Code without string-matching Error()'s text.
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
	_ = json.Unmarshal(body, &envelope) // best-effort; a malformed body just yields empty Code/Message
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// do sends one request and returns its status and raw body -- every
// method on Client goes through this, so request construction and error
// reading is written exactly once.
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

// GetOrder fetches GET /v1/orders/{externalID} -- used both to learn an
// order's current version before a write (ReportDepositFinal) and by
// callers that just need to read state.
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

// ReportReorg posts to /v1/orders/{externalID}/reorg -- see the C2 build
// spec's §A "Reporting a reorg": no actor field (the bearer token
// supplies it, same as every other C1 write), no occurred_at, no
// scenario flag. C1 alone decides, from the order's own current state,
// whether this is scenario A (order returns to quoted, routine, no loss)
// or scenario B (order state untouched, a real loss booked, the ledger
// now halted) -- this method only reports what happened on-chain and
// relays which scenario C1 says applied, in its own logging, at the
// right severity for each.
//
// originalEntryKey must be exactly the Idempotency-Key C2.7 used when it
// first reported this deposit final -- see DepositFinalIdempotencyKey,
// the single place that convention is defined, so this call and that one
// can never drift apart. A second call with the same originalEntryKey
// (a genuine retry, or the surrounding process restarting mid-report)
// hits C1's own already_reversed path (409): this method treats that as
// success, not an error to retry or alert on -- exactly the point of the
// idempotency key.
func (c *Client) ReportReorg(ctx context.Context, externalID, originalEntryKey string) error {
	// A DIFFERENT idempotency key from originalEntryKey -- that key
	// identifies the deposit_final entry being reversed; this one
	// identifies THIS report of that reversal, per §A's own two-key
	// convention ("watcher:reorg_report:<tx_hash>:<log_index>" against
	// "watcher:deposit_final:<tx_hash>:<log_index>"). Derived from
	// originalEntryKey's own suffix rather than threading tx_hash/log_index
	// through this method separately, since DepositFinalIdempotencyKey
	// guarantees that suffix's shape.
	idempotencyKey := "watcher:reorg_report:" + strings.TrimPrefix(originalEntryKey, finality.DepositFinalIdempotencyKeyPrefix)

	status, body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/reorg", externalID), idempotencyKey,
		map[string]string{"original_entry_key": originalEntryKey})
	if err != nil {
		return fmt.Errorf("ledgerclient: reorg report for %s: %w", externalID, err)
	}

	if status == http.StatusConflict {
		if apiErr := decodeAPIError(status, body); apiErr.Code == "already_reversed" {
			slog.Info("ledgerclient: reorg already reported (idempotent replay, C1's already_reversed path)",
				"external_id", externalID, "original_entry_key", originalEntryKey)
			return nil
		}
	}
	if status != http.StatusOK {
		return decodeAPIError(status, body)
	}

	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		return fmt.Errorf("ledgerclient: decoding reorg report response for %s: %w", externalID, err)
	}

	if order.State == "quoted" {
		slog.Info("ledgerclient: post-final reorg reported -- scenario A (routine, order returned to quoted, no loss)",
			"external_id", externalID, "original_entry_key", originalEntryKey)
	} else {
		slog.Error("ledgerclient: post-final reorg reported -- SCENARIO B: a real loss was booked and the ledger is now halted",
			"external_id", externalID, "original_entry_key", originalEntryKey, "order_state", order.State)
	}
	return nil
}

// AccountType is C1's closed set of account types, mirrored here rather
// than imported (C2 has no import path into the ledger module -- HTTP is
// the only boundary). Matches dispatcher/internal/ledgerclient's own
// identical mirror.
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
// creation endpoint (added for C5, real gap found and closed here too --
// see this method's own call site in reportDepositFinal). Safe to call
// on every deposit report, not just the first one for a given customer:
// a second call with the same code is a no-op read of the account as it
// actually exists.
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

// ReportDepositFinal posts the one write C2 ever makes to credit a
// deposit -- POST /v1/orders/{external_id}/transitions to funded, with
// the E1 entry inline, exactly the shape §A specifies. The
// Idempotency-Key header is set to the same key as the entry's own
// idempotency_key -- belt and suspenders, matching C1.8's rule that
// every write handler requires the header regardless of whether the
// operation underneath has its own idempotency semantics.
//
// sender_address rides alongside entry, sibling to it per §A of
// docs/03-build/c3-screening-build-prompts.md (the ledger repo's own
// build-prompts doc): the one hop of C2->C1->C3's sender-address relay
// this method is responsible for. C1 only accepts it on a transition
// into funded, which this call always is, and only ever writes it once
// -- a second, later quoted->funded transition for the same order (a
// reorg round-trip) legitimately overwrites it with whatever sender
// funded it that time.
//
// expected_version is fetched fresh via GetOrder rather than threaded in
// by candidate: the version at the moment this method actually calls C1
// is the only one that matters, and a value carried on candidate since
// whenever it finalized could already be stale.
//
// Error handling, per this chunk's own build spec:
//   - 409 version_conflict: a legitimate race (something else touched the
//     order between the GetOrder above and this POST) -- refetched and
//     retried exactly once with the fresh version, not treated as a
//     failure at all unless the retry ALSO conflicts.
//   - 423 system_halted: C1 is deliberately refusing money-moving writes
//     right now, not a problem with this specific report. Returned as a
//     plain (retryable) error -- finality.Tracker's own tick cadence
//     (C2.5) already is the "retry on an interval" mechanism this needs,
//     so no separate backoff timer is built here.
//   - 409 illegal_transition: the order left quoted before this call
//     reached C1 (e.g. it expired first). Wrapped in both
//     finality.ErrPermanentFailure and finality.ErrOrphanedDeposit --
//     retrying this exact call can never succeed, and finality.Tracker
//     routes the more specific sentinel to HandleUnreportable (C2.8),
//     which records it for manual reconciliation rather than just
//     logging and dropping it.
//   - 409 idempotency_conflict: structurally impossible if this method's
//     own key construction is correct (the same tx_hash:log_index always
//     produces the same payload). Also wrapped in
//     finality.ErrPermanentFailure and logged as a P1 alert -- retrying
//     an actual bug in a loop is strictly worse than surfacing it once,
//     loudly.
func (c *Client) ReportDepositFinal(ctx context.Context, candidate finality.Candidate) error {
	return c.reportDepositFinal(ctx, candidate, true)
}

func (c *Client) reportDepositFinal(ctx context.Context, candidate finality.Candidate, allowVersionRetry bool) error {
	order, err := c.GetOrder(ctx, candidate.ExternalID)
	if err != nil {
		c.recordReport("get_order_failed")
		return fmt.Errorf("ledgerclient: deposit_final for %s: fetching current version: %w", candidate.ExternalID, err)
	}

	idempotencyKey := finality.DepositFinalIdempotencyKey(candidate.TxHash, candidate.LogIndex)
	occurredAt := candidate.BlockTime.UTC().Format(time.RFC3339)
	// RELAY-tier orders (Model F, relayd) post into relayd's own suspense
	// account instead of Model D's per-order deposit account -- relayd
	// has no pre-funded treasury to draw from later (per
	// docs/01-strategy/model-f-relay-findings.md's own "never holds a net
	// position"), so its screened->dispatching entry must debit the EXACT
	// account this deposit landed in, not a Model-D-shaped one it never
	// looks at. Every other tier's behavior is byte-for-byte unchanged.
	var depositAccount string
	if order.Tier == relayTierName {
		depositAccount = relayTierDepositAccountCode(candidate.OrderID)
	} else {
		depositAccount = fmt.Sprintf("asset:bsc:deposit:%d", candidate.OrderID)
	}
	// :USDT_BEP20 suffix required -- matches the account-code convention
	// every other component in this project uses (e.g. dispatcher's own
	// customerAccountCode(customerID, asset)); C5's own E2 entry later
	// nets against this exact code when converting to TRC20, so a
	// mismatch here would silently create TWO different accounts for the
	// same customer instead of one. Found live, wiring the MVP proof run:
	// this was missing the suffix entirely.
	customerAccount := "liability:customer:" + candidate.CustomerID + ":USDT_BEP20"

	// Ensure both accounts this entry references actually exist before
	// posting it -- C1 does not auto-vivify an account on first journal
	// reference (the same real gap C5's own EnterDispatching hit and
	// fixed via EnsureAccount; C2 never got the equivalent call). The
	// deposit account is per-order and always new; the customer account
	// may already exist from a prior order for the same customer, in
	// which case this is a no-op read.
	if err := c.EnsureAccount(ctx, depositAccount, AccountAsset, "USDT_BEP20", idempotencyKey+":ensure-deposit"); err != nil {
		c.recordReport("ensure_account_failed")
		return fmt.Errorf("ledgerclient: deposit_final for %s: ensuring %s exists: %w", candidate.ExternalID, depositAccount, err)
	}
	if err := c.EnsureAccount(ctx, customerAccount, AccountLiability, "USDT_BEP20", idempotencyKey+":ensure-customer"); err != nil {
		c.recordReport("ensure_account_failed")
		return fmt.Errorf("ledgerclient: deposit_final for %s: ensuring %s exists: %w", candidate.ExternalID, customerAccount, err)
	}

	reqBody := map[string]any{
		"to_state":         "funded",
		"expected_version": order.Version,
		"reason":           "bep20_deposit_final",
		"occurred_at":      occurredAt,
		"entry": map[string]any{
			"entry_type":  "deposit_final",
			"occurred_at": occurredAt,
			"lines": []map[string]any{
				{"account_code": depositAccount, "asset": "USDT_BEP20", "amount": candidate.Amount.Format()},
				{"account_code": customerAccount, "asset": "USDT_BEP20", "amount": (-candidate.Amount).Format()},
			},
		},
	}

	// Omitted entirely, never sent as an explicit "", when this
	// candidate somehow carries no sender (should not happen for a real
	// Transfer log -- every log candidates.processLog builds sets this --
	// but a test fixture or a future caller might): C1's own
	// TransitionParams rejects a present-but-empty sender_address rather
	// than silently ignoring it, so sending "" would turn a harmless
	// omission into a hard 400 on every single deposit report.
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
		return fmt.Errorf("ledgerclient: deposit_final for %s: order no longer in quoted -- hand off to C2.8: %w: %w: %w",
			candidate.ExternalID, finality.ErrPermanentFailure, finality.ErrOrphanedDeposit, apiErr)
	case "idempotency_conflict":
		slog.Error("ledgerclient: P1 BUG ALERT -- deposit_final idempotency_conflict: this service's own key construction produced the same key for two different payloads",
			"external_id", candidate.ExternalID, "idempotency_key", idempotencyKey)
		return fmt.Errorf("%w: %w", finality.ErrPermanentFailure, apiErr)
	default:
		return apiErr
	}
}
