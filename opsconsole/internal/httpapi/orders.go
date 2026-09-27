package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/opclient"
)

const ordersPerPage = 50

// orderTabs group order statuses the way an operator thinks about them.
var orderTabs = []struct {
	Key, Label string
	Statuses   []string
}{
	{"all", "All", nil},
	{"waiting", "Waiting for deposit", []string{"AWAITING_DEPOSIT"}},
	{"active", "In progress", []string{"FORWARDING", "FORWARDED", "REFUND_PENDING"}},
	{"attention", "Needs attention", nil},
	{"completed", "Completed", []string{"SETTLED"}},
	{"refunded", "Refunded", []string{"REFUNDED"}},
	{"expired", "Expired", []string{"EXPIRED"}},
	{"failed", "Failed", []string{"FAILED", "UNRECOVERABLE"}},
}

type orderTab struct {
	Key, Label, URL string
	Count           int
	Active          bool
}

type ordersData struct {
	basePageData
	Tabs             []orderTab
	Tab, Status      string
	Query, Dir       string
	Orders           []opclient.RelayLeg
	Page, Pages      int
	Total            int
	PrevURL, NextURL string
}

func (s *Server) getOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := ordersData{basePageData: s.page(r, "orders", "Orders"), Tab: q.Get("tab"), Status: q.Get("status"),
		Query: strings.TrimSpace(q.Get("q")), Dir: q.Get("dir")}
	if data.Tab == "" {
		data.Tab = "all"
	}
	if s.Relayd == nil {
		data.Err = errRelaydMissing
		s.Templates.Render(w, "orders", data)
		return
	}
	var legs []opclient.RelayLeg
	var stats opclient.Stats
	var legsErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); legs, legsErr = s.Relayd.ListRelayLegs(r.Context(), "") }()
	go func() { defer wg.Done(); stats, _ = s.Relayd.GetStats(r.Context()) }()
	wg.Wait()
	if legsErr != nil {
		data.Err = legsErr.Error()
	}
	attention := map[string]bool{}
	for _, a := range stats.Attention {
		attention[a.ExternalID] = true
	}

	inTab := func(key string, l opclient.RelayLeg) bool {
		for _, t := range orderTabs {
			if t.Key != key {
				continue
			}
			if key == "attention" {
				return attention[l.ExternalID]
			}
			if t.Statuses == nil {
				return true
			}
			for _, st := range t.Statuses {
				if l.Status == st {
					return true
				}
			}
		}
		return false
	}
	matches := func(l opclient.RelayLeg) bool {
		if data.Dir != "" && l.Direction != data.Dir {
			return false
		}
		if data.Query == "" {
			return true
		}
		needle := strings.ToLower(data.Query)
		for _, hay := range []string{l.ExternalID, strconv.FormatInt(l.OrderID, 10), l.DepositAddress, l.DestinationAddress,
			l.CustomerID, deref(l.CustomerLabel), deref(l.SenderAddress), deref(l.UpstreamOrderID)} {
			if strings.Contains(strings.ToLower(hay), needle) {
				return true
			}
		}
		return false
	}

	sort.Slice(legs, func(i, j int) bool { return parseTime(legs[i].CreatedAt).After(parseTime(legs[j].CreatedAt)) })
	counts := map[string]int{}
	var shown []opclient.RelayLeg
	for _, l := range legs {
		if !matches(l) {
			continue
		}
		for _, t := range orderTabs {
			if inTab(t.Key, l) {
				counts[t.Key]++
			}
		}
		if data.Status != "" {
			if l.Status == data.Status {
				shown = append(shown, l)
			}
		} else if inTab(data.Tab, l) {
			shown = append(shown, l)
		}
	}
	for _, t := range orderTabs {
		v := url.Values{}
		if t.Key != "all" {
			v.Set("tab", t.Key)
		}
		if data.Query != "" {
			v.Set("q", data.Query)
		}
		if data.Dir != "" {
			v.Set("dir", data.Dir)
		}
		data.Tabs = append(data.Tabs, orderTab{Key: t.Key, Label: t.Label, Count: counts[t.Key], URL: "/orders?" + v.Encode(),
			Active: data.Status == "" && data.Tab == t.Key})
	}

	data.Total = len(shown)
	data.Pages = (data.Total + ordersPerPage - 1) / ordersPerPage
	data.Page, _ = strconv.Atoi(q.Get("page"))
	if data.Page < 1 {
		data.Page = 1
	}
	if data.Pages > 0 && data.Page > data.Pages {
		data.Page = data.Pages
	}
	start := (data.Page - 1) * ordersPerPage
	end := min(start+ordersPerPage, data.Total)
	if start < end {
		data.Orders = shown[start:end]
	}
	pageURL := func(n int) string {
		v := r.URL.Query()
		v.Set("page", strconv.Itoa(n))
		return "/orders?" + v.Encode()
	}
	if data.Page > 1 {
		data.PrevURL = pageURL(data.Page - 1)
	}
	if data.Page < data.Pages {
		data.NextURL = pageURL(data.Page + 1)
	}
	s.Templates.Render(w, "orders", data)
}

// orderStep is one step of an order's story, top to bottom.
type orderStep struct {
	State string // done, current, failed, skipped, pending
	Title string
	Lines []stepLine
}

// stepLine is one fact on a step, rendered by kind: a transaction, an
// address, a time, or plain text.
type stepLine struct {
	Label, Text string
	Addr        string
	Tx, Chain   string
	Time        time.Time
}

type orderData struct {
	basePageData
	ExternalID         string
	Leg                *opclient.RelayLeg
	Costs              opclient.JobCosts
	Steps              []orderStep
	SrcChain, DstChain string
	Created            time.Time
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "external_id")
	data := orderData{basePageData: s.page(r, "orders", "Order"), ExternalID: id}
	data.Subtitle = id
	if s.Relayd == nil {
		data.Err = errRelaydMissing
		s.Templates.Render(w, "order", data)
		return
	}
	detail, err := s.Relayd.GetLeg(r.Context(), id)
	if err != nil {
		data.Err = err.Error()
		s.Templates.Render(w, "order", data)
		return
	}
	leg := detail.Leg
	data.Leg, data.Costs = &leg, detail.Costs
	data.SrcChain, data.DstChain = srcChain(leg.Direction), dstChain(leg.Direction)
	data.Created = parseTime(leg.CreatedAt)
	data.Title = "Order " + short(leg.ExternalID)
	if leg.Status == "AWAITING_DEPOSIT" || leg.Status == "FORWARDING" || leg.Status == "FORWARDED" || leg.Status == "REFUND_PENDING" {
		data.Refresh = 20
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data.Steps = orderSteps(leg, detail.Costs, s.orderDeposits(ctx, leg))
	s.Templates.Render(w, "order", data)
}

// orderDeposits asks the watcher on the order's sending chain which
// deposits it recorded.
func (s *Server) orderDeposits(ctx context.Context, leg opclient.RelayLeg) []opclient.Deposit {
	var d []opclient.Deposit
	if srcChain(leg.Direction) == "BSC" && s.Watcher != nil {
		d, _ = s.Watcher.OrderDeposits(ctx, leg.OrderID)
	} else if srcChain(leg.Direction) == "TRON" && s.Tronwatcher != nil {
		d, _ = s.Tronwatcher.OrderDeposits(ctx, leg.OrderID)
	}
	return d
}

// orderSteps tells an order's story from what relayd and the watcher
// recorded.
func orderSteps(l opclient.RelayLeg, costs opclient.JobCosts, deposits []opclient.Deposit) []orderStep {
	src, dst := srcChain(l.Direction), dstChain(l.Direction)
	received := l.ReceivedAmount != nil
	final := map[string]bool{"SETTLED": true, "REFUNDED": true, "EXPIRED": true, "FAILED": true, "UNRECOVERABLE": true}[l.Status]

	var forward, refund *transferView
	var fees []stepLine
	for _, t := range costs.Transfers {
		tv := transferView{Purpose: t.Purpose, Chain: t.Chain, To: t.To, Amount: t.Amount, Asset: t.Asset, Status: t.Status, Tx: deref(t.TxHash), Failure: deref(t.Failure)}
		switch t.Purpose {
		case "FORWARD":
			if forward == nil || t.Status == "CONFIRMED" {
				forward = &tv
			}
		case "REFUND":
			if refund == nil || t.Status == "CONFIRMED" {
				refund = &tv
			}
		case "GAS_TOPUP", "TRX_TOPUP":
			label := "Gas top-up"
			if t.Purpose == "TRX_TOPUP" {
				label = "TRX top-up"
			}
			fees = append(fees, stepLine{Label: label + ", " + num(t.Amount) + " " + t.Asset + " (" + strings.ToLower(statusLabel(t.Status)) + ")", Tx: tv.Tx, Chain: t.Chain})
		}
	}
	for _, rent := range costs.Rentals {
		line := stepLine{Label: "Energy rental", Text: strconv.FormatInt(rent.Units, 10) + " energy"}
		if rent.Provider != nil {
			line.Text += " from " + *rent.Provider
		}
		if rent.CostTRX != nil {
			line.Text += " for " + num(*rent.CostTRX) + " TRX"
		}
		line.Text += " (" + strings.ToLower(statusLabel(rent.Status)) + ")"
		fees = append(fees, line)
	}

	steps := []orderStep{{State: "done", Title: "Order created", Lines: []stepLine{
		{Label: "When", Time: parseTime(l.CreatedAt)},
		{Label: "Quote", Text: "send " + num(l.AmountIn) + " USDT on " + src + ", receive ≈ " + num(l.AmountOutExpected) + " USDT on " + dst},
		{Label: "Customer", Text: firstNonEmpty(deref(l.CustomerLabel), l.CustomerID)},
	}}}

	dep := orderStep{Title: "Customer's deposit"}
	switch {
	case received:
		dep.State = "done"
		dep.Lines = append(dep.Lines, stepLine{Label: "Received", Text: num(*l.ReceivedAmount) + " USDT"})
		if l.SenderAddress != nil {
			dep.Lines = append(dep.Lines, stepLine{Label: "From", Addr: *l.SenderAddress})
		}
		for _, d := range deposits {
			dep.Lines = append(dep.Lines, stepLine{Label: "Transaction", Tx: d.TxHash, Chain: src})
		}
		dep.Lines = append(dep.Lines, stepLine{Label: "Into deposit wallet", Addr: l.DepositAddress})
	case l.Status == "AWAITING_DEPOSIT":
		dep.State = "current"
		dep.Lines = []stepLine{{Label: "Waiting for", Text: num(l.AmountIn) + " USDT on " + src}, {Label: "At", Addr: l.DepositAddress}}
	default:
		dep.State = "skipped"
		dep.Lines = []stepLine{{Text: "No payment arrived before the deadline."}, {Label: "Deposit wallet", Addr: l.DepositAddress}}
	}
	steps = append(steps, dep)
	if !received {
		if l.Status == "EXPIRED" {
			steps = append(steps, orderStep{State: "skipped", Title: "Expired", Lines: []stepLine{{Text: "The customer didn't pay in time; nothing was moved."}}})
		}
		return steps
	}

	ex := orderStep{Title: "Exchange order"}
	if l.UpstreamOrderID != nil {
		ex.State = "done"
		ex.Lines = []stepLine{{Label: "Exchange", Text: deref(l.UpstreamProviderName)}, {Label: "Order", Text: *l.UpstreamOrderID}}
		if l.UpstreamDepositAddress != nil {
			ex.Lines = append(ex.Lines, stepLine{Label: "Exchange's deposit address", Addr: *l.UpstreamDepositAddress})
		}
	} else if final {
		ex.State = "skipped"
		ex.Lines = []stepLine{{Text: "No exchange order was opened."}}
	} else {
		ex.State = "current"
		ex.Lines = []stepLine{{Text: "Screening the order and opening an exchange order."}}
	}
	steps = append(steps, ex)

	feeStep := orderStep{Title: "Network fees", Lines: fees}
	switch {
	case len(fees) > 0:
		feeStep.State = "done"
	case forward != nil:
		feeStep.State = "done"
		feeStep.Lines = []stepLine{{Text: "Nothing needed: the deposit wallet already had enough."}}
	case final:
		feeStep.State = "skipped"
	default:
		feeStep.State = "pending"
	}
	steps = append(steps, feeStep)

	fwd := orderStep{Title: "Sent to the exchange"}
	switch {
	case forward != nil && forward.Status == "CONFIRMED":
		fwd.State = "done"
		fwd.Lines = []stepLine{{Label: "Amount", Text: num(firstNonEmpty(deref(l.ForwardAmount), forward.Amount)) + " USDT"}, {Label: "Transaction", Tx: forward.Tx, Chain: src}}
	case forward != nil && (forward.Status == "FAILED" || forward.Status == "DROPPED"):
		fwd.State = "failed"
		fwd.Lines = []stepLine{{Label: "Failed", Text: firstNonEmpty(forward.Failure, statusLabel(forward.Status))}, {Label: "Transaction", Tx: forward.Tx, Chain: src}}
	case forward != nil:
		fwd.State = "current"
		fwd.Lines = []stepLine{{Label: "Status", Text: statusLabel(forward.Status)}, {Label: "Transaction", Tx: forward.Tx, Chain: src}}
	case final:
		fwd.State = "skipped"
	default:
		fwd.State = "pending"
	}
	steps = append(steps, fwd)

	pay := orderStep{Title: "Payout to the customer"}
	switch {
	case l.PayoutTxID != nil || l.Status == "SETTLED":
		pay.State = "done"
		pay.Lines = []stepLine{{Label: "Customer received", Text: num(firstNonEmpty(deref(l.AmountOutActual), l.AmountOutExpected)) + " USDT on " + dst}}
		if l.PayoutTxID != nil {
			pay.Lines = append(pay.Lines, stepLine{Label: "Transaction", Tx: *l.PayoutTxID, Chain: dst})
		}
		pay.Lines = append(pay.Lines, stepLine{Label: "To", Addr: l.DestinationAddress})
	case l.Status == "FORWARDED":
		pay.State = "current"
		pay.Lines = []stepLine{{Text: "Waiting for the exchange to pay out, then checking it on-chain."}, {Label: "To", Addr: l.DestinationAddress}}
	case l.Status == "UNRECOVERABLE":
		pay.State = "failed"
		pay.Lines = []stepLine{{Text: "The exchange didn't complete after our transfer landed. Contact the exchange with the order number above."}}
	case final:
		pay.State = "skipped"
	default:
		pay.State = "pending"
	}
	steps = append(steps, pay)

	switch l.Status {
	case "SETTLED":
		lines := []stepLine{{Text: "The payout was verified on-chain."}}
		if l.ProfitAmount != nil {
			lines = append(lines, stepLine{Label: "Our fee", Text: num(*l.ProfitAmount) + " USDT, kept in the deposit wallet until the next sweep"})
		}
		steps = append(steps, orderStep{State: "done", Title: "Completed", Lines: lines})
	case "REFUND_PENDING", "REFUNDED":
		st := orderStep{State: "current", Title: "Refunding the customer"}
		if l.Status == "REFUNDED" {
			st.State, st.Title = "done", "Refunded"
		}
		if refund != nil {
			st.Lines = []stepLine{{Label: "Amount", Text: num(refund.Amount) + " " + refund.Asset}, {Label: "Transaction", Tx: refund.Tx, Chain: refund.Chain}, {Label: "To", Addr: refund.To}}
		}
		steps = append(steps, st)
	case "FAILED":
		steps = append(steps, orderStep{State: "failed", Title: "Failed", Lines: []stepLine{{Text: "The order stopped; check the transfers below."}}})
	}
	return steps
}

type transferView struct {
	Purpose, Chain, To, Amount, Asset, Status, Tx, Failure string
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
