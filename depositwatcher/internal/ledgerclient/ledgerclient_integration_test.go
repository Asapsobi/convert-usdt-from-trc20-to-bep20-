//go:build integration

// Requires a real, reachable Postgres 16 instance (LEDGER_TEST_DATABASE_URL)
// AND a sibling checkout of the ledger module at ../../../ledger -- the
// same usdt-settlement-corridor layout this whole project already uses.
// This builds and runs the REAL ledgerd binary as a subprocess and drives
// it purely over HTTP, exactly as the C2.6 build spec demands ("against a
// real running C1 instance... confirming C1's actual response... not a
// mock of it") -- not go-ethereum's testcontainers-go dependency (this
// environment has no Docker), and not a call into ledger's own internal
// Go packages (blocked anyway: they're a different module's internal/
// tree). Run via `make test-integration` (build tag "integration").
package ledgerclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"

	"depositwatcher/internal/finality"
	"depositwatcher/internal/ledgerclient"
	"depositwatcher/internal/money"
)

const (
	ledgerListenAddr = ":18435"
	ledgerBaseURL    = "http://localhost:18435"
	ledgerAPIToken   = "c26-integration-test-token"
	ledgerActor      = "watcher" // must match the actor half of LEDGER_API_TOKENS below
)

// liveLedger is a real ledgerd process, built and started fresh for this
// test file, plus a raw SQL connection to the same database used ONLY
// for account-creation fixture setup -- there is no public HTTP endpoint
// for creating an account (C1's own chart of accounts is either seeded
// at boot or created via the internal accounts.Create Go function; C2
// never gets to call that), so this mirrors accounts.Create's own INSERT
// exactly, as test scaffolding, never as a claim that C2 itself can or
// should write to C1's database directly.
type liveLedger struct {
	t    *testing.T
	pool *pgxpool.Pool
}

func startLiveLedger(t *testing.T) *liveLedger {
	t.Helper()
	dbURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
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
	migrate.Dir = ledgerRoot // goose.Up uses a relative "migrations" path, resolved against the process's cwd
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
		"LEDGER_API_TOKENS="+ledgerAPIToken+":"+ledgerActor,
		"LEDGER_LISTEN_ADDR="+ledgerListenAddr,
	)
	// Surface ledgerd's own logs on test failure -- invaluable for
	// diagnosing a scenario-B halt or a rejected transition without
	// re-running under a debugger.
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

	ll := &liveLedger{t: t, pool: pool}
	ll.waitHealthy()
	return ll
}

func (ll *liveLedger) waitHealthy() {
	ll.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ledgerBaseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	ll.t.Fatalf("ledgerd never became healthy at %s within the deadline", ledgerBaseURL)
}

// createAccount mirrors accounts.Create's own INSERT exactly (see this
// file's own top-of-file comment for why raw SQL, not the Go function,
// is used here).
func (ll *liveLedger) createAccount(code, accountType, asset string, normalSide int) {
	ll.t.Helper()
	_, err := ll.pool.Exec(context.Background(), `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	if err != nil {
		ll.t.Fatalf("creating fixture account %q: %v", code, err)
	}
}

type orderResp struct {
	ID            int64   `json:"id"`
	ExternalID    string  `json:"external_id"`
	State         string  `json:"state"`
	Version       int32   `json:"version"`
	SenderAddress *string `json:"sender_address"`
}

type haltResp struct {
	Halted bool   `json:"halted"`
	Reason string `json:"reason"`
}

func (ll *liveLedger) do(method, path string, idempotencyKey string, body any) (*http.Response, []byte) {
	ll.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			ll.t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ledgerBaseURL+path, reader)
	if err != nil {
		ll.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ledgerAPIToken)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ll.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		ll.t.Fatalf("%s %s: reading response body: %v", method, path, err)
	}
	return resp, respBody
}

func (ll *liveLedger) createOrder(externalID, customerID, amountIn, amountOut, fee, networkFee string) orderResp {
	ll.t.Helper()
	now := time.Now().UTC()
	resp, body := ll.do(http.MethodPost, "/v1/orders", "create:"+externalID, map[string]any{
		"external_id":       externalID,
		"customer_id":       customerID,
		"tier":              "STANDARD",
		"amount_in":         amountIn,
		"amount_out":        amountOut,
		"fee_units":         fee,
		"network_fee_units": networkFee,
		"recipient_address": "T-recipient-" + externalID,
		"quoted_at":         now,
		"quote_expires_at":  now.Add(10 * time.Minute),
	})
	if resp.StatusCode != http.StatusCreated {
		ll.t.Fatalf("POST /v1/orders for %s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o orderResp
	if err := json.Unmarshal(body, &o); err != nil {
		ll.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

func (ll *liveLedger) transition(externalID, idempotencyKey, toState string, expectedVersion int32, entry map[string]any) orderResp {
	ll.t.Helper()
	reqBody := map[string]any{
		"to_state":         toState,
		"expected_version": expectedVersion,
		"reason":           "c2.6 integration test: " + toState,
		"occurred_at":      time.Now().UTC(),
	}
	if entry != nil {
		reqBody["entry"] = entry
	}
	resp, body := ll.do(http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", externalID), idempotencyKey, reqBody)
	if resp.StatusCode != http.StatusOK {
		ll.t.Fatalf("POST /v1/orders/%s/transitions to %s: status %d: %s", externalID, toState, resp.StatusCode, body)
	}
	var o orderResp
	if err := json.Unmarshal(body, &o); err != nil {
		ll.t.Fatalf("decoding transition response: %v: %s", err, body)
	}
	return o
}

func (ll *liveLedger) getOrder(externalID string) orderResp {
	ll.t.Helper()
	resp, body := ll.do(http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if resp.StatusCode != http.StatusOK {
		ll.t.Fatalf("GET /v1/orders/%s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o orderResp
	if err := json.Unmarshal(body, &o); err != nil {
		ll.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

func (ll *liveLedger) getHalt() haltResp {
	ll.t.Helper()
	resp, body := ll.do(http.MethodGet, "/v1/system/halt", "", nil)
	if resp.StatusCode != http.StatusOK {
		ll.t.Fatalf("GET /v1/system/halt: status %d: %s", resp.StatusCode, body)
	}
	var h haltResp
	if err := json.Unmarshal(body, &h); err != nil {
		ll.t.Fatalf("decoding halt response: %v: %s", err, body)
	}
	return h
}

func (ll *liveLedger) clearHalt(t *testing.T) {
	t.Helper()
	resp, body := ll.do(http.MethodPost, "/v1/system/halt", "clear:"+t.Name(), map[string]any{
		"action": "clear", "note": "c2.6/c2.7 integration test cleanup",
	})
	if resp.StatusCode != http.StatusOK {
		t.Logf("clearing halt after %s: status %d: %s (may already be clear)", t.Name(), resp.StatusCode, body)
	}
}

func (ll *liveLedger) setHalt(t *testing.T, reason string) {
	t.Helper()
	resp, body := ll.do(http.MethodPost, "/v1/system/halt", "set:"+t.Name(), map[string]any{
		"action": "set", "reason": reason, "note": "c2.7 integration test: forcing a halt",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setting halt for %s: status %d: %s", t.Name(), resp.StatusCode, body)
	}
}

func (ll *liveLedger) accountBalance(code string) string {
	ll.t.Helper()
	resp, body := ll.do(http.MethodGet, "/v1/accounts/"+code+"/balance", "", nil)
	if resp.StatusCode != http.StatusOK {
		ll.t.Fatalf("GET /v1/accounts/%s/balance: status %d: %s", code, resp.StatusCode, body)
	}
	var out struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		ll.t.Fatalf("decoding balance response: %v: %s", err, body)
	}
	return out.Balance
}

// ---------------------------------------------------------------------

const (
	testAmountIn   = "3000.000000" // baseAmounts() in ledger's own replay harness -- amountIn == amountOut+fee+networkFee, exactly
	testAmountOut  = "2990.700000"
	testFee        = "7.500000"
	testNetworkFee = "1.800000"
)

// parseMinorUnits parses a plain 6-decimal amount string ("2990.700000",
// "-7.500000") into an int64 count of minor units, without ever routing
// through a float -- same discipline the ledger's own money package
// uses, reimplemented minimally here since this is test-only code
// checking a balance DELTA (expense:loss:reorg is a shared, persistent
// account across every run against this test database, so its absolute
// balance is never itself the right thing to assert on).
func parseMinorUnits(t *testing.T, s string) int64 {
	t.Helper()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, fracPart, _ := strings.Cut(s, ".")
	for len(fracPart) < 6 {
		fracPart += "0"
	}
	v, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		t.Fatalf("parsing decimal amount %q: %v", s, err)
	}
	if neg {
		v = -v
	}
	return v
}

func TestReportReorg_ScenarioA_OrderReturnsToQuoted(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c26-scenario-a-" + fmt.Sprint(time.Now().UnixNano())

	order := ll.createOrder(externalID, "cust-a", testAmountIn, testAmountOut, testFee, testNetworkFee)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := fmt.Sprintf("liability:customer:%s:bep20", externalID)
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	txHash := "0x" + fmt.Sprintf("%064x", time.Now().UnixNano())
	originalEntryKey := finality.DepositFinalIdempotencyKey(common.HexToHash(txHash), 0)

	funded := ll.transition(externalID, originalEntryKey, "funded", order.Version, map[string]any{
		"entry_type":  "deposit_final",
		"occurred_at": time.Now().UTC(),
		"lines": []map[string]any{
			{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": testAmountIn},
			{"account_code": custBEP, "asset": "USDT_BEP20", "amount": "-" + testAmountIn},
		},
	})
	if funded.State != "funded" {
		t.Fatalf("order state after funding = %q, want funded", funded.State)
	}

	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	if err := client.ReportReorg(context.Background(), externalID, originalEntryKey); err != nil {
		t.Fatalf("ReportReorg (first call): %v", err)
	}

	after := ll.getOrder(externalID)
	if after.State != "quoted" {
		t.Fatalf("order state after scenario-A reorg = %q, want quoted", after.State)
	}

	// A retry (simulating a redelivered report) must succeed cleanly, not
	// error C2-side. This exercises orders.HandleDepositReorg's own
	// Quoted-state idempotency check (added to fix exactly this gap: an
	// order sitting in Quoted from scenario A's own prior success used to
	// fall through to ErrUnexpectedState instead of being recognized as
	// an already-handled reorg -- see reorgRetryFromQuoted in
	// ledger/internal/orders/reorg.go).
	if err := client.ReportReorg(context.Background(), externalID, originalEntryKey); err != nil {
		t.Fatalf("ReportReorg (retry, must be idempotent): %v", err)
	}
	afterRetry := ll.getOrder(externalID)
	if afterRetry.State != "quoted" || afterRetry.Version != after.Version {
		t.Fatalf("order after a retried scenario-A report = (state=%s, version=%d), want unchanged (state=quoted, version=%d)",
			afterRetry.State, afterRetry.Version, after.Version)
	}
}

func TestReportReorg_ScenarioB_LossBookedAndLedgerHalted(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c26-scenario-b-" + fmt.Sprint(time.Now().UnixNano())

	order := ll.createOrder(externalID, "cust-b", testAmountIn, testAmountOut, testFee, testNetworkFee)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := fmt.Sprintf("liability:customer:%s:bep20", externalID)
	custTRC := fmt.Sprintf("liability:customer:%s:trc20", externalID)
	slot := fmt.Sprintf("asset:tron:slot:%s", externalID)
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)
	ll.createAccount(custTRC, "LIABILITY", "USDT_TRC20", -1)
	ll.createAccount(slot, "ASSET", "USDT_TRC20", 1)

	txHash := "0x" + fmt.Sprintf("%064x", time.Now().UnixNano())
	originalEntryKey := finality.DepositFinalIdempotencyKey(common.HexToHash(txHash), 0)

	funded := ll.transition(externalID, originalEntryKey, "funded", order.Version, map[string]any{
		"entry_type":  "deposit_final",
		"occurred_at": time.Now().UTC(),
		"lines": []map[string]any{
			{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": testAmountIn},
			{"account_code": custBEP, "asset": "USDT_BEP20", "amount": "-" + testAmountIn},
		},
	})

	screened := ll.transition(externalID, "screen:"+externalID, "screened", funded.Version, nil)

	dispatching := ll.transition(externalID, "dispatch:"+externalID, "dispatching", screened.Version, map[string]any{
		"entry_type":  "conversion",
		"occurred_at": time.Now().UTC(),
		"lines": []map[string]any{
			{"account_code": custBEP, "asset": "USDT_BEP20", "amount": testAmountIn},
			{"account_code": "position:corridor:USDT_BEP20", "asset": "USDT_BEP20", "amount": "-" + testAmountIn},
			{"account_code": "position:corridor:USDT_TRC20", "asset": "USDT_TRC20", "amount": testAmountIn},
			{"account_code": custTRC, "asset": "USDT_TRC20", "amount": "-" + testAmountOut},
			{"account_code": "revenue:fee", "asset": "USDT_TRC20", "amount": "-" + testFee},
			{"account_code": "revenue:network_fee", "asset": "USDT_TRC20", "amount": "-" + testNetworkFee},
		},
	})

	settled := ll.transition(externalID, "settle:"+externalID, "settled", dispatching.Version, map[string]any{
		"entry_type":  "payout_settled",
		"occurred_at": time.Now().UTC(),
		"lines": []map[string]any{
			{"account_code": custTRC, "asset": "USDT_TRC20", "amount": testAmountOut},
			{"account_code": slot, "asset": "USDT_TRC20", "amount": "-" + testAmountOut},
		},
	})
	if settled.State != "settled" {
		t.Fatalf("order state after settling = %q, want settled", settled.State)
	}

	// expense:loss:reorg is a shared, pre-seeded account that persists
	// across every run against this test database -- its absolute
	// balance is never the right thing to assert on, only the delta this
	// one reorg report adds to it.
	lossBefore := parseMinorUnits(t, ll.accountBalance("expense:loss:reorg"))

	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	if err := client.ReportReorg(context.Background(), externalID, originalEntryKey); err != nil {
		t.Fatalf("ReportReorg (scenario B): %v", err)
	}
	t.Cleanup(func() { ll.clearHalt(t) })

	after := ll.getOrder(externalID)
	if after.State != "settled" {
		t.Fatalf("order state after scenario-B reorg = %q, want unchanged (settled) -- the payout already left", after.State)
	}

	halt := ll.getHalt()
	if !halt.Halted {
		t.Fatal("expected the ledger to be halted after a scenario-B reorg, got halted=false")
	}

	lossAfter := parseMinorUnits(t, ll.accountBalance("expense:loss:reorg"))
	wantDelta := parseMinorUnits(t, testAmountOut)
	if gotDelta := lossAfter - lossBefore; gotDelta != wantDelta {
		t.Fatalf("expense:loss:reorg grew by %d minor units, want exactly the reported amount_out (%d)", gotDelta, wantDelta)
	}

	// A retry must still hit already_reversed, not attempt a second loss.
	if err := client.ReportReorg(context.Background(), externalID, originalEntryKey); err != nil {
		t.Fatalf("ReportReorg (retry, must be idempotent): %v", err)
	}
	lossAfterRetry := parseMinorUnits(t, ll.accountBalance("expense:loss:reorg"))
	if lossAfterRetry != lossAfter {
		t.Fatalf("expense:loss:reorg balance changed after a retried report: %d -> %d minor units, want unchanged", lossAfter, lossAfterRetry)
	}
}

// ---------------------------------------------------------------------
// C2.7 -- ReportDepositFinal
// ---------------------------------------------------------------------

// newTxHash builds a fresh, distinct tx hash for each test -- these
// candidates don't come from a real chain, so all that matters is
// uniqueness (so idempotency keys never collide across test runs against
// this persistent database).
func newTxHash() common.Hash {
	return common.HexToHash(fmt.Sprintf("0x%x", time.Now().UnixNano()))
}

// newDepositCandidate builds a finality.Candidate as C2.5's own Tracker
// would hand it to ReportDepositFinal. Classification is left at its
// zero value (Exact) -- ReportDepositFinal never inspects it.
func newDepositCandidate(order orderResp, customerID string, amount money.Amount) finality.Candidate {
	return finality.Candidate{
		ObservedLog: finality.ObservedLog{
			TxHash:     newTxHash(),
			LogIndex:   0,
			Height:     1,
			BlockTime:  time.Now().UTC(),
			OrderID:    order.ID,
			ExternalID: order.ExternalID,
			CustomerID: customerID,
			Amount:     amount,
		},
	}
}

func TestReportDepositFinal_HappyPath(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c27-happy-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c27-happy-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.createOrder(externalID, customerID, testAmountIn, testAmountOut, testFee, testNetworkFee)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	// :USDT_BEP20 suffix required -- matches reportDepositFinal's own
	// real customerAccount construction exactly (ledgerclient.go). Was
	// missing here (a pre-existing gap, predating this session's own
	// change -- harmless until now since this is the only test in this
	// file that actually asserts on this balance).
	custBEP := "liability:customer:" + customerID + ":USDT_BEP20"
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	amount := money.Amount(3000_000000) // 3000.000000 -- matches testAmountIn
	candidate := newDepositCandidate(order, customerID, amount)

	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	if err := client.ReportDepositFinal(context.Background(), candidate); err != nil {
		t.Fatalf("ReportDepositFinal: %v", err)
	}

	after := ll.getOrder(externalID)
	if after.State != "funded" {
		t.Fatalf("order state after ReportDepositFinal = %q, want funded", after.State)
	}

	if got := ll.accountBalance(depositAcc); got != testAmountIn {
		t.Errorf("deposit account balance = %s, want %s", got, testAmountIn)
	}
	custBalance := ll.accountBalance(custBEP)
	if parseMinorUnits(t, custBalance) != -parseMinorUnits(t, testAmountIn) {
		t.Errorf("customer liability balance = %s, want the negation of %s", custBalance, testAmountIn)
	}
}

// TestReportDepositFinal_SenderAddressRoundTrips closes the loop this
// session opened: chain.ParseTransferLog observes a Transfer's sender,
// candidates.processLog now carries it onto finality.ObservedLog, and
// this method sends it to C1 as the sender_address field ledger's own
// build added (docs/03-build/c3-screening-build-prompts.md's "Read this
// first"). Against a REAL ledgerd, not a fake -- the only way to prove
// C1 actually persists and returns it, not just that this client sends
// something with the right key name.
func TestReportDepositFinal_SenderAddressRoundTrips(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c27-sender-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c27-sender-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.createOrder(externalID, customerID, testAmountIn, testAmountOut, testFee, testNetworkFee)
	if order.SenderAddress != nil {
		t.Fatalf("a freshly quoted order must have a null sender_address, got %v", *order.SenderAddress)
	}

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := "liability:customer:" + customerID
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	const sender = "0xAE2166bd7901Ea67c1E2Bc4179418fC228108F0"
	amount := money.Amount(3000_000000)
	candidate := newDepositCandidate(order, customerID, amount)
	candidate.SenderAddress = sender

	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	if err := client.ReportDepositFinal(context.Background(), candidate); err != nil {
		t.Fatalf("ReportDepositFinal: %v", err)
	}

	after := ll.getOrder(externalID)
	if after.SenderAddress == nil {
		t.Fatal("sender_address is null after ReportDepositFinal, want the reported address")
	}
	if *after.SenderAddress != sender {
		t.Fatalf("sender_address = %q, want %q", *after.SenderAddress, sender)
	}
}

func TestReportDepositFinal_IllegalTransition_OrderAlreadyExpired(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c27-expired-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c27-expired-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.createOrder(externalID, customerID, testAmountIn, testAmountOut, testFee, testNetworkFee)

	expired := ll.transition(externalID, "expire:"+externalID, "expired", order.Version, nil)
	if expired.State != "expired" {
		t.Fatalf("setup: order state = %q, want expired", expired.State)
	}

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := "liability:customer:" + customerID
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	candidate := newDepositCandidate(order, customerID, money.Amount(3000_000000))
	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	err := client.ReportDepositFinal(context.Background(), candidate)
	if err == nil {
		t.Fatal("ReportDepositFinal: expected an error for an already-expired order, got nil")
	}
	if !errors.Is(err, finality.ErrPermanentFailure) {
		t.Fatalf("ReportDepositFinal: got %v, want an error wrapping finality.ErrPermanentFailure", err)
	}
	var apiErr *ledgerclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "illegal_transition" {
		t.Fatalf("ReportDepositFinal: got %v, want an APIError with code illegal_transition", err)
	}

	// Confirm C1 itself is genuinely untouched -- no entry, no balance
	// change, still expired.
	stillExpired := ll.getOrder(externalID)
	if stillExpired.State != "expired" {
		t.Fatalf("order state after the rejected report = %q, want unchanged (expired)", stillExpired.State)
	}
	if got := ll.accountBalance(depositAcc); got != "0.000000" {
		t.Fatalf("deposit account balance = %s, want 0.000000 (nothing should have posted)", got)
	}
}

// TestReportDepositFinal_ReplayAfterRestart_DoesNotDoubleCredit exercises
// the exact scenario this chunk's own ACCEPTANCE section names: "a C2
// restart that re-processes a block it already handled." C2.5's own
// finality.Tracker keeps candidates entirely in process memory (a
// documented limitation, not an oversight -- see finality.go's own
// comment), so a real restart re-observing the same on-chain log calls
// ReportDepositFinal a second time for a candidate identical in every
// field, including its idempotency key. Exercises
// orders.replayIfAlreadyPosted (ledger/internal/orders/store.go), added
// to fix exactly this gap: Funded -> Funded used to fall straight
// through orders.Transition's transitionTable check to
// ErrIllegalTransition before ever reaching journal.Post's own
// idempotency-key handling.
func TestReportDepositFinal_ReplayAfterRestart_DoesNotDoubleCredit(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c27-replay-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c27-replay-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.createOrder(externalID, customerID, testAmountIn, testAmountOut, testFee, testNetworkFee)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := "liability:customer:" + customerID
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	candidate := newDepositCandidate(order, customerID, money.Amount(3000_000000))
	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)

	if err := client.ReportDepositFinal(context.Background(), candidate); err != nil {
		t.Fatalf("ReportDepositFinal (first call): %v", err)
	}
	firstBalance := ll.accountBalance(depositAcc)

	// The exact same candidate again -- same tx_hash:log_index, same
	// idempotency key, same amount. A real C2 restart re-observing this
	// block would produce exactly this call.
	if err := client.ReportDepositFinal(context.Background(), candidate); err != nil {
		t.Fatalf("ReportDepositFinal (replay): must succeed as a no-op, got: %v", err)
	}

	afterBalance := ll.accountBalance(depositAcc)
	if afterBalance != firstBalance {
		t.Fatalf("deposit account balance changed on replay: %s -> %s -- this is the exact double-credit this test exists to catch",
			firstBalance, afterBalance)
	}

	after := ll.getOrder(externalID)
	if after.State != "funded" {
		t.Fatalf("order state after replay = %q, want unchanged (funded)", after.State)
	}
}

// TestReportDepositFinal_QuotedToFundedIsNeverHaltBlocked checks a
// specific, load-bearing claim in this chunk's own build spec ("On 423
// system_halted: back off and retry") against orders.transitionTable's
// actual, already-shipped configuration: {Quoted, Funded} is NOT
// HaltBlocked (see reorg.go's own doc comment: "quoted->funded (deposit
// recording) must keep working while halted"). If that's still true,
// ReportDepositFinal's system_halted branch is unreachable for this
// specific transition -- not wrong to keep defensively, just never
// exercised by a real deposit_final call in practice.
func TestReportDepositFinal_QuotedToFundedIsNeverHaltBlocked(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c27-halted-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c27-halted-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.createOrder(externalID, customerID, testAmountIn, testAmountOut, testFee, testNetworkFee)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := "liability:customer:" + customerID
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	ll.setHalt(t, "MANUAL_TEST_HALT")
	t.Cleanup(func() { ll.clearHalt(t) })

	halt := ll.getHalt()
	if !halt.Halted {
		t.Fatal("setup: expected the ledger to be halted")
	}

	candidate := newDepositCandidate(order, customerID, money.Amount(3000_000000))
	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	err := client.ReportDepositFinal(context.Background(), candidate)
	if err != nil {
		t.Fatalf("ReportDepositFinal while halted: got an error (%v) -- if this now fails with system_halted, "+
			"HandleDepositReorg's transitionTable started HaltBlocking Quoted->Funded and this test (and its "+
			"own finding) is stale; ReportDepositFinal's system_halted handling would then actually be exercised", err)
	}
	after := ll.getOrder(externalID)
	if after.State != "funded" {
		t.Fatalf("order state while halted = %q, want funded (deposit recording is documented to work while halted)", after.State)
	}
}

// ---------------------------------------------------------------------
// RELAY-tier (Model F) deposit routing -- the real C2->relayd seam.
//
// These exercise the ACTUAL boundary between this service's real
// ReportDepositFinal and relayd's own real account-code expectations,
// against a real ledgerd -- not relayd/internal/replay's own in-process
// fixture, which posts directly into "asset:relay:leg:<id>" and so could
// never have caught this account name ever having drifted from what
// this client's own production code actually sends (see
// ledgerclient.go's own reportDepositFinal doc comment for the full
// story of how that drift happened and why it was invisible to the
// replay suite).

// createRelayOrder mirors createOrder exactly except tier -- a separate
// helper rather than adding a tier parameter to createOrder itself, so
// this fix's own diff stays minimal and every existing call site (every
// other test in this file) is untouched.
func (ll *liveLedger) createRelayOrder(externalID, customerID, amountIn, amountOut, fee, networkFee string) orderResp {
	ll.t.Helper()
	now := time.Now().UTC()
	resp, body := ll.do(http.MethodPost, "/v1/orders", "create:"+externalID, map[string]any{
		"external_id":       externalID,
		"customer_id":       customerID,
		"tier":              "RELAY",
		"amount_in":         amountIn,
		"amount_out":        amountOut,
		"amount_in_asset":   "USDT_BEP20", // required for tier=RELAY -- BEP20_TO_TRC20 direction, matching this file's own BEP20-deposit scenarios
		"amount_out_asset":  "USDT_TRC20",
		"fee_units":         fee,
		"network_fee_units": networkFee,
		"recipient_address": "T-recipient-" + externalID,
		"quoted_at":         now,
		"quote_expires_at":  now.Add(10 * time.Minute),
	})
	if resp.StatusCode != http.StatusCreated {
		ll.t.Fatalf("POST /v1/orders (tier=RELAY) for %s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o orderResp
	if err := json.Unmarshal(body, &o); err != nil {
		ll.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

// TestReportDepositFinal_RelayTier_PostsToRelayLegAccount is this fix's
// own core regression test: for a RELAY-tier order, the REAL
// ReportDepositFinal call (not a fixture standing in for it) must post
// into "asset:relay:leg:<order_id>" -- the exact account relayd's own
// orchestrate.go reads from -- never into Model D's
// "asset:bsc:deposit:<order_id>", which relayd never looks at.
func TestReportDepositFinal_RelayTier_PostsToRelayLegAccount(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c27-relay-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c27-relay-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.createRelayOrder(externalID, customerID, testAmountIn, testAmountOut, testFee, testNetworkFee)

	relayLegAcc := fmt.Sprintf("asset:relay:leg:%d", order.ID)
	wrongModelDAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	// :USDT_BEP20 suffix required -- matches reportDepositFinal's own
	// real customerAccount construction exactly (ledgerclient.go).
	custBEP := "liability:customer:" + customerID + ":USDT_BEP20"
	ll.createAccount(relayLegAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	amount := money.Amount(3000_000000) // matches testAmountIn
	candidate := newDepositCandidate(order, customerID, amount)

	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	if err := client.ReportDepositFinal(context.Background(), candidate); err != nil {
		t.Fatalf("ReportDepositFinal: %v", err)
	}

	after := ll.getOrder(externalID)
	if after.State != "funded" {
		t.Fatalf("order state after ReportDepositFinal = %q, want funded", after.State)
	}

	if got := ll.accountBalance(relayLegAcc); got != testAmountIn {
		t.Errorf("asset:relay:leg:%d balance = %s, want %s (relayd's own suspense account must hold the real deposit)",
			order.ID, got, testAmountIn)
	}
	custBalance := ll.accountBalance(custBEP)
	if parseMinorUnits(t, custBalance) != -parseMinorUnits(t, testAmountIn) {
		t.Errorf("customer liability balance = %s, want the negation of %s", custBalance, testAmountIn)
	}

	// The regression this test exists to catch: before this fix, the
	// deposit landed HERE instead, silently, while relayd's own
	// screened->dispatching entry tried (and failed to find funds in)
	// asset:relay:leg:<id>. Confirm the old account was never even
	// CREATED for a RELAY-tier order -- ll.accountBalance itself
	// t.Fatalf's on a non-200, so this checks the raw status directly:
	// a 404 (never created) is the correct, stronger proof that nothing
	// was ever posted there, not just that its balance nets to zero.
	resp, body := ll.do(http.MethodGet, "/v1/accounts/"+wrongModelDAcc+"/balance", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/accounts/%s/balance = status %d (%s), want 404 (Model D's own account must never be created for a RELAY order)",
			wrongModelDAcc, resp.StatusCode, body)
	}
}

// TestRelayTierDeposit_ScreenedToDispatching_ConsumesWithoutGoingNegative
// proves the OTHER half of the seam: once a real deposit has landed via
// ReportDepositFinal, relayd's own screened->dispatching entry (mirrored
// here exactly -- same account codes, same line shape as
// relayd/internal/orchestrate/orchestrate.go's own postForwardStartEntry
// and relayLegAccountCode/relayLegForwardingAccountCode; relayd's own Go
// code cannot be imported directly, separate module) must be able to
// fully consume that balance: asset:relay:leg:<id> nets to EXACTLY zero
// afterward, never negative, with the full amount having moved into the
// forwarding suspense account.
func TestRelayTierDeposit_ScreenedToDispatching_ConsumesWithoutGoingNegative(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c27-relay-consume-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c27-relay-consume-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.createRelayOrder(externalID, customerID, testAmountIn, testAmountOut, testFee, testNetworkFee)

	relayLegAcc := fmt.Sprintf("asset:relay:leg:%d", order.ID)
	forwardingAcc := fmt.Sprintf("asset:relay:leg:forwarding:%d", order.ID)
	custBEP := "liability:customer:" + customerID
	ll.createAccount(relayLegAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(forwardingAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	candidate := newDepositCandidate(order, customerID, money.Amount(3000_000000))
	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	if err := client.ReportDepositFinal(context.Background(), candidate); err != nil {
		t.Fatalf("ReportDepositFinal: %v", err)
	}

	// funded -> screened, C3's own real transition shape (screening/internal/ledgerclient.go's
	// own ReleaseHold: no entry, just the state change).
	funded := ll.getOrder(externalID)
	screened := ll.transition(externalID, "screen:"+externalID, "screened", funded.Version, nil)

	// screened -> dispatching, relayd's own real "relay_forward_start"
	// entry shape.
	ll.transition(externalID, "forward_start:"+externalID, "dispatching", screened.Version, map[string]any{
		"entry_type":  "relay_forward_start",
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
		"lines": []map[string]any{
			{"account_code": forwardingAcc, "asset": "USDT_BEP20", "amount": testAmountIn},
			{"account_code": relayLegAcc, "asset": "USDT_BEP20", "amount": "-" + testAmountIn},
		},
	})

	if got := ll.accountBalance(relayLegAcc); got != "0.000000" {
		t.Errorf("asset:relay:leg:%d balance after forwarding started = %s, want exactly 0.000000 (fully consumed, never negative)",
			order.ID, got)
	}
	if got := ll.accountBalance(forwardingAcc); got != testAmountIn {
		t.Errorf("asset:relay:leg:forwarding:%d balance = %s, want %s", order.ID, got, testAmountIn)
	}

	after := ll.getOrder(externalID)
	if after.State != "dispatching" {
		t.Fatalf("order state after relay_forward_start = %q, want dispatching", after.State)
	}
}
