// Package testtronwatcher is shared test infrastructure: it builds and
// runs a REAL tronwatcher (C2', TRON) tronwatcherd binary as a
// subprocess and drives it purely over HTTP, mirroring
// relayd/internal/testdepositwatcher's own identical shape (duplicated,
// not shared: separate Go modules, no common internal package, same
// convention as every other service here).
//
// Started in HTTP-boundary-only mode: TRONWATCHER_PROVIDERS is
// deliberately left unset, which cmd/tronwatcherd/main.go already
// documents as a supported configuration ("running without it (HTTP
// boundary only) is a legitimate, supported mode, mirroring
// depositwatcher/cmd/watcherd's own identical posture") -- no fake TRON
// RPC node is needed here at all, since the only calls this fixture
// exists for are real POST /v1/addresses / GET /v1/addresses/{order_id}.
//
// Not a _test.go file: Go doesn't let one package's _test.go helpers be
// imported by another package's tests, so this has to be an ordinary
// package, imported only from _test.go files in practice.
package testtronwatcher

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Watcher is a real tronwatcherd process, built and started fresh for
// one test.
type Watcher struct {
	t       *testing.T
	baseURL string
	token   string
}

// Start builds tronwatcher/cmd/migrate and tronwatcher/cmd/tronwatcherd
// from the sibling tronwatcher module (../../../tronwatcher, the
// usdt-settlement-corridor layout this whole project uses), runs
// migrations against dbURL, and starts tronwatcherd listening on
// listenAddr with the given xpub -- HTTP-boundary-only, no live
// chain-watching engine.
func Start(t *testing.T, dbURL, listenAddr, xpub, token, actor string) *Watcher {
	t.Helper()
	if dbURL == "" {
		t.Skip("no test database URL set; skipping integration test")
	}

	watcherRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "tronwatcher"))
	if err != nil {
		t.Fatalf("resolving tronwatcher module path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(watcherRoot, "go.mod")); err != nil {
		t.Skipf("no sibling tronwatcher module found at %s; skipping (expects the usdt-settlement-corridor layout)", watcherRoot)
	}

	tmpDir := t.TempDir()
	migrateBin := filepath.Join(tmpDir, "migrate_bin")
	tronwatcherdBin := filepath.Join(tmpDir, "tronwatcherd_bin")

	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = watcherRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, output)
		}
	}
	build(migrateBin, "./cmd/migrate")
	build(tronwatcherdBin, "./cmd/tronwatcherd")

	migrate := exec.Command(migrateBin, "up")
	migrate.Dir = watcherRoot
	migrate.Env = append(os.Environ(), "TRONWATCHER_DATABASE_URL="+dbURL)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("running tronwatcher migrations: %v\n%s", err, output)
	}

	tronwatcherd := exec.Command(tronwatcherdBin)
	tronwatcherd.Dir = watcherRoot
	tronwatcherd.Env = append(os.Environ(),
		"TRONWATCHER_DATABASE_URL="+dbURL,
		"TRONWATCHER_XPUB="+xpub,
		"TRONWATCHER_API_TOKENS="+token+":"+actor,
		"TRONWATCHER_LISTEN_ADDR="+listenAddr,
		// TRONWATCHER_PROVIDERS deliberately unset -- HTTP boundary only.
	)
	var logs bytes.Buffer
	tronwatcherd.Stdout = &logs
	tronwatcherd.Stderr = &logs
	if err := tronwatcherd.Start(); err != nil {
		t.Fatalf("starting tronwatcherd: %v", err)
	}
	t.Cleanup(func() {
		_ = tronwatcherd.Process.Kill()
		_ = tronwatcherd.Wait()
		if t.Failed() {
			t.Logf("tronwatcherd output:\n%s", logs.String())
		}
	})

	baseURL := "http://localhost" + listenAddr
	w := &Watcher{t: t, baseURL: baseURL, token: token}
	w.waitHealthy()
	return w
}

// BaseURL is this tronwatcherd instance's own base URL.
func (w *Watcher) BaseURL() string { return w.baseURL }

// Token is the bearer token this tronwatcherd instance was started with.
func (w *Watcher) Token() string { return w.token }

func (w *Watcher) waitHealthy() {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(w.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.t.Fatalf("tronwatcherd never became healthy at %s within the deadline", w.baseURL)
}
