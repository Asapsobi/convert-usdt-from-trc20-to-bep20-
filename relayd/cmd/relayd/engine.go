package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"relayd/internal/alert"
	"relayd/internal/db"
	"relayd/internal/driver"
	"relayd/internal/energy"
	"relayd/internal/evmbroadcast"
	"relayd/internal/ledgerclient"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/tronbroadcast"
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

	energyBaseURL, err := requiredEnv("RELAYD_ENERGY_BASE_URL")
	if err != nil {
		return nil, nil, err
	}
	energyToken, err := requiredEnv("RELAYD_ENERGY_TOKEN")
	if err != nil {
		return nil, nil, err
	}
	energyClient := energy.New(energyBaseURL, energyToken)

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

	swapProvider, providerName, err := upstreamProviderFromEnv()
	if err != nil {
		return nil, nil, err
	}
	slog.Info("relayd: upstream swap provider configured", "provider", providerName)

	feeBasisPoints, err := requiredEnvInt64("RELAYD_FEE_BASIS_POINTS")
	if err != nil {
		return nil, nil, err
	}
	quoteValidity := 10 * time.Minute
	if raw := os.Getenv("RELAYD_QUOTE_VALIDITY"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("relayd: RELAYD_QUOTE_VALIDITY: %w", err)
		}
		quoteValidity = parsed
	}

	// Unset (zero) leaves R5's own automatic refund-by-timeout disabled --
	// an explicit opt-in, not a default, matching this service's own
	// no-hardcoded-real-money-defaults posture. See
	// orchestrate.Config.ForwardingTimeout's own doc comment.
	var forwardingTimeout time.Duration
	if raw := os.Getenv("RELAYD_FORWARDING_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("relayd: RELAYD_FORWARDING_TIMEOUT: %w", err)
		}
		forwardingTimeout = parsed
	}

	store := relay.NewStore(pool)

	d := &driver.Driver{
		Ledger: ledger, Upstream: swapProvider, TronWatcher: tronWatcher, BEP20Watcher: bep20Watcher,
		Store: store, Cfg: driver.Config{FeeBasisPoints: feeBasisPoints, QuoteValidity: quoteValidity},
	}

	orch := orchestrate.New(store, ledger, swapProvider, energyClient, signer, broadcastClient, finalityReader,
		evmClient, evmClient, alert.LogAlerter{},
		orchestrate.Config{
			SlotID: slotID, SlotAddress: slotAddress, SlotEVMAddress: slotEVMAddress,
			EnergyPerTransferUnits: energyPerTransferUnits, ForwardingTimeout: forwardingTimeout,
		})

	return d, orch, nil
}

// orchestrateInterval reads RELAYD_ORCHESTRATE_INTERVAL, defaulting to
// orchestrate.DefaultInterval if unset.
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
