package httpapi

import (
	"fmt"
	"html/template"
	"net/http"
)

// Templates holds every parsed page template. Server-rendered HTML via
// html/template only -- no frontend build step, matching this
// project's convention of introducing no new tooling for one
// operator-facing dashboard (see §0's own STACK note).
type Templates struct {
	pages map[string]*template.Template
}

const layout = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Ops Console</title>
<style>
  body { font-family: -apple-system, sans-serif; margin: 0; background: #f7f7f8; color: #1a1a1a; }
  header { background: #1a1a1a; color: #fff; padding: 12px 20px; display: flex; justify-content: space-between; align-items: center; }
  header a { color: #fff; text-decoration: none; margin-right: 16px; }
  header a:hover { text-decoration: underline; }
  .halt-banner { background: #b00020; color: #fff; padding: 10px 20px; font-weight: bold; }
  main { padding: 20px; max-width: 1100px; margin: 0 auto; }
  table { border-collapse: collapse; width: 100%; background: #fff; }
  th, td { border: 1px solid #ddd; padding: 8px 10px; text-align: left; font-size: 14px; }
  th { background: #eee; }
  .card { background: #fff; border: 1px solid #ddd; border-radius: 6px; padding: 14px; margin-bottom: 12px; }
  .cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 12px; }
  .dot { display: inline-block; width: 10px; height: 10px; border-radius: 50%; margin-right: 6px; }
  .dot-green { background: #2e7d32; }
  .dot-red { background: #c62828; }
  .err { color: #b00020; }
  form.inline { display: inline; }
  button, input[type=submit] { cursor: pointer; padding: 6px 12px; border-radius: 4px; border: 1px solid #ccc; background: #fff; }
  button.danger { background: #b00020; color: #fff; border-color: #b00020; }
  input[type=text], input[type=password], input[type=number], textarea { padding: 6px; border: 1px solid #ccc; border-radius: 4px; width: 100%; box-sizing: border-box; }
  label { display: block; margin: 10px 0 4px; font-weight: bold; font-size: 13px; }
  .flash { padding: 10px; margin-bottom: 12px; border-radius: 4px; }
  .flash-error { background: #fde7e9; color: #b00020; }
  .flash-ok { background: #e6f4ea; color: #1e4620; }
  .row-alert { background: #fde7e9; }
</style>
</head>
<body>
{{ if .Session }}
<header>
  <div><a href="/">Home</a><a href="/ledger/halt">Ledger</a><a href="/watcher/cursor">Watcher</a><a href="/broker/reservations?status=FAILED">Broker</a><a href="/screening/holds">Screening</a><a href="/dispatcher/slots">Dispatcher</a><a href="/s1/approvals">S1</a><a href="/relayd/legs">Relayd</a><a href="/audit">Audit</a></div>
  <div>{{ .Session.DisplayName }} &middot; <form class="inline" method="post" action="/logout"><button>Log out</button></form></div>
</header>
{{ if .Halted }}<div class="halt-banner">LEDGER HALTED: {{ .HaltReason }} -- <a href="/ledger/halt" style="color:#fff">manage</a></div>{{ end }}
{{ end }}
<main>
{{ template "content" . }}
</main>
</body>
</html>`

// MustLoadTemplates parses every page template against layout, panicking
// on a template error -- this happens once at startup, the same "fail
// loud before serving traffic" posture every required-config check in
// this project takes.
func MustLoadTemplates() *Templates {
	pages := map[string]string{
		"login":               loginContent,
		"home":                homeContent,
		"ledger_halt":         ledgerHaltContent,
		"watcher_cursor":      watcherCursorContent,
		"broker_reservations": brokerReservationsContent,
		"broker_reconcile":    brokerReconcileContent,
		"broker_fallback":     brokerFallbackContent,
		"screening_holds":     screeningHoldsContent,
		"dispatcher_slots":    dispatcherSlotsContent,
		"s1_approvals":        s1ApprovalsContent,
		"relay_legs":          relayLegsContent,
		"audit":               auditContent,
	}
	t := &Templates{pages: make(map[string]*template.Template, len(pages))}
	for name, content := range pages {
		tmpl := template.New(name)
		tmpl = template.Must(tmpl.Parse(layout))
		tmpl = template.Must(tmpl.Parse(`{{ define "content" }}` + content + `{{ end }}`))
		t.pages[name] = tmpl
	}
	return t
}

// Render writes page's template to w with data.
func (t *Templates) Render(w http.ResponseWriter, page string, data any) {
	tmpl, ok := t.pages[page]
	if !ok {
		http.Error(w, fmt.Sprintf("opsconsole: unknown template %q", page), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
