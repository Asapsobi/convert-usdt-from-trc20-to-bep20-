// Command s1d serves S1, key management and signing, over HTTP: bearer
// auth (two separate scopes -- C5's own SigningService calls, and human
// approver actions, see internal/httpapi/auth.go), /healthz, /readyz,
// /metrics, and the full business API under /v1.
//
// This binary wires kmssign.Wrapper against a KMSClient built from
// S1_KMS_CLIENT: "fake" uses an in-process FakeKMSClient, seeded from
// S1_KMS_FAKE_SEED -- never for production, signing against a
// synthetic, worthless key. "privy" uses kmssign.PrivyKMSClient, real
// custody via Privy (privy.io) Server Wallets (PRIVY_APP_ID/
// PRIVY_APP_SECRET) -- slot-key CREATION itself stays a human, out-of-
// band operational step (see
// docs/02-architecture/s1-key-custody-architecture.md's own "Key
// generation and bootstrapping", and internal/kmssign/privy_kms_client.go's
// own top-of-file doc comment); this binary only ever consumes an
// already-created wallet's id as kms_key_id. There is deliberately no
// default for S1_KMS_CLIENT, so a misconfigured production environment
// fails to start instead of silently signing real payouts against fake,
// worthless keys.
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
	provisioningAuth, err := httpapi.ProvisioningAuthConfigFromEnv()
	if err != nil {
		return err
	}

	kmsClient, err := kmsClientFromEnv()
	if err != nil {
		return err
	}
	wrapper := kmssign.NewWrapper(kmsClient)

	depositKeysConfigured, bscDepositProvisioner, err := configureBSCDepositSigningFromEnv(pool, wrapper)
	if err != nil {
		return err
	}
	tronDepositKeysConfigured, tronDepositProvisioner, err := configureTronDepositSigningFromEnv(pool, wrapper)
	if err != nil {
		return err
	}

	slotStore := slots.NewStore(pool, wrapper)

	threshold, err := approvalThresholdFromEnv()
	if err != nil {
		return err
	}
	// wrapper itself satisfies requests.Signer, requests.DepositKeyGetter,
	// and requests.TronDepositKeyGetter -- when *KeysConfigured is false,
	// the corresponding Set*DepositKeys was never called, so wrapper's
	// own Sign*Deposit/PublicKeyFor*Deposit methods return
	// Err*DepositKeysNotConfigured, which Store surfaces to a caller as
	// Err*DepositSigningNotConfigured (see requests/store.go's own
	// signWith).
	var depositKeys requests.DepositKeyGetter
	if depositKeysConfigured {
		depositKeys = wrapper
	}
	var tronDepositKeys requests.TronDepositKeyGetter
	if tronDepositKeysConfigured {
		tronDepositKeys = wrapper
	}
	signingStore := requests.NewStore(pool, slotKeyGetterAdapter{slotStore}, depositKeys, tronDepositKeys, tronDepositProvisioner, bscDepositProvisioner, wrapper, requests.Config{ApprovalThresholdUSD: threshold})

	server := &httpapi.Server{
		Pool:         pool,
		C5Auth:       c5Auth,
		Approver:     approverAuth,
		Provisioning: provisioningAuth,
		Signing:      signingStore,
		BuildInfo:    buildInfo,
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
	case "privy":
		appID := os.Getenv("PRIVY_APP_ID")
		appSecret := os.Getenv("PRIVY_APP_SECRET")
		if appID == "" || appSecret == "" {
			return nil, errors.New("s1d: S1_KMS_CLIENT=privy requires PRIVY_APP_ID and PRIVY_APP_SECRET to both be set")
		}
		slog.Info("s1d: S1_KMS_CLIENT=privy -- slot keys are signed through real Privy Server Wallets")
		return kmssign.NewPrivyKMSClient(appID, appSecret), nil
	case "":
		return nil, errors.New("s1d: S1_KMS_CLIENT is not set (no default -- see this binary's own doc comment)")
	default:
		return nil, fmt.Errorf("s1d: S1_KMS_CLIENT=%q is not a recognized KMS client (\"fake\" or \"privy\")", os.Getenv("S1_KMS_CLIENT"))
	}
}

// configureBSCDepositSigningFromEnv is configureTronDepositSigningFromEnv's
// own BSC-deposit counterpart -- TWO possible backends, mutually
// exclusive: S1_BSC_DEPOSIT_XPRV/XPUB both set enables local, in-process
// BIP32 signing (unchanged, original behavior); PRIVY_APP_ID/
// PRIVY_APP_SECRET both set enables real custody via Privy Server
// Wallets instead (see internal/kmssign/privybsc.go) -- the SAME two env
// vars already used for slot-key and TRON-deposit-key Privy signing, one
// Privy app, independent wallets, not a new credential. ONLY the Privy
// branch returns a non-nil BSCDepositProvisioner -- provisioning new
// custody is meaningless in local-BIP32 mode, where deriving a child
// address is pure, free local math with no state to ever record.
// Neither backend configured is a legitimate, supported mode (disabled);
// BOTH configured is a real misconfiguration -- fails loud rather than
// silently preferring one over the other.
//
// OPERATIONAL INVARIANT this function cannot itself enforce: this
// deployment's BSC-SIGNING backend choice must agree with depositwatcher's
// own BSC-ADDRESSING backend choice (see that binary's own
// cmd/watcherd/main.go doc comment). If they disagree, signing for
// deposit index N silently uses a different key than the address at
// index N was derived from -- Privy mode has no local xpub to
// cross-check against, unlike local-BIP32 mode's own xprv/xpub match
// check.
func configureBSCDepositSigningFromEnv(pool *db.Pool, wrapper *kmssign.Wrapper) (bool, requests.BSCDepositProvisioner, error) {
	xprv := os.Getenv("S1_BSC_DEPOSIT_XPRV")
	xpub := os.Getenv("S1_BSC_DEPOSIT_XPUB")
	localSet := xprv != "" || xpub != ""
	if localSet && (xprv == "" || xpub == "") {
		return false, nil, errors.New("s1d: S1_BSC_DEPOSIT_XPRV and S1_BSC_DEPOSIT_XPUB must both be set together, or both left unset")
	}

	privyAppID := os.Getenv("PRIVY_APP_ID")
	privyAppSecret := os.Getenv("PRIVY_APP_SECRET")
	privySet := privyAppID != "" || privyAppSecret != ""
	if privySet && (privyAppID == "" || privyAppSecret == "") {
		return false, nil, errors.New("s1d: PRIVY_APP_ID and PRIVY_APP_SECRET must both be set together, or both left unset")
	}

	switch {
	case localSet && privySet:
		return false, nil, errors.New("s1d: S1_BSC_DEPOSIT_XPRV/XPUB and PRIVY_APP_ID/PRIVY_APP_SECRET are both set -- exactly one BSC deposit-signing backend must be configured, never both")
	case localSet:
		keys, err := kmssign.NewBSCDepositKeys(xprv, xpub)
		if err != nil {
			return false, nil, fmt.Errorf("s1d: configuring BSC deposit keys: %w", err)
		}
		wrapper.SetBSCDepositKeys(keys)
		slog.Info("s1d: BSC deposit-sweep signing enabled (local BIP32)")
		return true, nil, nil
	case privySet:
		privyKeys := kmssign.NewPrivyBSCDepositKeys(pool, privyAppID, privyAppSecret)
		wrapper.SetBSCDepositKeys(privyKeys)
		slog.Warn("s1d: BSC deposit-sweep signing enabled (Privy Server Wallets) -- confirm depositwatcher's own BSC-addressing backend is ALSO Privy-backed against this SAME S1 deployment, or signing will silently use the wrong key for a given deposit index")
		return true, bscDepositProvisionerAdapter{privyKeys}, nil
	default:
		slog.Info("s1d: S1_BSC_DEPOSIT_XPRV/XPUB and PRIVY_APP_ID/PRIVY_APP_SECRET not set -- BSC deposit-sweep signing is disabled on this deployment")
		return false, nil, nil
	}
}

// bscDepositProvisionerAdapter adapts *kmssign.PrivyBSCDepositKeys's own
// Provision (which returns kmssign.PrivyBSCKey, a type this package has
// no reason to depend on) to requests.BSCDepositProvisioner -- the same
// thin, package-boundary adapter shape as tronDepositProvisionerAdapter
// above.
type bscDepositProvisionerAdapter struct{ keys *kmssign.PrivyBSCDepositKeys }

func (a bscDepositProvisionerAdapter) Provision(ctx context.Context, index uint32) (string, [33]byte, error) {
	key, err := a.keys.Provision(ctx, index)
	if err != nil {
		return "", [33]byte{}, err
	}
	return key.Address, key.PublicKey, nil
}

// configureTronDepositSigningFromEnv is configureBSCDepositKeysFromEnv's
// own TRON-deposit counterpart, but with TWO possible backends instead
// of one -- see docs/02-architecture/s1-key-custody-architecture.md's
// own Privy custody work: S1_TRON_DEPOSIT_XPRV/XPUB both set enables
// local, in-process BIP32 signing (unchanged, original behavior);
// PRIVY_APP_ID/PRIVY_APP_SECRET both set enables real custody via Privy
// (privy.io) Server Wallets instead. ONLY the Privy branch returns a
// non-nil TronDepositProvisioner -- provisioning new custody is
// meaningless in local-BIP32 mode, where deriving a child address is
// pure, free local math with no state to ever record. Neither backend
// configured is a legitimate, supported mode (disabled); BOTH
// configured is a real misconfiguration -- fails loud rather than
// silently preferring one over the other.
//
// OPERATIONAL INVARIANT this function cannot itself enforce: this
// deployment's TRON-SIGNING backend choice must agree with
// tronwatcher's own TRON-ADDRESSING backend choice (see that binary's
// own cmd/tronwatcherd/main.go doc comment). If they disagree, signing
// for deposit index N silently uses a different key than the address at
// index N was derived from -- Privy mode has no local xpub to
// cross-check against, unlike today's xprv/xpub match checks.
func configureTronDepositSigningFromEnv(pool *db.Pool, wrapper *kmssign.Wrapper) (bool, requests.TronDepositProvisioner, error) {
	xprv := os.Getenv("S1_TRON_DEPOSIT_XPRV")
	xpub := os.Getenv("S1_TRON_DEPOSIT_XPUB")
	localSet := xprv != "" || xpub != ""
	if localSet && (xprv == "" || xpub == "") {
		return false, nil, errors.New("s1d: S1_TRON_DEPOSIT_XPRV and S1_TRON_DEPOSIT_XPUB must both be set together, or both left unset")
	}

	privyAppID := os.Getenv("PRIVY_APP_ID")
	privyAppSecret := os.Getenv("PRIVY_APP_SECRET")
	privySet := privyAppID != "" || privyAppSecret != ""
	if privySet && (privyAppID == "" || privyAppSecret == "") {
		return false, nil, errors.New("s1d: PRIVY_APP_ID and PRIVY_APP_SECRET must both be set together, or both left unset")
	}

	switch {
	case localSet && privySet:
		return false, nil, errors.New("s1d: S1_TRON_DEPOSIT_XPRV/XPUB and PRIVY_APP_ID/PRIVY_APP_SECRET are both set -- exactly one TRON deposit-signing backend must be configured, never both")
	case localSet:
		keys, err := kmssign.NewTronDepositKeys(xprv, xpub)
		if err != nil {
			return false, nil, fmt.Errorf("s1d: configuring TRON deposit keys: %w", err)
		}
		wrapper.SetTronDepositKeys(keys)
		slog.Info("s1d: TRON deposit-sweep signing enabled (local BIP32)")
		return true, nil, nil
	case privySet:
		privyKeys := kmssign.NewPrivyTronDepositKeys(pool, privyAppID, privyAppSecret)
		wrapper.SetTronDepositKeys(privyKeys)
		slog.Warn("s1d: TRON deposit-sweep signing enabled (Privy Server Wallets) -- confirm tronwatcher's own TRON-addressing backend is ALSO Privy-backed against this SAME S1 deployment, or signing will silently use the wrong key for a given deposit index")
		return true, tronDepositProvisionerAdapter{privyKeys}, nil
	default:
		slog.Info("s1d: S1_TRON_DEPOSIT_XPRV/XPUB and PRIVY_APP_ID/PRIVY_APP_SECRET not set -- TRON deposit-sweep signing is disabled on this deployment")
		return false, nil, nil
	}
}

// tronDepositProvisionerAdapter adapts *kmssign.PrivyTronDepositKeys's
// own Provision (which returns kmssign.PrivyTronKey, a type this
// package has no reason to depend on) to requests.TronDepositProvisioner
// -- the same thin, package-boundary adapter shape as
// slotKeyGetterAdapter above.
type tronDepositProvisionerAdapter struct{ keys *kmssign.PrivyTronDepositKeys }

func (a tronDepositProvisionerAdapter) Provision(ctx context.Context, index uint32) (string, [33]byte, error) {
	key, err := a.keys.Provision(ctx, index)
	if err != nil {
		return "", [33]byte{}, err
	}
	return key.Address, key.PublicKey, nil
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
