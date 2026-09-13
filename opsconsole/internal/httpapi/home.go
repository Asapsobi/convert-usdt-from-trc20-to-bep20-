package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const homeContent = `
<h1>Home</h1>
<div class="cards">
{{ range .Services }}
  <div class="card">
    <div><span class="dot {{ if .Healthy }}dot-green{{ else }}dot-red{{ end }}"></span><strong>{{ .Name }}</strong></div>
    {{ if .Error }}<div class="err">{{ .Error }}</div>{{ end }}
    {{ range .Facts }}<div>{{ .Label }}: {{ .Value }}</div>{{ end }}
  </div>
{{ end }}
</div>
`

type serviceFact struct {
	Label, Value string
}

type serviceCard struct {
	Name    string
	Healthy bool
	Error   string
	Facts   []serviceFact
}

type homePageData struct {
	basePageData
	Services []serviceCard
}

// checkService runs healthz plus fn (a service's own invariants fetch)
// concurrently with every other service, each bounded by its own
// timeout -- invariant 4: one slow/dead service degrades only its own
// card, never the whole page.
func checkService(ctx context.Context, name string, healthz func(context.Context) error, facts func(context.Context) ([]serviceFact, error)) serviceCard {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	card := serviceCard{Name: name}
	if err := healthz(ctx); err != nil {
		card.Error = err.Error()
		return card
	}
	card.Healthy = true
	if f, err := facts(ctx); err != nil {
		card.Error = err.Error()
	} else {
		card.Facts = f
	}
	return card
}

func (s *Server) collectServiceCards(r *http.Request) []serviceCard {
	type job struct {
		name    string
		healthz func(context.Context) error
		facts   func(context.Context) ([]serviceFact, error)
	}
	jobs := []job{
		{"ledger", s.Ledger.Healthz, func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Ledger.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{
				{"halted", boolStr(inv.Halted)},
				{"trial balance ok", boolStr(inv.TrialBalanceOK)},
				{"cache ok", boolStr(inv.CacheOK)},
			}, nil
		}},
		{"watcher", s.Watcher.Healthz, func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Watcher.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			facts := []serviceFact{}
			if inv.CursorLagBlocks != nil {
				facts = append(facts, serviceFact{"cursor lag (blocks)", int64Str(*inv.CursorLagBlocks)})
			}
			if inv.PendingFinalityCount != nil {
				facts = append(facts, serviceFact{"pending finality", intStr(*inv.PendingFinalityCount)})
			}
			return facts, nil
		}},
		{"screening", s.Screening.Healthz, func(ctx context.Context) ([]serviceFact, error) {
			q, err := s.Screening.GetQueue(ctx)
			if err != nil {
				return nil, err
			}
			facts := []serviceFact{}
			for status, depth := range q.DepthByStatus {
				facts = append(facts, serviceFact{"queue: " + status, intStr(depth)})
			}
			return facts, nil
		}},
		{"broker", s.Broker.Healthz, func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Broker.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{
				{"buffer available/target", int64Str(inv.BufferAvailable) + " / " + int64Str(inv.BufferTarget)},
				{"open fallback events", int64Str(inv.OpenManualFallbackEvents)},
			}, nil
		}},
		{"dispatcher", s.Dispatcher.Healthz, func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Dispatcher.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{
				{"open dispatching orders", intStr(inv.OpenDispatchingOrders)},
				{"stuck pending reconciliation", intStr(inv.StuckPendingReconciliation)},
			}, nil
		}},
		{"s1", s.S1.Healthz, func(ctx context.Context) ([]serviceFact, error) {
			pending, err := s.S1.ListPendingApprovals(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{{"pending approvals", intStr(len(pending))}}, nil
		}},
	}

	// Model F's own two services are optional (see Server's own doc
	// comment) -- only added as jobs if actually configured, so an
	// unconfigured deployment shows no card for them at all rather than
	// one permanently stuck on "unreachable."
	if s.Tronwatcher != nil {
		jobs = append(jobs, job{"tronwatcher", s.Tronwatcher.Healthz, func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Tronwatcher.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			facts := []serviceFact{}
			if inv.PendingFinalityCount != nil {
				facts = append(facts, serviceFact{"pending finality", intStr(*inv.PendingFinalityCount)})
			}
			return facts, nil
		}})
	}
	if s.Relayd != nil {
		jobs = append(jobs, job{"relayd", s.Relayd.Healthz, func(ctx context.Context) ([]serviceFact, error) {
			legs, err := s.Relayd.ListRelayLegs(ctx, "")
			if err != nil {
				return nil, err
			}
			byStatus := map[string]int{}
			for _, l := range legs {
				byStatus[l.Status]++
			}
			facts := []serviceFact{{"total legs", intStr(len(legs))}}
			if n := byStatus["UNRECOVERABLE"]; n > 0 {
				facts = append(facts, serviceFact{"UNRECOVERABLE (needs attention)", intStr(n)})
			}
			if n := byStatus["REFUND_PENDING"]; n > 0 {
				facts = append(facts, serviceFact{"refund pending", intStr(n)})
			}
			return facts, nil
		}})
	}

	cards := make([]serviceCard, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			cards[i] = checkService(r.Context(), j.name, j.healthz, j.facts)
		}(i, j)
	}
	wg.Wait()
	return cards
}

func (s *Server) getHome(w http.ResponseWriter, r *http.Request) {
	data := homePageData{basePageData: s.newBasePageData(r), Services: s.collectServiceCards(r)}
	s.Templates.Render(w, "home", data)
}

// getHomePartial serves the same cards without the surrounding page --
// the auto-refresh JS on the home page fetches this every 10s.
func (s *Server) getHomePartial(w http.ResponseWriter, r *http.Request) {
	s.getHome(w, r)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func intStr(i int) string     { return strconv.Itoa(i) }
func int64Str(i int64) string { return strconv.FormatInt(i, 10) }
