package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// fakeNode is a minimal JSON-RPC 2.0 server standing in for one BSC node.
// Tests dial it with the REAL ethclient.DialContext over REAL HTTP, so
// what's under test is this package's agreement/comparison logic against
// genuine wire-level RPC round trips -- not a hand-rolled interface mock
// that would bypass go-ethereum's own request/response handling entirely
// and risk passing even if this package misused that library's API.
type fakeNode struct {
	mu sync.Mutex

	finalizedHeight uint64
	finalizedHash   common.Hash
	finalizedErr    string        // non-empty -> eth_getBlockByNumber("finalized") returns this as an RPC error
	finalizedDelay  time.Duration // simulate a slow/timing-out provider on eth_getBlockByNumber

	blocks map[uint64]types.Header // height -> full header, for "latest" / a specific numeric height
	tip    uint64                  // what "latest" resolves to

	logs    []types.Log
	logsErr string
}

func newFakeNode() *fakeNode {
	return &fakeNode{blocks: make(map[uint64]types.Header)}
}

// client dials this fake node with the real ethclient and returns both
// the client and an httptest.Server the caller must Close.
func (n *fakeNode) client() (*ethclient.Client, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(n.handle))
	c, err := ethclient.DialContext(context.Background(), srv.URL)
	if err != nil {
		panic(fmt.Sprintf("dialing fake node: %v", err)) // fixture setup, not a test assertion
	}
	return c, srv
}

func (n *fakeNode) setFinalized(height uint64, hash common.Hash) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finalizedHeight, n.finalizedHash = height, hash
}

func (n *fakeNode) setFinalizedDelay(d time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finalizedDelay = d
}

func (n *fakeNode) setFinalizedError(msg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finalizedErr = msg
}

func (n *fakeNode) setLogs(logs []types.Log) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logs = logs
}

func (n *fakeNode) setLogsError(msg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logsErr = msg
}

// setBlock registers a full header at height, served for both a specific
// numeric eth_getBlockByNumber request and (if height == the current tip)
// "latest".
func (n *fakeNode) setBlock(height uint64, h types.Header) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blocks[height] = h
}

// setTip sets what "latest" resolves to.
func (n *fakeNode) setTip(height uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tip = height
}

// testHeader builds a minimal-but-valid types.Header for height, with
// parentHash as given -- every other required field gets a safe,
// arbitrary zero-ish value, since nothing in this package inspects them.
// Number (which does vary by height) is enough on its own to make
// different heights hash differently; nothing further is needed to avoid
// collisions.
func testHeader(height uint64, parentHash common.Hash) types.Header {
	return types.Header{
		ParentHash:  parentHash,
		UncleHash:   common.Hash{},
		Coinbase:    common.Address{},
		Root:        common.Hash{},
		TxHash:      common.Hash{},
		ReceiptHash: common.Hash{},
		Bloom:       types.Bloom{},
		Difficulty:  big.NewInt(0),
		Number:      new(big.Int).SetUint64(height),
		GasLimit:    30_000_000,
		GasUsed:     0,
		Time:        uint64(time.Now().Unix()),
		Extra:       []byte{},
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// firstParamString decodes a JSON-RPC params array's first element as a
// string -- every method this fake node serves takes a block-number-ish
// string ("finalized", "latest", "0x5") as its first argument.
func firstParamString(params json.RawMessage) (string, error) {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil {
		return "", err
	}
	if len(args) == 0 {
		return "", fmt.Errorf("expected at least one param")
	}
	var s string
	if err := json.Unmarshal(args[0], &s); err != nil {
		return "", err
	}
	return s, nil
}

func (n *fakeNode) handle(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	n.mu.Lock()
	delay := n.finalizedDelay
	n.mu.Unlock()
	if req.Method == "eth_getBlockByNumber" && delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return // client gave up; nothing to write back to
		}
	}

	switch req.Method {
	case "eth_getBlockByNumber":
		n.handleGetBlockByNumber(w, req)
	case "eth_getLogs":
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.logsErr != "" {
			writeRPCError(w, req.ID, n.logsErr)
			return
		}
		writeRPCResult(w, req.ID, n.logs)
	case "eth_chainId":
		writeRPCResult(w, req.ID, "0x38") // BSC mainnet (56) -- harmless default, unused by this package
	default:
		writeRPCError(w, req.ID, fmt.Sprintf("fakeNode: unsupported method %q", req.Method))
	}
}

// handleGetBlockByNumber branches on the requested tag: "finalized" uses
// the minimal shape C2.2's raw CallContext-based lookup parses (only
// "number"/"hash" -- json.Unmarshal into that smaller struct silently
// ignores anything else, so this shape works for that caller regardless
// of what else is present); "latest" and a specific numeric height serve
// a full types.Header for C2.3's ethclient.HeaderByNumber-based calls,
// which unmarshal directly into *types.Header and need every required
// field.
func (n *fakeNode) handleGetBlockByNumber(w http.ResponseWriter, req rpcRequest) {
	tag, err := firstParamString(req.Params)
	if err != nil {
		writeRPCError(w, req.ID, err.Error())
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if tag == "finalized" {
		if n.finalizedErr != "" {
			writeRPCError(w, req.ID, n.finalizedErr)
			return
		}
		writeRPCResult(w, req.ID, map[string]any{
			"number": fmt.Sprintf("0x%x", n.finalizedHeight),
			"hash":   n.finalizedHash.Hex(),
		})
		return
	}

	height := n.tip
	if tag != "latest" {
		h, err := hexToUint64(tag)
		if err != nil {
			writeRPCError(w, req.ID, fmt.Sprintf("bad block number %q: %v", tag, err))
			return
		}
		height = h
	}

	header, ok := n.blocks[height]
	if !ok {
		writeRPCResult(w, req.ID, nil) // a real node returns JSON null for a block that doesn't exist yet
		return
	}
	// A real node's eth_getBlockByNumber response is a rich block object
	// with "hash" as its own top-level field (the header's own computed
	// hash) alongside every raw header field -- unlike types.Header's own
	// default JSON marshaling (used for RLP/hash computation, and so
	// deliberately omits the derived hash itself). Phase 2's own
	// BlockHashAt (single_provider.go) needs a real "hash" field to
	// parse, exactly like the "finalized" branch above already provides
	// one -- synthesized here via the same real header.Hash() the go-
	// ethereum client itself would compute, not a fixture-only shortcut.
	body, err := json.Marshal(header)
	if err != nil {
		panic(err) // fixture bug, not a test assertion
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		panic(err)
	}
	fields["hash"] = header.Hash().Hex()
	writeRPCResult(w, req.ID, fields)
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	body, err := json.Marshal(result)
	if err != nil {
		panic(err) // fixture bug, not a test assertion
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": json.RawMessage(body),
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32000, "message": message},
	})
}
