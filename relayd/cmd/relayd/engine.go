package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"relayd/internal/addrcheck"
	"relayd/internal/alert"
	"relayd/internal/db"
	"relayd/internal/driver"
	"relayd/internal/energy"
	"relayd/internal/evmbroadcast"
	"relayd/internal/ledgerclient"
	"relayd/internal/orchestrate"
	"relayd/internal/pricing"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/sweeps"
	"relayd/internal/tronbroadcast"
	"relayd/internal/vendors"
	"relayd/internal/watcherclient"
)

// requiredEnv reads name, returning an error naming exactly what's
// missing -- matching every sibling service's own no-hardcoded-default
// posture for real-money/credential config.
func requiredEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("relayd: %s is not set", name)
	}
	return v, nil
}

func requiredEnvInt64(name string) (int64, error) {
	raw, err := requiredEnv(name)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("relayd: %s: %w", name, err)
	}
	return v, nil
}

func requiredEnvInt(name string) (int, error) {
	v, err := requiredEnvInt64(name)
	return int(v), err
}

// buildDriverAndOrchestrator wires every real client this service
// needs from the environment. Returns the front door (Driver, always
// needed) and the background orchestrator (also always needed -- unlike
// depositwatcher's/tronwatcher's own optional live-engine mode, relayd
// has no meaningful "HTTP boundary only" mode: a relay leg created but
// never advanced is just a stuck customer deposit).
func buildDriverAndOrchestrator(ctx context.Context, pool *db.Pool) (*driver.Driver, *orchestrate.Orchestrator, error) {
	ledgerBaseURL, err := requiredEnv("RELAYD_LEDGER_BASE_URL")
	if err != nil {
		return nil, nil, err
	}
	ledgerToken, err := requiredEnv("RELAYD_LEDGER_TOKEN")
	if err != nil {
		return nil, nil, err
	}
	ledger := ledgerclient.New(ledgerBaseURL, ledgerToken)

	tronWatcherBaseURL, err := requiredEnv("RELAYD_TRONWATCHER_BASE_URL")
	if err != nil {
		return nil, nil, err
	}
	tronWatcherToken, err := requiredEnv("RELAYD_TRONWATCHER_TOKEN")
	if err != nil {
		return nil, nil, err
	}
	tronWatcher := watcherclient.New(tronWatcherBaseURL, tronWatcherToken)

	bep20WatcherBaseURL, err := requiredEnv("RELAYD_DEPOSITWATCHER_BASE_URL")
	if err != nil {
		return nil, nil, err
	}
	bep20WatcherToken, err := requiredEnv("RELAYD_DEPOSITWATCHER_TOKEN")
	if err != nil {
		return nil, nil, err
	}
	bep20Watcher := watcherclient.New(bep20WatcherBaseURL, bep20WatcherToken)

	vendorStore := vendors.NewStore(pool)
	energyClient, energySource, err := energyClientFromEnv(ctx, vendorStore)
	if err != nil {
		return nil, nil, err
	}
	slog.Info("relayd: TRON energy provider configured", "provider", energySource)

	signingBaseURL, err := requiredEnv("RELAYD_SIGNING_BASE_URL")
	if err != nil {
		return nil, nil, err
	}
	signingToken, err := requiredEnv("RELAYD_SIGNING_TOKEN")
	if err != nil {
		return nil, nil, err
	}
	signer := signing.New(signingBaseURL, signingToken)

	slotID, err := requiredEnvInt("RELAYD_SLOT_ID")
	if err != nil {
		return nil, nil, err
	}
	// Fetched from S1 directly, not a separate env var: the slot's own
	// address (in either encoding) is S1's fact to report, not an
	// operator's to keep in sync by hand across two places.
	slotAddress, err := signer.SlotAddress(ctx, slotID)
	if err != nil {
		return nil, nil, fmt.Errorf("relayd: fetching slot %d's own TRON address from S1: %w", slotID, err)
	}
	slotEVMAddress, err := signer.EVMAddress(ctx, slotID)
	if err != nil {
		return nil, nil, fmt.Errorf("relayd: fetching slot %d's own EVM address from S1: %w", slotID, err)
	}
	// RELAYD_TREASURY_SLOT_IDS names extra treasury wallets (S1 slots),
	// after the primary RELAYD_SLOT_ID: a top-up comes from whichever
	// treasury can pay for it.
	treasuries := []orchestrate.Treasury{{SlotID: slotID, EVMAddress: slotEVMAddress, TronAddress: slotAddress}}
	for _, raw := range strings.Split(os.Getenv("RELAYD_TREASURY_SLOT_IDS"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		extra, err := strconv.Atoi(raw)
		if err != nil || extra <= 0 {
			return nil, nil, fmt.Errorf("relayd: RELAYD_TREASURY_SLOT_IDS: %q is not a slot id", raw)
		}
		if extra == slotID {
			continue
		}
		tron, err := signer.SlotAddress(ctx, extra)
		if err != nil {
			return nil, nil, fmt.Errorf("relayd: fetching treasury slot %d's TRON address from S1: %w", extra, err)
		}
		evm, err := signer.EVMAddress(ctx, extra)
		if err != nil {
			return nil, nil, fmt.Errorf("relayd: fetching treasury slot %d's EVM address from S1: %w", extra, err)
		}
		treasuries = append(treasuries, orchestrate.Treasury{SlotID: extra, EVMAddress: evm, TronAddress: tron})
	}
	for _, t := range treasuries {
		slog.Info("relayd: treasury wallet", "slot", t.SlotID, "bsc", t.EVMAddress, "tron", t.TronAddress)
	}

	energyPerTransferUnits, err := requiredEnvInt64("RELAYD_ENERGY_PER_TRANSFER_UNITS")
	if err != nil {
		return nil, nil, err
	}

	tronGRPCAddr, err := requiredEnv("RELAYD_TRON_GRPC_ADDR")
	if err != nil {
		return nil, nil, err
	}
	broadcastClient, err := tronbroadcast.NewGrpcBroadcastClient(tronGRPCAddr, 15*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("relayd: connecting to TRON node %q: %w", tronGRPCAddr, err)
	}
	if key := os.Getenv("RELAYD_TRONGRID_API_KEY"); key != "" {
		broadcastClient.SetAPIKey(key)
	} else {
		slog.Warn("relayd: RELAYD_TRONGRID_API_KEY is not set -- TronGrid rate-limits keyless calls (HTTP 429)")
	}
	tronAPIBaseURL, err := requiredEnv("RELAYD_TRON_API_BASE_URL")
	if err != nil {
		return nil, nil, err
	}
	finalityReader := tronbroadcast.NewFinalityReader(tronAPIBaseURL)

	bscRPCURL, err := requiredEnv("RELAYD_BSC_RPC_URL")
	if err != nil {
		return nil, nil, err
	}
	evmClient, err := evmbroadcast.NewClient(ctx, bscRPCURL)
	if err != nil {
		return nil, nil, fmt.Errorf("relayd: connecting to BSC node %q: %w", bscRPCURL, err)
	}

	swapProvider, providerName, err := upstreamProviderFromEnv(ctx, vendorStore)
	if err != nil {
		return nil, nil, err
	}
	slog.Info("relayd: upstream swap provider configured", "provider", providerName)

	if err := runStartupSelfChecks(ctx, startupDeps{
		bsc: evmClient, provider: swapProvider, ledger: ledger,
		bep20Watcher: bep20Watcher, tronWatcher: tronWatcher,
	}); err != nil {
		return nil, nil, err
	}

	quoteValidity, err := optionalDuration("RELAYD_QUOTE_VALIDITY", 10*time.Minute)
	if err != nil {
		return nil, nil, err
	}
	depositWindow, err := optionalDuration("RELAYD_DEPOSIT_WINDOW", 30*time.Minute)
	if err != nil {
		return nil, nil, err
	}
	depositGrace, err := optionalDuration("RELAYD_DEPOSIT_GRACE", 30*time.Minute)
	if err != nil {
		return nil, nil, err
	}
	pricingStore := pricing.NewStore(pool)
	if _, err := pricingStore.Get(ctx); err != nil {
		return nil, nil, fmt.Errorf("relayd: reading pricing settings: %w", err)
	}

	var forwardingTimeout time.Duration
	if raw := os.Getenv("RELAYD_FORWARDING_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("relayd: RELAYD_FORWARDING_TIMEOUT: %w", err)
		}
		forwardingTimeout = parsed
	}

	// Unset (zero) leaves the stale-relay-leg reconciliation alarm
	// disabled -- an explicit opt-in, not a default, matching
	// forwardingTimeout's own identical posture. See
	// orchestrate.Config.StaleLegAlertAfter's own doc comment.
	var staleLegAlertAfter time.Duration
	if raw := os.Getenv("RELAYD_STALE_LEG_ALERT_AFTER"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("relayd: RELAYD_STALE_LEG_ALERT_AFTER: %w", err)
		}
		staleLegAlertAfter = parsed
	}

	sweepToBSC, sweepToTRON := os.Getenv("RELAYD_SWEEP_TO_BSC"), os.Getenv("RELAYD_SWEEP_TO_TRON")
	if sweepToBSC != "" {
		if err := addrcheck.EVM(sweepToBSC); err != nil {
			return nil, nil, fmt.Errorf("relayd: RELAYD_SWEEP_TO_BSC: %w", err)
		}
	}
	if sweepToTRON != "" {
		if err := addrcheck.TRON(sweepToTRON); err != nil {
			return nil, nil, fmt.Errorf("relayd: RELAYD_SWEEP_TO_TRON: %w", err)
		}
	}
	sweepStore := sweeps.NewStore(pool)
	if _, err := sweepStore.Settings(ctx); err != nil {
		return nil, nil, fmt.Errorf("relayd: reading sweep settings: %w", err)
	}
	slog.Info("relayd: profit sweeps go to", "bsc", firstNonEmpty(sweepToBSC, slotEVMAddress), "tron", firstNonEmpty(sweepToTRON, slotAddress))

	store := relay.NewStore(pool)

	d := &driver.Driver{
		Ledger: ledger, Upstream: swapProvider, TronWatcher: tronWatcher, BEP20Watcher: bep20Watcher,
		Store: store, Pricing: pricingStore, Cfg: driver.Config{QuoteValidity: quoteValidity, DepositWindow: depositWindow},
	}

	orch := orchestrate.New(store, ledger, swapProvider, energyClient, signer, broadcastClient, finalityReader,
		evmClient, evmClient, alert.LogAlerter{}, bep20Watcher, tronWatcher,
		orchestrate.Config{
			SlotID: slotID, SlotAddress: slotAddress, SlotEVMAddress: slotEVMAddress,
			EnergyPerTransferUnits: energyPerTransferUnits, ForwardingTimeout: forwardingTimeout,
			StaleLegAlertAfter: staleLegAlertAfter, DepositGrace: depositGrace,
			SweepToBSC: sweepToBSC, SweepToTRON: sweepToTRON, Treasuries: treasuries,
		})

	orch.Pricing = pricingStore
	orch.Sweeps = sweepStore
	orch.EVMPayouts, orch.TRONPayouts = evmClient, finalityReader

	return d, orch, nil
}

// orchestrateInterval reads RELAYD_ORCHESTRATE_INTERVAL, defaulting to
// orchestrate.DefaultInterval if unset.
// energyClientFromEnv picks where TRON energy is rented: straight from
// the vendors (RELAYD_CATFEE_API_KEY/SECRET -- routed, with failover, by
// vendors.EnergyRouter), or through the energy broker service
// (RELAYD_ENERGY_BASE_URL/TOKEN). With neither, TRON transfers that need
// energy fail with a clear error; BSC is unaffected.
func energyClientFromEnv(ctx context.Context, store *vendors.Store) (orchestrate.EnergyClient, string, error) {
	vs := map[string]vendors.EnergyVendor{}
	if key, secret := os.Getenv("RELAYD_CATFEE_API_KEY"), os.Getenv("RELAYD_CATFEE_API_SECRET"); key != "" || secret != "" {
		catfee, err := vendors.NewCatFee(key, secret, os.Getenv("RELAYD_CATFEE_BASE_URL"))
		if err != nil {
			return nil, "", fmt.Errorf("relayd: RELAYD_CATFEE_API_KEY/RELAYD_CATFEE_API_SECRET: %w", err)
		}
		vs["catfee"] = catfee
	}
	if len(vs) > 0 {
		router, err := vendors.NewEnergyRouter(ctx, store, vs)
		if err != nil {
			return nil, "", err
		}
		return router, "vendors", nil
	}
	if base := os.Getenv("RELAYD_ENERGY_BASE_URL"); base != "" {
		token, err := requiredEnv("RELAYD_ENERGY_TOKEN")
		if err != nil {
			return nil, "", err
		}
		return energy.New(base, token), "broker", nil
	}
	slog.Warn("relayd: no TRON energy provider configured -- TRON transfers that need energy will fail until RELAYD_CATFEE_API_KEY/SECRET are set")
	return nil, "none", nil
}

// optionalDuration reads a duration env var, or def when it is unset.
func optionalDuration(name string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("relayd: %s: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("relayd: %s must be positive, got %s", name, d)
	}
	return d, nil
}

func orchestrateInterval() (time.Duration, error) {
	raw := os.Getenv("RELAYD_ORCHESTRATE_INTERVAL")
	if raw == "" {
		return orchestrate.DefaultInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("relayd: RELAYD_ORCHESTRATE_INTERVAL: %w", err)
	}
	return d, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
