package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opsconsole/internal/auditlog"
	"opsconsole/internal/opclient"
	"opsconsole/internal/session"
)

func str(s string) *string { return &s }

// settledLeg is a completed BSC -> TRON order, as relayd reports it.
func settledLeg() opclient.RelayLeg {
	return opclient.RelayLeg{ExternalID: "relay-web-0001", OrderID: 15, Direction: "BEP20_TO_TRC20", Status: "SETTLED",
		CustomerID: "cust", DestinationAddress: "TJcS48zPKvBeXeLfX6RLxR62M8sQPkcpn6", AmountIn: "2.000000", AmountOutExpected: "1.639000",
		AmountOutActual: str("1.636000"), UpstreamProviderName: str("fixedfloat"), UpstreamOrderID: str("FF123"),
		CreatedAt: "2026-09-26T11:11:48Z", UpdatedAt: "2026-09-26T11:39:27Z", CustomerLabel: str("alice"),
		DepositAddress: "0xC6767876B25C5c66e103cfE5FaF0b215A7Ae38F3", SenderAddress: str("0x1111111111111111111111111111111111111111"),
		ProfitBPS: func() *int64 { v := int64(25); return &v }(), ReceivedAmount: str("2.000000"), ProfitAmount: str("0.005000"),
		ForwardAmount: str("1.995000"), VendorFeeAmount: str("0.359000"),
		PayoutTxID: str("fcbaf11000000000000000000000000000000000000000000000000000068c6"), UpstreamDepositAddress: str("0x8fb3993b00000000000000000000000000000000")}
}

func settledCosts(t *testing.T) opclient.JobCosts {
	var costs opclient.JobCosts
	if err := json.Unmarshal([]byte(`{"transfers":[
		{"purpose":"GAS_TOPUP","chain":"BSC","from":"0xT","to":"0xC6767876B25C5c66e103cfE5FaF0b215A7Ae38F3","asset":"BNB","amount":"0.00001","status":"CONFIRMED","tx_hash":"0xaaa","created_at":"2026-09-26T11:20:00Z"},
		{"purpose":"FORWARD","chain":"BSC","from":"0xC6767876B25C5c66e103cfE5FaF0b215A7Ae38F3","to":"0x8fb3993b00000000000000000000000000000000","asset":"USDT_BEP20","amount":"1.995","status":"CONFIRMED","tx_hash":"0x51c380fd955a","created_at":"2026-09-26T11:21:00Z"}],
		"rentals":[],"gas_topups_bnb":"0.00001","trx_topups":"0","energy_rentals_trx":"0"}`), &costs); err != nil {
		t.Fatal(err)
	}
	return costs
}

// Every page renders with realistic data, and html/template never had to
// neutralize an unsafe value (it writes ZgotmplZ when it does).
func TestEveryPageRenders(t *testing.T) {
	tpl := MustLoadTemplates()
	sess := &session.Session{Username: "admin", DisplayName: "Admin"}
	base := func(nav, title string) basePageData {
		return basePageData{Session: sess, Nav: nav, Title: title, EnvLabel: "Live", EnvLive: true, Counts: navCounts{Attention: 2, Unmatched: 1},
			Legacy: &legacyNav{Broker: true, Dispatcher: true}, OK: "Saved."}
	}
	leg := settledLeg()
	costs := settledCosts(t)
	lag := int64(12)
	energy := []map[string]string{{"vendor": "catfee", "units": "65000", "cost_trx": "1.3", "order_units": "131000", "order_cost_trx": "2.62",
		"balance_trx": "1.11145", "orders_covered": "0", "top_up_address": "TAQULprL4CudRA1Ga3r4KPHahmXXQFE2VF"}}
	balance := &opclient.WalletBalance{USDT: "0", Native: "0.00001895", NativeFor: "BNB"}
	treasury := &opclient.Treasury{
		BSC:     opclient.TreasuryChain{Treasury: "0x11fE07Bd634A031Ee95e9202F6a0Acc067B07Dd5", SweepTo: "0x11fE07Bd634A031Ee95e9202F6a0Acc067B07Dd5", Balance: balance},
		TRON:    opclient.TreasuryChain{Treasury: "TBcLoefSwAVSUhtqJ8ZVjg3xWpoSq5Tovz", SweepTo: "TBcLoefSwAVSUhtqJ8ZVjg3xWpoSq5Tovz", Balance: &opclient.WalletBalance{Native: "0.099", NativeFor: "TRX", USDT: "0"}},
		Wallets: []opclient.TreasuryWallet{{SlotID: 2, BSC: "0x11fE07Bd634A031Ee95e9202F6a0Acc067B07Dd5", TRON: "TBcLoefSwAVSUhtqJ8ZVjg3xWpoSq5Tovz", BSCBalance: balance}},
	}
	profit := opclient.ProfitWallets{Wallets: []opclient.ProfitWallet{{Chain: "BSC", Address: "0xC6767876B25C5c66e103cfE5FaF0b215A7Ae38F3", Legs: 1,
		Earned: "0.005", Swept: "0", Unswept: "0.005", OnChain: &opclient.WalletBalance{USDT: "0.005", Native: "0.0000074", NativeFor: "BNB"}}},
		UnsweptTotals: map[string]string{"USDT_BEP20": "0.005"}}
	var pool []opclient.PoolWallet
	if err := json.Unmarshal([]byte(`[{"address":"0xC6767876B25C5c66e103cfE5FaF0b215A7Ae38F3","derivation_index":3,"network":"BSC","status":"ACTIVE","available":false,
		"available_after":"2026-09-25T10:00:00Z","lease":{"external_id":"relay-web-0001","quote_expires_at":"2026-09-25T10:30:00Z"}},
		{"address":"0xB000000000000000000000000000000000000000","derivation_index":4,"network":"BSC","status":"DISABLED","available":false,"available_after":"2026-09-25T10:00:00Z"}]`), &pool); err != nil {
		t.Fatal(err)
	}
	day := opclient.StatsPeriod{Name: "24h", Orders: 2, Settled: 1, Volume: "2", Profit: "0.005", GasBNB: "0.00001", TRX: "0", EnergyTRX: "0"}

	pages := map[string]struct {
		data any
		want []string
	}{
		"overview": {overviewData{basePageData: base("overview", "Overview"), GeneratedAt: time.Now(), StatsOK: true,
			Attention: []attentionItem{{Tone: "bad", Title: "catfee can't pay for one TRON → BSC order", Detail: "Send TRX.", Link: "/vendors", LinkText: "Vendors"}},
			Day:       day, Week: day, All: day, ByStatus: []statusCount{{"SETTLED", 3}}, Recent: []opclient.RelayLeg{leg},
			Treasury: treasury, Energy: energy, Unswept: profit.UnsweptTotals,
			Services: []serviceHealth{{Name: "relayd", Healthy: true}, {Name: "ledger", Error: "timeout"}}, BSCLag: &lag},
			[]string{"Needs attention", "catfee can&#39;t pay", "Completed · 3", "12 blocks behind", "Energy broker"}},
		"orders": {ordersData{basePageData: base("orders", "Orders"), Tab: "all", Orders: []opclient.RelayLeg{leg}, Page: 1, Pages: 2, Total: 51,
			NextURL: "/orders?page=2", Tabs: []orderTab{{Key: "all", Label: "All", Count: 51, URL: "/orders", Active: true}}},
			[]string{"relay-web-0001", "alice", "1.636", "Older →"}},
		"order": {orderData{basePageData: base("orders", "Order"), ExternalID: leg.ExternalID, Leg: &leg, Costs: costs,
			Steps:    orderSteps(leg, costs, []opclient.Deposit{{TxHash: "0xdeadbeef", Amount: "2", SenderAddress: "0x1111111111111111111111111111111111111111"}}),
			SrcChain: "BSC", DstChain: "TRON", Created: parseTime(leg.CreatedAt)},
			[]string{"What happened", "https://tronscan.org/#/transaction/fcbaf110", "https://bscscan.com/tx/0x51c380fd955a", "Customer received", "Gas top-up"}},
		"deposits": {depositsData{basePageData: base("deposits", "Unmatched deposits"), Deposits: []unmatchedDeposit{{
			OrphanedDeposit: opclient.OrphanedDeposit{ID: 7, OrderID: 11, ExternalID: "relay-web-0011", TxID: "abcd", Amount: "2", DetectedAt: time.Now(), OrderStateAtDetection: "EXPIRED"},
			Chain:           "TRON", Key: "tron"}}}, []string{"/deposits/tron/7/resolve", "Mark resolved"}},
		"treasury": {treasuryData{basePageData: base("treasury", "Treasury & profit"), Treasury: treasury, Energy: energy,
			Settings: &opclient.SweepSettings{Enabled: false, IntervalMinutes: 60, MinAmount: map[string]string{"USDT_BEP20": "10", "USDT_TRC20": "50"}},
			Wallets:  profit, Sweeps: []opclient.Sweep{{ID: 1, Chain: "TRON", FromAddress: "TX", Amount: "50", Status: "FAILED", Error: str("no energy"), CreatedAt: time.Now()}}},
			[]string{"Treasury · slot 2", "Can't pay for one TRON", "Sweep automatically", "no energy"}},
		"pricing": {pricingData{basePageData: base("pricing", "Pricing"), Pricing: &opclient.Pricing{ProfitBPS: 25, MinProfit: "0", MinAmountIn: "1", MaxAmountIn: "10000"},
			DirectionRows: []directionRow{{Key: "TRC20_TO_BEP20", Label: "TRON → BSC", ProfitBPS: "25", MinProfit: "1.5"}},
			CalcAmount:    "100", CalcDir: "BEP20_TO_TRC20", Calc: &opclient.Quote{AmountIn: "100.000000", OurFee: "0.250000", VendorFee: "0.35", AmountOut: "99.4", Vendor: "fixedfloat"}},
			[]string{"0.25% of the amount", "TRC20_TO_BEP20_min_profit", "99.4"}},
		"vendors": {vendorsData{basePageData: base("vendors", "Vendors"), View: opclient.Vendors{
			Vendors:    []opclient.Vendor{{Service: "conversion", Name: "fixedfloat", Enabled: true, Available: false, ConsecutiveFailures: 3, LastError: str("timeout")}},
			Strategies: map[string]string{"conversion": "best_margin", "energy": "cheapest"}},
			Prices: &opclient.VendorPrices{Amount: "100", BSCToTRON: []map[string]string{{"vendor": "fixedfloat", "customer_receives": "98.9", "vendor_fee": "1.1"}},
				TRONToBSC: []map[string]string{{"vendor": "fixedfloat", "error": "timeout"}}, Energy: energy}},
			[]string{"can't pay for one TRON", "3 failures", "Most revenue for us"}},
		"wallets": {walletsData{basePageData: base("wallets", "Deposit wallets"), Chain: "bsc", ChainName: "BSC",
			Settings: &opclient.PoolSettings{MaxWallets: 10, CooldownAfterUse: "30m0s", CooldownAfterExpiry: "6h0m0s"},
			Wallets:  []poolWallet{{PoolWallet: pool[0], Profit: &profit.Wallets[0]}, {PoolWallet: pool[1]}}, InUse: 1},
			[]string{"Serving", "Disabled", "0 = never add a new wallet"}},
		"screening": {screeningData{basePageData: base("screening", "Screening"), Status: "OPEN",
			Holds: []holdRow{{ID: 1, OrderID: 9, ExternalID: "relay-web-0009", ReasonCode: "SANCTIONS_HIT", OpenedAt: time.Now(), Status: "OPEN"}}},
			[]string{"SANCTIONS_HIT", "Reject &amp; refund"}},
		"approvals": {approvalsData{basePageData: base("approvals", "Signing approvals"), HasApproverToken: true,
			Requests: []approvalRow{{ID: 561, SlotID: 1, EstimatedUSD: 1250.5, CreatedAt: time.Now()}}}, []string{"#561", "$1250.50"}},
		"system": {systemData{basePageData: base("system", "System"), Services: []serviceHealth{{Name: "relayd", Healthy: true}},
			BSC: &opclient.WatcherInvariants{CursorLagBlocks: &lag}, TRON: &opclient.TronwatcherInvariants{},
			Cursor: &opclient.Cursor{LastScanned: 124311947}, Version: "dev (abc)"}, []string{"12 blocks", "124311947", "Halt everything"}},
		"audit": {auditData{basePageData: base("audit", "Audit log"), Entries: []auditlog.Entry{{Time: time.Now(), Operator: "admin",
			Action: "relayd.pricing.update", Target: "pricing", Detail: map[string]any{"profit_bps": 25}}}}, []string{"relayd.pricing.update", "profit_bps"}},
		"login":                   {loginPageData{basePageData: basePageData{Title: "Log in", EnvLabel: "Local test", Err: "invalid username or password"}}, []string{"invalid username or password", "Local test"}},
		"legacy_broker":           {brokerData{basePageData: base("legacy-broker", "Energy broker"), Status: "FAILED", Reservations: []reservationRow{{ID: 3, Status: "FAILED"}}}, []string{"Reconcile"}},
		"legacy_broker_reconcile": {brokerReconcileData{basePageData: base("legacy-broker", "Reconcile"), ID: 3}, []string{"/legacy/broker/3/reconcile"}},
		"legacy_broker_fallback":  {brokerFallbackData{basePageData: base("legacy-broker", "Fallback"), Events: []fallbackRow{{ID: 4, Reason: "all vendors down", TriggeredAt: time.Now()}}}, []string{"all vendors down"}},
		"legacy_dispatcher":       {dispatcherData{basePageData: base("legacy-dispatcher", "Dispatcher"), Slots: []slotRow{{ID: 1, Status: "ACTIVE"}}}, []string{"Retire now"}},
	}
	for name := range tpl.pages {
		if _, ok := pages[name]; !ok {
			t.Errorf("page %q has no render test", name)
		}
	}
	for name, p := range pages {
		page, ok := tpl.pages[name]
		if !ok {
			t.Errorf("no page %q", name)
			continue
		}
		var buf bytes.Buffer
		if err := page.Execute(&buf, p.data); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		out := buf.String()
		if !strings.Contains(out, "</html>") {
			t.Errorf("%s: rendered an incomplete page", name)
		}
		if strings.Contains(out, "ZgotmplZ") {
			t.Errorf("%s: html/template neutralized an unsafe value", name)
		}
		for _, want := range p.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q", name, want)
			}
		}
	}
}

func stepStates(steps []orderStep) string {
	var s []string
	for _, st := range steps {
		s = append(s, st.Title+"="+st.State)
	}
	return strings.Join(s, ", ")
}

func TestOrderSteps(t *testing.T) {
	settled := settledLeg()
	if got := stepStates(orderSteps(settled, settledCosts(t), nil)); got != "Order created=done, Customer's deposit=done, Exchange order=done, "+
		"Network fees=done, Sent to the exchange=done, Payout to the customer=done, Completed=done" {
		t.Errorf("settled order: %s", got)
	}

	waiting := opclient.RelayLeg{ExternalID: "w", Direction: "TRC20_TO_BEP20", Status: "AWAITING_DEPOSIT", AmountIn: "2", AmountOutExpected: "1.6",
		DepositAddress: "TVt2MhV2NAbB6A3i9cqGtDt99nk1rsJ7de", CreatedAt: "2026-09-27T10:00:00Z"}
	if got := stepStates(orderSteps(waiting, opclient.JobCosts{}, nil)); got != "Order created=done, Customer's deposit=current" {
		t.Errorf("waiting order: %s", got)
	}

	expired := waiting
	expired.Status = "EXPIRED"
	if got := stepStates(orderSteps(expired, opclient.JobCosts{}, nil)); got != "Order created=done, Customer's deposit=skipped, Expired=skipped" {
		t.Errorf("expired order: %s", got)
	}

	stuck := settled
	stuck.Status, stuck.PayoutTxID, stuck.AmountOutActual = "UNRECOVERABLE", nil, nil
	steps := orderSteps(stuck, settledCosts(t), nil)
	if last := steps[len(steps)-1]; last.Title != "Payout to the customer" || last.State != "failed" {
		t.Errorf("unrecoverable order ends with %s=%s", last.Title, last.State)
	}
}

func TestBuildAttention(t *testing.T) {
	lag := int64(6000)
	d := overviewData{
		Services: []serviceHealth{{Name: "ledger", Error: "connection refused"}, {Name: "relayd", Healthy: true}},
		Treasury: &opclient.Treasury{
			BSC:  opclient.TreasuryChain{Treasury: "0xT", Balance: &opclient.WalletBalance{Native: "0.00003", NativeFor: "BNB"}},
			TRON: opclient.TreasuryChain{Treasury: "TT", Balance: &opclient.WalletBalance{Native: "5", NativeFor: "TRX"}},
		},
		Energy: []map[string]string{{"vendor": "catfee", "orders_covered": "0", "balance_trx": "1.1", "order_cost_trx": "2.62", "top_up_address": "TAQ"}},
		BSCLag: &lag,
	}
	stats := &opclient.Stats{Attention: []opclient.Attention{{ExternalID: "o1", Status: "UNRECOVERABLE", Reason: "needs an operator"}}}
	vendors := &opclient.Vendors{Vendors: []opclient.Vendor{{Service: "conversion", Name: "fixedfloat", Enabled: true, Available: true}}}
	items := buildAttention(d, stats, vendors, 1, 0, 2)

	var titles []string
	for _, it := range items {
		titles = append(titles, it.Tone+": "+it.Title)
	}
	joined := strings.Join(titles, "\n")
	for _, want := range []string{
		"bad: ledger is not answering",
		"bad: Order o1: needs an operator",
		"bad: 1 payment arrived with no order waiting",
		"bad: catfee can't pay for one TRON → BSC order",
		"bad: The BSC watcher is 6000 blocks behind the chain",
		"warn: 2 signatures are waiting for approval",
		"warn: The BSC treasury is low: 0.00003 BNB",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "TRON treasury") || strings.Contains(joined, "screening") || strings.Contains(joined, "No exchange") {
		t.Errorf("warned about something healthy:\n%s", joined)
	}
	// Worst first: every bad item comes before every warning.
	seenWarn := false
	for _, it := range items {
		if it.Tone == "warn" {
			seenWarn = true
		} else if seenWarn {
			t.Errorf("a bad item comes after a warning:\n%s", joined)
		}
	}
}

// The panel's own routes: public styles and login, everything else
// behind a session, and the old addresses redirecting to the new pages.
func TestRoutes(t *testing.T) {
	signer, err := session.NewSigner(strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Sessions: signer}
	h := NewRouter(srv)
	cookie, err := signer.Sign(session.Session{Username: "admin", DisplayName: "Admin", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string, withSession bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if withSession {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/static/app.css", false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ".sidebar") {
		t.Errorf("stylesheet: %d", rec.Code)
	}
	if rec := get("/login", false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Log in") {
		t.Errorf("login page: %d", rec.Code)
	}
	if rec := get("/orders", false); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("orders without a session: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	for old, want := range map[string]string{
		"/relayd/legs": "/orders", "/relayd/legs/relay-web-1": "/orders/relay-web-1", "/relayd/pricing": "/pricing",
		"/relayd/sweeps": "/treasury", "/relayd/pool/tron": "/wallets/tron", "/s1/approvals": "/approvals", "/ledger/halt": "/system",
		"/wallets": "/wallets/bsc",
	} {
		if rec := get(old, true); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != want {
			t.Errorf("%s: %d -> %s, want 301 -> %s", old, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	if rec := get("/legacy/broker", true); rec.Code != http.StatusNotFound {
		t.Errorf("Model D pages without a broker configured: %d, want 404", rec.Code)
	}
}
