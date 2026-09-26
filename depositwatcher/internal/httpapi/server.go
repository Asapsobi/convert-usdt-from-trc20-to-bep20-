// Package httpapi exposes C2 as a service, mirroring C1.8's shape and
// discipline (auth, idempotency, decimal-string amounts, a stable error
// code per condition) -- this is the C6-facing boundary once C6 exists.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/db"
	"depositwatcher/internal/finality"
)

// Server holds everything a handler needs. ChainPool and Tracker may be
// nil -- a deployment's RPC providers, HD xpub, and ledgerclient wiring
// are configured independently of the HTTP boundary itself, and standing
// up this API to inspect address/orphaned-deposit state does not require
// the full ingestion+finality engine to be running yet. Handlers that
// depend on either (GET /system/providers, GET /system/invariants)
// degrade explicitly rather than panic -- see their own doc comments.
type Server struct {
	Pool      *db.Pool
	ChainPool *chain.Pool
	Tracker   *finality.Tracker
	Auth      AuthConfig
	Metrics   *Metrics
	BuildInfo func() (version, commit string)
}

// NewRouter builds the full route table. /healthz, /readyz, and /metrics
// are deliberately unauthenticated, same reasoning as C1.8: operational
// endpoints scraped by infrastructure, not part of the service-to-service
// business API AUTH governs.
func NewRouter(s *Server) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if s.Metrics == nil {
		s.Metrics = NewMetrics(registry, s.Pool, s.ChainPool)
	} else {
		registry.MustRegister(s.Metrics.CandidatesDetectedTotal, s.Metrics.CandidatesFinalizedTotal, s.Metrics.ReportsToLedgerTotal)
	}
	registry.MustRegister(newCursorLagCollector(s))

	router := chi.NewRouter()
	router.Use(corsMiddleware)

	router.Get("/healthz", s.healthzHandler)
	router.Get("/readyz", s.readyzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	router.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(s.Auth))

		r.With(requireIdempotencyKey).Post("/addresses", s.postAddress)
		r.Get("/addresses/{order_id}", s.getAddress)
		r.Get("/addresses/{order_id}/deposits", s.getOrderDeposits)
		r.Get("/addresses/{order_id}/scan-status", s.getScanStatus)

		r.Get("/wallets", s.getWallets)
		r.Post("/wallets", s.postWallet)
		r.Post("/wallets/{address}/enable", s.postWalletStatus(true))
		r.Post("/wallets/{address}/disable", s.postWalletStatus(false))
		r.Get("/pool/settings", s.getPoolSettings)
		r.Put("/pool/settings", s.putPoolSettings)
		r.With(requireIdempotencyKey).Post("/addresses/{order_id}/retire", s.postRetireAddress)

		r.Get("/orphaned-deposits", s.getOrphanedDeposits)
		r.With(requireIdempotencyKey).Post("/orphaned-deposits/{id}/resolve", s.postResolveOrphanedDeposit)

		r.Get("/system/providers", s.getProviders)
		r.Get("/system/invariants", s.getInvariants)
		r.Get("/system/cursor", s.getCursor)
		r.With(requireIdempotencyKey).Post("/system/cursor", s.postCursor)
	})

	return router
}
