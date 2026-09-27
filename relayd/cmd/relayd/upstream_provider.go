package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"relayd/internal/upstream"
	"relayd/internal/vendors"
)

// upstreamProviderFromEnv builds the conversion vendor(s) UPSTREAM_PROVIDER
// names. Every real vendor goes through vendors.ConversionRouter -- even a
// single one -- so its health is tracked, an administrator can disable it,
// and adding a second vendor later is only configuration:
//   - "fixedfloat", "changenow", "sideshift": that one vendor
//   - "router" (or "best_rate"): every vendor listed in UPSTREAM_VENDORS
//     (or UPSTREAM_BEST_RATE_PROVIDERS), e.g. "fixedfloat,changenow" --
//     each opted in explicitly, never just because credentials exist
//   - "placeholder" (with UPSTREAM_ALLOW_PLACEHOLDER=true): no vendor at all
//   - "demo" (with UPSTREAM_ALLOW_DEMO=true): a simulated vendor, for trying
//     the product locally without any vendor account
func upstreamProviderFromEnv(ctx context.Context, store *vendors.Store) (upstream.SwapProvider, string, error) {
	name := os.Getenv("UPSTREAM_PROVIDER")
	var names []string
	switch name {
	case "":
		return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER is not set -- set UPSTREAM_PROVIDER=fixedfloat, " +
			"UPSTREAM_PROVIDER=changenow, UPSTREAM_PROVIDER=sideshift, or UPSTREAM_PROVIDER=router for a real vendor (or vendors), " +
			"or UPSTREAM_PROVIDER=placeholder and UPSTREAM_ALLOW_PLACEHOLDER=true to run against a placeholder, " +
			"or UPSTREAM_PROVIDER=demo and UPSTREAM_ALLOW_DEMO=true to try the product locally")
	case "placeholder":
		if os.Getenv("UPSTREAM_ALLOW_PLACEHOLDER") != "true" {
			return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER=placeholder also requires UPSTREAM_ALLOW_PLACEHOLDER=true " +
				"-- upstream.PlaceholderProvider answers every call with ErrNoVendorConfigured, never a real quote")
		}
		return upstream.PlaceholderProvider{}, "placeholder", nil
	case "demo":
		// Quotes and orders come from upstream.MockProvider: nothing reaches a
		// real exchange, and its deposit addresses are not real addresses, so
		// nothing can ever be forwarded to it. Never part of a router list.
		if os.Getenv("UPSTREAM_ALLOW_DEMO") != "true" {
			return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER=demo also requires UPSTREAM_ALLOW_DEMO=true " +
				"-- the demo vendor simulates quotes and orders and must never serve real customers")
		}
		slog.Warn("relayd: UPSTREAM_PROVIDER=demo -- quotes and orders are simulated; never use this with real deposits")
		router, err := vendors.NewConversionRouter(ctx, store, map[string]upstream.SwapProvider{
			"demo": upstream.NewMockProvider("demo", time.Now().UnixNano()),
		})
		if err != nil {
			return nil, "", err
		}
		return router, "demo", nil
	case "fixedfloat", "changenow", "sideshift":
		names = []string{name}
	case "router", "best_rate":
		raw := os.Getenv("UPSTREAM_VENDORS")
		if raw == "" {
			raw = os.Getenv("UPSTREAM_BEST_RATE_PROVIDERS")
		}
		if raw == "" {
			return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER=%s also requires UPSTREAM_VENDORS "+
				"(comma-separated vendor names, e.g. \"fixedfloat,changenow\") -- no default, so a deployment can "+
				"never silently start routing through a vendor nobody explicitly opted in", name)
		}
		for _, n := range strings.Split(raw, ",") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
	default:
		return nil, "", fmt.Errorf("relayd: unrecognized UPSTREAM_PROVIDER %q (supported: \"fixedfloat\", \"changenow\", \"sideshift\", \"router\", \"placeholder\", \"demo\")", name)
	}

	providers := make(map[string]upstream.SwapProvider, len(names))
	for _, n := range names {
		var p upstream.SwapProvider
		var err error
		switch n {
		case "fixedfloat":
			p, err = buildFixedFloatProviderFromEnv()
		case "changenow":
			p, err = buildChangeNowProviderFromEnv()
		case "sideshift":
			p, err = buildSideshiftProviderFromEnv()
		default:
			return nil, "", fmt.Errorf("relayd: unrecognized conversion vendor %q (supported: \"fixedfloat\", \"changenow\", \"sideshift\")", n)
		}
		if err != nil {
			return nil, "", err
		}
		providers[n] = p
	}
	router, err := vendors.NewConversionRouter(ctx, store, providers)
	if err != nil {
		return nil, "", err
	}
	return router, strings.Join(names, ","), nil
}

// buildFixedFloatProviderFromEnv wires upstream.FixedFloatProvider from
// FIXEDFLOAT_* env vars -- factored out of the "fixedfloat" case above
// so "best_rate" (below) can build the exact same provider as one of
// its own routed vendors, never a second, drifted construction path.
func buildFixedFloatProviderFromEnv() (*upstream.FixedFloatProvider, error) {
	provider, err := upstream.NewFixedFloatProvider(upstream.FixedFloatConfig{
		APIKey:       os.Getenv("FIXEDFLOAT_API_KEY"),
		APISecret:    os.Getenv("FIXEDFLOAT_API_SECRET"),
		RefCode:      os.Getenv("FIXEDFLOAT_REFCODE"),
		USDTTRC20Ccy: os.Getenv("FIXEDFLOAT_CCY_USDT_TRC20"),
		USDTBEP20Ccy: os.Getenv("FIXEDFLOAT_CCY_USDT_BEP20"),
	})
	if err != nil {
		return nil, fmt.Errorf("relayd: configuring fixedfloat provider: %w -- set FIXEDFLOAT_API_KEY, "+
			"FIXEDFLOAT_API_SECRET, FIXEDFLOAT_CCY_USDT_TRC20, and FIXEDFLOAT_CCY_USDT_BEP20 "+
			"(the last two must be verified against a real GET /api/v2/ccies response, not guessed -- "+
			"see internal/upstream/fixedfloat.go's own top-of-file doc comment)", err)
	}
	return provider, nil
}

// buildChangeNowProviderFromEnv wires upstream.ChangeNowProvider from
// CHANGENOW_* env vars -- the second real vendor behind "best_rate"
// routing, per R2's own decision record.
func buildChangeNowProviderFromEnv() (*upstream.ChangeNowProvider, error) {
	provider, err := upstream.NewChangeNowProvider(upstream.ChangeNowConfig{
		APIKey:       os.Getenv("CHANGENOW_API_KEY"),
		USDTTRC20Ccy: os.Getenv("CHANGENOW_CCY_USDT_TRC20"),
		USDTBEP20Ccy: os.Getenv("CHANGENOW_CCY_USDT_BEP20"),
	})
	if err != nil {
		return nil, fmt.Errorf("relayd: configuring changenow provider: %w -- set CHANGENOW_API_KEY, "+
			"CHANGENOW_CCY_USDT_TRC20, and CHANGENOW_CCY_USDT_BEP20 "+
			"(the last two must be verified against a real GET /v1/currencies response, not guessed -- "+
			"see internal/upstream/changenow.go's own top-of-file doc comment)", err)
	}
	return provider, nil
}

// buildSideshiftProviderFromEnv wires upstream.SideshiftProvider from
// SIDESHIFT_* env vars. AffiliateID is SideShift's own account "id" as
// reported by GET /v2/account (authenticated by Secret alone) -- not a
// separately-issued value, confirmed live while building this
// integration (see internal/upstream/sideshift.go's own top-of-file
// doc comment).
func buildSideshiftProviderFromEnv() (*upstream.SideshiftProvider, error) {
	provider, err := upstream.NewSideshiftProvider(upstream.SideshiftConfig{
		Secret:           os.Getenv("SIDESHIFT_SECRET"),
		AffiliateID:      os.Getenv("SIDESHIFT_AFFILIATE_ID"),
		USDTTRC20Coin:    os.Getenv("SIDESHIFT_COIN_USDT_TRC20"),
		USDTTRC20Network: os.Getenv("SIDESHIFT_NETWORK_USDT_TRC20"),
		USDTBEP20Coin:    os.Getenv("SIDESHIFT_COIN_USDT_BEP20"),
		USDTBEP20Network: os.Getenv("SIDESHIFT_NETWORK_USDT_BEP20"),
	})
	if err != nil {
		return nil, fmt.Errorf("relayd: configuring sideshift provider: %w -- set SIDESHIFT_SECRET, "+
			"SIDESHIFT_AFFILIATE_ID (your account's own \"id\" from GET /v2/account), "+
			"SIDESHIFT_COIN_USDT_TRC20/SIDESHIFT_NETWORK_USDT_TRC20 (\"USDT\"/\"tron\", verified live), and "+
			"SIDESHIFT_COIN_USDT_BEP20/SIDESHIFT_NETWORK_USDT_BEP20 (\"USDT\"/\"bsc\", verified live)", err)
	}
	return provider, nil
}
