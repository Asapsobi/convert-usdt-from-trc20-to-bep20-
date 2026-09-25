package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"relayd/internal/driver"
	"relayd/internal/httpapi"
	"relayd/internal/orchestrate"
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
			"bsc_sweep_to": firstNonEmpty(orch.Cfg.SweepToBSC, orch.Cfg.SlotEVMAddress),
			"tron_sweep_to": firstNonEmpty(orch.Cfg.SweepToTRON, orch.Cfg.SlotAddress),
		},
	}
	if d.BEP20Watcher != nil {
		admin.Watchers["bsc"] = d.BEP20Watcher
	}
	if d.TronWatcher != nil {
		admin.Watchers["tron"] = d.TronWatcher
	}
	return admin, nil
}
