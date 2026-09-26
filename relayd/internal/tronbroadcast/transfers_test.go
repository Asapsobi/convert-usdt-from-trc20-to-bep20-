package tronbroadcast

import "testing"

// A TRC-20 log's hex contract and 32-byte topic both decode to the same
// base58 address -- here USDT's own contract.
func TestTronAddressFromHex(t *testing.T) {
	const usdt = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	for _, h := range []string{
		"a614f803b6fd780986a42c78ec9c7f77e6ded13c",
		"000000000000000000000000a614f803b6fd780986a42c78ec9c7f77e6ded13c",
	} {
		got, err := tronAddressFromHex(h)
		if err != nil || got != usdt {
			t.Errorf("tronAddressFromHex(%s) = %q, %v; want %s", h, got, err, usdt)
		}
	}
	if _, err := tronAddressFromHex("abcd"); err == nil {
		t.Error("a short address must be rejected")
	}
}
