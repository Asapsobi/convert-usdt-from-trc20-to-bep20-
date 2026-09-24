// Package tests1 is shared test infrastructure: it builds and runs a
// REAL S1 (s1d) binary as a subprocess and drives it purely over HTTP,
// mirroring relayd/internal/testledger's and
// relayd/internal/testdepositwatcher's own identical shape (duplicated,
// not shared: separate Go modules, no common internal package, same
// convention as every other service here).
//
// Started with S1_KMS_CLIENT=fake (the only KMS client this codebase
// has -- see s1/cmd/s1d/main.go's own doc comment: no real cloud KMS
// adapter exists yet) and a real S1_BSC_DEPOSIT_XPRV/XPUB pair, so this
// fixture's own signatures are real, recoverable ECDSA signatures from
// a real per-order derived key -- just not custodied behind a real
// HSM/KMS boundary. That's exactly the gap this whole fix is scoped
// around (see the plan this fixture was built for): proving the
// SIGNING CORRECTNESS (right key, right address) is independent of
// proving real KMS custody, which remains a separate, later gap.
//
// Not a _test.go file: Go doesn't let one package's _test.go helpers be
// imported by another package's tests, so this has to be an ordinary
// package, imported only from _test.go files in practice.
package tests1

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Signer is a real s1d process, built and started fresh for one test.
type Signer struct {
	t       *testing.T
	baseURL string
	token   string
}

// Config is what Start needs beyond the database URL/listen address --
// the BIP32 xprv/xpub pairs that tie this instance's own BSC/TRON
// deposit-key signing to whichever testdepositwatcher/testtronwatcher
// instance a test also started (same xpub on both sides), and the
// approval threshold (set high enough that a test's own small amounts
// always auto-sign, unless a test deliberately wants to exercise the
// pending-approval path). BSCDepositXPRV/XPUB and TronDepositXPRV/XPUB
// are each independently optional -- leave a pair empty to leave that
// chain's deposit-sweep signing disabled on this instance, matching
// s1d's own S1_BSC_DEPOSIT_XPRV/XPUB and S1_TRON_DEPOSIT_XPRV/XPUB
// both-unset-is-supported posture.
type Config struct {
	BSCDepositXPRV       string
	BSCDepositXPUB       string
	TronDepositXPRV      string
	TronDepositXPUB      string
	ApprovalThresholdUSD float64
}

// Start builds s1/cmd/migrate and s1/cmd/s1d from the sibling s1 module
// (../../../s1, the usdt-settlement-corridor layout this whole project
// uses), runs migrations against dbURL, and starts s1d listening on
// listenAddr.
func Start(t *testing.T, dbURL, listenAddr, c5Token, approverToken string, cfg Config) *Signer {
	t.Helper()
	if dbURL == "" {
		t.Skip("no test database URL set; skipping integration test")
	}

	s1Root, err := filepath.Abs(filepath.Join("..", "..", "..", "s1"))
	if err != nil {
		t.Fatalf("resolving s1 module path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s1Root, "go.mod")); err != nil {
		t.Skipf("no sibling s1 module found at %s; skipping (expects the usdt-settlement-corridor layout)", s1Root)
	}

	tmpDir := t.TempDir()
	migrateBin := filepath.Join(tmpDir, "migrate_bin")
	s1dBin := filepath.Join(tmpDir, "s1d_bin")

	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = s1Root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, output)
		}
	}
	build(migrateBin, "./cmd/migrate")
	build(s1dBin, "./cmd/s1d")

	migrate := exec.Command(migrateBin, "up")
	migrate.Dir = s1Root
	migrate.Env = append(os.Environ(), "S1_DATABASE_URL="+dbURL)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("running s1 migrations: %v\n%s", err, output)
	}

	threshold := cfg.ApprovalThresholdUSD
	if threshold <= 0 {
		threshold = 1_000_000 // generous default: auto-sign any realistic test amount
	}

	s1d := exec.Command(s1dBin)
	s1d.Dir = s1Root
	s1d.Env = append(os.Environ(),
		"S1_DATABASE_URL="+dbURL,
		"S1_C5_API_TOKENS="+c5Token+":relayd",
		"S1_APPROVER_API_TOKENS="+approverToken+":test-approver",
		// S1_PROVISIONING_API_TOKENS is required unconditionally (see
		// cmd/s1d's own doc comment) even though this fixture's own tests
		// never provision a TRON deposit key through it -- s1d fails to
		// start at all without it. A fixed test-only value: this fixture's
		// own tests currently have no caller that exercises the
		// provisioning route, so nothing else needs to know this value.
		"S1_PROVISIONING_API_TOKENS=test-provisioning-token:test-tronwatcher",
		"S1_KMS_CLIENT=fake",
		"S1_APPROVAL_THRESHOLD_USD="+strconv.FormatFloat(threshold, 'f', -1, 64),
		"S1_LISTEN_ADDR="+listenAddr,
		"S1_BSC_DEPOSIT_XPRV="+cfg.BSCDepositXPRV,
		"S1_BSC_DEPOSIT_XPUB="+cfg.BSCDepositXPUB,
		"S1_TRON_DEPOSIT_XPRV="+cfg.TronDepositXPRV,
		"S1_TRON_DEPOSIT_XPUB="+cfg.TronDepositXPUB,
	)
	var logs bytes.Buffer
	s1d.Stdout = &logs
	s1d.Stderr = &logs
	if err := s1d.Start(); err != nil {
		t.Fatalf("starting s1d: %v", err)
	}
	t.Cleanup(func() {
		_ = s1d.Process.Kill()
		_ = s1d.Wait()
		if t.Failed() {
			t.Logf("s1d output:\n%s", logs.String())
		}
	})

	baseURL := "http://localhost" + listenAddr
	s := &Signer{t: t, baseURL: baseURL, token: c5Token}
	s.waitHealthy()
	return s
}

// BaseURL is this s1d instance's own base URL.
func (s *Signer) BaseURL() string { return s.baseURL }

// Token is the C5-scoped bearer token this s1d instance was started
// with -- the one relayd's own signing.Client authenticates with.
func (s *Signer) Token() string { return s.token }

func (s *Signer) waitHealthy() {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(s.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("s1d never became healthy at %s within the deadline", s.baseURL)
}
