// Package httpapi is relayd's own tiny customer-facing HTTP surface --
// mirrors proofrun/internal/httpapi's own shape and identical posture
// (no auth, no rate limiting: a minimal order-origination front door,
// not a step toward a real gateway integration -- see internal/driver's
// own doc comment).
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"relayd/internal/driver"
)

// Server holds this driver's one real dependency.
type Server struct {
	Driver    *driver.Driver
	BuildInfo func() (version, commit string)
	// Admin is the administrator API (admin.go); nil or with no tokens,
	// every admin route answers 404.
	Admin *Admin
	// QuoteLimit and OrderLimit bound each client's quotes and new orders;
	// nil means unlimited.
	QuoteLimit *RateLimit
	OrderLimit *RateLimit
}

// NewRouter builds the full route table.
func NewRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Use(corsMiddleware)
	r.Get("/healthz", s.healthz)
	r.Route("/v1", func(r chi.Router) {
		// Public: what a customer's storefront needs.
		r.With(s.QuoteLimit.Middleware).Post("/quotes", s.postQuote)
		r.With(s.OrderLimit.Middleware).Post("/relay-legs", s.postRelayLeg)
		r.Get("/relay-legs/{external_id}", s.getRelayLeg)

		// Every customer's orders: operators only.
		r.With(s.Admin.require).Get("/relay-legs", s.getRelayLegs)
		r.With(s.Admin.require).Get("/relay-legs/{external_id}/refund-entry", s.getRelayLegRefundEntry)
		r.Route("/admin", s.adminRoutes)
	})
	return r
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	version, commit := "unknown", "unknown"
	if s.BuildInfo != nil {
		version, commit = s.BuildInfo()
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version, "commit": commit})
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("httpapi: encoding response failed", "error", err)
	}
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	respondJSON(w, status, errorBody{Error: err.Error()})
}
