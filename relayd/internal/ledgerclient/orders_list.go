package ledgerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DefaultPollLimit mirrors dispatcher/internal/ledgerclient's own
// identical constant.
const DefaultPollLimit = 100

// OrderRef is one row of an orders list page -- just enough to decide
// whether to advance a relay leg without carrying the whole Order,
// mirroring every sibling ledgerclient's own identical OrderRef.
type OrderRef struct {
	OrderID    int64
	ExternalID string
	UpdatedAt  time.Time
}

type listOrderResp struct {
	ID         int64     `json:"id"`
	ExternalID string    `json:"external_id"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type listOrdersResp struct {
	Orders     []listOrderResp `json:"orders"`
	NextCursor string          `json:"next_cursor"`
}

// ListOrdersByState calls GET /v1/orders?state=<state>&updated_after=<cursor>,
// the same route dispatcher's own orchestrate loop polls for
// state=screened. relayd's own driving loop (internal/orchestrate) uses
// this the same way -- a live filter re-query every tick, not a
// cursor-log -- so an order left behind one tick naturally resurfaces
// the next.
func (c *Client) ListOrdersByState(ctx context.Context, state, cursor string) (refs []OrderRef, newCursor string, err error) {
	path := "/v1/orders?state=" + url.QueryEscape(state) + "&limit=" + strconv.Itoa(DefaultPollLimit)
	if cursor != "" {
		path += "&updated_after=" + url.QueryEscape(cursor)
	}

	status, body, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", classify(decodeAPIError(status, body))
	}

	var resp listOrdersResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("ledgerclient: decoding orders list response: %w", err)
	}

	refs = make([]OrderRef, len(resp.Orders))
	for i, o := range resp.Orders {
		refs[i] = OrderRef{OrderID: o.ID, ExternalID: o.ExternalID, UpdatedAt: o.UpdatedAt}
	}
	return refs, resp.NextCursor, nil
}
