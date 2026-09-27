package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"opsconsole/internal/opclient"
)

// Thresholds for the overview's balance warnings.
const (
	// lowTreasuryBNB is about five gas top-ups at today's BSC fees.
	lowTreasuryBNB = 0.00005
	// gasTopUpBNB is roughly what one top-up takes from the treasury.
	gasTopUpBNB = 0.000011
	// lowTreasuryTRX is what activating one new TRON deposit wallet costs.
	lowTreasuryTRX = 1.2
	// watcherLagWarn/Bad are how many BSC blocks behind the chain the
	// watcher may fall before it's worth a warning (about 5 min / 1 h).
	watcherLagWarn = 400
	watcherLagBad  = 4800
)

type overviewData struct {
	basePageData
	GeneratedAt time.Time
	StatsOK     bool
	Attention   []attentionItem
	Day, Week   opclient.StatsPeriod
	Month, All  opclient.StatsPeriod
	ByStatus    []statusCount
	Recent      []opclient.RelayLeg
	Treasury    *opclient.Treasury
	Energy      []map[string]string
	Unswept     map[string]string
	Services    []serviceHealth
	BSCLag      *int64
}

// attentionItem is one line of the overview's "needs attention" list.
type attentionItem struct {
	Tone, Title, Detail, Link, LinkText string
}

type statusCount struct {
	Status string
	Count  int
}

type serviceHealth struct {
	Name, Detail, Error string
	Healthy             bool
}

var statusOrder = []string{"AWAITING_DEPOSIT", "FORWARDING", "FORWARDED", "REFUND_PENDING", "SETTLED", "REFUNDED", "EXPIRED", "FAILED", "UNRECOVERABLE"}

func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	data := overviewData{basePageData: s.page(r, "overview", "Overview"), GeneratedAt: time.Now()}
	data.Refresh = 60
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	var (
		stats                *opclient.Stats
		vendors              *opclient.Vendors
		legs                 []opclient.RelayLeg
		unmatched, holds, ap int
		mu                   sync.Mutex
		wg                   sync.WaitGroup
	)
	async := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	if s.Relayd != nil {
		async(func() {
			if st, err := s.Relayd.GetStats(ctx); err == nil {
				mu.Lock()
				stats = &st
				mu.Unlock()
			}
		})
		async(func() {
			if t, err := s.Relayd.GetTreasury(ctx); err == nil {
				mu.Lock()
				data.Treasury = &t
				mu.Unlock()
			}
		})
		async(func() {
			if e, err := s.Relayd.GetEnergy(ctx); err == nil {
				mu.Lock()
				data.Energy = e
				mu.Unlock()
			}
		})
		async(func() {
			if v, err := s.Relayd.GetVendors(ctx); err == nil {
				mu.Lock()
				vendors = &v
				mu.Unlock()
			}
		})
		async(func() {
			if pw, err := s.Relayd.GetProfitWallets(ctx); err == nil {
				mu.Lock()
				data.Unswept = pw.UnsweptTotals
				mu.Unlock()
			}
		})
		async(func() {
			if l, err := s.Relayd.ListRelayLegs(ctx, ""); err == nil {
				mu.Lock()
				legs = l
				mu.Unlock()
			}
		})
	}
	for _, list := range []func(context.Context, bool) ([]opclient.OrphanedDeposit, error){s.orphanedBSC(), s.orphanedTRON()} {
		if list == nil {
			continue
		}
		list := list
		async(func() {
			if d, err := list(ctx, false); err == nil {
				mu.Lock()
				unmatched += len(d)
				mu.Unlock()
			}
		})
	}
	if s.Screening != nil {
		async(func() {
			if h, err := s.Screening.ListHolds(ctx, "OPEN"); err == nil {
				mu.Lock()
				holds = len(h)
				mu.Unlock()
			}
		})
	}
	if s.S1 != nil {
		async(func() {
			if p, err := s.S1.ListPendingApprovals(ctx); err == nil {
				mu.Lock()
				ap = len(p)
				mu.Unlock()
			}
		})
	}
	var services []serviceHealth
	async(func() {
		svc, lag := s.collectHealth(ctx)
		mu.Lock()
		services, data.BSCLag = svc, lag
		mu.Unlock()
	})
	wg.Wait()
	data.Services = services

	if stats != nil {
		data.StatsOK = true
		for _, p := range stats.Periods {
			switch p.Name {
			case "24h":
				data.Day = p
			case "7d":
				data.Week = p
			case "30d":
				data.Month = p
			case "all":
				data.All = p
			}
		}
		for _, st := range statusOrder {
			if n := stats.OrdersByStatus[st]; n > 0 {
				data.ByStatus = append(data.ByStatus, statusCount{st, n})
			}
		}
	}
	sort.Slice(legs, func(i, j int) bool { return parseTime(legs[i].CreatedAt).After(parseTime(legs[j].CreatedAt)) })
	if len(legs) > 8 {
		legs = legs[:8]
	}
	data.Recent = legs

	data.Attention = buildAttention(data, stats, vendors, unmatched, holds, ap)
	s.setWarnings(len(data.Attention))
	data.Counts.Warnings = len(data.Attention)
	s.Templates.Render(w, "overview", data)
}

func (s *Server) orphanedBSC() func(context.Context, bool) ([]opclient.OrphanedDeposit, error) {
	if s.Watcher == nil {
		return nil
	}
	return s.Watcher.ListOrphanedDeposits
}

func (s *Server) orphanedTRON() func(context.Context, bool) ([]opclient.OrphanedDeposit, error) {
	if s.Tronwatcher == nil {
		return nil
	}
	return s.Tronwatcher.ListOrphanedDeposits
}

// buildAttention turns everything the overview read into the short list
// of things an operator should act on, worst first.
func buildAttention(d overviewData, stats *opclient.Stats, vendors *opclient.Vendors, unmatched, holds, approvals int) []attentionItem {
	var bad, warn []attentionItem
	for _, svc := range d.Services {
		if !svc.Healthy {
			bad = append(bad, attentionItem{Tone: "bad", Title: svc.Name + " is not answering", Detail: svc.Error, Link: "/system", LinkText: "System"})
		}
	}
	if stats == nil {
		warn = append(warn, attentionItem{Tone: "warn", Title: "Order figures are unavailable", Detail: "relayd did not answer in time."})
	} else if n := len(stats.Attention); n > 3 {
		bad = append(bad, attentionItem{Tone: "bad", Title: fmt.Sprintf("%d orders need attention", n), Link: "/orders?tab=attention", LinkText: "Review them"})
	} else {
		for _, a := range stats.Attention {
			tone := "warn"
			if a.Status == "UNRECOVERABLE" || a.Status == "FAILED" {
				tone = "bad"
			}
			item := attentionItem{Tone: tone, Title: "Order " + a.ExternalID + ": " + a.Reason, Link: "/orders/" + a.ExternalID, LinkText: "Open order"}
			if tone == "bad" {
				bad = append(bad, item)
			} else {
				warn = append(warn, item)
			}
		}
	}
	if unmatched > 0 {
		bad = append(bad, attentionItem{Tone: "bad", Title: plural(unmatched, "payment", "payments") + " arrived with no order waiting",
			Detail: "Usually a customer paying after their order expired. Refund them and record it.", Link: "/deposits", LinkText: "Unmatched deposits"})
	}
	if holds > 0 {
		warn = append(warn, attentionItem{Tone: "warn", Title: plural(holds, "order is", "orders are") + " held by screening", Link: "/screening", LinkText: "Review"})
	}
	if approvals > 0 {
		warn = append(warn, attentionItem{Tone: "warn", Title: plural(approvals, "signature is", "signatures are") + " waiting for approval", Link: "/approvals", LinkText: "Review"})
	}
	if t := d.Treasury; t != nil {
		if b := t.BSC.Balance; b != nil && b.Error == "" {
			if bnb, err := strconv.ParseFloat(b.Native, 64); err == nil && bnb < lowTreasuryBNB {
				item := attentionItem{Tone: "warn", Title: fmt.Sprintf("The BSC treasury is low: %s BNB", num(b.Native)),
					Detail: fmt.Sprintf("Enough for about %s. Send BNB to %s.", plural(int(math.Floor(bnb/gasTopUpBNB)), "gas top-up", "gas top-ups"), t.BSC.Treasury),
					Link:   "/treasury", LinkText: "Treasury"}
				if bnb < gasTopUpBNB {
					item.Tone = "bad"
					bad = append(bad, item)
				} else {
					warn = append(warn, item)
				}
			}
		}
		if b := t.TRON.Balance; b != nil && b.Error == "" {
			if trx, err := strconv.ParseFloat(b.Native, 64); err == nil && trx < lowTreasuryTRX {
				warn = append(warn, attentionItem{Tone: "warn", Title: fmt.Sprintf("The TRON treasury is low: %s TRX", num(b.Native)),
					Detail: fmt.Sprintf("A new TRON deposit wallet can't be activated (about 1.1 TRX). Send TRX to %s.", t.TRON.Treasury),
					Link:   "/treasury", LinkText: "Treasury"})
			}
		}
	}
	for _, e := range d.Energy {
		switch covered := e["orders_covered"]; {
		case e["balance_error"] != "":
			warn = append(warn, attentionItem{Tone: "warn", Title: "Couldn't read the " + e["vendor"] + " balance", Detail: e["balance_error"], Link: "/vendors", LinkText: "Vendors"})
		case covered == "0":
			bad = append(bad, attentionItem{Tone: "bad", Title: fmt.Sprintf("%s can't pay for one TRON → BSC order", e["vendor"]),
				Detail: fmt.Sprintf("Prepaid balance %s TRX; one order's energy costs up to %s TRX. Send TRX to %s.", num(e["balance_trx"]), num(e["order_cost_trx"]), e["top_up_address"]),
				Link:   "/vendors", LinkText: "Vendors"})
		case covered == "1" || covered == "2":
			warn = append(warn, attentionItem{Tone: "warn", Title: fmt.Sprintf("%s's balance covers only %s TRON → BSC orders", e["vendor"], covered),
				Detail: "Top it up at " + e["top_up_address"] + ".", Link: "/vendors", LinkText: "Vendors"})
		}
	}
	if vendors != nil {
		available := 0
		for _, v := range vendors.Vendors {
			if v.Service != "conversion" || !v.Enabled {
				continue
			}
			if v.Available {
				available++
			} else {
				warn = append(warn, attentionItem{Tone: "warn", Title: "The exchange " + v.Name + " is out of rotation",
					Detail: "It failed " + strconv.Itoa(v.ConsecutiveFailures) + " times in a row and returns once it answers again.", Link: "/vendors", LinkText: "Vendors"})
			}
		}
		if available == 0 {
			bad = append(bad, attentionItem{Tone: "bad", Title: "No exchange is available", Detail: "Customers can't get a quote.", Link: "/vendors", LinkText: "Vendors"})
		}
	}
	if lag := d.BSCLag; lag != nil && *lag > watcherLagWarn {
		item := attentionItem{Tone: "warn", Title: fmt.Sprintf("The BSC watcher is %d blocks behind the chain", *lag),
			Detail: "BSC deposits are seen late.", Link: "/system", LinkText: "System"}
		if *lag > watcherLagBad {
			item.Tone = "bad"
			bad = append(bad, item)
		} else {
			warn = append(warn, item)
		}
	}
	return append(bad, warn...)
}

// collectHealth checks every service, and reads how far behind the BSC
// watcher is.
func (s *Server) collectHealth(ctx context.Context) ([]serviceHealth, *int64) {
	type check struct {
		name    string
		healthz func(context.Context) error
	}
	checks := []check{}
	if s.Relayd != nil {
		checks = append(checks, check{"relayd", s.Relayd.Healthz})
	}
	if s.Watcher != nil {
		checks = append(checks, check{"BSC watcher", s.Watcher.Healthz})
	}
	if s.Tronwatcher != nil {
		checks = append(checks, check{"TRON watcher", s.Tronwatcher.Healthz})
	}
	if s.Ledger != nil {
		checks = append(checks, check{"ledger", s.Ledger.Healthz})
	}
	if s.Screening != nil {
		checks = append(checks, check{"screening", s.Screening.Healthz})
	}
	if s.S1 != nil {
		checks = append(checks, check{"signing (S1)", s.S1.Healthz})
	}
	out := make([]serviceHealth, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c check) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			out[i] = serviceHealth{Name: c.name, Healthy: true}
			if err := c.healthz(ctx); err != nil {
				out[i].Healthy, out[i].Error = false, err.Error()
			}
		}(i, c)
	}
	var lag *int64
	if s.Watcher != nil {
		if inv, err := s.Watcher.GetInvariants(ctx); err == nil {
			lag = inv.CursorLagBlocks
		}
	}
	wg.Wait()
	return out, lag
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
