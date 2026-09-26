//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package candidates_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
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
	"depositwatcher/internal/candidates"
	"depositwatcher/internal/chain"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/money"
	"depositwatcher/internal/orphaned"
)

// fakeNode is a minimal JSON-RPC 2.0 server standing in for one BSC
// node, supporting exactly what ScanRange needs: eth_getLogs and
// eth_getBlockByNumber for a specific numeric height (block time).
type fakeNode struct {
	mu        sync.Mutex
	logs      []types.Log
	blocks    map[uint64]types.Header
	finalized uint64 // answer to eth_getBlockByNumber("finalized"); 0 means none yet
}

// setFinalized makes height (which must have a block time set) this
// node's finalized block.
func (n *fakeNode) setFinalized(height uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finalized = height
}

func newFakeNode() *fakeNode { return &fakeNode{blocks: make(map[uint64]types.Header)} }

func (n *fakeNode) client() (*ethclient.Client, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(n.handle))
	c, err := ethclient.DialContext(context.Background(), srv.URL)
	if err != nil {
		panic(fmt.Sprintf("dialing fake node: %v", err))
	}
	return c, srv
}

func (n *fakeNode) setLogs(logs []types.Log) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logs = logs
}

func (n *fakeNode) setBlockTime(height uint64, t time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blocks[height] = types.Header{
		Number: new(big.Int).SetUint64(height), Time: uint64(t.Unix()), Difficulty: big.NewInt(0),
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (n *fakeNode) handle(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	switch req.Method {
	case "eth_getLogs":
		writeRPCResult(w, req.ID, n.logs)
	case "eth_getBlockByNumber":
		var args []json.RawMessage
		_ = json.Unmarshal(req.Params, &args)
		var tag string
		_ = json.Unmarshal(args[0], &tag)
		if tag == "finalized" {
			header, ok := n.blocks[n.finalized]
			if !ok || n.finalized == 0 {
				writeRPCError(w, req.ID, "fakeNode: no finalized block set")
				return
			}
			writeRPCResult(w, req.ID, header)
			return
		}
		var height uint64
		if _, err := fmt.Sscanf(tag, "0x%x", &height); err != nil {
			writeRPCError(w, req.ID, err.Error())
			return
		}
		header, ok := n.blocks[height]
		if !ok {
			writeRPCResult(w, req.ID, nil)
			return
		}
		writeRPCResult(w, req.ID, header)
	case "eth_chainId":
		writeRPCResult(w, req.ID, "0x38")
	default:
		writeRPCError(w, req.ID, fmt.Sprintf("fakeNode: unsupported method %q", req.Method))
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	body, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(body)})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32000, "message": message},
	})
}

func twoNodePool(t *testing.T) (pool *chain.Pool, a, b *fakeNode) {
	t.Helper()
	a, b = newFakeNode(), newFakeNode()
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

var testContract = common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
var testTopic = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

// newTransferLog builds a well-formed Transfer log at height, to
// toTopic, carrying minorUnits (this service's 6-decimal convention),
// rescaled to the real contract's 18 on-chain decimals -- the inverse of
// chain.ParseTransferLog's own rescale.
func newTransferLog(height uint64, txHash common.Hash, toTopic common.Hash, index uint, minorUnits int64) types.Log {
	raw := new(big.Int).Mul(big.NewInt(minorUnits), big.NewInt(1_000_000_000_000))
	data := make([]byte, 32)
	raw.FillBytes(data)
	return types.Log{
		Address:     testContract,
		Topics:      []common.Hash{testTopic, common.Hash{}, toTopic},
		Data:        data,
		BlockNumber: height,
		TxHash:      txHash,
		Index:       index,
	}
}

func addressTopic(addr addresses.Address) common.Hash {
	return common.BytesToHash(common.HexToAddress(string(addr)).Bytes())
}

func newTxHash() common.Hash {
	return common.HexToHash(fmt.Sprintf("0x%x", time.Now().UnixNano()))
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	// The pool limit and cooldowns are exercised by their own tests; here
	// every order simply gets a free wallet, as with one wallet per order.
	if _, err := sqlDB.Exec(`UPDATE pool_settings SET max_wallets = 1000000, cooldown_after_use = '0', cooldown_after_expiry = '0'`); err != nil {
		t.Fatalf("relaxing pool settings for tests: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	master, err := bip32.NewMasterKey([]byte("candidates integration test fixture -- never use"))
	if err != nil {
		t.Fatalf("generating test master key: %v", err)
	}
	if err := addresses.Configure(master.PublicKey().B58Serialize()); err != nil {
		t.Fatalf("configuring addresses: %v", err)
	}
	return pool
}

var seq int64
var seqMu sync.Mutex

func uniqueOrderID() int64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return time.Now().UnixNano() + seq
}

func assignAddress(t *testing.T, pool *pgxpool.Pool, orderID int64, externalID, customerID string) addresses.Address {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	addr, err := addresses.Assign(context.Background(), pool, orderID, externalID, customerID, now, now.Add(90*time.Second))
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	return addr
}

type fakeQuotes struct{ amount money.Amount }

func (f fakeQuotes) QuotedAmount(context.Context, string) (money.Amount, error) { return f.amount, nil }

type noopReorgReporter struct{}

func (noopReorgReporter) ReportReorg(context.Context, string, string) error { return nil }

type noopOrphanedRecorder struct{}

func (noopOrphanedRecorder) RecordOrphanedDeposit(context.Context, finality.Candidate, error) error {
	return nil
}

func newTestTracker(t *testing.T) *finality.Tracker {
	t.Helper()
	tr, err := finality.New(finality.Config{
		ContractAddress:         testContract,
		TransferTopic:           testTopic,
		OnFinal:                 func(context.Context, finality.Candidate) error { return nil },
		ReorgReporter:           noopReorgReporter{},
		OrphanedDepositRecorder: noopOrphanedRecorder{},
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	return tr
}

func TestScanRange_WatchingAddress_TracksCandidate(t *testing.T) {
	pool := testPool(t)
	chainPool, a, b := twoNodePool(t)
	tracker := newTestTracker(t)

	orderID := uniqueOrderID()
	addr := assignAddress(t, pool, orderID, fmt.Sprintf("ext-%d", orderID), "cust-1")

	log := newTransferLog(100, newTxHash(), addressTopic(addr), 0, 3000_000000)
	a.setLogs([]types.Log{log})
	b.setLogs([]types.Log{log})
	a.setBlockTime(100, time.Now())
	b.setBlockTime(100, time.Now())

	err := candidates.ScanRange(context.Background(), chainPool, pool, fakeQuotes{amount: 3000_000000}, tracker,
		candidates.Config{ContractAddress: testContract, TransferTopic: testTopic, DustFloor: 1_000000}, 100, 100)
	if err != nil {
		t.Fatalf("ScanRange: %v", err)
	}
	if got := tracker.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() = %d, want 1", got)
	}
}

func TestScanRange_UnknownAddress_IgnoredEntirely(t *testing.T) {
	pool := testPool(t)
	chainPool, a, b := twoNodePool(t)
	tracker := newTestTracker(t)

	unknownTopic := common.BytesToHash(common.HexToAddress(fmt.Sprintf("0x%x", time.Now().UnixNano())).Bytes())
	log := newTransferLog(100, newTxHash(), unknownTopic, 0, 3000_000000)
	a.setLogs([]types.Log{log})
	b.setLogs([]types.Log{log})
	a.setBlockTime(100, time.Now())
	b.setBlockTime(100, time.Now())

	err := candidates.ScanRange(context.Background(), chainPool, pool, fakeQuotes{amount: 3000_000000}, tracker,
		candidates.Config{ContractAddress: testContract, TransferTopic: testTopic, DustFloor: 1_000000}, 100, 100)
	if err != nil {
		t.Fatalf("ScanRange: %v", err)
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0 -- unknown address must be ignored entirely", got)
	}
}

func TestScanRange_RetiredAddress_RecordsLateDeposit(t *testing.T) {
	pool := testPool(t)
	chainPool, a, b := twoNodePool(t)
	tracker := newTestTracker(t)

	orderID := uniqueOrderID()
	externalID := fmt.Sprintf("ext-%d", orderID)
	addr := assignAddress(t, pool, orderID, externalID, "cust-1")
	if err := addresses.Retire(context.Background(), pool, orderID, "settled"); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	txHash := newTxHash()
	log := newTransferLog(100, txHash, addressTopic(addr), 0, 3000_000000)
	a.setLogs([]types.Log{log})
	b.setLogs([]types.Log{log})
	a.setBlockTime(100, time.Now())
	b.setBlockTime(100, time.Now())

	err := candidates.ScanRange(context.Background(), chainPool, pool, fakeQuotes{amount: 3000_000000}, tracker,
		candidates.Config{ContractAddress: testContract, TransferTopic: testTopic, DustFloor: 1_000000}, 100, 100)
	if err != nil {
		t.Fatalf("ScanRange: %v", err)
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0 -- a late deposit is never a finality candidate", got)
	}

	deposits, err := orphaned.List(context.Background(), pool, nil)
	if err != nil {
		t.Fatalf("orphaned.List: %v", err)
	}
	found := false
	for _, d := range deposits {
		if d.TxHash == txHash.Hex() {
			found = true
			if d.OrderStateAtDetection != "address_retired" {
				t.Errorf("OrderStateAtDetection = %q, want address_retired", d.OrderStateAtDetection)
			}
		}
	}
	if !found {
		t.Fatal("orphaned.List did not include the late deposit")
	}
}

func TestScanRange_WrongTokenShape_FilteredNotErrored(t *testing.T) {
	pool := testPool(t)
	chainPool, a, b := twoNodePool(t)
	tracker := newTestTracker(t)

	log := types.Log{
		Address: testContract, Topics: []common.Hash{common.HexToHash("0xdeadbeef")},
		Data: make([]byte, 32), BlockNumber: 100, TxHash: newTxHash(), Index: 0,
	}
	a.setLogs([]types.Log{log})
	b.setLogs([]types.Log{log})

	err := candidates.ScanRange(context.Background(), chainPool, pool, fakeQuotes{amount: 3000_000000}, tracker,
		candidates.Config{ContractAddress: testContract, TransferTopic: testTopic, DustFloor: 1_000000}, 100, 100)
	if err != nil {
		t.Fatalf("ScanRange: unexpected error for a wrong-shaped log: %v", err)
	}
	if got := tracker.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0", got)
	}
}
