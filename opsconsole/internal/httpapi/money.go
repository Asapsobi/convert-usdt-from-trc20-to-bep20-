package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

const errRelaydMissing = "relayd is not configured (OC_RELAYD_BASE_URL unset)"

// relaydAction audits an operator's change, runs it, and returns to back
// with the outcome.
func (s *Server) relaydAction(w http.ResponseWriter, r *http.Request, back, action, target string, detail map[string]any, run func() error) {
	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, action, target, detail)
	q := url.Values{}
	if s.Relayd == nil {
		q.Set("err", errRelaydMissing)
	} else if err := run(); err != nil {
		q.Set("err", err.Error())
	} else {
		q.Set("ok", "Saved.")
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, back+sep+q.Encode(), http.StatusFound)
}

// --- Treasury & profit ---

type treasuryData struct {
	basePageData
	Treasury *opclient.Treasury
	Energy   []map[string]string
	Settings *opclient.SweepSettings
	Wallets  opclient.ProfitWallets
	Sweeps   []opclient.Sweep
}

func (s *Server) getTreasury(w http.ResponseWriter, r *http.Request) {
	data := treasuryData{basePageData: s.page(r, "treasury", "Treasury & profit")}
	data.Subtitle = "Where network fees come from, and where profit goes"
	if s.Relayd == nil {
		data.Err = errRelaydMissing
		s.Templates.Render(w, "treasury", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	var errs []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err.Error())
		mu.Unlock()
	}
	wg.Add(5)
	go func() {
		defer wg.Done()
		if t, err := s.Relayd.GetTreasury(ctx); err != nil {
			fail(err)
		} else {
			data.Treasury = &t
		}
	}()
	go func() {
		defer wg.Done()
		if e, err := s.Relayd.GetEnergy(ctx); err == nil {
			data.Energy = e
		}
	}()
	go func() {
		defer wg.Done()
		if st, err := s.Relayd.GetSweepSettings(ctx); err != nil {
			fail(err)
		} else {
			data.Settings = &st
		}
	}()
	go func() {
		defer wg.Done()
		pw, err := s.Relayd.GetProfitWallets(ctx)
		if err != nil {
			fail(err)
		}
		data.Wallets = pw
	}()
	go func() {
		defer wg.Done()
		sw, err := s.Relayd.ListSweeps(ctx)
		if err != nil {
			fail(err)
		}
		data.Sweeps = sw
	}()
	wg.Wait()
	if len(errs) > 0 {
		data.Err = strings.Join(errs, "; ")
	}
	s.Templates.Render(w, "treasury", data)
}

func (s *Server) postSweepSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	interval, _ := strconv.ParseInt(r.FormValue("interval_minutes"), 10, 64)
	settings := opclient.SweepSettings{
		Enabled: r.FormValue("enabled") == "true", IntervalMinutes: interval,
		MinAmount: map[string]string{
			"USDT_BEP20": strings.TrimSpace(r.FormValue("min_bep20")), "USDT_TRC20": strings.TrimSpace(r.FormValue("min_trc20")),
		},
	}
	s.relaydAction(w, r, "/treasury", "relayd.sweep.settings", "sweep",
		map[string]any{"enabled": settings.Enabled, "interval_minutes": interval, "min_amount": settings.MinAmount},
		func() error { return s.Relayd.PutSweepSettings(r.Context(), settings) })
}

func (s *Server) postSweepRun(w http.ResponseWriter, r *http.Request) {
	s.relaydAction(w, r, "/treasury", "relayd.sweep.run", "sweep", nil,
		func() error { return s.Relayd.RunSweep(r.Context()) })
}

// --- Pricing ---

var pricingDirections = []struct{ Key, Label string }{
	{"BEP20_TO_TRC20", "BSC → TRON"}, {"TRC20_TO_BEP20", "TRON → BSC"},
}

type directionRow struct {
	Key, Label, ProfitBPS, MinProfit string
}

type pricingData struct {
	basePageData
	Pricing       *opclient.Pricing
	DirectionRows []directionRow
	CalcAmount    string
	CalcDir       string
	Calc          *opclient.Quote
	CalcErr       string
}

func (s *Server) getPricing(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := pricingData{basePageData: s.page(r, "pricing", "Pricing"), CalcAmount: q.Get("amount"), CalcDir: q.Get("direction")}
	data.Subtitle = "What customers pay, per direction"
	if data.CalcAmount == "" {
		data.CalcAmount = "100"
	}
	if data.CalcDir == "" {
		data.CalcDir = "BEP20_TO_TRC20"
	}
	if s.Relayd == nil {
		data.Err = errRelaydMissing
		s.Templates.Render(w, "pricing", data)
		return
	}
	if p, err := s.Relayd.GetPricing(r.Context()); err != nil {
		data.Err = err.Error()
	} else {
		data.Pricing = &p
		for _, d := range pricingDirections {
			row := directionRow{Key: d.Key, Label: d.Label}
			if m, ok := p.Directions[d.Key]; ok {
				row.ProfitBPS, row.MinProfit = strconv.FormatInt(m.ProfitBPS, 10), m.MinProfit
			}
			data.DirectionRows = append(data.DirectionRows, row)
		}
	}
	amount := data.CalcAmount
	if f, err := strconv.ParseFloat(amount, 64); err == nil && f > 0 {
		amount = strconv.FormatFloat(f, 'f', 6, 64)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if quote, err := s.Relayd.Quote(ctx, data.CalcDir, amount); err != nil {
			data.CalcErr = err.Error()
		} else {
			data.Calc = &quote
		}
	} else {
		data.CalcErr = "Enter an amount greater than 0."
	}
	s.Templates.Render(w, "pricing", data)
}

func (s *Server) postPricing(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	bps, _ := strconv.ParseInt(r.FormValue("profit_bps"), 10, 64)
	p := opclient.Pricing{
		ProfitBPS: bps, MinProfit: strings.TrimSpace(r.FormValue("min_profit")),
		MinAmountIn: strings.TrimSpace(r.FormValue("min_amount_in")), MaxAmountIn: strings.TrimSpace(r.FormValue("max_amount_in")),
	}
	for _, d := range pricingDirections {
		rawBPS, minProfit := strings.TrimSpace(r.FormValue(d.Key+"_profit_bps")), strings.TrimSpace(r.FormValue(d.Key+"_min_profit"))
		if rawBPS == "" && minProfit == "" {
			continue // uses the default
		}
		dirBPS, _ := strconv.ParseInt(rawBPS, 10, 64)
		if minProfit == "" {
			minProfit = "0"
		}
		if p.Directions == nil {
			p.Directions = map[string]opclient.DirectionMargin{}
		}
		p.Directions[d.Key] = opclient.DirectionMargin{ProfitBPS: dirBPS, MinProfit: minProfit}
	}
	s.relaydAction(w, r, "/pricing", "relayd.pricing.update", "pricing",
		map[string]any{"profit_bps": p.ProfitBPS, "min_profit": p.MinProfit, "min_amount_in": p.MinAmountIn, "max_amount_in": p.MaxAmountIn, "directions": p.Directions},
		func() error { return s.Relayd.PutPricing(r.Context(), p) })
}

// --- Vendors ---

type vendorsData struct {
	basePageData
	View   opclient.Vendors
	Prices *opclient.VendorPrices
}

func (s *Server) getVendors(w http.ResponseWriter, r *http.Request) {
	data := vendorsData{basePageData: s.page(r, "vendors", "Vendors")}
	data.Subtitle = "Exchanges that convert, and providers that rent TRON energy"
	if s.Relayd == nil {
		data.Err = errRelaydMissing
	} else if v, err := s.Relayd.GetVendors(r.Context()); err != nil {
		data.Err = err.Error()
	} else {
		data.View = v
		if prices, err := s.Relayd.GetVendorPrices(r.Context(), "100"); err == nil {
			data.Prices = &prices
		}
	}
	s.Templates.Render(w, "vendors", data)
}

func (s *Server) postVendor(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	service, name := chi.URLParam(r, "service"), chi.URLParam(r, "name")
	var enabled *bool
	var priority *int
	if raw := r.FormValue("enabled"); raw != "" {
		v := raw == "true"
		enabled = &v
	}
	if raw := r.FormValue("priority"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			priority = &v
		}
	}
	s.relaydAction(w, r, "/vendors", "relayd.vendor.update", service+":"+name,
		map[string]any{"enabled": enabled, "priority": priority},
		func() error { return s.Relayd.PatchVendor(r.Context(), service, name, enabled, priority) })
}

func (s *Server) postVendorTerms(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	service, name := chi.URLParam(r, "service"), chi.URLParam(r, "name")
	revenue, _ := strconv.Atoi(r.FormValue("revenue_bps"))
	notes := strings.TrimSpace(r.FormValue("notes"))
	s.relaydAction(w, r, "/vendors", "relayd.vendor.terms", service+":"+name,
		map[string]any{"revenue_bps": revenue, "notes": notes},
		func() error { return s.Relayd.SetVendorTerms(r.Context(), service, name, revenue, notes) })
}

func (s *Server) postVendorStrategy(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	service, strategy := chi.URLParam(r, "service"), r.FormValue("strategy")
	s.relaydAction(w, r, "/vendors", "relayd.vendor.strategy", service, map[string]any{"strategy": strategy},
		func() error { return s.Relayd.PutVendorStrategy(r.Context(), service, strategy) })
}
