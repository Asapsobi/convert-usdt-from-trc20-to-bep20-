package httpapi

import (
	"net/http"
	"time"
)

type healthzResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	version, commit := "unknown", "unknown"
	if s.BuildInfo != nil {
		version, commit = s.BuildInfo()
	}
	respondJSON(w, http.StatusOK, healthzResponse{Status: "ok", Version: version, Commit: commit})
}

// readyzHandler additionally checks the database is actually reachable.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type invariantsResponse struct {
	PendingFinalityCount             *int     `json:"pending_finality_count,omitempty"`
	OldestPendingCandidateAgeSeconds *float64 `json:"oldest_pending_candidate_age_seconds,omitempty"`
}

// getInvariants is GET /v1/system/invariants: pending-finality count and
// oldest pending candidate age -- omitted (not a 503) if Tracker isn't
// configured on this instance, rather than an all-or-nothing failure
// over a partial deployment. Unlike depositwatcher's own version, there
// is no cursor-lag or provider-health figure here yet -- this service's
// chain.Pool has no equivalent health-snapshot bookkeeping in this
// pass's scope; add it if/when that becomes operationally necessary.
func (s *Server) getInvariants(w http.ResponseWriter, r *http.Request) {
	var resp invariantsResponse

	if s.Tracker != nil {
		pending := s.Tracker.PendingCount()
		resp.PendingFinalityCount = &pending

		if oldest, found := s.Tracker.OldestPendingDetectedAt(); found {
			age := time.Since(oldest).Seconds()
			resp.OldestPendingCandidateAgeSeconds = &age
		}
	}

	respondJSON(w, http.StatusOK, resp)
}
