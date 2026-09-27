package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"opsconsole/internal/session"
)

// basePageData is what the layout needs on every page.
type basePageData struct {
	Session    *session.Session
	Halted     bool
	HaltReason string
	Title      string
	Subtitle   string
	Nav        string // the sidebar entry to highlight
	EnvLabel   string
	EnvLive    bool
	Counts     navCounts
	Legacy     *legacyNav // nil when no Model D service is configured
	Refresh    int        // seconds between automatic reloads; 0 = never
	Err, OK    string     // the result of the last action, from ?err= / ?ok=
}

// legacyNav says which Model D pages to offer.
type legacyNav struct{ Broker, Dispatcher bool }

// navCounts are the sidebar's badges: things waiting for an operator.
type navCounts struct {
	Attention int // orders that need a look
	Unmatched int // payments no order was waiting for
	Holds     int // orders held by screening
	Approvals int // signatures waiting for an approver
	Warnings  int // everything on the overview's attention list
}

// page is the layout data for a page: nav is the sidebar entry to
// highlight.
func (s *Server) page(r *http.Request, nav, title string) basePageData {
	q := r.URL.Query()
	data := basePageData{Nav: nav, Title: title, EnvLabel: s.EnvLabel, EnvLive: strings.EqualFold(s.EnvLabel, "live"),
		Err: q.Get("err"), OK: q.Get("ok")}
	if sess, ok := sessionFromContext(r.Context()); ok {
		data.Session = &sess
	}
	if s.Ledger != nil {
		if halt, err := s.Ledger.GetHaltState(r.Context()); err == nil {
			data.Halted, data.HaltReason = halt.Halted, halt.Reason
		}
	}
	if s.Broker != nil || s.Dispatcher != nil {
		data.Legacy = &legacyNav{Broker: s.Broker != nil, Dispatcher: s.Dispatcher != nil}
	}
	if data.Session != nil {
		data.Counts = s.navCounts(r.Context())
	}
	return data
}

// navCountsTTL is how long the sidebar's badges are reused before being
// read again.
const navCountsTTL = 30 * time.Second

type navCache struct {
	mu     sync.Mutex
	counts navCounts
	at     time.Time
}

func (s *Server) navCounts(ctx context.Context) navCounts {
	s.nav.mu.Lock()
	defer s.nav.mu.Unlock()
	if time.Since(s.nav.at) < navCountsTTL {
		return s.nav.counts
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	warnings := s.nav.counts.Warnings // only the overview recomputes these
	s.nav.counts = s.collectNavCounts(ctx)
	s.nav.counts.Warnings = warnings
	s.nav.at = time.Now()
	return s.nav.counts
}

// setWarnings records the overview's attention count for the sidebar.
func (s *Server) setWarnings(n int) {
	s.nav.mu.Lock()
	s.nav.counts.Warnings = n
	s.nav.mu.Unlock()
}

// collectNavCounts reads every badge in parallel; a source that fails
// counts as zero rather than holding the page up.
func (s *Server) collectNavCounts(ctx context.Context) navCounts {
	var out navCounts
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(f func() int, dst *int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := f()
			mu.Lock()
			*dst += n
			mu.Unlock()
		}()
	}
	if s.Relayd != nil {
		run(func() int {
			st, err := s.Relayd.GetStats(ctx)
			if err != nil {
				return 0
			}
			return len(st.Attention)
		}, &out.Attention)
	}
	if s.Watcher != nil {
		run(func() int {
			d, err := s.Watcher.ListOrphanedDeposits(ctx, false)
			if err != nil {
				return 0
			}
			return len(d)
		}, &out.Unmatched)
	}
	if s.Tronwatcher != nil {
		run(func() int {
			d, err := s.Tronwatcher.ListOrphanedDeposits(ctx, false)
			if err != nil {
				return 0
			}
			return len(d)
		}, &out.Unmatched)
	}
	if s.Screening != nil {
		run(func() int {
			h, err := s.Screening.ListHolds(ctx, "OPEN")
			if err != nil {
				return 0
			}
			return len(h)
		}, &out.Holds)
	}
	if s.S1 != nil {
		run(func() int {
			p, err := s.S1.ListPendingApprovals(ctx)
			if err != nil {
				return 0
			}
			return len(p)
		}, &out.Approvals)
	}
	wg.Wait()
	return out
}
