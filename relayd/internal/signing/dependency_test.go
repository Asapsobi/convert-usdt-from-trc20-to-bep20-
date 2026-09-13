package signing

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenIdentifiers mirrors dispatcher/internal/signing's own
// identical list -- this module has no legitimate reason to ever
// construct a private key or sign anything itself.
var forbiddenIdentifiers = []string{
	"secp256k1.PrivKeyFromBytes",
	"secp256k1.GeneratePrivateKey",
	"ecdsa.Sign(",
}

// TestNoSigningMaterialOutsideSigningPackage scans every .go file in
// this module, excluding internal/signing itself and _test.go files,
// for a forbidden identifier.
func TestNoSigningMaterialOutsideSigningPackage(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	signingDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving internal/signing's own absolute path: %v", err)
	}

	fset := token.NewFileSet()
	err = filepath.Walk(moduleRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(abs, signingDir+string(filepath.Separator)) || abs == signingDir {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		content := string(data)
		for _, forbidden := range forbiddenIdentifiers {
			if strings.Contains(content, forbidden) {
				t.Errorf("%s references forbidden identifier %q -- signing-capable code belongs only behind a real S1, called only from internal/signing", path, forbidden)
			}
		}

		_, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module for signing-material check: %v", err)
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking up from internal/signing")
		}
		dir = parent
	}
}
