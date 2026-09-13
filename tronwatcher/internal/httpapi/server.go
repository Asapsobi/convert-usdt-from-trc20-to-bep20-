// Package httpapi exposes tronwatcher as a service, mirroring
// depositwatcher/internal/httpapi's shape and discipline (auth,
// idempotency, decimal-string amounts, a stable error code per
// condition).
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"tronwatcher/internal/db"
	"tronwatcher/internal/finality"
)

// Server holds everything a handler needs. Tracker may be nil -- a
// deployment's providers, HD xpub, and ledgerclient wiring are
// configured independently of the HTTP boundary itself, and standing
// up this API to inspect address/orphaned-deposit state does not
// require the full ingestion+finality engine to be running yet.
type Server struct {
	Pool      *db.Pool
	Tracker   *finality.Tracker
	Auth      AuthConfig
	BuildInfo func() (version, commit string)
}

// NewRouter builds the full route table. /healthz, /readyz, and
// /metrics are deliberately unauthenticated, matching every sibling
// service's own convention.
func NewRouter(s *Server) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	router := chi.NewRouter()
	router.Use(corsMiddleware)

	router.Get("/healthz", s.healthzHandler)
	router.Get("/readyz", s.readyzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	router.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(s.Auth))

		r.With(requireIdempotencyKey).Post("/addresses", s.postAddress)
		r.Get("/addresses/{order_id}", s.getAddress)
		r.With(requireIdempotencyKey).Post("/addresses/{order_id}/retire", s.postRetireAddress)

		r.Get("/orphaned-deposits", s.getOrphanedDeposits)
		r.With(requireIdempotencyKey).Post("/orphaned-deposits/{id}/resolve", s.postResolveOrphanedDeposit)

		r.Get("/system/invariants", s.getInvariants)
	})

	return router
}
