package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"relayd/internal/money"
	"relayd/internal/upstream"
	"strings"

	"relayd/internal/driver"
	"relayd/internal/httpapi"
	"relayd/internal/orchestrate"
	"relayd/internal/transfers"
	"relayd/internal/vendors"
)

// minAdminTokenLength keeps an admin token from being guessable.
const minAdminTokenLength = 24

// adminFromEnv builds the administrator API from RELAYD_ADMIN_TOKENS
// ("token:operator,token:operator"). Unset leaves the admin API off.
func adminFromEnv(d *driver.Driver, orch *orchestrate.Orchestrator) (*httpapi.Admin, error) {
	tokens := map[string]string{}
	for _, pair := range strings.Split(os.Getenv("RELAYD_ADMIN_TOKENS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		token, name, ok := strings.Cut(pair, ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("relayd: RELAYD_ADMIN_TOKENS: each entry must be token:operator-name")
		}
		if len(token) < minAdminTokenLength {
			return nil, fmt.Errorf("relayd: RELAYD_ADMIN_TOKENS: the token for %q is shorter than %d characters", name, minAdminTokenLength)
		}
		tokens[token] = name
	}
	if len(tokens) == 0 {
		slog.Warn("relayd: RELAYD_ADMIN_TOKENS unset -- the admin API and the order listing are off")
	}
	admin := &httpapi.Admin{
		Tokens: tokens, Pricing: d.Pricing, Vendors: vendors.NewStore(orch.Store.DB()),
		Sweeps: orch.Sweeps, Legs: orch.Store, Transfers: orch.Transfers, SweepNow: orch.RequestSweepScan,
		Watchers: map[string]httpapi.WatcherForwarder{},
		Treasury: map[string]string{
			"bsc_treasury": orch.Cfg.SlotEVMAddress, "tron_treasury": orch.Cfg.SlotAddress,
			"bsc_sweep_to":  firstNonEmpty(orch.Cfg.SweepToBSC, orch.Cfg.SlotEVMAddress),
			"tron_sweep_to": firstNonEmpty(orch.Cfg.SweepToTRON, orch.Cfg.SlotAddress),
		},
	}
	admin.Balance = func(ctx context.Context, chain, address string) (httpapi.WalletBalance, error) {
		b, err := orch.BalanceOf(ctx, transfers.Chain(chain), address)
		if err != nil {
			return httpapi.WalletBalance{}, err
		}
		nativeDecimals := 18
		if b.NativeName == "TRX" {
			nativeDecimals = 6
		}
		out := httpapi.WalletBalance{USDT: formatRaw(b.USDTRaw, b.USDTDecimals), Native: formatRaw(b.NativeRaw, nativeDecimals), NativeFor: b.NativeName}
		if b.NativeName == "TRX" {
			out.Energy, out.Bandwidth = &b.Energy, &b.Bandwidth
		}
		return out, nil
	}
	admin.Prices = func(ctx context.Context, amount string) (any, error) {
		return vendorPrices(ctx, d, orch, amount)
	}
	for _, t := range orch.Treasuries() {
		admin.Treasuries = append(admin.Treasuries, httpapi.TreasuryWallet{SlotID: t.SlotID, BSC: t.EVMAddress, TRON: t.TronAddress})
	}
	if d.BEP20Watcher != nil {
		admin.Watchers["bsc"] = d.BEP20Watcher
	}
	if d.TronWatcher != nil {
		admin.Watchers["tron"] = d.TronWatcher
	}
	return admin, nil
}

// formatRaw renders raw on-chain units with decimals places, trimmed.
func formatRaw(raw *big.Int, decimals int) string {
	if raw == nil {
		return "0"
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, frac := new(big.Int).QuoRem(raw, scale, new(big.Int))
	digits := frac.String()
	for len(digits) < decimals {
		digits = "0" + digits
	}
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		return whole.String()
	}
	return whole.String() + "." + digits
}

// vendorPrices asks every configured vendor for its live price.
// orderEnergyUnits is the most energy one order's TRON transfer needs:
// sending USDT to an address that has never held any (order 16 needed
// 130,285). The admin panel measures an energy vendor's prepaid balance in
// orders of this size.
const orderEnergyUnits = 131_000

func vendorPrices(ctx context.Context, d *driver.Driver, orch *orchestrate.Orchestrator, amount string) (any, error) {
	out := map[string]any{"amount": amount}
	if router, ok := d.Upstream.(*vendors.ConversionRouter); ok {
		for _, pair := range []upstream.Pair{{From: money.USDT_BEP20, To: money.USDT_TRC20}, {From: money.USDT_TRC20, To: money.USDT_BEP20}} {
			in, err := money.ParseDecimal(amount, pair.From)
			if err != nil {
				return nil, fmt.Errorf("amount: %w", err)
			}
			var rows []map[string]string
			for _, q := range router.QuoteEach(ctx, pair, in) {
				row := map[string]string{"vendor": q.Vendor}
				if q.Err != nil {
					row["error"] = q.Err.Error()
				} else {
					row["customer_receives"], _ = money.Format(q.Quote.AmountOut)
					fee := money.Amount{Asset: pair.From, Units: in.Units - q.Quote.AmountOut.Units}
					row["vendor_fee"], _ = money.Format(fee)
				}
				rows = append(rows, row)
			}
			out[string(pair.From)+"_TO_"+string(pair.To)] = rows
		}
	}
	if energy, ok := orch.Energy.(*vendors.EnergyRouter); ok {
		orderCost := map[string]int64{}
		for _, q := range energy.QuoteEach(ctx, orderEnergyUnits) {
			if q.Err == nil && q.CostSun > 0 {
				orderCost[q.Vendor] = q.CostSun
			}
		}
		var rows []map[string]string
		for _, q := range energy.QuoteEach(ctx, 65_000) {
			row := map[string]string{"vendor": q.Vendor, "units": fmt.Sprint(q.Units)}
			if q.Err != nil {
				row["error"] = q.Err.Error()
			} else {
				row["cost_trx"] = formatRaw(big.NewInt(q.CostSun), 6)
			}
			cost, priced := orderCost[q.Vendor]
			if priced {
				row["order_units"] = fmt.Sprint(orderEnergyUnits)
				row["order_cost_trx"] = formatRaw(big.NewInt(cost), 6)
			}
			if acct, ok, err := energy.Account(ctx, q.Vendor); ok && err != nil {
				row["balance_error"] = err.Error()
			} else if ok {
				row["balance_trx"] = formatRaw(big.NewInt(acct.BalanceSun), 6)
				row["top_up_address"] = acct.TopUpAddress
				if priced {
					row["orders_covered"] = fmt.Sprint(acct.BalanceSun / cost)
				}
			}
			rows = append(rows, row)
		}
		out["energy"] = rows
	}
	return out, nil
}
