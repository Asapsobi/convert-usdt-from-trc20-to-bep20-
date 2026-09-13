package addresses

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenIdentifiers are qualified references to actual signing-capable
// constructs from this package's dependencies -- deliberately NOT the bare
// word "PrivateKey" or similar, because this package's own vocabulary for
// REJECTING private key material (ErrPrivateKeyMaterial,
// rejectPrivateKeyShaped) legitimately contains that word throughout; a
// naive substring match would flag the very code that enforces the
// guarantee. What must never appear is a reference to a dependency's own
// private-key type or signing/generation entry points.
var forbiddenIdentifiers = []string{
	"secp256k1.PrivKeyFromBytes",
	"secp256k1.NewPrivateKey",
	"secp256k1.GeneratePrivateKey",
	"bip32.NewMasterKey", // go-bip32's seed -> private extended key constructor
	".Sign(",             // any ECDSA/Schnorr signing call
	"bip39",              // any mnemonic library, should one ever be added by accident
}

// TestPackageSourceNeverReferencesPrivateKeyMaterial scans every .go file
// in this package (excluding this test file itself and other tests, which
// are allowed to name these identifiers in error messages and comments
// when testing that they're rejected) for the forbidden identifiers above.
func TestPackageSourceNeverReferencesPrivateKeyMaterial(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue // test files may legitimately name these to assert rejection
		}
		path := filepath.Join(".", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		content := string(data)
		for _, forbidden := range forbiddenIdentifiers {
			if strings.Contains(content, forbidden) {
				t.Errorf("%s references forbidden identifier %q -- this package must never "+
					"handle private key material, see derive.go's doc comment", path, forbidden)
			}
		}
	}
}
