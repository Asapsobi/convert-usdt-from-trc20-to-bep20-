package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

// unmatchedDeposit is an orphaned deposit with the chain it arrived on.
type unmatchedDeposit struct {
	opclient.OrphanedDeposit
	Chain string // BSC or TRON
	Key   string // bsc or tron, for the resolve URL
}

type depositsData struct {
	basePageData
	Resolved bool
	Deposits []unmatchedDeposit
}

func (s *Server) getDeposits(w http.ResponseWriter, r *http.Request) {
	data := depositsData{basePageData: s.page(r, "deposits", "Unmatched deposits"), Resolved: r.URL.Query().Get("show") == "resolved"}
	data.Subtitle = "Payments that reached a deposit wallet when no order was waiting for them"
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var errs []string
	add := func(chain, key string, list func(context.Context, bool) ([]opclient.OrphanedDeposit, error)) {
		if list == nil {
			return
		}
		deps, err := list(ctx, data.Resolved)
		if err != nil {
			errs = append(errs, chain+": "+err.Error())
			return
		}
		for _, d := range deps {
			data.Deposits = append(data.Deposits, unmatchedDeposit{OrphanedDeposit: d, Chain: chain, Key: key})
		}
	}
	add("BSC", "bsc", s.orphanedBSC())
	add("TRON", "tron", s.orphanedTRON())
	sort.Slice(data.Deposits, func(i, j int) bool { return data.Deposits[i].DetectedAt.After(data.Deposits[j].DetectedAt) })
	if len(errs) > 0 {
		data.Err = strings.Join(errs, "; ")
	}
	s.Templates.Render(w, "deposits", data)
}

func (s *Server) postResolveDeposit(w http.ResponseWriter, r *http.Request) {
	chain := chi.URLParam(r, "chain")
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	resolution := strings.TrimSpace(r.FormValue("resolution"))
	back := func(key, msg string) {
		http.Redirect(w, r, "/deposits?"+url.Values{key: {msg}}.Encode(), http.StatusFound)
	}
	if err != nil || resolution == "" {
		back("err", "Describe what you did (for example the refund transaction) before marking it resolved.")
		return
	}
	var resolve func(context.Context, int64, string) error
	switch {
	case chain == "bsc" && s.Watcher != nil:
		resolve = s.Watcher.ResolveOrphanedDeposit
	case chain == "tron" && s.Tronwatcher != nil:
		resolve = s.Tronwatcher.ResolveOrphanedDeposit
	default:
		http.NotFound(w, r)
		return
	}
	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, "deposit.orphaned.resolve", chain+":"+strconv.FormatInt(id, 10), map[string]any{"resolution": resolution})
	if err := resolve(r.Context(), id, resolution); err != nil {
		back("err", err.Error())
		return
	}
	s.expireNavCounts()
	back("ok", "Marked resolved.")
}

// expireNavCounts makes the sidebar's badges re-read on the next page.
func (s *Server) expireNavCounts() {
	s.nav.mu.Lock()
	s.nav.at = time.Time{}
	s.nav.mu.Unlock()
}
