package httpapi

import (
	"net/http"

	"opsconsole/internal/opclient"
)

// relayLegsContent lists Model F's own relay legs -- read-only, unlike
// screening holds or dispatcher slots: R5's own automatic refund path
// (relayd/internal/orchestrate/refund.go) already handles every
// leg-level failure an operator could safely act on from here (a stuck
// forward attempt, or upstream.CreateOrder never once succeeding), and
// UNRECOVERABLE's own resolution is an off-system operational decision
// (a vendor support ticket, a compensation reserve -- see
// docs/03-build/model-f-relay-build-prompts.md's own "Open items"), not
// a button this console could correctly offer. This page's job is
// purely to make that state visible, not to act on it.
const relayLegsContent = `
<h1>Relay legs</h1>
{{ if .Error }}<div class="flash flash-error">{{ .Error }}</div>{{ end }}
<p>
  <a href="/relayd/legs">all</a> &middot;
  <a href="/relayd/legs?status=FORWARDING">forwarding</a> &middot;
  <a href="/relayd/legs?status=REFUND_PENDING">refund pending</a> &middot;
  <a href="/relayd/legs?status=UNRECOVERABLE">unrecoverable</a> &middot;
  <a href="/relayd/legs?status=SETTLED">settled</a> &middot;
  <a href="/relayd/legs?status=REFUNDED">refunded</a>
</p>
<table>
<tr><th>External ID</th><th>Order</th><th>Direction</th><th>Status</th><th>Customer</th>
    <th>Amount in</th><th>Amount out</th><th>Upstream order</th><th>Tx</th><th>Updated</th></tr>
{{ range .Legs }}
<tr{{ if eq .Status "UNRECOVERABLE" }} class="row-alert"{{ end }}>
  <td>{{ .ExternalID }}</td><td>{{ .OrderID }}</td><td>{{ .Direction }}</td>
  <td>{{ .Status }}</td><td>{{ .CustomerID }}</td>
  <td>{{ .AmountIn }}</td><td>{{ .AmountOut }}</td>
  <td>{{ .UpstreamOrder }}</td><td>{{ .Tx }}</td><td>{{ .UpdatedAt }}</td>
</tr>
{{ end }}
</table>
`

type relayLegRow struct {
	ExternalID    string
	OrderID       int64
	Direction     string
	Status        string
	CustomerID    string
	AmountIn      string
	AmountOut     string
	UpstreamOrder string
	Tx            string
	UpdatedAt     string
}

type relayLegsPageData struct {
	basePageData
	Legs  []relayLegRow
	Error string
}

func relayLegRowFrom(l opclient.RelayLeg) relayLegRow {
	amountOut := l.AmountOutExpected + " (expected)"
	if l.AmountOutActual != nil {
		amountOut = *l.AmountOutActual
	}
	upstreamOrder := "-"
	if l.UpstreamProviderName != nil && l.UpstreamOrderID != nil {
		upstreamOrder = *l.UpstreamProviderName + ": " + *l.UpstreamOrderID
	}
	tx := "-"
	if l.RefundTxID != nil {
		tx = "refund: " + *l.RefundTxID
	} else if l.ForwardTxID != nil {
		tx = "forward: " + *l.ForwardTxID
	}
	return relayLegRow{
		ExternalID: l.ExternalID, OrderID: l.OrderID, Direction: l.Direction, Status: l.Status,
		CustomerID: l.CustomerID, AmountIn: l.AmountIn, AmountOut: amountOut,
		UpstreamOrder: upstreamOrder, Tx: tx, UpdatedAt: l.UpdatedAt,
	}
}

func (s *Server) getRelayLegs(w http.ResponseWriter, r *http.Request) {
	data := relayLegsPageData{basePageData: s.newBasePageData(r)}
	if s.Relayd == nil {
		data.Error = "relayd is not configured on this ops console deployment (OC_RELAYD_BASE_URL unset)"
		s.Templates.Render(w, "relay_legs", data)
		return
	}
	legs, err := s.Relayd.ListRelayLegs(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		data.Error = err.Error()
	}
	for _, l := range legs {
		data.Legs = append(data.Legs, relayLegRowFrom(l))
	}
	s.Templates.Render(w, "relay_legs", data)
}
