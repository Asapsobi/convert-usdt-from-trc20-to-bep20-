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
func upstreamProviderFromEnv() (upstream.SwapProvider, string, error) {
	name := os.Getenv("UPSTREAM_PROVIDER")
	switch name {
	case "":
		return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER is not set -- no real vendor is wired yet (R2), " +
			"set UPSTREAM_PROVIDER=placeholder and UPSTREAM_ALLOW_PLACEHOLDER=true to run against a placeholder for now")
	case "placeholder":
		if os.Getenv("UPSTREAM_ALLOW_PLACEHOLDER") != "true" {
			return nil, "", fmt.Errorf("relayd: UPSTREAM_PROVIDER=placeholder also requires UPSTREAM_ALLOW_PLACEHOLDER=true " +
				"-- upstream.PlaceholderProvider answers every call with ErrNoVendorConfigured, never a real quote")
		}
		return upstream.PlaceholderProvider{}, "placeholder", nil
	default:
		return nil, "", fmt.Errorf("relayd: unrecognized UPSTREAM_PROVIDER %q (supported: unset, \"placeholder\" -- "+
			"no real vendor integration exists yet, see docs/03-build/model-f-relay-build-prompts.md's own R2/R4)", name)
	}
}
