package evmbroadcast

import "testing"

// The rest of this package (Broadcast, IsFinal, CurrentNonce,
// SuggestGasPrice) needs a real BSC node connection to test meaningfully
// -- NOT exercised against a live node in this pass, matching
// dispatcher/internal/dispatch/tronchain.go's own identical, explicitly
// flagged limitation for the TRC20-side broadcast client (that file's
// own doc comment: "this has NOT been exercised against a live node").
// trimHexPrefix is the one piece of real logic here that doesn't need
// one.
func TestTrimHexPrefix(t *testing.T) {
	cases := map[string]string{
		"0x1a": "1a",
		"0X1a": "1a",
		"1a":   "1a",
		"":     "",
		"0x":   "",
		"x1a":  "x1a",
	}
	for in, want := range cases {
		if got := trimHexPrefix(in); got != want {
			t.Errorf("trimHexPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
