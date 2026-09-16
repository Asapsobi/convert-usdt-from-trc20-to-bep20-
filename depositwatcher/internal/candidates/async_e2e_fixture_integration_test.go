//go:build integration

// Fixture layer for the Design B (async finality) real-lifecycle E2E test
// (async_e2e_integration_test.go). Kept in its own file, same package
// (candidates_test), for the same reason every other integration suite in
// this codebase builds its own fixtures rather than importing another
// package's unexported test helpers -- internal/replay's own simNode/
// simChain are unexported and package-private, so this is a deliberate,
// consistent duplication, not an oversight.
//
// Requires a real, reachable Postgres 16 instance (WATCHER_TEST_DATABASE_URL,
// LEDGER_TEST_DATABASE_URL) AND a sibling checkout of the ledger module at
// ../../../ledger. Run via `go test -tags integration ./internal/candidates/...`.
package candidates_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/tyler-smith/go-bip32"

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/chain"
)

// ---------------------------------------------------------------------
// Deterministic, per-provider-controllable fake RPC node.
//
// Mirrors internal/replay's own simNode (same overall shape: a real
// JSON-RPC 2.0 HTTP server, dialed with the real ethclient, so what's
// under test is genuine wire-level chain.Pool behavior, not a hand-rolled
// interface mock) with ONE deliberate addition: the specific-height
// branch of eth_getBlockByNumber now also serves a top-level "hash"
// field (via the real, computed header.Hash()), matching what a real BSC
// node's own JSON response actually contains -- required for Phase 2's
// BlockHashAt (internal/chain/single_provider.go), which parses that
// field directly via a raw CallContext, not ethclient's own typed
// decode. Without this, every asyncSimNode would look correct for
// Design A (HeaderByNumber, LatestBlockHeader, LogsAt) but silently fail
// Design B's own independent per-provider hash re-verification.
// ---------------------------------------------------------------------

type asyncSimNode struct {
	mu   sync.Mutex
	name string

	dark bool // true: every RPC call fails -- a provider going fully offline

	tip          uint64
	blocks       map[uint64]types.Header
	logsByHeight map[uint64][]types.Log

	finalizedHeight uint64
	finalizedHash   common.Hash
}

func newAsyncSimNode(name string) *asyncSimNode {
	return &asyncSimNode{name: name, blocks: make(map[uint64]types.Header), logsByHeight: make(map[uint64][]types.Log)}
}

func (n *asyncSimNode) client() (*ethclient.Client, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(n.handle))
	c, err := ethclient.DialContext(context.Background(), srv.URL)
	if err != nil {
		panic(fmt.Sprintf("dialing async sim node %s: %v", n.name, err))
	}
	return c, srv
}

func (n *asyncSimNode) setDark(dark bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dark = dark
}

// commitBlock records height's header (parent hash chained from whatever
// this node currently has at height-1, exactly like internal/replay's
// own simNode.commitBlock) and its logs, advancing tip if needed.
// Calling this again for a height this node already has -- with a
// different `at` (changing the computed hash) and/or different logs --
// is how a scenario simulates a reorg or an independent disagreement,
// scoped to only the node(s) it's called on.
func (n *asyncSimNode) commitBlock(height uint64, at time.Time, logs []types.Log) common.Hash {
	n.mu.Lock()
	defer n.mu.Unlock()

	var parentHash common.Hash
	if parent, ok := n.blocks[height-1]; ok {
		parentHash = parent.Hash()
	}
	header := types.Header{
		ParentHash: parentHash,
		Number:     new(big.Int).SetUint64(height),
		Difficulty: big.NewInt(0),
		GasLimit:   30_000_000,
		Time:       uint64(at.Unix()),
		Extra:      []byte{},
	}
	n.blocks[height] = header
	if height > n.tip {
		n.tip = height
	}
	cp := append([]types.Log(nil), logs...)
	for i := range cp {
		cp[i].BlockNumber = height
	}
	n.logsByHeight[height] = cp
	return header.Hash()
}

// setFinalized advances this node's own finalized tag to height. Each
// node's finalized tag is fully independent -- what makes a 2-of-3
// quorum across different ticks, and a provider that never reaches a
// given height at all, both directly expressible.
func (n *asyncSimNode) setFinalized(height uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finalizedHeight = height
	if h, ok := n.blocks[height]; ok {
		n.finalizedHash = h.Hash()
	}
}

type asyncRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (n *asyncSimNode) handle(w http.ResponseWriter, r *http.Request) {
	var req asyncRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	n.mu.Lock()
	dark := n.dark
	n.mu.Unlock()
	if dark {
		asyncWriteRPCError(w, req.ID, fmt.Sprintf("asyncSimNode %s: offline", n.name))
		return
	}

	switch req.Method {
	case "eth_getBlockByNumber":
		n.handleGetBlockByNumber(w, req)
	case "eth_getLogs":
		n.handleGetLogs(w, req)
	case "eth_chainId":
		asyncWriteRPCResult(w, req.ID, "0x38")
	default:
		asyncWriteRPCError(w, req.ID, fmt.Sprintf("asyncSimNode %s: unsupported method %q", n.name, req.Method))
	}
}

func (n *asyncSimNode) handleGetBlockByNumber(w http.ResponseWriter, req asyncRPCRequest) {
	var args []json.RawMessage
	if err := json.Unmarshal(req.Params, &args); err != nil || len(args) == 0 {
		asyncWriteRPCError(w, req.ID, "malformed eth_getBlockByNumber params")
		return
	}
	var tag string
	if err := json.Unmarshal(args[0], &tag); err != nil {
		asyncWriteRPCError(w, req.ID, "malformed block tag")
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if tag == "finalized" {
		asyncWriteRPCResult(w, req.ID, map[string]any{
			"number": fmt.Sprintf("0x%x", n.finalizedHeight),
			"hash":   n.finalizedHash.Hex(),
		})
		return
	}

	height := n.tip
	if tag != "latest" {
		h, err := asyncHexToUint64(tag)
		if err != nil {
			asyncWriteRPCError(w, req.ID, fmt.Sprintf("bad block number %q: %v", tag, err))
			return
		}
		height = h
	}
	header, ok := n.blocks[height]
	if !ok {
		asyncWriteRPCResult(w, req.ID, nil) // a real node returns JSON null for a block it doesn't have
		return
	}

	// Real BSC nodes serve eth_getBlockByNumber as a rich block object
	// with "hash" as its own top-level field (the header's own computed
	// hash) alongside every raw header field -- unlike types.Header's
	// default JSON marshaling (used for RLP/hash computation, which
	// deliberately omits the derived hash itself). Phase 2's own
	// BlockHashAt needs a real "hash" field to parse; ethclient's typed
	// HeaderByNumber decode (used by HeaderByNumber/LatestBlockHeader,
	// Design A + ingestion) computes the hash client-side and never
	// looks for this field, so adding it changes nothing for those
	// callers.
	body, err := json.Marshal(header)
	if err != nil {
		panic(err) // fixture bug, not a test assertion
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		panic(err)
	}
	fields["hash"] = header.Hash().Hex()
	asyncWriteRPCResult(w, req.ID, fields)
}

// handleGetLogs performs real fromBlock/toBlock range filtering against
// this node's own logsByHeight -- both ScanRange's own detection-time
// joint query and Design B's own per-provider LogsAtFrom/checkPostFinalReorgs
// depend on genuine range behavior, not one fixed canned slice.
func (n *asyncSimNode) handleGetLogs(w http.ResponseWriter, req asyncRPCRequest) {
	var args []struct {
		FromBlock string `json:"fromBlock"`
		ToBlock   string `json:"toBlock"`
	}
	if err := json.Unmarshal(req.Params, &args); err != nil || len(args) == 0 {
		asyncWriteRPCError(w, req.ID, "malformed eth_getLogs params")
		return
	}
	from, err := asyncHexToUint64(args[0].FromBlock)
	if err != nil {
		asyncWriteRPCError(w, req.ID, "malformed fromBlock")
		return
	}
	to, err := asyncHexToUint64(args[0].ToBlock)
	if err != nil {
		asyncWriteRPCError(w, req.ID, "malformed toBlock")
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	var out []types.Log
	for h := from; h <= to; h++ {
		out = append(out, n.logsByHeight[h]...)
	}
	asyncWriteRPCResult(w, req.ID, out)
}

func asyncHexToUint64(s string) (uint64, error) {
	var v uint64
	_, err := fmt.Sscanf(s, "0x%x", &v)
	return v, err
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

// asyncSimChain wraps three asyncSimNodes ("A","B","C" -- matching this
// suite's own AsyncMinAgreement=2-of-3 quorum size, plus one spare so a
// dark-provider scenario still leaves enough for both detection's own
// joint agreement and Design B's own quorum to remain reachable) behind
// one real chain.Pool.
type asyncSimChain struct {
	nodes []*asyncSimNode
	srvs  []*httptest.Server
	pool  *chain.Pool
}

func newAsyncSimChain(t *testing.T) *asyncSimChain {
	t.Helper()
	names := []string{"A", "B", "C"}
	sc := &asyncSimChain{}
	var providers []chain.Provider
	for _, name := range names {
		node := newAsyncSimNode(name)
		client, srv := node.client()
		sc.nodes = append(sc.nodes, node)
		sc.srvs = append(sc.srvs, srv)
		providers = append(providers, chain.Provider{Name: name, Client: client})
	}
	pool, err := chain.NewPool(providers, chain.Config{MinAgreement: 2})
	if err != nil {
		for _, srv := range sc.srvs {
			srv.Close()
		}
		t.Fatalf("chain.NewPool: %v", err)
	}
	sc.pool = pool
	t.Cleanup(func() {
		for _, srv := range sc.srvs {
			srv.Close()
		}
	})
	return sc
}

// commitToAll commits height identically to every node -- the routine,
// no-disagreement case (also what candidate DETECTION always sees in
// every scenario below: divergence is only ever introduced afterward,
// for the finality-confirmation phase, mirroring the real threat model
// -- a deposit's initial detection is a routine, agreed event; whether
// it can be SAFELY FINALIZED is what Design B's own independent
// re-verification exists to decide).
func (sc *asyncSimChain) commitToAll(height uint64, at time.Time, logs []types.Log) {
	for _, n := range sc.nodes {
		n.commitBlock(height, at, logs)
	}
}

// nodeByName returns the node named "A", "B", or "C".
func (sc *asyncSimChain) nodeByName(name string) *asyncSimNode {
	for _, n := range sc.nodes {
		if n.name == name {
			return n
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// A real, running ledgerd -- this suite's own copy of the
// startLiveLedger/startReplayLedgerd pattern already used by every other
// integration suite in this module.
// ---------------------------------------------------------------------

const (
	asyncE2ELedgerListenAddr = ":18437"
	asyncE2ELedgerBaseURL    = "http://localhost:18437"
	asyncE2ELedgerToken      = "c2-phase2-e2e-test-token"
	asyncE2ELedgerActor      = "watcher"
)

func startAsyncE2ELedgerd(t *testing.T, ledgerDBURL string) *pgxpool.Pool {
	t.Helper()
	ledgerRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "ledger"))
	if err != nil {
		t.Fatalf("resolving ledger module path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ledgerRoot, "go.mod")); err != nil {
		t.Skipf("no sibling ledger module found at %s; skipping", ledgerRoot)
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
	migrate.Env = append(os.Environ(), "LEDGER_DATABASE_URL="+ledgerDBURL)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("running ledger migrations: %v\n%s", err, output)
	}

	pool, err := pgxpool.New(context.Background(), ledgerDBURL)
	if err != nil {
		t.Fatalf("connecting to ledger database: %v", err)
	}
	t.Cleanup(pool.Close)

	ledgerd := exec.Command(ledgerdBin)
	ledgerd.Dir = ledgerRoot
	ledgerd.Env = append(os.Environ(),
		"LEDGER_DATABASE_URL="+ledgerDBURL,
		"LEDGER_API_TOKENS="+asyncE2ELedgerToken+":"+asyncE2ELedgerActor,
		"LEDGER_LISTEN_ADDR="+asyncE2ELedgerListenAddr,
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

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(asyncE2ELedgerBaseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return pool
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("ledgerd never became healthy within the deadline")
	return nil
}

// freshIsolatedWatcherPoolForAsyncE2E mirrors internal/replay's own
// freshIsolatedWatcherPool exactly: a throwaway database on the same
// cluster as WATCHER_TEST_DATABASE_URL, migrated fresh, so this run's own
// ingestion/candidate-scan cursors and watched addresses can't collide
// with anything else running concurrently under `go test ./...`.
func freshIsolatedWatcherPoolForAsyncE2E(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseURL := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parsing WATCHER_TEST_DATABASE_URL: %v", err)
	}
	adminURL := *u
	adminURL.Path = "/postgres"
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connecting to admin database: %v", err)
	}
	dbName := fmt.Sprintf("watcher_async_e2e_%d", time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("creating isolated database: %v", err)
	}
	adminPool.Close()

	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), adminURL.String())
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})

	freshURL := *u
	freshURL.Path = "/" + dbName

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", freshURL.String())
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("running watcher migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, freshURL.String())
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	master, err := bip32.NewMasterKey([]byte("candidates async e2e fixture -- never use"))
	if err != nil {
		t.Fatalf("generating test master key: %v", err)
	}
	if err := addresses.Configure(master.PublicKey().B58Serialize()); err != nil {
		t.Fatalf("configuring addresses: %v", err)
	}
	return pool
}

// ---------------------------------------------------------------------
// A minimal real-ledgerd HTTP fixture -- only what this suite's own
// scenarios need (create an order, its accounts, and read its state
// back), mirroring internal/replay's own ledgerFixture in miniature.
// ---------------------------------------------------------------------

type asyncE2ELedgerFixture struct {
	baseURL string
	token   string
	http    *http.Client
}

func newAsyncE2ELedgerFixture() *asyncE2ELedgerFixture {
	return &asyncE2ELedgerFixture{baseURL: asyncE2ELedgerBaseURL, token: asyncE2ELedgerToken, http: &http.Client{Timeout: 15 * time.Second}}
}

func (f *asyncE2ELedgerFixture) do(ctx context.Context, method, path, idempotencyKey string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

type asyncE2EOrder struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
	State      string `json:"state"`
}

func (f *asyncE2ELedgerFixture) createOrder(ctx context.Context, externalID, customerID, amountIn string) (asyncE2EOrder, error) {
	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, "/v1/orders", "async-e2e:create:"+externalID, map[string]any{
		"external_id": externalID, "customer_id": customerID, "tier": "STANDARD",
		"amount_in": amountIn, "amount_out": "1.000000", "fee_units": "0.000000", "network_fee_units": "0.000000",
		"recipient_address": "T-recipient-" + externalID, "quoted_at": now, "quote_expires_at": now.Add(time.Hour),
	})
	if err != nil {
		return asyncE2EOrder{}, err
	}
	if status != http.StatusCreated {
		return asyncE2EOrder{}, fmt.Errorf("POST /v1/orders for %s: status %d: %s", externalID, status, body)
	}
	var o asyncE2EOrder
	if err := json.Unmarshal(body, &o); err != nil {
		return asyncE2EOrder{}, fmt.Errorf("decoding order response: %w: %s", err, body)
	}
	return o, nil
}

func (f *asyncE2ELedgerFixture) getOrder(ctx context.Context, externalID string) (asyncE2EOrder, error) {
	status, body, err := f.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if err != nil {
		return asyncE2EOrder{}, err
	}
	if status != http.StatusOK {
		return asyncE2EOrder{}, fmt.Errorf("GET /v1/orders/%s: status %d: %s", externalID, status, body)
	}
	var o asyncE2EOrder
	if err := json.Unmarshal(body, &o); err != nil {
		return asyncE2EOrder{}, fmt.Errorf("decoding order response: %w: %s", err, body)
	}
	return o, nil
}

// createAccount mirrors accounts.Create's own INSERT exactly -- no
// public HTTP endpoint exists for this (same reasoning as every other
// integration fixture in this module).
func (f *asyncE2ELedgerFixture) createAccount(ctx context.Context, pool *pgxpool.Pool, code, accountType, asset string, normalSide int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	return err
}

// pollUntil polls check every 10ms until it returns true or timeout
// elapses, returning whether it succeeded -- this suite's own
// deterministic-outcome (if not deterministic-timing) substitute for a
// fixed sleep: every assertion below waits for a real, observable effect
// of the real RunLoop/RunIngestionLoop background goroutines rather than
// guessing how long that should take.
func pollUntil(t *testing.T, timeout time.Duration, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdsFor asserts check() stays true (or, used negated, stays false)
// for the entire duration -- for "no early settlement" style assertions,
// where the thing under test is the ABSENCE of an effect across several
// real tick opportunities, not its eventual presence.
func holdsFor(d time.Duration, check func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !check() {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}
