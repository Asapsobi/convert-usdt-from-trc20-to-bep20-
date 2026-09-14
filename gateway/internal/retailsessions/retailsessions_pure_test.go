package retailsessions_test

import (
	"testing"

	"gateway/internal/retailsessions"
)

func TestHashToken_DeterministicAndDistinct(t *testing.T) {
	h1 := retailsessions.HashToken("rs_abc")
	h2 := retailsessions.HashToken("rs_abc")
	if h1 != h2 {
		t.Fatalf("HashToken is not deterministic: %q != %q", h1, h2)
	}
	h3 := retailsessions.HashToken("rs_xyz")
	if h1 == h3 {
		t.Fatal("HashToken produced the same hash for two different tokens")
	}
	if h1 == "rs_abc" {
		t.Fatal("HashToken returned the raw token unchanged -- not actually hashed")
	}
}
