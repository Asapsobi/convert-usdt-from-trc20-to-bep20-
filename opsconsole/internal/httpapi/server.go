// Package httpapi is the admin panel's HTTP boundary: routes and
// server-rendered pages (templates and styles in ui/).
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"opsconsole/internal/auditlog"
	"opsconsole/internal/opclient"
	"opsconsole/internal/session"
)

// Operator is one entry from OC_OPERATORS.
type Operator struct {
	Username    string
	BcryptHash  string
	DisplayName string
}

// Server holds every client the panel talks to. Broker and Dispatcher
// (Model D) are optional and nil when not configured.
type Server struct {
	Ledger     *opclient.LedgerClient
	Watcher    *opclient.WatcherClient
	Screening  *opclient.ScreeningClient
	Broker     *opclient.BrokerClient
	Dispatcher *opclient.DispatcherClient
	S1         *opclient.S1Client

	Relayd      *opclient.RelaydClient
	Tronwatcher *opclient.TronwatcherClient

	Operators []Operator
	Sessions  *session.Signer
	Audit     *auditlog.Log
	AuditPath string // the file Audit writes to; the audit page tails it

	// EnvLabel is shown in the top bar, e.g. "Live" or "Local test".
	EnvLabel string

	Templates *Templates
	BuildInfo func() (version, commit string)

	nav navCache
}

func (s *Server) findOperator(username string) (Operator, bool) {
	for _, op := range s.Operators {
		if op.Username == username {
			return op, true
		}
	}
	return Operator{}, false
}

// NewRouter builds the full route table. /healthz, /metrics and /static
// are public; everything else requires a session.
func NewRouter(s *Server) http.Handler {
	if s.Templates == nil {
		s.Templates = MustLoadTemplates()
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	router := chi.NewRouter()
	router.Get("/healthz", s.healthzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	router.Handle("/static/*", staticHandler())

	router.Get("/login", s.getLogin)
	router.Post("/login", s.postLogin)
	router.Post("/logout", s.postLogout)

	router.Group(func(r chi.Router) {
		r.Use(s.requireSession)

		r.Get("/", s.getOverview)

		r.Get("/orders", s.getOrders)
		r.Get("/orders/{external_id}", s.getOrder)

		r.Get("/deposits", s.getDeposits)
		r.Post("/deposits/{chain}/{id}/resolve", s.postResolveDeposit)

		r.Get("/treasury", s.getTreasury)
		r.Post("/treasury/sweeps/settings", s.postSweepSettings)
		r.Post("/treasury/sweeps/run", s.postSweepRun)

		r.Get("/wallets", redirect("/wallets/bsc"))
		r.Get("/wallets/{chain}", s.getWallets)
		r.Post("/wallets/{chain}/settings", s.postWalletSettings)
		r.Post("/wallets/{chain}/add", s.postWalletAdd)
		r.Post("/wallets/{chain}/{address}/enable", s.postWalletStatus(true))
		r.Post("/wallets/{chain}/{address}/disable", s.postWalletStatus(false))

		r.Get("/pricing", s.getPricing)
		r.Post("/pricing", s.postPricing)

		r.Get("/vendors", s.getVendors)
		r.Post("/vendors/{service}/strategy", s.postVendorStrategy)
		r.Post("/vendors/{service}/{name}", s.postVendor)
		r.Post("/vendors/{service}/{name}/terms", s.postVendorTerms)

		r.Get("/screening", s.getScreening)
		r.Post("/screening/{id}/release", s.postHold(true))
		r.Post("/screening/{id}/reject", s.postHold(false))

		r.Get("/approvals", s.getApprovals)
		r.Post("/approvals/{id}/approve", s.postApproval(true))
		r.Post("/approvals/{id}/reject", s.postApproval(false))

		r.Get("/system", s.getSystem)
		r.Post("/system/halt/set", s.postHalt(true))
		r.Post("/system/halt/clear", s.postHalt(false))
		r.Post("/system/watcher-cursor", s.postWatcherCursor)

		r.Get("/audit", s.getAudit)

		if s.Broker != nil {
			r.Get("/legacy/broker", s.getBroker)
			r.Get("/legacy/broker/{id}/reconcile", s.getBrokerReconcile)
			r.Post("/legacy/broker/{id}/reconcile", s.postBrokerReconcile)
			r.Get("/legacy/broker/fallback", s.getBrokerFallback)
			r.Post("/legacy/broker/fallback/{id}/resolve", s.postBrokerFallbackResolve)
		}
		if s.Dispatcher != nil {
			r.Get("/legacy/dispatcher", s.getDispatcher)
			r.Post("/legacy/dispatcher/{id}/retire", s.postDispatcherSlotRetire)
		}

		// Where the previous version of the panel kept each page.
		r.Get("/relayd/legs", redirect("/orders"))
		r.Get("/relayd/legs/{external_id}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/orders/"+chi.URLParam(r, "external_id"), http.StatusMovedPermanently)
		})
		r.Get("/relayd/pricing", redirect("/pricing"))
		r.Get("/relayd/vendors", redirect("/vendors"))
		r.Get("/relayd/sweeps", redirect("/treasury"))
		r.Get("/relayd/pool/{chain}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/wallets/"+chi.URLParam(r, "chain"), http.StatusMovedPermanently)
		})
		r.Get("/screening/holds", redirect("/screening"))
		r.Get("/s1/approvals", redirect("/approvals"))
		r.Get("/ledger/halt", redirect("/system"))
		r.Get("/watcher/cursor", redirect("/system"))
		r.Get("/partial/home", redirect("/"))
	})

	return router
}

func redirect(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to, http.StatusMovedPermanently)
	}
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	version, commit := "unknown", "unknown"
	if s.BuildInfo != nil {
		version, commit = s.BuildInfo()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","version":"` + version + `","commit":"` + commit + `"}`))
}
