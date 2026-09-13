// Command relayd serves Model F's own zero-float relay: a customer-
// facing front door (POST /v1/relay-legs, GET /v1/relay-legs/{id},
// mirroring proofrun's own precedent) plus the background orchestrate
// loop that carries a screened relay leg through energy reservation,
// signing, broadcast, and upstream settlement -- see internal/driver's
// and internal/orchestrate's own doc comments for the full shape.
//
// Unlike depositwatcher/tronwatcher's own optional live-engine mode,
// there is no "HTTP boundary only" mode here: a relay leg created but
// never advanced is just a stuck customer deposit, so every dependency
// this binary needs is required at startup, no defaults.
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

	"relayd/internal/db"
	"relayd/internal/httpapi"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("relayd exited with error", "error", err)
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

	d, orch, err := buildDriverAndOrchestrator(ctx, pool)
	if err != nil {
		return err
	}

	interval, err := orchestrateInterval()
	if err != nil {
		return err
	}

	engineCtx, stopEngine := context.WithCancel(context.Background())
	defer stopEngine()
	go func() {
		if err := orch.RunLoop(engineCtx, interval); err != nil && engineCtx.Err() == nil {
			panic("relayd: orchestrate loop exited unexpectedly: " + err.Error())
		}
	}()
	slog.Info("relayd: orchestrate loop started", "interval", interval)

	server := &httpapi.Server{Driver: d, BuildInfo: buildInfo}
	router := httpapi.NewRouter(server)

	srv := &http.Server{Addr: listenAddr(), Handler: router}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("relayd listening", "addr", srv.Addr)
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
	slog.Info("relayd shut down cleanly")
	return nil
}

func listenAddr() string {
	if addr := os.Getenv("RELAYD_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8093"
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
