package httpapi

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// The admin panel's pages: server-rendered html/template, one stylesheet
// and one small script, all embedded -- no frontend build step.

//go:embed ui/layout.html ui/components.html ui/pages/*.html
var uiTemplates embed.FS

//go:embed ui/static
var uiStatic embed.FS

// Templates holds every parsed page.
type Templates struct {
	pages map[string]*template.Template
}

// MustLoadTemplates parses every page against the layout and shared
// components, panicking on a template error -- once, at startup.
func MustLoadTemplates() *Templates {
	read := func(name string) string {
		b, err := uiTemplates.ReadFile(name)
		if err != nil {
			panic(err)
		}
		return string(b)
	}
	layout, components := read("ui/layout.html"), read("ui/components.html")
	files, err := fs.Glob(uiTemplates, "ui/pages/*.html")
	if err != nil {
		panic(err)
	}
	t := &Templates{pages: make(map[string]*template.Template, len(files))}
	for _, file := range files {
		name := strings.TrimSuffix(path.Base(file), ".html")
		page := template.New(name).Funcs(templateFuncs)
		template.Must(page.Parse(`{{ template "layout" . }}`))
		template.Must(page.Parse(layout))
		template.Must(page.Parse(components))
		template.Must(page.Parse(read(file)))
		t.pages[name] = page
	}
	return t
}

// Render writes page with data.
func (t *Templates) Render(w http.ResponseWriter, page string, data any) {
	tmpl, ok := t.pages[page]
	if !ok {
		http.Error(w, fmt.Sprintf("opsconsole: unknown page %q", page), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// staticHandler serves the stylesheet and script.
func staticHandler() http.Handler {
	sub, err := fs.Sub(uiStatic, "ui/static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(w, r)
	})
}

var templateFuncs = template.FuncMap{
	"short":       short,
	"addrURL":     addrURL,
	"txURL":       txURL,
	"statusLabel": statusLabel,
	"statusTone":  statusTone,
	"srcChain":    srcChain,
	"dstChain":    dstChain,
	"lower":       strings.ToLower,
	"deref":       deref,
	"derefFloat": func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	},
	"derefInt": func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	},
	"num":       num,
	"bps":       bps,
	"rfc3339":   func(t time.Time) string { return t.Format(time.RFC3339) },
	"fmtTime":   func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") },
	"parseTime": parseTime,
	"dict":      dict,
	"add":       func(a, b int) int { return a + b },
	"list":      func(items ...string) []string { return items },
}

// short abbreviates a long address or hash: 0x1234…abcd.
func short(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:8] + "…" + s[len(s)-6:]
}

// chainOf tells which chain an address or transaction hash belongs to.
func chainOf(s string) string {
	switch {
	case strings.HasPrefix(s, "0x"):
		return "BSC"
	case strings.HasPrefix(s, "T") && len(s) == 34:
		return "TRON"
	case len(s) == 64:
		return "TRON" // a TRON transaction id is bare hex
	}
	return ""
}

func addrURL(addr string) string {
	switch chainOf(addr) {
	case "BSC":
		return "https://bscscan.com/address/" + addr
	case "TRON":
		return "https://tronscan.org/#/address/" + addr
	}
	return "#"
}

// txURL links a transaction on its explorer; chain may be "" to guess
// from the hash's shape.
func txURL(chain, hash string) string {
	if chain == "" {
		chain = chainOf(hash)
	}
	switch strings.ToUpper(chain) {
	case "BSC":
		if !strings.HasPrefix(hash, "0x") {
			hash = "0x" + hash
		}
		return "https://bscscan.com/tx/" + hash
	case "TRON":
		return "https://tronscan.org/#/transaction/" + strings.TrimPrefix(hash, "0x")
	}
	return "#"
}

var statuses = map[string]struct{ label, tone string }{
	// orders
	"AWAITING_DEPOSIT": {"Waiting for deposit", "info"},
	"FORWARDING":       {"Processing", "info"},
	"FORWARDED":        {"With the exchange", "info"},
	"SETTLED":          {"Completed", "ok"},
	"EXPIRED":          {"Expired", "neutral"},
	"REFUND_PENDING":   {"Refunding", "warn"},
	"REFUNDED":         {"Refunded", "neutral"},
	"FAILED":           {"Failed", "bad"},
	"UNRECOVERABLE":    {"Needs an operator", "bad"},
	// transfers, rentals, sweeps
	"BUILT": {"Built", "info"}, "SIGNED": {"Signed", "info"}, "BROADCAST": {"Sent", "info"},
	"CONFIRMED": {"Confirmed", "ok"}, "DROPPED": {"Dropped", "neutral"}, "ABANDONED": {"Abandoned", "neutral"},
	"PENDING": {"Pending", "warn"},
	// screening holds, wallets, deposits
	"OPEN": {"Open", "warn"}, "RELEASED": {"Released", "ok"}, "REJECTED": {"Rejected", "bad"},
	"ACTIVE": {"Active", "ok"}, "DISABLED": {"Disabled", "neutral"}, "RETIRED": {"Retired", "neutral"},
	"DETECTED": {"Confirming", "info"}, "REPORTED": {"Received", "ok"}, "ORPHANED": {"Unmatched", "bad"},
}

func statusLabel(s string) string {
	if v, ok := statuses[s]; ok {
		return v.label
	}
	if s == "" {
		return "—"
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(strings.ReplaceAll(s[1:], "_", " "))
}

func statusTone(s string) string {
	if v, ok := statuses[s]; ok {
		return v.tone
	}
	return "neutral"
}

func srcChain(direction string) string {
	if strings.HasPrefix(direction, "TRC20") {
		return "TRON"
	}
	return "BSC"
}

func dstChain(direction string) string {
	if strings.HasPrefix(direction, "TRC20") {
		return "BSC"
	}
	return "TRON"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// num trims a decimal's trailing zeros: "2.000000" -> "2".
func num(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// bps renders basis points as a percentage: 25 -> "0.25%".
func bps(b int64) string {
	return num(fmt.Sprintf("%.2f", float64(b)/100)) + "%"
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// dict builds a map from key/value pairs, for passing several values to a
// shared template.
func dict(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}
