//go:build integration

// The real-seam proof for Design B (Phase 2): provider queries ->
// ProviderObservation -> the async decision engine -> the EXISTING
// finalize/drop paths -> a REAL ledgerclient.Client.ReportDepositFinal
// call -> a REAL, running ledgerd -> real Postgres. Nothing here is a
// mock standing in for the final seam: this file proves the whole chain
// the way TestReportDepositFinal_HappyPath (ledgerclient's own
// integration suite) already proves Design A's identical final leg,
// except starting from two real, independent chain.Pool providers
// (over real HTTP, dialed with the real ethclient) instead of
// hand-constructed Go values. Closing exactly the gap this session
// already found once for real: the C2->relayd account-naming mismatch
// was only caught once a test exercised the real client instead of a
// fixture standing in for it.
//
// Requires a real, reachable Postgres 16 instance (LEDGER_TEST_DATABASE_URL)
// AND a sibling checkout of the ledger module at ../../../ledger -- same
// requirement and layout as internal/ledgerclient's own integration
// suite. Run via `go test -tags integration ./internal/finality/...`.
package finality_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5/pgxpool"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/ledgerclient"
	"depositwatcher/internal/money"
)

// ---------------------------------------------------------------------
// A real, running ledgerd -- this file's own copy of the startLiveLedger
// pattern (internal/ledgerclient/ledgerclient_integration_test.go's own
// version is unexported and package-private to a different package;
// every integration suite in this codebase already builds its own such
// helper independently, consistent with that established convention,
// not a new one).
// ---------------------------------------------------------------------

const (
	asyncLedgerListenAddr = ":18436"
	asyncLedgerBaseURL    = "http://localhost:18436"
	asyncLedgerAPIToken   = "c2-phase2-integration-test-token"
	asyncLedgerActor      = "watcher"
)

type liveLedgerAsync struct {
	t    *testing.T
	pool *pgxpool.Pool
}

func startLiveLedgerAsync(t *testing.T) *liveLedgerAsync {
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
		"LEDGER_API_TOKENS="+asyncLedgerAPIToken+":"+asyncLedgerActor,
		"LEDGER_LISTEN_ADDR="+asyncLedgerListenAddr,
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

	ll := &liveLedgerAsync{t: t, pool: pool}
	ll.waitHealthy()
	return ll
}

func (ll *liveLedgerAsync) waitHealthy() {
	ll.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(asyncLedgerBaseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	ll.t.Fatalf("ledgerd never became healthy at %s within the deadline", asyncLedgerBaseURL)
}

// createAccount mirrors accounts.Create's own INSERT exactly -- there is
// no public HTTP endpoint for account creation (see this file's own
// ledgerclient sibling for the same reasoning).
func (ll *liveLedgerAsync) createAccount(code, accountType, asset string, normalSide int) {
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

type asyncOrderResp struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
	State      string `json:"state"`
}

func (ll *liveLedgerAsync) do(method, path string, idempotencyKey string, body any) (*http.Response, []byte) {
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
	req, err := http.NewRequest(method, asyncLedgerBaseURL+path, reader)
	if err != nil {
		ll.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+asyncLedgerAPIToken)
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

func (ll *liveLedgerAsync) createOrder(externalID, customerID, amountIn, amountOut, fee, networkFee string) asyncOrderResp {
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
	var o asyncOrderResp
	if err := json.Unmarshal(body, &o); err != nil {
		ll.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

func (ll *liveLedgerAsync) getOrder(externalID string) asyncOrderResp {
	ll.t.Helper()
	resp, body := ll.do(http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if resp.StatusCode != http.StatusOK {
		ll.t.Fatalf("GET /v1/orders/%s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o asyncOrderResp
	if err := json.Unmarshal(body, &o); err != nil {
		ll.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

const (
	asyncTestAmountIn   = "3000.000000"
	asyncTestAmountOut  = "2990.700000"
	asyncTestFee        = "7.500000"
	asyncTestNetworkFee = "1.800000"
)

// ---------------------------------------------------------------------
// A real chain.Pool, dialed with the real ethclient over real HTTP
// against two independent fake nodes -- mirroring chain/pool_test.go's
// own fakeNode fixture (unexported, package-private to internal/chain,
// so this file needs its own copy, same established convention as
// every other integration test in this codebase). Only what BlockHashAt/
// FinalizedFrom/LogsAtFrom/LogsAt actually need: eth_getBlockByNumber
// (both the "finalized" tag and a specific numeric height, sharing the
// same {number, hash} shape) and eth_getLogs.
// ---------------------------------------------------------------------

type asyncFakeNode struct {
	mu sync.Mutex

	finalizedHeight uint64
	finalizedHash   common.Hash

	blocks map[uint64]common.Hash

	logs    []types.Log
	logsErr string
}

func newAsyncFakeNode() *asyncFakeNode {
	return &asyncFakeNode{blocks: make(map[uint64]common.Hash)}
}

func (n *asyncFakeNode) client() (*ethclient.Client, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(n.handle))
	c, err := ethclient.DialContext(context.Background(), srv.URL)
	if err != nil {
		panic(fmt.Sprintf("dialing async fake node: %v", err))
	}
	return c, srv
}

func (n *asyncFakeNode) setFinalized(height uint64, h common.Hash) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finalizedHeight, n.finalizedHash = height, h
}

func (n *asyncFakeNode) setBlock(height uint64, h common.Hash) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blocks[height] = h
}

func (n *asyncFakeNode) setLogs(logs []types.Log) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logs = logs
}

type asyncRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (n *asyncFakeNode) handle(w http.ResponseWriter, r *http.Request) {
	var req asyncRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	switch req.Method {
	case "eth_getBlockByNumber":
		var args []json.RawMessage
		_ = json.Unmarshal(req.Params, &args)
		var tag string
		_ = json.Unmarshal(args[0], &tag)

		if tag == "finalized" {
			asyncWriteRPCResult(w, req.ID, map[string]any{
				"number": fmt.Sprintf("0x%x", n.finalizedHeight),
				"hash":   n.finalizedHash.Hex(),
			})
			return
		}
		var height uint64
		fmt.Sscanf(tag, "0x%x", &height)
		h, ok := n.blocks[height]
		if !ok {
			asyncWriteRPCResult(w, req.ID, nil)
			return
		}
		asyncWriteRPCResult(w, req.ID, map[string]any{
			"number": fmt.Sprintf("0x%x", height),
			"hash":   h.Hex(),
		})
	case "eth_getLogs":
		if n.logsErr != "" {
			asyncWriteRPCError(w, req.ID, n.logsErr)
			return
		}
		asyncWriteRPCResult(w, req.ID, n.logs)
	case "eth_chainId":
		asyncWriteRPCResult(w, req.ID, "0x38")
	default:
		asyncWriteRPCError(w, req.ID, fmt.Sprintf("asyncFakeNode: unsupported method %q", req.Method))
	}
}

func asyncWriteRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	body, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(body)})
}

func asyncWriteRPCError(w http.ResponseWriter, id json.RawMessage, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32000, "message": message},
	})
}

// twoNodeAsyncPool wires two independent fake nodes into a real
// chain.Pool, dialed with the real ethclient.
func twoNodeAsyncPool(t *testing.T) (pool *chain.Pool, a, b *asyncFakeNode) {
	t.Helper()
	a, b = newAsyncFakeNode(), newAsyncFakeNode()
	clientA, srvA := a.client()
	clientB, srvB := b.client()
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)

	pool, err := chain.NewPool([]chain.Provider{
		{Name: "A", Client: clientA},
		{Name: "B", Client: clientB},
	}, chain.Config{MinAgreement: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool, a, b
}

// ---------------------------------------------------------------------
// The real-seam scenario.
// ---------------------------------------------------------------------

// TestCheckFinalityAsync_RealSeam_ProviderQueriesToRealLedgerSettlement
// drives the ENTIRE real chain: a candidate observed via OnLogObserved,
// two real, independently-queried chain.Pool providers (real HTTP, real
// ethclient) each independently re-deriving a block hash and re-querying
// logs at the candidate's own height across simulated ticks, quorum
// reached via the real CheckFinalityAsync orchestration, routed through
// the EXISTING t.finalize() into a REAL ledgerclient.Client whose
// ReportDepositFinal call lands on a REAL, running ledgerd -- and the
// order's real state in Postgres, read back over ledgerd's own real
// HTTP API, must flip to "funded". This is what actually proves
// "depositwatcher provider queries -> ProviderObservation -> async
// decision engine -> existing ledger/finalization behavior" as one real
// chain, not three separately-mocked pieces.
func TestCheckFinalityAsync_RealSeam_ProviderQueriesToRealLedgerSettlement(t *testing.T) {
	ll := startLiveLedgerAsync(t)

	externalID := "c2-phase2-realseam-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c2-phase2-realseam-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.createOrder(externalID, customerID, asyncTestAmountIn, asyncTestAmountOut, asyncTestFee, asyncTestNetworkFee)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := "liability:customer:" + customerID + ":USDT_BEP20"
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	client := ledgerclient.New(asyncLedgerBaseURL, asyncLedgerAPIToken)

	pool, a, b := twoNodeAsyncPool(t)

	tr, err := finality.New(finality.Config{
		ContractAddress:         testContract,
		TransferTopic:           testTopic,
		OnFinal:                 client.ReportDepositFinal,
		ReorgReporter:           &fakeReorgReporter{},
		OrphanedDepositRecorder: &fakeOrphanedDepositRecorder{},
		AsyncMinAgreement:       2,
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}

	const height = 1000
	txHash := bigHash(1)
	amount := money.Amount(3000_000000) // matches asyncTestAmountIn exactly

	obs := finality.ObservedLog{
		TxHash:     txHash,
		LogIndex:   0,
		Height:     height,
		BlockTime:  time.Now().UTC(),
		OrderID:    order.ID,
		ExternalID: order.ExternalID,
		CustomerID: customerID,
		Amount:     amount,
	}
	if err := tr.OnLogObserved(context.Background(), obs, chain.Exact); err != nil {
		t.Fatalf("OnLogObserved: %v", err)
	}

	blockHash := common.HexToHash("0xaa1")

	logAtHeight := transferLog(height, txHash, 0, amount)

	// Tick 1: only provider A has reached this height -- one confirmation,
	// below the 2-of-2 quorum this Tracker requires. No settlement yet.
	a.setFinalized(height, blockHash)
	a.setBlock(height, blockHash)
	a.setLogs([]types.Log{logAtHeight})

	if err := tr.CheckFinalityAsync(context.Background(), pool); err != nil {
		t.Fatalf("tick 1: CheckFinalityAsync: %v", err)
	}
	mid := ll.getOrder(externalID)
	if mid.State == "funded" {
		t.Fatalf("order settled after only 1 of 2 required real provider confirmations -- quorum must not be satisfied by a single provider")
	}

	// Tick 2: provider B independently reaches the same height and
	// reports the exact same block hash and log content -- real quorum,
	// via two real, separately-dialed HTTP round trips.
	b.setFinalized(height, blockHash)
	b.setBlock(height, blockHash)
	b.setLogs([]types.Log{logAtHeight})

	if err := tr.CheckFinalityAsync(context.Background(), pool); err != nil {
		t.Fatalf("tick 2: CheckFinalityAsync: %v", err)
	}

	after := ll.getOrder(externalID)
	if after.State != "funded" {
		t.Fatalf("order state after real 2-of-2 async quorum = %q, want funded", after.State)
	}
	if tr.PendingCount() != 0 {
		t.Fatalf("PendingCount() = %d after real finalization, want 0", tr.PendingCount())
	}
}
