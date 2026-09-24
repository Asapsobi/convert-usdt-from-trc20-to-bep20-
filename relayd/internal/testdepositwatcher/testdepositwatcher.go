// Package testdepositwatcher is shared test infrastructure: it builds
// and runs a REAL depositwatcher (C2, BSC) watcherd binary as a
// subprocess and drives it purely over HTTP, mirroring
// relayd/internal/testledger's own identical shape (duplicated, not
// shared: separate Go modules, no common internal package, same
// convention as every other service here).
//
// Started in HTTP-boundary-only mode: WATCHER_RPC_PROVIDERS is
// deliberately left unset, which cmd/watcherd/main.go already documents
// as a supported configuration ("a deployment that only wants the HTTP
// boundary... without the live chain-watching engine is a legitimate,
// already-supported configuration") -- no fake BSC RPC node is needed
// here at all, since the only calls this fixture exists for are real
// POST /v1/addresses / GET /v1/addresses/{order_id}.
//
// Not a _test.go file: Go doesn't let one package's _test.go helpers be
// imported by another package's tests, so this has to be an ordinary
// package, imported only from _test.go files in practice.
package testdepositwatcher

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Watcher is a real watcherd process, built and started fresh for one
// test.
type Watcher struct {
	t       *testing.T
	baseURL string
	token   string
}

// Start builds depositwatcher/cmd/migrate and depositwatcher/cmd/watcherd
// from the sibling depositwatcher module (../../../depositwatcher, the
// usdt-settlement-corridor layout this whole project uses), runs
// migrations against dbURL, and starts watcherd listening on listenAddr
// with the given xpub -- HTTP-boundary-only, no live chain-watching
// engine.
func Start(t *testing.T, dbURL, listenAddr, xpub, token, actor string) *Watcher {
	t.Helper()
	if dbURL == "" {
		t.Skip("no test database URL set; skipping integration test")
	}

	watcherRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "depositwatcher"))
	if err != nil {
		t.Fatalf("resolving depositwatcher module path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(watcherRoot, "go.mod")); err != nil {
		t.Skipf("no sibling depositwatcher module found at %s; skipping (expects the usdt-settlement-corridor layout)", watcherRoot)
	}

	tmpDir := t.TempDir()
	migrateBin := filepath.Join(tmpDir, "migrate_bin")
	watcherdBin := filepath.Join(tmpDir, "watcherd_bin")

	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = watcherRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, output)
		}
	}
	build(migrateBin, "./cmd/migrate")
	build(watcherdBin, "./cmd/watcherd")

	migrate := exec.Command(migrateBin, "up")
	migrate.Dir = watcherRoot
	migrate.Env = append(os.Environ(), "WATCHER_DATABASE_URL="+dbURL)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("running depositwatcher migrations: %v\n%s", err, output)
	}

	watcherd := exec.Command(watcherdBin)
	watcherd.Dir = watcherRoot
	watcherd.Env = append(os.Environ(),
		"WATCHER_DATABASE_URL="+dbURL,
		"WATCHER_XPUB="+xpub,
		"WATCHER_API_TOKENS="+token+":"+actor,
		"WATCHER_LISTEN_ADDR="+listenAddr,
		// WATCHER_RPC_PROVIDERS deliberately unset -- HTTP boundary only.
	)
	var logs bytes.Buffer
	watcherd.Stdout = &logs
	watcherd.Stderr = &logs
	if err := watcherd.Start(); err != nil {
		t.Fatalf("starting watcherd: %v", err)
	}
	t.Cleanup(func() {
		_ = watcherd.Process.Kill()
		_ = watcherd.Wait()
		if t.Failed() {
			t.Logf("watcherd output:\n%s", logs.String())
		}
	})

	baseURL := "http://localhost" + listenAddr
	w := &Watcher{t: t, baseURL: baseURL, token: token}
	w.waitHealthy()
	return w
}

// BaseURL is this watcherd instance's own base URL.
func (w *Watcher) BaseURL() string { return w.baseURL }

// Token is the bearer token this watcherd instance was started with.
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
	w.t.Fatalf("watcherd never became healthy at %s within the deadline", w.baseURL)
}
