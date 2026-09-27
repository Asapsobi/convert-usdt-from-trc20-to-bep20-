package httpapi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"opsconsole/internal/opclient"
)

// Every relayd admin page renders with realistic data -- templates are
// otherwise only checked when an operator opens the page.
func TestRelaydAdminPagesRender(t *testing.T) {
	tpl := MustLoadTemplates()
	str := func(s string) *string { return &s }
	var costs opclient.JobCosts
	if err := json.Unmarshal([]byte(`{"transfers":[{"purpose":"FORWARD","chain":"BSC","from":"0xA","to":"0xB","asset":"USDT_BEP20","amount":"99.75","status":"CONFIRMED","tx_hash":"0x1"}],
		"rentals":[{"resource":"ENERGY","units":65000,"provider":"catfee","status":"CONFIRMED","cost_trx":"3.9"}],
		"gas_topups_bnb":"0.0005","trx_topups":"0","energy_rentals_trx":"3.9"}`), &costs); err != nil {
		t.Fatal(err)
	}
	var pool []opclient.PoolWallet
	if err := json.Unmarshal([]byte(`[{"address":"0xA","derivation_index":3,"network":"BSC","status":"ACTIVE","available":false,
		"available_after":"2026-09-25T10:00:00Z","lease":{"external_id":"relay-web-1","quote_expires_at":"2026-09-25T10:30:00Z"}},
		{"address":"0xB","derivation_index":4,"network":"BSC","status":"DISABLED","available":false,"available_after":"2026-09-25T10:00:00Z"}]`), &pool); err != nil {
		t.Fatal(err)
	}
	leg := opclient.RelayLeg{ExternalID: "relay-web-1", Status: "SETTLED", AmountIn: "100", AmountOutExpected: "99", AmountOutActual: str("98.9"),
		ReceivedAmount: str("100"), ProfitAmount: str("0.25"), ForwardAmount: str("99.75"), VendorFeeAmount: str("0.85"), CustomerLabel: str("alice"),
		PayoutTxID: str("0xpayout")}

	for name, data := range map[string]any{
		"relayd_pricing": relaydPricingData{Pricing: &opclient.Pricing{ProfitBPS: 25, MinProfit: "0", MinAmountIn: "5", MaxAmountIn: "10000"},
			DirectionRows: []directionRow{{Key: "TRC20_TO_BEP20", Label: "TRON → BSC", ProfitBPS: "40", MinProfit: "1.5"}, {Key: "BEP20_TO_TRC20", Label: "BSC → TRON"}}},
		"relayd_vendors": relaydVendorsData{View: opclient.Vendors{
			Vendors:    []opclient.Vendor{{Service: "conversion", Name: "fixedfloat", Enabled: true, Available: false, LastError: str("timeout")}},
			Strategies: map[string]string{"conversion": "best_margin", "energy": "cheapest"},
		}, Prices: &opclient.VendorPrices{Amount: "100",
			BSCToTRON: []map[string]string{{"vendor": "fixedfloat", "customer_receives": "98.9", "vendor_fee": "1.1"}},
			TRONToBSC: []map[string]string{{"vendor": "fixedfloat", "error": "timeout"}},
			Energy: []map[string]string{{"vendor": "catfee", "units": "65000", "cost_trx": "1.95", "order_units": "131000",
				"order_cost_trx": "3.93", "balance_trx": "1.11145", "orders_covered": "0", "top_up_address": "TTopUp"}}}},
		"relayd_sweeps": relaydSweepsData{
			Settings: &opclient.SweepSettings{Enabled: true, IntervalMinutes: 60, MinAmount: map[string]string{"USDT_BEP20": "10", "USDT_TRC20": "50"}},
			Wallets: opclient.ProfitWallets{Wallets: []opclient.ProfitWallet{
				{Chain: "TRON", Address: "TX", Legs: 3, Unswept: "0.75", Busy: true, OnChain: &opclient.WalletBalance{USDT: "0.75", Native: "2.1", NativeFor: "TRX"}},
				{Chain: "BSC", Address: "0xA", Legs: 1, Unswept: "0", OnChain: &opclient.WalletBalance{Error: "403"}},
			}, UnsweptTotals: map[string]string{"USDT_TRC20": "0.75"}},
			Treasury: &opclient.Treasury{
				BSC: opclient.TreasuryChain{Treasury: "0xT", SweepTo: "0xT"}, TRON: opclient.TreasuryChain{Treasury: "TT", SweepTo: "TCold"},
				Wallets: []opclient.TreasuryWallet{
					{SlotID: 2, BSC: "0xT", TRON: "TT", BSCBalance: &opclient.WalletBalance{USDT: "12", Native: "0.05", NativeFor: "BNB"},
						TRONBalance: &opclient.WalletBalance{Error: "unreachable"}},
				},
			},
			Sweeps: []opclient.Sweep{{ID: 1, Chain: "BSC", Amount: "12", Status: "FAILED", Error: str("gas"), Costs: &costs}},
		},
		"relayd_pool": relaydPoolData{Chain: "bsc", ChainName: "BSC", Settings: &opclient.PoolSettings{MaxWallets: 10, CooldownAfterUse: "30m0s"}, Wallets: pool},
		"relayd_leg":  relaydLegData{ExternalID: "relay-web-1", Detail: &opclient.LegDetail{Leg: leg, Costs: costs}},
		"relay_legs":  relayLegsPageData{Legs: []relayLegRow{relayLegRowFrom(leg)}},
	} {
		page, ok := tpl.pages[name]
		if !ok {
			t.Fatalf("no template %q", name)
		}
		var buf bytes.Buffer
		if err := page.Execute(&buf, data); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(buf.String(), "</html>") {
			t.Errorf("%s: rendered an incomplete page", name)
		}
	}
}

// The Vendors page warns when an energy vendor's prepaid balance can't pay
// for one TRON -> BSC order, and stays quiet when it can.
func TestRelaydVendorsWarnsOnLowEnergyBalance(t *testing.T) {
	tpl := MustLoadTemplates()
	page := func(covered string) string {
		var buf bytes.Buffer
		if err := tpl.pages["relayd_vendors"].Execute(&buf, relaydVendorsData{Prices: &opclient.VendorPrices{Amount: "100",
			Energy: []map[string]string{{"vendor": "catfee", "units": "65000", "cost_trx": "1.95", "order_cost_trx": "3.93",
				"balance_trx": "1.11145", "orders_covered": covered, "top_up_address": "TTopUp"}}}}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if low := page("0"); !strings.Contains(low, "not enough for the energy of one TRON") || !strings.Contains(low, "TTopUp") {
		t.Fatal("no low-balance warning with a balance that covers no order")
	}
	if ok := page("3"); strings.Contains(ok, "not enough for the energy") || !strings.Contains(ok, "3 order(s)") {
		t.Fatal("wrong output with a balance that covers 3 orders")
	}
}
