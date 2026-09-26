package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
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
