package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"relayd/internal/evmbroadcast"
	"relayd/internal/evmtx"
	"relayd/internal/ledgerclient"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

// startupDeps is everything runStartupSelfChecks probes.
type startupDeps struct {
	bsc          *evmbroadcast.Client
	provider     upstream.SwapProvider
	ledger       *ledgerclient.Client
	bep20Watcher *watcherclient.Client
	tronWatcher  *watcherclient.Client
}

// runStartupSelfChecks refuses to start relayd on anything that is
// definitely misconfigured -- a rejected token, a bad vendor key, the
// wrong chain, the wrong token decimals -- each of which previously
// surfaced only when a real customer order failed. A dependency that is
// merely unreachable is logged as a warning instead: it may come back,
// and relayd's own per-tick retries already handle a peer being down.
func runStartupSelfChecks(ctx context.Context, d startupDeps) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var fatal []string
	fail := func(format string, args ...any) { fatal = append(fatal, fmt.Sprintf(format, args...)) }
	warn := func(format string, args ...any) { slog.Warn("relayd: startup check: " + fmt.Sprintf(format, args...)) }

	if id, err := d.bsc.ChainID(ctx); err != nil {
		warn("BSC node unreachable, chain id not verified: %v", err)
	} else if id.Int64() != evmtx.BSCChainID {
		fail("BSC node reports chain id %s, want %d (BSC mainnet)", id, evmtx.BSCChainID)
	}
	if decimals, err := d.bsc.TokenDecimals(ctx, evmtx.USDTContractAddress); err != nil {
		warn("could not read USDT decimals() from the BSC node: %v", err)
	} else if int(decimals) != evmtx.USDTOnChainDecimals {
		fail("USDT contract %s reports %d decimals on-chain, but relayd encodes transfers for %d -- every BEP20 transfer would move the wrong amount",
			evmtx.USDTContractAddress, decimals, evmtx.USDTOnChainDecimals)
	}

	if checker, ok := d.provider.(upstream.SelfChecker); ok {
		var apiErr *upstream.APIError
		if err := checker.SelfCheck(ctx); errors.As(err, &apiErr) || errors.Is(err, upstream.ErrMisconfigured) {
			fail("upstream swap provider rejected relayd's configuration: %v", err)
		} else if err != nil {
			warn("upstream swap provider unreachable, credentials not verified: %v", err)
		}
	}

	_, err := d.ledger.GetOrder(ctx, "relayd-startup-token-probe")
	var ledgerErr *ledgerclient.APIError
	status := 0
	if errors.As(err, &ledgerErr) {
		status = ledgerErr.Status
	}
	checkPeerToken("ledger", err, status, fail, warn)

	for name, w := range map[string]*watcherclient.Client{"depositwatcher": d.bep20Watcher, "tronwatcher": d.tronWatcher} {
		_, err := w.GetAddress(ctx, 0)
		var watcherErr *watcherclient.APIError
		status := 0
		if errors.As(err, &watcherErr) {
			status = watcherErr.Status
		}
		checkPeerToken(name, err, status, fail, warn)
	}

	if len(fatal) > 0 {
		return fmt.Errorf("relayd: startup self-checks failed:\n  - %s", strings.Join(fatal, "\n  - "))
	}
	slog.Info("relayd: startup self-checks passed")
	return nil
}

// checkPeerToken classifies a probe against a peer that should answer
// "not found" for a made-up id. status is the peer's HTTP status, or 0
// if it never answered. 401/403 means relayd's token for that peer is
// wrong (fatal); any other answer means the token was accepted; no
// answer at all means the peer is down (warning).
func checkPeerToken(name string, err error, status int, fail, warn func(string, ...any)) {
	switch {
	case err == nil:
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		fail("%s rejected relayd's token (HTTP %d) -- check the token relayd sends against %s's own configured tokens", name, status, name)
	case status != 0:
	default:
		warn("%s unreachable, token not verified: %v", name, err)
	}
}
