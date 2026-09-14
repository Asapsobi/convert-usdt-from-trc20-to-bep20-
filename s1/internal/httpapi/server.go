// Package httpapi exposes S1 as a service: SigningService for C5
// (service-to-service bearer auth) and the human approve/reject actions
// for approvers (a SEPARATE bearer auth scope -- see auth.go's own doc
// comment for why the two must never share a token set).
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"s1/internal/db"
	"s1/internal/requests"
)

// Server holds everything a handler needs.
type Server struct {
	Pool      *db.Pool
	C5Auth    AuthConfig
	Approver  AuthConfig
	Signing   *requests.Store
	Metrics   *Metrics
	BuildInfo func() (version, commit string)
}

// NewRouter builds the full route table. /healthz, /readyz, and
// /metrics are deliberately unauthenticated -- operational endpoints
// scraped by infrastructure, not part of either business API auth scope
// governs, same posture as every prior component's own NewRouter.
func NewRouter(s *Server) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if s.Metrics == nil {
		s.Metrics = NewMetrics(registry)
	} else {
		registry.MustRegister(s.Metrics.SigningRequestsTotal, s.Metrics.ApprovalsTotal)
	}

	router := chi.NewRouter()
	router.Use(corsMiddleware)

	router.Get("/healthz", s.healthzHandler)
	router.Get("/readyz", s.readyzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	router.Route("/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(authMiddleware(s.C5Auth))
			r.Post("/signing-requests", s.postSigningRequest)
			r.Post("/deposit-signing-requests", s.postDepositSigningRequest)
			r.Get("/signing-requests", s.getSigningRequests)
			r.Get("/signing-requests/{id}", s.getSigningRequest)
			r.Get("/slots/{id}/address", s.getSlotAddress)
			r.Get("/slots/{id}/evm-address", s.getSlotEVMAddress)
		})
		r.Group(func(r chi.Router) {
			r.Use(authMiddleware(s.Approver))
			r.Post("/signing-requests/{id}/approve", s.postApprove)
			r.Post("/signing-requests/{id}/reject", s.postReject)
		})
	})

	return router
}
