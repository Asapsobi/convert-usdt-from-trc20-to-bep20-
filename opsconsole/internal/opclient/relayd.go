package opclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RelaydClient calls one relayd (Model F's own front door + driving
// loop) instance.
type RelaydClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewRelaydClient returns a RelaydClient for baseURL, authenticating
// every call with token.
func NewRelaydClient(baseURL, token string) *RelaydClient {
	return &RelaydClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

// Healthz reports whether relayd's own /healthz responds 200.
func (c *RelaydClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "relayd", c.baseURL)
}

// RelayLeg is relayd's own GET /v1/relay-legs row shape
// (relayd/internal/httpapi/relay_legs_handlers.go's own
// relayLegSummary) -- this service's own local data only, not
// cross-referenced against C1's order state (see that handler's own doc
// comment on why: one row per leg, not one extra HTTP round trip to C1
// per row).
type RelayLeg struct {
	ExternalID           string  `json:"external_id"`
	OrderID              int64   `json:"order_id"`
	Direction            string  `json:"direction"`
	Status               string  `json:"status"`
	CustomerID           string  `json:"customer_id"`
	DestinationAddress   string  `json:"destination_address"`
	AmountIn             string  `json:"amount_in"`
	AmountOutExpected    string  `json:"amount_out_expected"`
	AmountOutActual      *string `json:"amount_out_actual,omitempty"`
	UpstreamProviderName *string `json:"upstream_provider_name,omitempty"`
	UpstreamOrderID      *string `json:"upstream_order_id,omitempty"`
	ForwardTxID          *string `json:"forward_tx_id,omitempty"`
	RefundTxID           *string `json:"refund_tx_id,omitempty"`
	CreatedAt            string  `json:"created_at"`
	UpdatedAt            string  `json:"updated_at"`
}

// ListRelayLegs calls relayd's own GET /v1/relay-legs?status=. An empty
// status lists every leg regardless of status.
func (c *RelaydClient) ListRelayLegs(ctx context.Context, status string) ([]RelayLeg, error) {
	u := c.baseURL + "/v1/relay-legs"
	if status != "" {
		u += "?status=" + url.QueryEscape(status)
	}
	var out struct {
		Legs []RelayLeg `json:"legs"`
	}
	err := do(ctx, c.http, "relayd", c.token, http.MethodGet, u, nil, &out)
	return out.Legs, err
}
