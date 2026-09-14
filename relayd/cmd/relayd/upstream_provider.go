package main

import (
	"fmt"
	"os"
	"strings"

	"relayd/internal/upstream"
)

// upstreamProviderFromEnv reads UPSTREAM_PROVIDER. Mirrors screening's
// own screeningProviderFromEnv double-gate convention exactly (see
// screening/cmd/screend/main.go): unset means no real vendor is wired
// (relayd's own front door and orchestrate loop cannot run without one
// -- unlike screening's own optional engine, a swap provider is not
// optional here), "placeholder" requires UPSTREAM_ALLOW_PLACEHOLDER=true
// alongside it so a real deployment can never reach
// upstream.PlaceholderProvider through one mistyped env var alone.
// "fixedfloat"/"changenow" are R4's own real vendors, per R2's decision
// record (docs/01-strategy/model-f-relay-findings.md) -- every one of
// their own credential/currency-code fields is required with no
// default, the same "no hardcoded defaults for anything real-money-
// shaped" discipline this function's own placeholder branch already
// follows for UPSTREAM_ALLOW_PLACEHOLDER. "best_rate" wires
// upstream.MultiProvider across 2+ of the above, per
// UPSTREAM_BEST_RATE_PROVIDERS -- see that branch's own comment.
func upstreamProviderFromEnv() (upstream.SwapProvider, string, error) {
	name := os.Getenv("UPSTREAM_PROVIDER")
	switch name {
	case "":
		return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER is not set -- set UPSTREAM_PROVIDER=fixedfloat, " +
			"UPSTREAM_PROVIDER=changenow, or UPSTREAM_PROVIDER=best_rate for a real vendor (or vendors), " +
			"or UPSTREAM_PROVIDER=placeholder and UPSTREAM_ALLOW_PLACEHOLDER=true to run against a placeholder")
	case "placeholder":
		if os.Getenv("UPSTREAM_ALLOW_PLACEHOLDER") != "true" {
			return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER=placeholder also requires UPSTREAM_ALLOW_PLACEHOLDER=true " +
				"-- upstream.PlaceholderProvider answers every call with ErrNoVendorConfigured, never a real quote")
		}
		return upstream.PlaceholderProvider{}, "placeholder", nil
	case "fixedfloat":
		provider, err := buildFixedFloatProviderFromEnv()
		if err != nil {
			return nil, "", err
		}
		return provider, "fixedfloat", nil
	case "changenow":
		provider, err := buildChangeNowProviderFromEnv()
		if err != nil {
			return nil, "", err
		}
		return provider, "changenow", nil
	case "best_rate":
		provider, err := buildBestRateProviderFromEnv()
		if err != nil {
			return nil, "", err
		}
		return provider, "best_rate", nil
	default:
		return nil, "", fmt.Errorf("relayd: unrecognized UPSTREAM_PROVIDER %q (supported: unset, \"placeholder\", \"fixedfloat\", \"changenow\", \"best_rate\")", name)
	}
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

// buildBestRateProviderFromEnv wires upstream.MultiProvider across every
// vendor named in UPSTREAM_BEST_RATE_PROVIDERS (comma-separated, e.g.
// "fixedfloat,changenow") -- required with no default, same reasoning
// as every other real-money-shaped config in this function: a
// deployment must explicitly opt every vendor in, never silently route
// through whichever ones happen to have credentials set. Each named
// vendor's own credentials are read from that vendor's own env vars
// (FIXEDFLOAT_*/CHANGENOW_*), exactly as the standalone "fixedfloat"/
// "changenow" branches above read them -- adding a real vendor to the
// router later needs no new plumbing here beyond adding its name to the
// list, once that vendor has its own build*ProviderFromEnv function.
func buildBestRateProviderFromEnv() (*upstream.MultiProvider, error) {
	raw := os.Getenv("UPSTREAM_BEST_RATE_PROVIDERS")
	if raw == "" {
		return nil, fmt.Errorf("relayd: UPSTREAM_PROVIDER=best_rate also requires UPSTREAM_BEST_RATE_PROVIDERS " +
			"(comma-separated vendor names, e.g. \"fixedfloat,changenow\") -- no default, so a deployment can " +
			"never silently start routing through a vendor nobody explicitly opted in")
	}
	names := strings.Split(raw, ",")

	providers := make([]upstream.NamedProvider, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		switch name {
		case "fixedfloat":
			provider, err := buildFixedFloatProviderFromEnv()
			if err != nil {
				return nil, err
			}
			providers = append(providers, upstream.NamedProvider{Name: "fixedfloat", Provider: provider})
		case "changenow":
			provider, err := buildChangeNowProviderFromEnv()
			if err != nil {
				return nil, err
			}
			providers = append(providers, upstream.NamedProvider{Name: "changenow", Provider: provider})
		default:
			return nil, fmt.Errorf("relayd: UPSTREAM_BEST_RATE_PROVIDERS names unrecognized vendor %q (supported: \"fixedfloat\", \"changenow\")", name)
		}
	}

	router, err := upstream.NewMultiProvider(providers)
	if err != nil {
		return nil, fmt.Errorf("relayd: configuring best_rate router: %w", err)
	}
	return router, nil
}
