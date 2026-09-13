// Package testledger is shared test infrastructure: it builds and runs
// a REAL ledgerd binary as a subprocess and drives it purely over HTTP,
// mirroring dispatcher/internal/testledger's own identical helper
// (duplicated, not shared: separate Go modules, no common internal
// package, same convention as every other service here). Adapted for
// RELAY-tier orders and the zero-float relay-leg suspense-account
// fixture flow instead of Model D's treasury-deposit flow.
//
// Not a _test.go file: Go doesn't let one package's _test.go helpers be
// imported by another package's tests, so this has to be an ordinary
// package, imported only from _test.go files in practice.
package testledger

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Ledger is a real ledgerd process, built and started fresh for one
// test, plus a raw SQL connection to the same database used only for
// account-creation fixture setup and direct assertions.
type Ledger struct {
	t       *testing.T
	baseURL string
	token   string
	pool    *pgxpool.Pool
}

// Start builds ledger/cmd/migrate and ledger/cmd/ledgerd from the
// sibling ledger module (../../../ledger, the
// usdt-settlement-corridor layout this whole project uses), runs
// migrations against dbURL, and starts ledgerd listening on listenAddr.
func Start(t *testing.T, dbURL, listenAddr, token, actor string) *Ledger {
	t.Helper()
	if dbURL == "" {
		t.Skip("no test database URL set; skipping integration test")
	}

	ledgerRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "ledger"))
	if err != nil {
		t.Fatalf("resolving ledger module path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ledgerRoot, "go.mod")); err != nil {
		t.Skipf("no sibling ledger module found at %s; skipping (expects the usdt-settlement-corridor layout)", ledgerRoot)
	}

	tmpDir := t.TempDir()
	migrateBin := filepath.Join(tmpDir, "migrate_bin")
	ledgerdBin := filepath.Join(tmpDir, "ledgerd_bin")

	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = ledgerRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, output)
		}
	}
	build(migrateBin, "./cmd/migrate")
	build(ledgerdBin, "./cmd/ledgerd")

	migrate := exec.Command(migrateBin, "up")
	migrate.Dir = ledgerRoot
	migrate.Env = append(os.Environ(), "LEDGER_DATABASE_URL="+dbURL)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("running ledger migrations: %v\n%s", err, output)
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connecting to ledger test database: %v", err)
	}
	t.Cleanup(pool.Close)

	ledgerd := exec.Command(ledgerdBin)
	ledgerd.Dir = ledgerRoot
	ledgerd.Env = append(os.Environ(),
		"LEDGER_DATABASE_URL="+dbURL,
		"LEDGER_API_TOKENS="+token+":"+actor,
		"LEDGER_LISTEN_ADDR="+listenAddr,
	)
	var logs bytes.Buffer
	ledgerd.Stdout = &logs
	ledgerd.Stderr = &logs
	if err := ledgerd.Start(); err != nil {
		t.Fatalf("starting ledgerd: %v", err)
	}
	t.Cleanup(func() {
		_ = ledgerd.Process.Kill()
		_ = ledgerd.Wait()
		if t.Failed() {
			t.Logf("ledgerd output:\n%s", logs.String())
		}
	})

	baseURL := "http://localhost" + listenAddr
	l := &Ledger{t: t, baseURL: baseURL, token: token, pool: pool}
	l.waitHealthy()
	return l
}

// BaseURL is this ledgerd instance's own base URL.
func (l *Ledger) BaseURL() string { return l.baseURL }

// Token is the bearer token this ledgerd instance was started with.
func (l *Ledger) Token() string { return l.token }

func (l *Ledger) waitHealthy() {
	l.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(l.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	l.t.Fatalf("ledgerd never became healthy at %s within the deadline", l.baseURL)
}

// Do sends one authenticated request to this ledgerd instance and
// returns its raw response and body.
func (l *Ledger) Do(method, path string, idempotencyKey string, body any) (*http.Response, []byte) {
	l.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			l.t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, l.baseURL+path, reader)
	if err != nil {
		l.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		l.t.Fatalf("%s %s: reading response body: %v", method, path, err)
	}
	return resp, respBody
}

// OrderResp is the subset of C1's order resource this test helper
// decodes.
type OrderResp struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
	CustomerID string `json:"customer_id"`
	State      string `json:"state"`
	Version    int32  `json:"version"`
}

// CreateRelayOrder posts POST /v1/orders with tier=RELAY,
// TRC20_TO_BEP20's own asset pairing, and a fixed, valid set of amounts
// (100 TRC20 in, 99.7 BEP20 out, 0.3 TRC20 fee -- both fee and
// amount_out share amount_out's own asset, per
// ledger/internal/orders/store.go's own validate()). recipientAddress
// is a real, valid, checksummed BEP20-shaped address is not required
// here (C1 does not validate its checksum the way txbuild does for a
// TRON address) but a plausible one is used anyway for realism.
func (l *Ledger) CreateRelayOrder(externalID, customerID string) OrderResp {
	l.t.Helper()
	now := time.Now().UTC()
	resp, body := l.Do(http.MethodPost, "/v1/orders", "create:"+externalID, map[string]any{
		"external_id":       externalID,
		"customer_id":       customerID,
		"tier":              "RELAY",
		"amount_in":         "100.000000",
		"amount_out":        "99.700000",
		"fee_units":         "0.300000",
		"network_fee_units": "0.000000",
		"recipient_address": "0x00000000000000000000000000000000000001",
		"quoted_at":         now,
		"quote_expires_at":  now.Add(10 * time.Minute),
		"amount_in_asset":   "USDT_TRC20",
		"amount_out_asset":  "USDT_BEP20",
	})
	if resp.StatusCode != http.StatusCreated {
		l.t.Fatalf("POST /v1/orders for %s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o OrderResp
	if err := json.Unmarshal(body, &o); err != nil {
		l.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

// CreateRelayOrderBEP20ToTRC20 is CreateRelayOrder's own mirror-direction
// sibling: BEP20_TO_TRC20's own asset pairing (100 BEP20 in, 99.7 TRC20
// out, 0.3 BEP20 fee -- fee shares amount_in's own asset, per
// ledger/internal/orders/store.go's own RELAY-tier validate() rule).
func (l *Ledger) CreateRelayOrderBEP20ToTRC20(externalID, customerID string) OrderResp {
	l.t.Helper()
	now := time.Now().UTC()
	resp, body := l.Do(http.MethodPost, "/v1/orders", "create:"+externalID, map[string]any{
		"external_id":       externalID,
		"customer_id":       customerID,
		"tier":              "RELAY",
		"amount_in":         "100.000000",
		"amount_out":        "99.700000",
		"fee_units":         "0.300000",
		"network_fee_units": "0.000000",
		"recipient_address": "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		"quoted_at":         now,
		"quote_expires_at":  now.Add(10 * time.Minute),
		"amount_in_asset":   "USDT_BEP20",
		"amount_out_asset":  "USDT_TRC20",
	})
	if resp.StatusCode != http.StatusCreated {
		l.t.Fatalf("POST /v1/orders for %s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o OrderResp
	if err := json.Unmarshal(body, &o); err != nil {
		l.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

// AdvanceToScreenedBEP20ToTRC20 is AdvanceToScreened's own mirror-direction
// sibling: the relay-leg suspense account and customer liability are
// opened in USDT_BEP20 (what the customer actually deposited via C2 in
// this direction), not USDT_TRC20.
func (l *Ledger) AdvanceToScreenedBEP20ToTRC20(order OrderResp) OrderResp {
	l.t.Helper()
	relayLegAccount := "asset:relay:leg:" + strconv.FormatInt(order.ID, 10)
	bepLiability := "liability:customer:" + order.CustomerID + ":USDT_BEP20"
	l.CreateAccount(relayLegAccount, "ASSET", "USDT_BEP20")
	l.CreateAccount(bepLiability, "LIABILITY", "USDT_BEP20")

	now := time.Now().UTC()
	resp, body := l.Do(http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "fund:"+order.ExternalID, map[string]any{
		"to_state":         "funded",
		"expected_version": order.Version,
		"reason":           "bep20_relay_deposit_final",
		"occurred_at":      now,
		"entry": map[string]any{
			"entry_type":  "deposit_final",
			"occurred_at": now,
			"lines": []map[string]any{
				{"account_code": relayLegAccount, "asset": "USDT_BEP20", "amount": "100.000000"},
				{"account_code": bepLiability, "asset": "USDT_BEP20", "amount": "-100.000000"},
			},
		},
	})
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("funding %s: status %d: %s", order.ExternalID, resp.StatusCode, body)
	}
	var funded OrderResp
	if err := json.Unmarshal(body, &funded); err != nil {
		l.t.Fatalf("decoding funded order response: %v: %s", err, body)
	}

	resp, body = l.Do(http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "screen:"+order.ExternalID, map[string]any{
		"to_state":         "screened",
		"expected_version": funded.Version,
		"reason":           "screening_pass",
		"occurred_at":      now,
	})
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("screening %s: status %d: %s", order.ExternalID, resp.StatusCode, body)
	}
	var screened OrderResp
	if err := json.Unmarshal(body, &screened); err != nil {
		l.t.Fatalf("decoding screened order response: %v: %s", err, body)
	}
	return screened
}

// AdvanceToScreened walks a freshly created RELAY order (in quoted)
// through funded and into screened -- mirroring what tronwatcher's own
// ledgerclient.ReportDepositFinal does over HTTP in a real deployment
// (crediting the relay-leg suspense account, asset:relay:leg:<order_id>,
// not a treasury account) and what C3's discovery loop does for the
// funded->screened step, both reproduced directly here since this
// helper has no real tronwatcher/screening process to call.
func (l *Ledger) AdvanceToScreened(order OrderResp) OrderResp {
	l.t.Helper()
	return l.advanceToScreened(order, "")
}

// AdvanceToScreenedWithSender is AdvanceToScreened's own sibling that
// also records senderAddress on the funded transition (C1 only accepts
// sender_address on a transition INTO funded -- see
// ledger/internal/orders/store.go's own validate()) -- needed by any
// test exercising R5's own refund path, which only ever returns funds to
// this recorded address.
func (l *Ledger) AdvanceToScreenedWithSender(order OrderResp, senderAddress string) OrderResp {
	l.t.Helper()
	return l.advanceToScreened(order, senderAddress)
}

func (l *Ledger) advanceToScreened(order OrderResp, senderAddress string) OrderResp {
	l.t.Helper()
	relayLegAccount := "asset:relay:leg:" + strconv.FormatInt(order.ID, 10)
	trcLiability := "liability:customer:" + order.CustomerID + ":USDT_TRC20"
	l.CreateAccount(relayLegAccount, "ASSET", "USDT_TRC20")
	l.CreateAccount(trcLiability, "LIABILITY", "USDT_TRC20")

	now := time.Now().UTC()
	fundBody := map[string]any{
		"to_state":         "funded",
		"expected_version": order.Version,
		"reason":           "trc20_relay_deposit_final",
		"occurred_at":      now,
		"entry": map[string]any{
			"entry_type":  "deposit_final",
			"occurred_at": now,
			"lines": []map[string]any{
				{"account_code": relayLegAccount, "asset": "USDT_TRC20", "amount": "100.000000"},
				{"account_code": trcLiability, "asset": "USDT_TRC20", "amount": "-100.000000"},
			},
		},
	}
	if senderAddress != "" {
		fundBody["sender_address"] = senderAddress
	}
	resp, body := l.Do(http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "fund:"+order.ExternalID, fundBody)
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("funding %s: status %d: %s", order.ExternalID, resp.StatusCode, body)
	}
	var funded OrderResp
	if err := json.Unmarshal(body, &funded); err != nil {
		l.t.Fatalf("decoding funded order response: %v: %s", err, body)
	}

	resp, body = l.Do(http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "screen:"+order.ExternalID, map[string]any{
		"to_state":         "screened",
		"expected_version": funded.Version,
		"reason":           "screening_pass",
		"occurred_at":      now,
	})
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("screening %s: status %d: %s", order.ExternalID, resp.StatusCode, body)
	}
	var screened OrderResp
	if err := json.Unmarshal(body, &screened); err != nil {
		l.t.Fatalf("decoding screened order response: %v: %s", err, body)
	}
	return screened
}

// CreateAccount mirrors accounts.Create's own INSERT exactly.
func (l *Ledger) CreateAccount(code, accountType, asset string) {
	l.t.Helper()
	normalSide := 1
	switch accountType {
	case "LIABILITY", "REVENUE", "EQUITY":
		normalSide = -1
	case "POSITION":
		normalSide = 0
	}
	_, err := l.pool.Exec(context.Background(), `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	if err != nil {
		l.t.Fatalf("creating fixture account %q: %v", code, err)
	}
}

// AccountBalance sums journal_lines.amount_units for the account named
// code.
func (l *Ledger) AccountBalance(code string) int64 {
	l.t.Helper()
	var balance int64
	err := l.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(jl.amount_units), 0)
		FROM journal_lines jl
		JOIN accounts a ON a.id = jl.account_id
		WHERE a.code = $1
	`, code).Scan(&balance)
	if err != nil {
		l.t.Fatalf("summing balance for account %q: %v", code, err)
	}
	return balance
}

// GetOrder fetches an order's current state directly over HTTP.
func (l *Ledger) GetOrder(externalID string) OrderResp {
	l.t.Helper()
	resp, body := l.Do(http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("GET /v1/orders/%s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o OrderResp
	if err := json.Unmarshal(body, &o); err != nil {
		l.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}
