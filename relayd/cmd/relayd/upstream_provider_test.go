package main

import (
	"context"
	"strings"
	"testing"
)

// The demo vendor needs its own second switch, like the placeholder.
func TestUpstreamProvider_DemoNeedsExplicitOptIn(t *testing.T) {
	t.Setenv("UPSTREAM_PROVIDER", "demo")
	t.Setenv("UPSTREAM_ALLOW_DEMO", "")
	_, _, err := upstreamProviderFromEnv(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "UPSTREAM_ALLOW_DEMO=true") {
		t.Fatalf("got %v, want an error asking for UPSTREAM_ALLOW_DEMO=true", err)
	}
}

// The demo vendor can never be routed alongside real vendors, where it
// could win a best-rate comparison against them.
func TestUpstreamProvider_DemoIsNeverARoutedVendor(t *testing.T) {
	t.Setenv("UPSTREAM_PROVIDER", "router")
	t.Setenv("UPSTREAM_VENDORS", "demo")
	t.Setenv("UPSTREAM_ALLOW_DEMO", "true")
	_, _, err := upstreamProviderFromEnv(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), `unrecognized conversion vendor "demo"`) {
		t.Fatalf("got %v, want demo rejected as a routed vendor", err)
	}
}
