package ledgerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"relayd/internal/money"
)

type postEntryRequest struct {
	EntryType  string             `json:"entry_type"`
	OccurredAt time.Time          `json:"occurred_at"`
	Lines      []entryLineRequest `json:"lines"`
	Metadata   map[string]any     `json:"metadata,omitempty"`
}

// PostEntry calls POST /v1/entries: a journal entry that belongs to no
// order transition (a sweep moving profit to the treasury). A retried
// call with the same idempotencyKey replays the same entry. Returns the
// entry's id.
func (c *Client) PostEntry(ctx context.Context, entryType string, occurredAt time.Time, lines []EntryLine, metadata map[string]any, idempotencyKey string) (int64, error) {
	reqLines := make([]entryLineRequest, len(lines))
	for i, l := range lines {
		amountStr, err := money.Format(l.Amount)
		if err != nil {
			return 0, fmt.Errorf("ledgerclient: formatting entry line %d: %w", i, err)
		}
		reqLines[i] = entryLineRequest{AccountCode: l.AccountCode, Asset: l.Asset, Amount: amountStr}
	}
	status, respBody, err := c.do(ctx, http.MethodPost, "/v1/entries", idempotencyKey, postEntryRequest{
		EntryType: entryType, OccurredAt: occurredAt, Lines: reqLines, Metadata: metadata,
	})
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return 0, classify(decodeAPIError(status, respBody))
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return 0, fmt.Errorf("ledgerclient: decoding entry response: %w", err)
	}
	return resp.ID, nil
}
