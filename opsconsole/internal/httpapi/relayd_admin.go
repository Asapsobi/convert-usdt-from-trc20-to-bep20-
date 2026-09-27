package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

// Model F administration through relayd's admin API: pricing, vendors,
// the deposit-wallet pools, profit sweeps, and each order's full record.
// Every change is written to the audit log before it is made.

const flashSnippet = `{{ if .Err }}<div class="flash flash-error">{{ .Err }}</div>{{ end }}{{ if .OK }}<div class="flash flash-ok">{{ .OK }}</div>{{ end }}`

const relaydNav = `<p><a href="/relayd/legs">Orders</a> &middot; <a href="/relayd/pricing">Pricing</a> &middot; <a href="/relayd/vendors">Vendors</a> &middot; <a href="/relayd/sweeps">Profit &amp; sweeps</a> &middot; <a href="/relayd/pool/bsc">BSC wallets</a> &middot; <a href="/relayd/pool/tron">TRON wallets</a></p>`

type flashData struct {
	basePageData
	Err string
	OK  string
}

func (s *Server) newFlashData(r *http.Request) flashData {
	return flashData{basePageData: s.newBasePageData(r), Err: r.URL.Query().Get("err"), OK: r.URL.Query().Get("ok")}
}

// relaydAction audits and runs one change, then returns to back with its
// outcome.
func (s *Server) relaydAction(w http.ResponseWriter, r *http.Request, back, action, target string, detail map[string]any, run func() error) {
	sess, _ := sessionFromContext(r.Context())
	_ = s.Audit.Write(sess.Username, action, target, detail)
	q := url.Values{}
	if s.Relayd == nil {
		q.Set("err", "relayd is not configured (OC_RELAYD_BASE_URL unset)")
	} else if err := run(); err != nil {
		q.Set("err", err.Error())
	} else {
		q.Set("ok", "Saved.")
	}
	http.Redirect(w, r, back+"?"+q.Encode(), http.StatusFound)
}

const relaydPricingContent = relaydNav + `
<h1>Pricing</h1>` + flashSnippet + `
<p>Our profit is taken from what the customer actually deposits; the rest goes to the conversion vendor, who pays the customer.
Each order keeps the pricing it was quoted under -- a change applies to new orders only.</p>
{{ with .Pricing }}
<form method="post" action="/relayd/pricing" class="card">
  <label>Profit (basis points; 100 = 1%)</label><input type="number" name="profit_bps" min="0" max="9999" value="{{ .ProfitBPS }}">
  <label>Minimum profit per order (USDT)</label><input type="text" name="min_profit" value="{{ .MinProfit }}">
  <label>Smallest deposit accepted (USDT) -- smaller deposits are refunded</label><input type="text" name="min_amount_in" value="{{ .MinAmountIn }}">
  <label>Largest deposit accepted (USDT)</label><input type="text" name="max_amount_in" value="{{ .MaxAmountIn }}">
  <h3>Per direction (optional)</h3>
  <p>Leave empty to use the default above. Sending from a TRON deposit wallet rents energy (around 1 USD per order), so
  TRON &rarr; BSC usually needs a higher margin or minimum than BSC &rarr; TRON.</p>
  <table>
  <tr><th>Direction</th><th>Profit (bps)</th><th>Minimum profit (USDT)</th></tr>
  {{ range $.DirectionRows }}
  <tr><td>{{ .Label }}</td>
    <td><input type="number" name="{{ .Key }}_profit_bps" min="0" max="9999" value="{{ .ProfitBPS }}"></td>
    <td><input type="text" name="{{ .Key }}_min_profit" value="{{ .MinProfit }}"></td></tr>
  {{ end }}
  </table>
  <p><button type="submit">Save pricing</button></p>
</form>
{{ end }}
`

type relaydPricingData struct {
	flashData
	Pricing       *opclient.Pricing
	DirectionRows []directionRow
}

type directionRow struct {
	Key, Label, ProfitBPS, MinProfit string
}

var pricingDirections = []struct{ Key, Label string }{
	{"TRC20_TO_BEP20", "TRON → BSC"}, {"BEP20_TO_TRC20", "BSC → TRON"},
}

func (s *Server) getRelaydPricing(w http.ResponseWriter, r *http.Request) {
	data := relaydPricingData{flashData: s.newFlashData(r)}
	if s.Relayd == nil {
		data.Err = "relayd is not configured (OC_RELAYD_BASE_URL unset)"
	} else if p, err := s.Relayd.GetPricing(r.Context()); err != nil {
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
	s.Templates.Render(w, "relayd_pricing", data)
}

func (s *Server) postRelaydPricing(w http.ResponseWriter, r *http.Request) {
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
	s.relaydAction(w, r, "/relayd/pricing", "relayd.pricing.update", "pricing",
		map[string]any{"profit_bps": p.ProfitBPS, "min_profit": p.MinProfit, "min_amount_in": p.MinAmountIn, "max_amount_in": p.MaxAmountIn, "directions": p.Directions},
		func() error { return s.Relayd.PutPricing(r.Context(), p) })
}

const relaydVendorsContent = relaydNav + `
<h1>Vendors</h1>` + flashSnippet + `
<p>A vendor that fails is taken out automatically and returns once it answers again. Disabling one here takes it out until you enable it;
its orders already in progress are still followed.</p>
{{ range $service, $strategy := .View.Strategies }}
<form method="post" action="/relayd/vendors/{{ $service }}/strategy" class="card">
  <b>{{ $service }}</b> vendor selection:
  <select name="strategy">
    {{ if eq $service "conversion" }}<option value="best_rate"{{ if eq $strategy "best_rate" }} selected{{ end }}>best rate for the customer</option>
    <option value="best_margin"{{ if eq $strategy "best_margin" }} selected{{ end }}>most revenue for us (vendor's revenue share)</option>{{ end }}
    {{ if eq $service "energy" }}<option value="cheapest"{{ if eq $strategy "cheapest" }} selected{{ end }}>cheapest</option>{{ end }}
    <option value="priority"{{ if eq $strategy "priority" }} selected{{ end }}>fixed priority (lowest number first)</option>
  </select>
  <button type="submit">Save</button>
</form>
{{ end }}
<table>
<tr><th>Service</th><th>Vendor</th><th>Enabled</th><th>Health</th><th>Priority</th><th>Pays us (bps) / notes</th><th>Last error</th><th></th></tr>
{{ range .View.Vendors }}
<tr{{ if not .Available }} class="row-alert"{{ end }}>
  <td>{{ .Service }}</td><td>{{ .Name }}</td><td>{{ if .Enabled }}yes{{ else }}no{{ end }}</td>
  <td>{{ if .Available }}<span class="dot dot-green"></span>ok{{ else }}<span class="dot dot-red"></span>out{{ if .UnavailableUntil }} until {{ .UnavailableUntil.Format "15:04:05" }}{{ end }} ({{ .ConsecutiveFailures }} failures){{ end }}</td>
  <td><form class="inline" method="post" action="/relayd/vendors/{{ .Service }}/{{ .Name }}">
    <input type="number" name="priority" value="{{ .Priority }}" style="width:70px"><button type="submit">Set</button></form></td>
  <td><form class="inline" method="post" action="/relayd/vendors/{{ .Service }}/{{ .Name }}/terms">
    <input type="number" name="revenue_bps" min="0" max="9999" value="{{ .RevenueBPS }}" style="width:70px">
    <input type="text" name="notes" value="{{ .Notes }}" placeholder="terms, limits, contact" style="width:160px"><button type="submit">Save</button></form></td>
  <td>{{ if .LastError }}{{ .LastError }}{{ end }}</td>
  <td><form class="inline" method="post" action="/relayd/vendors/{{ .Service }}/{{ .Name }}">
    {{ if .Enabled }}<input type="hidden" name="enabled" value="false"><button class="danger" type="submit">Disable</button>
    {{ else }}<input type="hidden" name="enabled" value="true"><button type="submit">Enable</button>{{ end }}</form></td>
</tr>
{{ end }}
</table>
<h2>Live prices{{ with .Prices }} for {{ .Amount }} USDT{{ end }}</h2>
{{ with .Prices }}
<table>
<tr><th>Direction</th><th>Vendor</th><th>Customer receives</th><th>Vendor fee</th></tr>
{{ range .BSCToTRON }}<tr><td>BSC → TRON</td><td>{{ index . "vendor" }}</td><td>{{ index . "customer_receives" }}</td><td>{{ index . "vendor_fee" }}{{ index . "error" }}</td></tr>{{ end }}
{{ range .TRONToBSC }}<tr><td>TRON → BSC</td><td>{{ index . "vendor" }}</td><td>{{ index . "customer_receives" }}</td><td>{{ index . "vendor_fee" }}{{ index . "error" }}</td></tr>{{ end }}
</table>
{{ if .Energy }}<table><tr><th>Energy vendor</th><th>Energy</th><th>Cost (TRX)</th></tr>
{{ range .Energy }}<tr><td>{{ index . "vendor" }}</td><td>{{ index . "units" }}</td><td>{{ index . "cost_trx" }}{{ index . "error" }}</td></tr>{{ end }}</table>{{ end }}
{{ else }}<p>Prices unavailable right now.</p>{{ end }}
`

type relaydVendorsData struct {
	flashData
	View   opclient.Vendors
	Prices *opclient.VendorPrices
}

func (s *Server) getRelaydVendors(w http.ResponseWriter, r *http.Request) {
	data := relaydVendorsData{flashData: s.newFlashData(r)}
	if s.Relayd == nil {
		data.Err = "relayd is not configured (OC_RELAYD_BASE_URL unset)"
	} else if v, err := s.Relayd.GetVendors(r.Context()); err != nil {
		data.Err = err.Error()
	} else {
		data.View = v
		if prices, err := s.Relayd.GetVendorPrices(r.Context(), "100"); err == nil {
			data.Prices = &prices
		}
	}
	s.Templates.Render(w, "relayd_vendors", data)
}

func (s *Server) postRelaydVendor(w http.ResponseWriter, r *http.Request) {
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
	s.relaydAction(w, r, "/relayd/vendors", "relayd.vendor.update", service+":"+name,
		map[string]any{"enabled": enabled, "priority": priority},
		func() error { return s.Relayd.PatchVendor(r.Context(), service, name, enabled, priority) })
}

func (s *Server) postRelaydVendorTerms(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	service, name := chi.URLParam(r, "service"), chi.URLParam(r, "name")
	revenue, _ := strconv.Atoi(r.FormValue("revenue_bps"))
	notes := strings.TrimSpace(r.FormValue("notes"))
	s.relaydAction(w, r, "/relayd/vendors", "relayd.vendor.terms", service+":"+name,
		map[string]any{"revenue_bps": revenue, "notes": notes},
		func() error { return s.Relayd.SetVendorTerms(r.Context(), service, name, revenue, notes) })
}

func (s *Server) postRelaydVendorStrategy(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	service, strategy := chi.URLParam(r, "service"), r.FormValue("strategy")
	s.relaydAction(w, r, "/relayd/vendors", "relayd.vendor.strategy", service, map[string]any{"strategy": strategy},
		func() error { return s.Relayd.PutVendorStrategy(r.Context(), service, strategy) })
}

const relaydSweepsContent = relaydNav + `
<h1>Profit &amp; sweeps</h1>` + flashSnippet + `
<div class="cards">{{ range $asset, $amount := .Wallets.UnsweptTotals }}<div class="card"><b>{{ $asset }}</b><br>{{ $amount }} unswept</div>{{ end }}</div>
<h2>Treasury wallets</h2>
{{ with .Treasury }}
<p>Gas and TRX top-ups come from whichever treasury can pay. Profit sweeps go to {{ .BSC.SweepTo }} (BSC) and {{ .TRON.SweepTo }} (TRON).</p>
<table>
<tr><th>Slot</th><th>BSC address</th><th>BSC balance</th><th>TRON address</th><th>TRON balance</th></tr>
{{ range .Wallets }}
<tr><td>{{ .SlotID }}</td><td>{{ .BSC }}</td>
<td>{{ with .BSCBalance }}{{ if .Error }}<span class="err">unavailable</span>{{ else }}{{ .Native }} BNB, {{ .USDT }} USDT{{ end }}{{ end }}</td>
<td>{{ .TRON }}</td>
<td>{{ with .TRONBalance }}{{ if .Error }}<span class="err">unavailable</span>{{ else }}{{ .Native }} TRX, {{ .USDT }} USDT{{ end }}{{ end }}</td></tr>
{{ end }}
</table>
{{ end }}
<p>Each deposit wallet keeps our profit from the orders it served. Once a wallet's profit reaches the minimum and no order is using it,
relayd sends it to the treasury in one transfer (on TRON, renting only the energy that transfer needs). Only profit recorded on settled
orders is ever swept -- anything else a wallet holds stays for an operator.</p>
{{ with .Settings }}
<form method="post" action="/relayd/sweeps/settings" class="card">
  <label><input type="checkbox" name="enabled" value="true"{{ if .Enabled }} checked{{ end }}> Sweep automatically</label>
  <label>Check every (minutes)</label><input type="number" name="interval_minutes" min="5" value="{{ .IntervalMinutes }}">
  <label>Least profit worth a BSC sweep (USDT)</label><input type="text" name="min_bep20" value="{{ index .MinAmount "USDT_BEP20" }}">
  <label>Least profit worth a TRON sweep (USDT)</label><input type="text" name="min_trc20" value="{{ index .MinAmount "USDT_TRC20" }}">
  <p><button type="submit">Save</button></p>
</form>
{{ end }}
<form method="post" action="/relayd/sweeps/run"><button type="submit">Check for sweeps now</button></form>
<h2>Deposit wallets</h2>
<table>
<tr><th>Chain</th><th>Address</th><th>Orders</th><th>Earned</th><th>Swept</th><th>Unswept</th><th>On chain now</th><th>State</th></tr>
{{ range .Wallets.Wallets }}
<tr><td>{{ .Chain }}</td><td>{{ .Address }}</td><td>{{ .Legs }}</td><td>{{ .Earned }}</td><td>{{ .Swept }}</td><td><b>{{ .Unswept }}</b></td>
<td>{{ with .OnChain }}{{ if .Error }}<span class="err">unavailable</span>{{ else }}{{ .USDT }} USDT, {{ .Native }} {{ .NativeFor }}{{ end }}{{ end }}</td>
<td>{{ if .SweepPending }}sweeping{{ else if .Busy }}in use{{ else if .LastFailedAt }}last sweep failed {{ .LastFailedAt.Format "2006-01-02 15:04" }}{{ else }}idle{{ end }}</td></tr>
{{ end }}
</table>
<h2>Recent sweeps</h2>
<table>
<tr><th>#</th><th>Chain</th><th>From</th><th>Amount</th><th>Status</th><th>Tx</th><th>Costs</th><th>Started</th></tr>
{{ range .Sweeps }}
<tr{{ if eq .Status "FAILED" }} class="row-alert"{{ end }}><td>{{ .ID }}</td><td>{{ .Chain }}</td><td>{{ .FromAddress }}</td><td>{{ .Amount }} {{ .Asset }}</td>
<td>{{ .Status }}{{ if .Error }}: {{ .Error }}{{ end }}</td><td>{{ if .TxHash }}{{ .TxHash }}{{ end }}</td>
<td>{{ with .Costs }}gas {{ .GasBNB }} BNB, TRX {{ .TopUpTRX }}, energy {{ .RentalsTRX }} TRX{{ end }}</td><td>{{ .CreatedAt.Format "2006-01-02 15:04" }}</td></tr>
{{ end }}
</table>
`

type relaydSweepsData struct {
	flashData
	Settings *opclient.SweepSettings
	Wallets  opclient.ProfitWallets
	Sweeps   []opclient.Sweep
	Treasury *opclient.Treasury
}

func (s *Server) getRelaydSweeps(w http.ResponseWriter, r *http.Request) {
	data := relaydSweepsData{flashData: s.newFlashData(r)}
	if s.Relayd == nil {
		data.Err = "relayd is not configured (OC_RELAYD_BASE_URL unset)"
		s.Templates.Render(w, "relayd_sweeps", data)
		return
	}
	var errs []string
	if settings, err := s.Relayd.GetSweepSettings(r.Context()); err != nil {
		errs = append(errs, err.Error())
	} else {
		data.Settings = &settings
	}
	var err error
	if data.Wallets, err = s.Relayd.GetProfitWallets(r.Context()); err != nil {
		errs = append(errs, err.Error())
	}
	if data.Sweeps, err = s.Relayd.ListSweeps(r.Context()); err != nil {
		errs = append(errs, err.Error())
	}
	if treasury, err := s.Relayd.GetTreasury(r.Context()); err != nil {
		errs = append(errs, err.Error())
	} else {
		data.Treasury = &treasury
	}
	if len(errs) > 0 {
		data.Err = strings.Join(errs, "; ")
	}
	s.Templates.Render(w, "relayd_sweeps", data)
}

func (s *Server) postRelaydSweepSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	interval, _ := strconv.ParseInt(r.FormValue("interval_minutes"), 10, 64)
	settings := opclient.SweepSettings{
		Enabled: r.FormValue("enabled") == "true", IntervalMinutes: interval,
		MinAmount: map[string]string{
			"USDT_BEP20": strings.TrimSpace(r.FormValue("min_bep20")), "USDT_TRC20": strings.TrimSpace(r.FormValue("min_trc20")),
		},
	}
	s.relaydAction(w, r, "/relayd/sweeps", "relayd.sweep.settings", "sweep",
		map[string]any{"enabled": settings.Enabled, "interval_minutes": interval, "min_amount": settings.MinAmount},
		func() error { return s.Relayd.PutSweepSettings(r.Context(), settings) })
}

func (s *Server) postRelaydSweepRun(w http.ResponseWriter, r *http.Request) {
	s.relaydAction(w, r, "/relayd/sweeps", "relayd.sweep.run", "sweep", nil,
		func() error { return s.Relayd.RunSweep(r.Context()) })
}

const relaydPoolContent = relaydNav + `
<h1>{{ .ChainName }} deposit wallets</h1>` + flashSnippet + `
<p>Customers are given a wallet from this pool; a wallet returns to it after a cooldown once its order is finished. Keeping the pool
small keeps profit in few wallets and sweeps cheap. A disabled wallet is never handed out again.</p>
{{ with .Settings }}
<form method="post" action="/relayd/pool/{{ $.Chain }}/settings" class="card">
  <label>Most wallets the pool may grow to (0 = never add a new one)</label><input type="number" name="max_wallets" min="0" value="{{ .MaxWallets }}">
  <label>Cooldown after a finished order (e.g. 30m)</label><input type="text" name="cooldown_after_use" value="{{ .CooldownAfterUse }}">
  <label>Cooldown after an unpaid, expired order (e.g. 6h)</label><input type="text" name="cooldown_after_expiry" value="{{ .CooldownAfterExpiry }}">
  <p><button type="submit">Save</button></p>
</form>
{{ end }}
<form method="post" action="/relayd/pool/{{ .Chain }}/wallets"><button type="submit">Add a wallet to the pool</button></form>
<table>
<tr><th>Address</th><th>Index</th><th>Status</th><th>Now</th><th>Order</th><th>Last used</th><th></th></tr>
{{ range .Wallets }}
<tr><td>{{ .Address }}</td><td>{{ .DerivationIndex }}</td><td>{{ .Status }}</td>
<td>{{ if .Lease }}in use{{ else if .Available }}available{{ else }}cooling down until {{ .AvailableAfter.Format "15:04" }}{{ end }}</td>
<td>{{ with .Lease }}<a href="/relayd/legs/{{ .ExternalID }}">{{ .ExternalID }}</a>{{ end }}</td>
<td>{{ if .LastLeasedAt }}{{ .LastLeasedAt.Format "2006-01-02 15:04" }}{{ end }}</td>
<td><form class="inline" method="post" action="/relayd/pool/{{ $.Chain }}/wallets/{{ .Address }}/{{ if eq .Status "ACTIVE" }}disable{{ else }}enable{{ end }}">
  <button{{ if eq .Status "ACTIVE" }} class="danger"{{ end }} type="submit">{{ if eq .Status "ACTIVE" }}Disable{{ else }}Enable{{ end }}</button></form></td></tr>
{{ end }}
</table>
`

type relaydPoolData struct {
	flashData
	Chain, ChainName string
	Settings         *opclient.PoolSettings
	Wallets          []opclient.PoolWallet
}

func poolChain(r *http.Request) (string, string, bool) {
	switch chi.URLParam(r, "chain") {
	case "bsc":
		return "bsc", "BSC", true
	case "tron":
		return "tron", "TRON", true
	}
	return "", "", false
}

func (s *Server) getRelaydPool(w http.ResponseWriter, r *http.Request) {
	chain, name, ok := poolChain(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data := relaydPoolData{flashData: s.newFlashData(r), Chain: chain, ChainName: name}
	if s.Relayd == nil {
		data.Err = "relayd is not configured (OC_RELAYD_BASE_URL unset)"
		s.Templates.Render(w, "relayd_pool", data)
		return
	}
	var errs []string
	if settings, err := s.Relayd.GetPoolSettings(r.Context(), chain); err != nil {
		errs = append(errs, err.Error())
	} else {
		data.Settings = &settings
	}
	var err error
	if data.Wallets, err = s.Relayd.ListPool(r.Context(), chain); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		data.Err = strings.Join(errs, "; ")
	}
	s.Templates.Render(w, "relayd_pool", data)
}

func (s *Server) postRelaydPoolSettings(w http.ResponseWriter, r *http.Request) {
	chain, _, ok := poolChain(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	max, _ := strconv.Atoi(r.FormValue("max_wallets"))
	settings := opclient.PoolSettings{MaxWallets: max, CooldownAfterUse: strings.TrimSpace(r.FormValue("cooldown_after_use")),
		CooldownAfterExpiry: strings.TrimSpace(r.FormValue("cooldown_after_expiry"))}
	s.relaydAction(w, r, "/relayd/pool/"+chain, "relayd.pool.settings", chain,
		map[string]any{"max_wallets": max, "cooldown_after_use": settings.CooldownAfterUse, "cooldown_after_expiry": settings.CooldownAfterExpiry},
		func() error { return s.Relayd.PutPoolSettings(r.Context(), chain, settings) })
}

func (s *Server) postRelaydPoolProvision(w http.ResponseWriter, r *http.Request) {
	chain, _, ok := poolChain(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.relaydAction(w, r, "/relayd/pool/"+chain, "relayd.pool.provision", chain, nil,
		func() error { return s.Relayd.ProvisionWallet(r.Context(), chain) })
}

func (s *Server) postRelaydPoolWallet(enable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chain, _, ok := poolChain(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		address := chi.URLParam(r, "address")
		action := "relayd.pool.disable"
		if enable {
			action = "relayd.pool.enable"
		}
		s.relaydAction(w, r, "/relayd/pool/"+chain, action, chain+":"+address, nil,
			func() error { return s.Relayd.SetPoolWallet(r.Context(), chain, address, enable) })
	}
}

const relaydLegContent = relaydNav + `
<h1>Order {{ .ExternalID }}</h1>` + flashSnippet + `
{{ with .Detail }}{{ with .Leg }}
<div class="cards">
  <div class="card"><b>Status</b><br>{{ .Status }}</div>
  <div class="card"><b>Direction</b><br>{{ .Direction }}</div>
  <div class="card"><b>Quoted</b><br>{{ .AmountIn }}</div>
  <div class="card"><b>Received</b><br>{{ if .ReceivedAmount }}{{ .ReceivedAmount }}{{ else }}-{{ end }}</div>
  <div class="card"><b>Our profit</b><br>{{ if .ProfitAmount }}{{ .ProfitAmount }}{{ else }}-{{ end }}{{ if .ProfitBPS }} ({{ .ProfitBPS }} bps){{ end }}</div>
  <div class="card"><b>Sent to vendor</b><br>{{ if .ForwardAmount }}{{ .ForwardAmount }}{{ else }}-{{ end }}</div>
  <div class="card"><b>Vendor fee</b><br>{{ if .VendorFeeAmount }}{{ .VendorFeeAmount }}{{ else }}-{{ end }}</div>
  <div class="card"><b>Customer received</b><br>{{ if .AmountOutActual }}{{ .AmountOutActual }}{{ else }}{{ .AmountOutExpected }} (expected){{ end }}</div>
</div>
<table>
<tr><th>Customer</th><td>{{ if .CustomerLabel }}{{ .CustomerLabel }} / {{ end }}{{ .CustomerID }}</td></tr>
<tr><th>Deposit wallet</th><td>{{ .DepositAddress }}</td></tr>
<tr><th>Paid from</th><td>{{ if .SenderAddress }}{{ .SenderAddress }}{{ end }}</td></tr>
<tr><th>Customer's destination</th><td>{{ .DestinationAddress }}</td></tr>
<tr><th>Vendor order</th><td>{{ if .UpstreamProviderName }}{{ .UpstreamProviderName }}: {{ end }}{{ if .UpstreamOrderID }}{{ .UpstreamOrderID }}{{ end }}</td></tr>
<tr><th>Vendor's payout transaction</th><td>{{ if .PayoutTxID }}{{ .PayoutTxID }}{{ else }}-{{ end }}</td></tr>
<tr><th>Created / updated</th><td>{{ .CreatedAt }} / {{ .UpdatedAt }}</td></tr>
</table>
{{ end }}
<h2>Transfers</h2>
<p>Resource costs: gas top-ups {{ .Costs.GasBNB }} BNB, TRX top-ups {{ .Costs.TopUpTRX }} TRX, energy rentals {{ .Costs.RentalsTRX }} TRX.</p>
<table>
<tr><th>Purpose</th><th>Chain</th><th>From</th><th>To</th><th>Amount</th><th>Status</th><th>Tx</th></tr>
{{ range .Costs.Transfers }}
<tr{{ if eq .Status "FAILED" }} class="row-alert"{{ end }}><td>{{ .Purpose }}</td><td>{{ .Chain }}</td><td>{{ .From }}</td><td>{{ .To }}</td><td>{{ .Amount }} {{ .Asset }}</td>
<td>{{ .Status }}{{ if .Failure }}: {{ .Failure }}{{ end }}</td><td>{{ if .TxHash }}{{ .TxHash }}{{ end }}</td></tr>
{{ end }}
</table>
<h2>Energy rentals</h2>
<table>
<tr><th>Resource</th><th>Units</th><th>Vendor</th><th>Status</th><th>Cost (TRX)</th></tr>
{{ range .Costs.Rentals }}
<tr><td>{{ .Resource }}</td><td>{{ .Units }}</td><td>{{ if .Provider }}{{ .Provider }}{{ end }}</td><td>{{ .Status }}{{ if .Error }}: {{ .Error }}{{ end }}</td><td>{{ if .CostTRX }}{{ .CostTRX }}{{ end }}</td></tr>
{{ end }}
</table>
{{ end }}
`

type relaydLegData struct {
	flashData
	ExternalID string
	Detail     *opclient.LegDetail
}

func (s *Server) getRelaydLeg(w http.ResponseWriter, r *http.Request) {
	data := relaydLegData{flashData: s.newFlashData(r), ExternalID: chi.URLParam(r, "external_id")}
	if s.Relayd == nil {
		data.Err = "relayd is not configured (OC_RELAYD_BASE_URL unset)"
	} else if detail, err := s.Relayd.GetLeg(r.Context(), data.ExternalID); err != nil {
		data.Err = err.Error()
	} else {
		data.Detail = &detail
	}
	s.Templates.Render(w, "relayd_leg", data)
}
