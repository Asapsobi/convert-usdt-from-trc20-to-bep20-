package upstream

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenVendorImportFragments are substrings that would appear in an
// import PATH for a specific instant-exchange vendor's own SDK. No real
// vendor is chosen yet (R2 is still open), so this list starts empty --
// the mechanism itself is what R0 ships, ready for R4 to add real
// fragments to the moment a vendor SDK is vendored in, the same way
// energybroker/internal/provider/dependency_test.go matches only import
// declarations, never arbitrary file text, so a vendor's name used as a
// plain config/business string elsewhere in this module (e.g. a
// provider-name constant) is never mistaken for an SDK dependency.
var forbiddenVendorImportFragments = []string{}

// TestNoVendorSDKOutsideUpstreamPackage parses the import block of every
// .go file in this module, excluding internal/upstream itself (where a
// real vendor client belongs) and _test.go files (which may legitimately
// name a vendor in a comment or string when documenting this exact
// rule), for an import path containing a forbidden vendor fragment. Same
// posture as C2's, C3's, and C4's own dependency-boundary tests: a
// mechanical guard against a specific accidental dependency, not a style
// preference.
func TestNoVendorSDKOutsideUpstreamPackage(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	upstreamDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving internal/upstream's own absolute path: %v", err)
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
		if strings.HasPrefix(abs, upstreamDir+string(filepath.Separator)) || abs == upstreamDir {
			return nil // internal/upstream is where a vendor SDK belongs
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range file.Imports {
			importPath := strings.ToLower(strings.Trim(imp.Path.Value, `"`))
			for _, fragment := range forbiddenVendorImportFragments {
				if strings.Contains(importPath, fragment) {
					t.Errorf("%s imports %q, containing vendor-SDK fragment %q -- vendor integrations belong only in internal/upstream, behind SwapProvider",
						path, importPath, fragment)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module for vendor-SDK check: %v", err)
	}
}

// findModuleRoot walks up from the current package directory until it
// finds go.mod, so this test works regardless of the working directory
// `go test` is invoked from.
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
			t.Fatal("could not find go.mod walking up from internal/upstream")
		}
		dir = parent
	}
}
