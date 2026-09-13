// Command opsconsoled serves the ops console: a small internal web
// dashboard in front of C1-C5/S1, per docs/03-build/
// ops-console-build-prompts.md. Holds no database of its own for
// business data -- session state is a signed cookie (OC.1), not a
// server-side session table.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"opsconsole/internal/auditlog"
	"opsconsole/internal/httpapi"
	"opsconsole/internal/opclient"
	"opsconsole/internal/session"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("opsconsoled exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := configFromEnv()
	if err != nil {
		return err
	}

	sessions, err := session.NewSigner(cfg.sessionSecret)
	if err != nil {
		return err
	}
	operators, err := httpapi.ParseOperators(cfg.operatorsRaw)
	if err != nil {
		return err
	}
	audit, err := auditlog.Open(cfg.auditLogPath)
	if err != nil {
		return err
	}
	defer audit.Close()

	server := &httpapi.Server{
		Ledger:     opclient.NewLedgerClient(cfg.ledgerBaseURL, cfg.ledgerToken),
		Watcher:    opclient.NewWatcherClient(cfg.watcherBaseURL, cfg.watcherToken),
		Screening:  opclient.NewScreeningClient(cfg.screeningBaseURL, cfg.screeningToken),
		Broker:     opclient.NewBrokerClient(cfg.brokerBaseURL, cfg.brokerToken),
		Dispatcher: opclient.NewDispatcherClient(cfg.dispatcherBaseURL, cfg.dispatcherToken),
		S1:         opclient.NewS1Client(cfg.s1BaseURL, cfg.s1C5Token),
		Operators:  operators,
		Sessions:   sessions,
		Audit:      audit,
		AuditPath:  cfg.auditLogPath,
		BuildInfo:  buildInfo,
	}
	// Model F's own two services are optional -- see httpapi.Server's own
	// doc comment on why an unset OC_RELAYD_BASE_URL/OC_TRONWATCHER_BASE_URL
	// leaves these nil rather than failing startup.
	if cfg.relaydBaseURL != "" && cfg.relaydToken != "" {
		server.Relayd = opclient.NewRelaydClient(cfg.relaydBaseURL, cfg.relaydToken)
	}
	if cfg.tronwatcherBaseURL != "" && cfg.tronwatcherToken != "" {
		server.Tronwatcher = opclient.NewTronwatcherClient(cfg.tronwatcherBaseURL, cfg.tronwatcherToken)
	}
	router := httpapi.NewRouter(server)

	httpServer := &http.Server{Addr: cfg.listenAddr, Handler: router}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("opsconsoled listening", "addr", cfg.listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("opsconsoled shut down cleanly")
	return nil
}

type config struct {
	listenAddr    string
	sessionSecret string
	operatorsRaw  string
	auditLogPath  string

	ledgerBaseURL, ledgerToken         string
	watcherBaseURL, watcherToken       string
	screeningBaseURL, screeningToken   string
	brokerBaseURL, brokerToken         string
	dispatcherBaseURL, dispatcherToken string
	s1BaseURL, s1C5Token               string

	// Model F's own two services -- optional, unlike every pair above
	// (see httpapi.Server's own doc comment): a Model-D-only deployment
	// of this console leaves these both empty.
	relaydBaseURL, relaydToken           string
	tronwatcherBaseURL, tronwatcherToken string
}

// configFromEnv reads every OC_* config value -- required, no default,
// same posture every other real-money-adjacent threshold or credential
// in this project takes: a misconfigured deployment fails loud at
// startup, not silently at the first request that needed the missing
// value.
func configFromEnv() (config, error) {
	var cfg config
	pairs := []struct {
		name string
		dst  *string
	}{
		{"OC_LEDGER_BASE_URL", &cfg.ledgerBaseURL}, {"OC_LEDGER_TOKEN", &cfg.ledgerToken},
		{"OC_WATCHER_BASE_URL", &cfg.watcherBaseURL}, {"OC_WATCHER_TOKEN", &cfg.watcherToken},
		{"OC_SCREENING_BASE_URL", &cfg.screeningBaseURL}, {"OC_SCREENING_TOKEN", &cfg.screeningToken},
		{"OC_BROKER_BASE_URL", &cfg.brokerBaseURL}, {"OC_BROKER_TOKEN", &cfg.brokerToken},
		{"OC_DISPATCHER_BASE_URL", &cfg.dispatcherBaseURL}, {"OC_DISPATCHER_TOKEN", &cfg.dispatcherToken},
		{"OC_S1_BASE_URL", &cfg.s1BaseURL}, {"OC_S1_C5_TOKEN", &cfg.s1C5Token},
		{"OC_SESSION_SECRET", &cfg.sessionSecret},
		{"OC_OPERATORS", &cfg.operatorsRaw},
		{"OC_AUDIT_LOG_PATH", &cfg.auditLogPath},
	}
	for _, p := range pairs {
		v := os.Getenv(p.name)
		if v == "" {
			return config{}, fmt.Errorf("opsconsoled: %s is required", p.name)
		}
		*p.dst = v
	}

	cfg.listenAddr = ":8091"
	if v := os.Getenv("OC_LISTEN_ADDR"); v != "" {
		cfg.listenAddr = v
	}

	// Optional, unlike every pair read above -- Model F is a separate
	// product line not every deployment of this console runs (see
	// httpapi.Server's own doc comment). Read as a pair regardless: a
	// base URL with no token (or vice versa) is a misconfiguration, not
	// a considered "leave it off" choice, so it's left for main's own
	// both-set check to silently treat as unconfigured rather than
	// half-wiring a client that will fail every real call with a 401.
	cfg.relaydBaseURL = os.Getenv("OC_RELAYD_BASE_URL")
	cfg.relaydToken = os.Getenv("OC_RELAYD_TOKEN")
	cfg.tronwatcherBaseURL = os.Getenv("OC_TRONWATCHER_BASE_URL")
	cfg.tronwatcherToken = os.Getenv("OC_TRONWATCHER_TOKEN")

	return cfg, nil
}

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
