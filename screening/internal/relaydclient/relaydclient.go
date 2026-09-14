// Package relaydclient is C3's only path to relayd (Model F's own
// front door + driving loop) -- one narrow call,
// internal/holds.RelayAwareRefundEntryBuilder's only dependency. Mirrors
// internal/ledgerclient's own Client shape (baseURL/token/http.Client),
// never shared with it (no common internal package between the two
// modules this pair straddles).
package relaydclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls one relayd instance, authenticating with a single bearer
// token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for baseURL, authenticating every call with
// token.
func New(baseURL, token string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

// ErrNotARelayLeg means relayd has no relay leg at all for the given
// external_id -- not a RELAY-tier order, so RELAY-aware refund-entry
// construction does not apply to it. Distinguished from every other
// error so a caller can fall back to a different owner (or a stub)
// rather than treating "not ours" as a real failure.
var ErrNotARelayLeg = errors.New("relaydclient: no relay leg exists for that external_id")

// ErrLegNotEligible means a relay leg exists but is not currently
// eligible for a refund entry -- relayd has already engaged it (past
// AWAITING_DEPOSIT), or C1's own order is not currently held. Mirrors
// relayd's own driver.ErrLegNotEligibleForRefundEntry, on this side of
// the HTTP boundary.
var ErrLegNotEligible = errors.New("relaydclient: this leg is not currently eligible for a refund entry")

// RefundEntryLine is one line of a RefundEntry.
type RefundEntryLine struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

// RefundEntry is relayd's own computed held->refunded ledger entry,
// ready to embed verbatim as C1's own "entry" transition field.
type RefundEntry struct {
	EntryType  string            `json:"entry_type"`
	OccurredAt string            `json:"occurred_at"`
	Lines      []RefundEntryLine `json:"lines"`
}

type apiErrorBody struct {
	Error string `json:"error"`
}

// GetRefundEntry calls relayd's own GET /v1/relay-legs/{externalID}/refund-entry.
func (c *Client) GetRefundEntry(ctx context.Context, externalID string) (RefundEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/relay-legs/"+externalID+"/refund-entry", nil)
	if err != nil {
		return RefundEntry{}, fmt.Errorf("relaydclient: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return RefundEntry{}, fmt.Errorf("relaydclient: GET refund-entry for %s: %w", externalID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return RefundEntry{}, fmt.Errorf("relaydclient: reading response for %s: %w", externalID, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var entry RefundEntry
		if err := json.Unmarshal(body, &entry); err != nil {
			return RefundEntry{}, fmt.Errorf("relaydclient: decoding refund entry for %s: %w", externalID, err)
		}
		return entry, nil
	case http.StatusNotFound:
		return RefundEntry{}, fmt.Errorf("%w: %s", ErrNotARelayLeg, externalID)
	case http.StatusConflict:
		return RefundEntry{}, fmt.Errorf("%w: %s", ErrLegNotEligible, externalID)
	default:
		var apiErr apiErrorBody
		_ = json.Unmarshal(body, &apiErr)
		return RefundEntry{}, fmt.Errorf("relaydclient: GET refund-entry for %s: status %d: %s", externalID, resp.StatusCode, apiErr.Error)
	}
}
