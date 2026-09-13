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

	xpub := os.Getenv("TRONWATCHER_XPUB")
	if xpub == "" {
		return errors.New("tronwatcherd: TRONWATCHER_XPUB is not set")
	}
	if err := addresses.Configure(xpub); err != nil {
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
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("tronwatcherd shut down cleanly")
	return nil
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
