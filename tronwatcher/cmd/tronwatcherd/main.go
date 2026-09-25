// Command tronwatcherd serves C2', the TRON-side deposit watcher, over
// HTTP and, when TRONWATCHER_PROVIDERS is set, runs the live
// chain-watching engine too: per-address TronGrid scanning and the
// candidate/finality pipeline, reporting to a real C1 via ledgerclient.
// See engine.go's own doc comment for exactly which env vars that opts
// into; running without it (HTTP boundary only) is a legitimate,
// supported mode, mirroring depositwatcher/cmd/watcherd's own identical
// posture.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"tronwatcher/internal/addresses"
	"tronwatcher/internal/db"
	"tronwatcher/internal/httpapi"
	"tronwatcher/internal/s1client"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("tronwatcherd exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Exactly one tronwatcherd may drive this database: two would report the same
	// deposits and race on the scan cursor.
	lock, err := db.AcquireInstanceLock(ctx, pool, "tronwatcherd", 30*time.Second)
	if err != nil {
		return err
	}
	defer lock.Release()

	if err := configureAddressingFromEnv(); err != nil {
		return err
	}

	auth, err := httpapi.AuthConfigFromEnv()
	if err != nil {
		return err
	}

	eng, err := newEngineFromEnv(pool)
	if err != nil {
		return err
	}

	server := &httpapi.Server{
		Pool:      pool,
		Auth:      auth,
		BuildInfo: buildInfo,
	}
	if eng != nil {
		server.Tracker = eng.tracker
	}
	router := httpapi.NewRouter(server)

	if eng != nil {
		engineCtx, stopEngine := context.WithCancel(context.Background())
		defer stopEngine()
		eng.run(engineCtx, pool)
		slog.Info("tronwatcherd: live chain-watching engine started",
			"contract", eng.cfg.ContractAddress, "dust_floor", eng.cfg.DustFloor)
	} else {
		slog.Info("tronwatcherd: TRONWATCHER_PROVIDERS not set -- serving the HTTP boundary only, no live chain-watching engine")
	}

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("tronwatcherd listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-serveErr:
		return err
	case <-lock.Lost():
		return errors.New("tronwatcherd: lost the instance lock -- exiting so a second instance never runs alongside this one")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("tronwatcherd shut down cleanly")
	return nil
}

// configureAddressingFromEnv chooses which backend addresses.Assign
// derives/provisions TRON deposit addresses through -- the mirror of
// s1d's own configureTronDepositSigningFromEnv, since this deployment's
// TRON-ADDRESSING backend must agree with S1's own TRON-SIGNING backend
// (see that function's own doc comment for the operational invariant
// neither side can mechanically check). Exactly one of TRONWATCHER_XPUB
// (local, pure-derivation mode) or TRONWATCHER_S1_BASE_URL +
// TRONWATCHER_S1_PROVISIONING_TOKEN (Privy-backed, via S1's own POST
// /v1/tron-deposit-keys) must be set -- neither or both is a real
// misconfiguration, failing loud rather than silently picking one.
func configureAddressingFromEnv() error {
	xpub := os.Getenv("TRONWATCHER_XPUB")
	s1BaseURL := os.Getenv("TRONWATCHER_S1_BASE_URL")
	s1Token := os.Getenv("TRONWATCHER_S1_PROVISIONING_TOKEN")
	s1Set := s1BaseURL != "" || s1Token != ""
	if s1Set && (s1BaseURL == "" || s1Token == "") {
		return errors.New("tronwatcherd: TRONWATCHER_S1_BASE_URL and TRONWATCHER_S1_PROVISIONING_TOKEN must both be set together, or both left unset")
	}

	switch {
	case xpub != "" && s1Set:
		return errors.New("tronwatcherd: TRONWATCHER_XPUB and TRONWATCHER_S1_BASE_URL/TRONWATCHER_S1_PROVISIONING_TOKEN are both set -- exactly one TRON-addressing backend must be configured, never both")
	case xpub != "":
		slog.Info("tronwatcherd: TRON-addressing backend is local xpub derivation -- confirm S1's own TRON-signing backend is ALSO local BIP32 against the SAME xpub, or signing will silently use the wrong key")
		return addresses.Configure(xpub)
	case s1Set:
		slog.Warn("tronwatcherd: TRON-addressing backend is S1-provisioned (Privy Server Wallets) -- confirm S1's own TRON-signing backend is ALSO Privy-backed against this SAME S1 deployment, or signing will silently use the wrong key")
		addresses.ConfigureS1Provisioning(s1client.New(s1BaseURL, s1Token))
		return nil
	default:
		return errors.New("tronwatcherd: neither TRONWATCHER_XPUB nor TRONWATCHER_S1_BASE_URL/TRONWATCHER_S1_PROVISIONING_TOKEN is set -- exactly one TRON-addressing backend must be configured")
	}
}

func listenAddr() string {
	if addr := os.Getenv("TRONWATCHER_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8092"
}

// buildInfo reads module version and VCS revision from the binary's own
// embedded build metadata. Duplicated from every sibling service's own
// identical implementation -- separate modules, no common internal
// package.
func buildInfo() (version, commit string) {
	version, commit = "unknown", "unknown"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if info.Main.Version != "" {
		version = info.Main.Version
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			commit = s.Value
		}
	}
	return
}
