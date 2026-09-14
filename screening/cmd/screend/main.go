// Command screend serves C3, the screening service, over HTTP: the
// hold queue, screening-result audit lookups, and re-screen flags
// (C3.8) -- plus the discovery background loop (C3.3), which polls C1
// for newly-funded orders and resolves their sender_address.
//
// SCREENING_PROVIDER selects what pipeline.RunLoop/rescreen.RunLoop (the
// screening and re-screen engines, C3.4/C3.7) actually screen against:
//   - unset (default): neither loop starts. No real
//     provider.ScreeningProvider exists yet -- a real AML vendor client,
//     one of the candidates component-map.md's own C3 "provider call"
//     line names without picking one -- and wiring a mock into a
//     production compliance path would silently screen real orders
//     against a fake, seeded verdict, which is worse than the honest
//     gap left here. A deployment running this binary this way
//     discovers funded orders and serves the manual-review/audit
//     surface correctly, nothing more.
//   - "always_clean": both loops start against
//     provider.AlwaysCleanProvider -- NOT a real compliance decision,
//     see that type's own doc comment. Exists only for
//     docs/03-build/mvp-proof-run-plan.md's own supervised, no-real-
//     customer-traffic proof run. Refuses to start if
//     SCREENING_ALLOW_ALWAYS_CLEAN is not also "true", so this can't be
//     reached by an unexamined env default.
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

	"screening/internal/db"
	"screening/internal/discovery"
	"screening/internal/holds"
	"screening/internal/httpapi"
	"screening/internal/ledgerclient"
	"screening/internal/pipeline"
	"screening/internal/provider"
	"screening/internal/relaydclient"
	"screening/internal/rescreen"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("screend exited with error", "error", err)
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

	auth, err := httpapi.AuthConfigFromEnv()
	if err != nil {
		return err
	}

	ledgerBaseURL := os.Getenv("SCREENING_LEDGER_BASE_URL")
	ledgerToken := os.Getenv("SCREENING_LEDGER_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return errors.New("screend: SCREENING_LEDGER_BASE_URL and SCREENING_LEDGER_TOKEN are both required " +
			"(POST /holds/{id}/release and /reject, and the discovery loop's own PollFundedOrders/GetSenderAddress, all call C1 through this client)")
	}
	ledgerClient := ledgerclient.New(ledgerBaseURL, ledgerToken)

	// Optional, unlike ledgerClient above -- Model F is a separate
	// product line not every deployment of this service runs alongside
	// (see httpapi.Server's own doc comment). Left unset, POST
	// /v1/holds/{id}/reject keeps today's exact behavior for every
	// order (holds.StubRefundEntryBuilder, 501
	// refund_entry_not_implemented) -- this never blocks startup.
	var refundEntryBuilder holds.RefundEntryBuilder = holds.StubRefundEntryBuilder{}
	relaydBaseURL := os.Getenv("SCREENING_RELAYD_BASE_URL")
	relaydToken := os.Getenv("SCREENING_RELAYD_TOKEN")
	if relaydBaseURL != "" && relaydToken != "" {
		refundEntryBuilder = holds.RelayAwareRefundEntryBuilder{Relayd: relaydclient.New(relaydBaseURL, relaydToken)}
		slog.Info("screend: RELAY-aware refund entry builder configured", "relayd_base_url", relaydBaseURL)
	}

	server := &httpapi.Server{
		Pool:               pool,
		Auth:               auth,
		LedgerClient:       ledgerClient,
		RefundEntryBuilder: refundEntryBuilder,
		BuildInfo:          buildInfo,
	}
	router := httpapi.NewRouter(server)

	// ledgerClient implements both discovery.FundedOrderPoller
	// (PollFundedOrders) and provider.SenderAddressLookup
	// (GetSenderAddress) for real, against the real, running C1 this
	// binary was configured to talk to above -- no fake, no vendor
	// dependency, so this loop has nothing blocking it from running in
	// production today. See this file's own doc comment for why
	// pipeline/rescreen aren't started alongside it.
	go func() {
		if err := discovery.RunLoop(ctx, pool, ledgerClient, ledgerClient, 0); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("screend: discovery loop exited", "error", err)
		}
	}()

	screeningProvider, providerName, err := screeningProviderFromEnv()
	if err != nil {
		return err
	}
	if screeningProvider != nil {
		// ledgerClient implements both pipeline.Reporter (ReportVerdict)
		// and rescreen.OrderLister (ListOrdersByState) for real, against
		// the same running C1 -- see this file's own doc comment for what
		// SCREENING_PROVIDER=always_clean actually means.
		go func() {
			cfg := pipeline.Config{ProviderName: providerName}
			if err := pipeline.RunLoop(ctx, pool, screeningProvider, ledgerClient, cfg, 0); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("screend: pipeline loop exited", "error", err)
			}
		}()
		go func() {
			cfg := rescreen.Config{ProviderName: providerName}
			if err := rescreen.RunLoop(ctx, pool, ledgerClient, screeningProvider, cfg, 0); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("screend: rescreen loop exited", "error", err)
			}
		}()
		slog.Warn("screend: pipeline and rescreen loops started against a PLACEHOLDER screening provider -- not a real compliance decision",
			"provider", providerName)
	}

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("screend listening", "addr", srv.Addr)
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
	slog.Info("screend shut down cleanly")
	return nil
}

// screeningProviderFromEnv reads SCREENING_PROVIDER. Unset (the
// default): returns (nil, "", nil) -- pipeline/rescreen stay unstarted,
// exactly this binary's prior behavior. "always_clean": returns
// provider.AlwaysCleanProvider, but only if SCREENING_ALLOW_ALWAYS_CLEAN
// is also exactly "true" -- a second, explicit flag so a real deployment
// can never reach a placeholder compliance decision through one
// mistyped env var alone. Any other SCREENING_PROVIDER value is a
// startup error: this binary refuses to guess what an unrecognized
// provider name was supposed to mean.
func screeningProviderFromEnv() (provider.ScreeningProvider, string, error) {
	name := os.Getenv("SCREENING_PROVIDER")
	switch name {
	case "":
		return nil, "", nil
	case "always_clean":
		if os.Getenv("SCREENING_ALLOW_ALWAYS_CLEAN") != "true" {
			return nil, "", fmt.Errorf("screend: SCREENING_PROVIDER=always_clean also requires SCREENING_ALLOW_ALWAYS_CLEAN=true " +
				"-- provider.AlwaysCleanProvider is not a real compliance decision, see its own doc comment")
		}
		return provider.AlwaysCleanProvider{}, provider.AlwaysCleanProviderName, nil
	default:
		return nil, "", fmt.Errorf("screend: unrecognized SCREENING_PROVIDER %q (supported: unset, \"always_clean\")", name)
	}
}

func listenAddr() string {
	if addr := os.Getenv("SCREENING_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8083"
}

// buildInfo reads module version and VCS revision from the binary's own
// embedded build metadata. Same implementation as C1's ledgerd and C2's
// watcherd -- duplicated rather than shared, since these are three
// separate modules with no common internal package between them.
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
