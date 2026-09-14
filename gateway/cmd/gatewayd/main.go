// Command gatewayd serves C6, the API gateway, over HTTP.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"gateway/internal/c1client"
	"gateway/internal/c2client"
	"gateway/internal/customers"
	"gateway/internal/db"
	"gateway/internal/httpapi"
	"gateway/internal/orders"
	"gateway/internal/quotes"
	"gateway/internal/ratelimit"
	"gateway/internal/reconcile"
	"gateway/internal/retailcustomers"
	"gateway/internal/retailsessions"
	"gateway/internal/sandbox"
	"gateway/internal/webhooks"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("gatewayd exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	ledgerBaseURL := os.Getenv("GATEWAY_LEDGER_BASE_URL")
	ledgerToken := os.Getenv("GATEWAY_LEDGER_API_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return fmt.Errorf("gatewayd: GATEWAY_LEDGER_BASE_URL and GATEWAY_LEDGER_API_TOKEN are required")
	}
	watcherBaseURL := os.Getenv("GATEWAY_WATCHER_BASE_URL")
	watcherToken := os.Getenv("GATEWAY_WATCHER_API_TOKEN")
	if watcherBaseURL == "" || watcherToken == "" {
		return fmt.Errorf("gatewayd: GATEWAY_WATCHER_BASE_URL and GATEWAY_WATCHER_API_TOKEN are required")
	}

	ordersStore := orders.NewStore(pool)
	quotesStore := quotes.NewStore(pool)
	customersStore := customers.NewStore(pool)
	ledger := c1client.New(ledgerBaseURL, ledgerToken)
	watcher := c2client.New(watcherBaseURL, watcherToken)

	// C6.4: closes the address_pending gap C6.3's own choreography can
	// leave behind. Not optional -- see c6-api-gateway-build-prompts.md's
	// own note on why this ships alongside C6.3, not after it.
	reconciler := reconcile.NewReconciler(ordersStore, quotesStore, watcher, nil, reconcile.Config{})
	go func() {
		if err := reconciler.RunLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("reconciliation loop exited with error", "error", err)
		}
	}()

	server := &httpapi.Server{
		Pool:           pool,
		Customers:      customersStore,
		RateLimiter:    ratelimit.New(),
		Quotes:         quotesStore,
		Orders:         ordersStore,
		Ledger:         ledger,
		Watcher:        watcher,
		Sandbox:        sandbox.NewStore(pool),
		PendingAddress: reconciler,
		BuildInfo:      func() (string, string) { return "dev", "dev" },
	}
	// Model D's own B2C channel (docs/01-strategy/model-d-model-f-product-separation.md,
	// 14 Sep 2026 decision) -- opt-in, not on by default: GATEWAY_ENABLE_RETAIL=true
	// registers /v1/retail/... (see NewRouter); unset leaves this
	// deployment exactly as it was before this channel existed, the same
	// "optional, degrades cleanly if unset" posture every other
	// cross-cutting capability in this repo uses.
	if os.Getenv("GATEWAY_ENABLE_RETAIL") == "true" {
		server.RetailCustomers = retailcustomers.NewStore(pool)
		server.RetailSessions = retailsessions.NewStore(pool)
		slog.Info("gatewayd: Model D's own B2C channel enabled (/v1/retail/...)")
	}
	router := httpapi.NewRouter(server) // fills in server.Metrics if left nil

	// C6.6: notices settled/held/refunded and delivers the resulting
	// webhook -- see "Read this fourth" on why this is a tight, dedicated
	// poll, not folded into any other loop's own interval.
	webhooksStore := webhooks.NewStore(pool)
	trigger := webhooks.NewTrigger(pool, ledger, webhooksStore, customersStore)
	go func() {
		if err := trigger.RunTriggerLoop(ctx, 0); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("webhook trigger loop exited with error", "error", err)
		}
	}()
	deliverer := webhooks.NewDeliverer(webhooksStore, customersStore, nil, server.Metrics)
	go func() {
		if err := deliverer.RunDeliverLoop(ctx, 0); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("webhook deliver loop exited with error", "error", err)
		}
	}()

	addr := ":8087"
	if v := os.Getenv("GATEWAY_LISTEN_ADDR"); v != "" {
		addr = v
	}
	httpServer := &http.Server{Addr: addr, Handler: router}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("gatewayd listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
		return httpServer.Shutdown(context.Background())
	}
}
