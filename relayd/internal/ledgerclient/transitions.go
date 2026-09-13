package ledgerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"relayd/internal/money"
)

// EntryLine is one line of an entry this client posts inline with a
// transition -- mirrors C1's own entryLineRequest exactly.
type EntryLine struct {
	AccountCode string
	Asset       string
	Amount      money.Amount
}

// TransitionWithEntry calls POST /v1/orders/{externalID}/transitions
// with an inline entry -- relayd's own screened->dispatching and
// dispatching->settled transitions both go through this one call shape,
// mirroring dispatcher/internal/ledgerclient's identical method.
// idempotencyKey becomes both the request's own Idempotency-Key header
// AND the posted entry's own idempotency key -- a retried call with the
// same key replays the identical entry, never posts a second one.
func (c *Client) TransitionWithEntry(ctx context.Context, externalID, toState string, expectedVersion int32, reason, entryType string, occurredAt time.Time, lines []EntryLine, idempotencyKey string) (Order, error) {
	reqLines := make([]entryLineRequest, len(lines))
	for i, l := range lines {
		amountStr, err := money.Format(l.Amount)
		if err != nil {
			return Order{}, fmt.Errorf("ledgerclient: formatting entry line %d: %w", i, err)
		}
		reqLines[i] = entryLineRequest{AccountCode: l.AccountCode, Asset: l.Asset, Amount: amountStr}
	}
	body := postTransitionRequest{
		ToState: toState, ExpectedVersion: expectedVersion, Reason: reason, OccurredAt: occurredAt,
		Entry: &transitionEntryRequest{EntryType: entryType, OccurredAt: occurredAt, Lines: reqLines},
	}
	return c.postTransition(ctx, externalID, idempotencyKey, body)
}

type entryLineRequest struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

type transitionEntryRequest struct {
	EntryType  string             `json:"entry_type"`
	OccurredAt time.Time          `json:"occurred_at"`
	Lines      []entryLineRequest `json:"lines"`
}

type postTransitionRequest struct {
	ToState         string                  `json:"to_state"`
	ExpectedVersion int32                   `json:"expected_version"`
	Reason          string                  `json:"reason"`
	OccurredAt      time.Time               `json:"occurred_at"`
	Entry           *transitionEntryRequest `json:"entry,omitempty"`
}

func (c *Client) postTransition(ctx context.Context, externalID, idempotencyKey string, body postTransitionRequest) (Order, error) {
	status, respBody, err := c.do(ctx, http.MethodPost, "/v1/orders/"+externalID+"/transitions", idempotencyKey, body)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, classify(decodeAPIError(status, respBody))
	}
	var resp orderResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding transition response for %s: %w", externalID, err)
	}
	return resp.toOrder()
}
