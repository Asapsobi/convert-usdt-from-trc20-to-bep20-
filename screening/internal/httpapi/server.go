// Package httpapi exposes C3 as a service, mirroring C1.8's and C2.9's
// shape and discipline: service-to-service bearer auth, a stable error
// code per condition, an OpenAPI spec every route must appear in.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"screening/internal/db"
	"screening/internal/holds"
	"screening/internal/ledgerclient"
)

// Server holds everything a handler needs. LedgerClient is used by
// Release/Reject (internal/holds' own Releaser/Rejecter interfaces) --
// nil-safe callers aren't needed here, since every write route that
// touches it requires auth and a real Server is always constructed with
// one in cmd/screend. RefundEntryBuilder defaults to
// holds.StubRefundEntryBuilder{} in NewRouter if left nil -- cmd/screend
// wires holds.RelayAwareRefundEntryBuilder instead when relayd is
// actually configured (SCREENING_RELAYD_BASE_URL/TOKEN, both optional --
// Model F is a separate product line, same "optional Model F dependency"
// posture opsconsole's own Relayd/Tronwatcher clients already take).
type Server struct {
	Pool               *db.Pool
	Auth               AuthConfig
	LedgerClient       *ledgerclient.Client
	RefundEntryBuilder holds.RefundEntryBuilder
	Metrics            *Metrics
	BuildInfo          func() (version, commit string)
}

// NewRouter builds the full route table. /healthz, /readyz, and
// /metrics are deliberately unauthenticated -- operational endpoints
// scraped by infrastructure, not part of the service-to-service
// business API AUTH governs, same posture as C1.8/C2.9.
func NewRouter(s *Server) http.Handler {
	if s.RefundEntryBuilder == nil {
		s.RefundEntryBuilder = holds.StubRefundEntryBuilder{}
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if s.Metrics == nil {
		s.Metrics = NewMetrics(registry)
	} else {
		registry.MustRegister(
			s.Metrics.VerdictsTotal, s.Metrics.HoldsOpenedTotal, s.Metrics.HoldsReleasedTotal,
			s.Metrics.HoldsRejectedTotal, s.Metrics.RescreenFlagsTotal, s.Metrics.ScreeningVendorUnavailableTotal,
		)
	}
	for _, c := range newQueueGaugeCollectors(s.Pool) {
		registry.MustRegister(c)
	}

	router := chi.NewRouter()
	router.Use(corsMiddleware)

	router.Get("/healthz", s.healthzHandler)
	router.Get("/readyz", s.readyzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	router.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(s.Auth))

		r.Get("/holds", s.getHolds)
		r.Post("/holds/{id}/release", s.postReleaseHold)
		r.Post("/holds/{id}/reject", s.postRejectHold)

		r.Get("/screening-results", s.getScreeningResults)
		r.Post("/screening-results/{id}/invalidate", s.postInvalidateScreeningResult)

		r.Get("/rescreen-flags", s.getRescreenFlags)
		r.Post("/rescreen-flags/{id}/resolve", s.postResolveRescreenFlag)

		r.Get("/system/queue", s.getSystemQueue)
	})

	return router
}
