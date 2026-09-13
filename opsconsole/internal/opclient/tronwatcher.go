package opclient

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// TronwatcherClient calls one tronwatcher (C2', Model F's own TRON-side
// deposit watcher) instance.
type TronwatcherClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewTronwatcherClient returns a TronwatcherClient for baseURL,
// authenticating every call with token.
func NewTronwatcherClient(baseURL, token string) *TronwatcherClient {
	return &TronwatcherClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

// Healthz reports whether tronwatcher's own /healthz responds 200.
func (c *TronwatcherClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "tronwatcher", c.baseURL)
}

// TronwatcherInvariants is tronwatcher's own GET /v1/system/invariants
// response shape (tronwatcher/internal/httpapi/system_handlers.go's own
// invariantsResponse). Unlike C2, there is no cursor-lag figure here --
// tronwatcher scans per-address via TronGrid, not a block cursor, so
// that concept has no equivalent here (see that handler's own doc
// comment).
type TronwatcherInvariants struct {
	PendingFinalityCount             *int     `json:"pending_finality_count,omitempty"`
	OldestPendingCandidateAgeSeconds *float64 `json:"oldest_pending_candidate_age_seconds,omitempty"`
}

// GetInvariants reads tronwatcher's system invariants.
func (c *TronwatcherClient) GetInvariants(ctx context.Context) (TronwatcherInvariants, error) {
	var out TronwatcherInvariants
	err := do(ctx, c.http, "tronwatcher", c.token, http.MethodGet, c.baseURL+"/v1/system/invariants", nil, &out)
	return out, err
}
