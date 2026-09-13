// Command s1d serves S1, key management and signing, over HTTP: bearer
// auth (two separate scopes -- C5's own SigningService calls, and human
// approver actions, see internal/httpapi/auth.go), /healthz, /readyz,
// /metrics, and the full business API under /v1.
//
// This binary wires kmssign.Wrapper against a KMSClient built from
// S1_KMS_CLIENT: "fake" (the only option today) uses an in-process
// FakeKMSClient, seeded from S1_KMS_FAKE_SEED -- there is no real cloud
// KMS adapter in this module yet (see
// docs/02-architecture/s1-key-custody-architecture.md's own "Key
// generation and bootstrapping": that step is an operational procedure a
// human runs with real cloud credentials this repository does not have,
// not something this binary can do unattended). A real deployment must
// not set S1_KMS_CLIENT=fake -- there is deliberately no default, so a
// misconfigured production environment fails to start instead of silently
// signing real payouts against fake, worthless keys.
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
	"strconv"
	"syscall"
	"time"

	"s1/internal/db"
	"s1/internal/httpapi"
	"s1/internal/kmssign"
	"s1/internal/requests"
	"s1/internal/slots"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("s1d exited with error", "error", err)
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

	c5Auth, err := httpapi.C5AuthConfigFromEnv()
	if err != nil {
		return err
	}
	approverAuth, err := httpapi.ApproverAuthConfigFromEnv()
	if err != nil {
		return err
	}

	kmsClient, err := kmsClientFromEnv()
	if err != nil {
		return err
	}
	wrapper := kmssign.NewWrapper(kmsClient)

	slotStore := slots.NewStore(pool, wrapper)

	threshold, err := approvalThresholdFromEnv()
	if err != nil {
		return err
	}
	signingStore := requests.NewStore(pool, slotKeyGetterAdapter{slotStore}, wrapper, requests.Config{ApprovalThresholdUSD: threshold})

	server := &httpapi.Server{
		Pool:      pool,
		C5Auth:    c5Auth,
		Approver:  approverAuth,
		Signing:   signingStore,
		BuildInfo: buildInfo,
	}
	router := httpapi.NewRouter(server)

	srv := &http.Server{
		Addr:    listenAddr(),
		Handler: router,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("s1d listening", "addr", srv.Addr)
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
	slog.Info("s1d shut down cleanly")
	return nil
}

// slotKeyGetterAdapter adapts *slots.Store to requests.SlotKeyGetter --
// the same thin, package-boundary adapter this module's own integration
// tests use, promoted here since production wiring needs it too.
type slotKeyGetterAdapter struct{ store *slots.Store }

func (a slotKeyGetterAdapter) Get(ctx context.Context, slotID int) (requests.SlotKeyInfo, error) {
	key, err := a.store.Get(ctx, slotID)
	if err != nil {
		return requests.SlotKeyInfo{}, err
	}
	evmAddress, err := slots.DeriveEVMAddress(key.PublicKey)
	if err != nil {
		return requests.SlotKeyInfo{}, fmt.Errorf("deriving EVM address for slot %d: %w", slotID, err)
	}
	return requests.SlotKeyInfo{
		KMSKeyID: key.KMSKeyID, PublicKey: key.PublicKey, TronAddress: key.TronAddress, EVMAddress: evmAddress,
	}, nil
}

// kmsClientFromEnv builds the KMSClient this binary signs through.
// S1_KMS_CLIENT has no default -- see this file's own doc comment for
// why an unset value must fail loudly rather than quietly falling back
// to a fake.
func kmsClientFromEnv() (kmssign.KMSClient, error) {
	switch os.Getenv("S1_KMS_CLIENT") {
	case "fake":
		seed := int64(1)
		if raw := os.Getenv("S1_KMS_FAKE_SEED"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("s1d: S1_KMS_FAKE_SEED: %w", err)
			}
			seed = parsed
		}
		slog.Warn("s1d: S1_KMS_CLIENT=fake -- signing against an in-process fake key, never a real one; this must never be set in production")
		return kmssign.NewFakeKMSClient(seed), nil
	case "":
		return nil, errors.New("s1d: S1_KMS_CLIENT is not set (no default -- see this binary's own doc comment)")
	default:
		return nil, fmt.Errorf("s1d: S1_KMS_CLIENT=%q is not a recognized KMS client (only \"fake\" exists in this codebase today -- a real cloud KMS adapter has not been built yet)", os.Getenv("S1_KMS_CLIENT"))
	}
}

// approvalThresholdFromEnv reads S1_APPROVAL_THRESHOLD_USD -- see
// docs/02-architecture/s1-key-custody-architecture.md's own "What's
// actually settled here" for why this is required config, not a
// hardcoded default, even though that document's own starting
// recommendation is $10,000.
func approvalThresholdFromEnv() (float64, error) {
	raw := os.Getenv("S1_APPROVAL_THRESHOLD_USD")
	if raw == "" {
		return 0, errors.New("s1d: S1_APPROVAL_THRESHOLD_USD is not set")
	}
	threshold, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("s1d: S1_APPROVAL_THRESHOLD_USD: %w", err)
	}
	if threshold <= 0 {
		return 0, fmt.Errorf("s1d: S1_APPROVAL_THRESHOLD_USD must be positive, got %v", threshold)
	}
	return threshold, nil
}

func listenAddr() string {
	if addr := os.Getenv("S1_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8085"
}

// buildInfo reads module version and VCS revision from the binary's own
// embedded build metadata. Same implementation as every prior
// component's own daemon -- duplicated rather than shared, since these
// are separate modules with no common internal package between them.
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
