// Command watcherd serves C2, the deposit watcher, over HTTP and, when
// WATCHER_RPC_PROVIDERS is set, runs the live chain-watching engine too:
// C2.3's ingestion loop and C2.10's candidate pipeline, reporting to a
// real C1 via ledgerclient. See engine.go's own doc comment for exactly
// which env vars that opts into, and httpapi.Server's for why running
// without it (HTTP boundary only) is a legitimate, supported mode, not a
// half-finished one.
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

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/db"
	"depositwatcher/internal/httpapi"
	"depositwatcher/internal/s1client"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("watcherd exited with error", "error", err)
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

	// Exactly one watcherd may drive this database: two would report the same
	// deposits and race on the scan cursor.
	lock, err := db.AcquireInstanceLock(ctx, pool, "watcherd", 30*time.Second)
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

	// Built before NewRouter so Server.ChainPool/Tracker are already set
	// by the time NewRouter builds its reactive Prometheus gauges --
	// those read s.ChainPool at scrape time through a closure over the
	// Server pointer, so it must be the real pool by then, not nil.
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
		server.ChainPool = eng.chainPool
		server.Tracker = eng.tracker
	}
	router := httpapi.NewRouter(server)

	if eng != nil {
		engineCtx, stopEngine := context.WithCancel(context.Background())
		defer stopEngine()
		eng.run(engineCtx, pool)
		slog.Info("watcherd: live chain-watching engine started",
			"contract", eng.cfg.ContractAddress, "dust_floor", eng.cfg.DustFloor)
	} else {
		slog.Info("watcherd: WATCHER_RPC_PROVIDERS not set -- serving the HTTP boundary only, no live chain-watching engine")
	}

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("watcherd listening", "addr", srv.Addr)
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
		return errors.New("watcherd: lost the instance lock -- exiting so a second instance never runs alongside this one")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("watcherd shut down cleanly")
	return nil
}

// configureAddressingFromEnv chooses which backend addresses.Assign
// derives/provisions BSC deposit addresses through -- the mirror of
// s1d's own configureBSCDepositSigningFromEnv, since this deployment's
// BSC-ADDRESSING backend must agree with S1's own BSC-SIGNING backend
// (see that function's own doc comment for the operational invariant
// neither side can mechanically check). Exactly one of WATCHER_XPUB
// (local, pure-derivation mode) or WATCHER_S1_BASE_URL +
// WATCHER_S1_PROVISIONING_TOKEN (Privy-backed, via S1's own POST
// /v1/bsc-deposit-keys) must be set -- neither or both is a real
// misconfiguration, failing loud rather than silently picking one.
// WATCHER_* (not DEPOSITWATCHER_*) matches every other env var this
// binary already reads (WATCHER_XPUB, WATCHER_LISTEN_ADDR,
// WATCHER_RPC_PROVIDERS, ...), not a mechanical copy of tronwatcherd's
// own TRONWATCHER_ prefix.
func configureAddressingFromEnv() error {
	xpub := os.Getenv("WATCHER_XPUB")
	s1BaseURL := os.Getenv("WATCHER_S1_BASE_URL")
	s1Token := os.Getenv("WATCHER_S1_PROVISIONING_TOKEN")
	s1Set := s1BaseURL != "" || s1Token != ""
	if s1Set && (s1BaseURL == "" || s1Token == "") {
		return errors.New("watcherd: WATCHER_S1_BASE_URL and WATCHER_S1_PROVISIONING_TOKEN must both be set together, or both left unset")
	}

	switch {
	case xpub != "" && s1Set:
		return errors.New("watcherd: WATCHER_XPUB and WATCHER_S1_BASE_URL/WATCHER_S1_PROVISIONING_TOKEN are both set -- exactly one BSC-addressing backend must be configured, never both")
	case xpub != "":
		slog.Info("watcherd: BSC-addressing backend is local xpub derivation -- confirm S1's own BSC-signing backend is ALSO local BIP32 against the SAME xpub, or signing will silently use the wrong key")
		return addresses.Configure(xpub)
	case s1Set:
		slog.Warn("watcherd: BSC-addressing backend is S1-provisioned (Privy Server Wallets) -- confirm S1's own BSC-signing backend is ALSO Privy-backed against this SAME S1 deployment, or signing will silently use the wrong key")
		addresses.ConfigureS1Provisioning(s1client.New(s1BaseURL, s1Token))
		return nil
	default:
		return errors.New("watcherd: neither WATCHER_XPUB nor WATCHER_S1_BASE_URL/WATCHER_S1_PROVISIONING_TOKEN is set -- exactly one BSC-addressing backend must be configured")
	}
}

func listenAddr() string {
	if addr := os.Getenv("WATCHER_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8082"
}

// buildInfo reads module version and VCS revision from the binary's own
// embedded build metadata rather than requiring -ldflags at build time.
// Same implementation as C1's ledgerd -- duplicated rather than shared
// because these are two separate modules with no common internal package
// between them.
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
