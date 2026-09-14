package main

import (
	"fmt"
	"os"

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
// "fixedfloat" is R4's own real vendor, per R2's decision record
// (docs/01-strategy/model-f-relay-findings.md) -- every one of its own
// credential/currency-code fields is required with no default, the same
// "no hardcoded defaults for anything real-money-shaped" discipline this
// function's own placeholder branch already follows for
// UPSTREAM_ALLOW_PLACEHOLDER.
func upstreamProviderFromEnv() (upstream.SwapProvider, string, error) {
	name := os.Getenv("UPSTREAM_PROVIDER")
	switch name {
	case "":
		return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER is not set -- set UPSTREAM_PROVIDER=fixedfloat for the real vendor, " +
			"or UPSTREAM_PROVIDER=placeholder and UPSTREAM_ALLOW_PLACEHOLDER=true to run against a placeholder")
	case "placeholder":
		if os.Getenv("UPSTREAM_ALLOW_PLACEHOLDER") != "true" {
			return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER=placeholder also requires UPSTREAM_ALLOW_PLACEHOLDER=true " +
				"-- upstream.PlaceholderProvider answers every call with ErrNoVendorConfigured, never a real quote")
		}
		return upstream.PlaceholderProvider{}, "placeholder", nil
	case "fixedfloat":
		provider, err := upstream.NewFixedFloatProvider(upstream.FixedFloatConfig{
			APIKey:       os.Getenv("FIXEDFLOAT_API_KEY"),
			APISecret:    os.Getenv("FIXEDFLOAT_API_SECRET"),
			RefCode:      os.Getenv("FIXEDFLOAT_REFCODE"),
			USDTTRC20Ccy: os.Getenv("FIXEDFLOAT_CCY_USDT_TRC20"),
			USDTBEP20Ccy: os.Getenv("FIXEDFLOAT_CCY_USDT_BEP20"),
		})
		if err != nil {
			return nil, "", fmt.Errorf("relayd: configuring fixedfloat provider: %w -- set FIXEDFLOAT_API_KEY, "+
				"FIXEDFLOAT_API_SECRET, FIXEDFLOAT_CCY_USDT_TRC20, and FIXEDFLOAT_CCY_USDT_BEP20 "+
				"(the last two must be verified against a real GET /api/v2/ccies response, not guessed -- "+
				"see internal/upstream/fixedfloat.go's own top-of-file doc comment)", err)
		}
		return provider, "fixedfloat", nil
	default:
		return nil, "", fmt.Errorf("relayd: unrecognized UPSTREAM_PROVIDER %q (supported: unset, \"placeholder\", \"fixedfloat\")", name)
	}
}
