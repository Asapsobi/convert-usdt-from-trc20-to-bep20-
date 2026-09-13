// Package tronbroadcast broadcasts a signed TRC20 forward transfer to a
// real TRON node and checks its own finality. Copied from
// dispatcher/internal/dispatch/tronchain.go's GrpcBroadcastClient and
// HTTPFinalityReader (separate Go modules, no shared internal package,
// same convention as every other service here) -- verified live against
// grpc.trongrid.io:50051 / api.trongrid.io while that component was
// built; nothing about the broadcast/finality mechanics changes for
// relayd's own forward leg.
package tronbroadcast

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/client"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"relayd/internal/txbuild"
)

// GrpcBroadcastClient implements broadcasting against a real TRON
// node's gRPC surface, via gotron-sdk's own GrpcClient -- the same
// library relayd/internal/txbuild already depends on for construction.
type GrpcBroadcastClient struct {
	grpc *client.GrpcClient
}

// NewGrpcBroadcastClient connects to a TRON node's gRPC endpoint (e.g.
// "grpc.trongrid.io:50051"). Plaintext
// (grpc.WithTransportCredentials(insecure.NewCredentials())), not TLS --
// confirmed live against grpc.trongrid.io:50051 while building C5: a
// TLS handshake against that same endpoint fails immediately, while
// plaintext gRPC succeeds.
func NewGrpcBroadcastClient(nodeAddress string, timeout time.Duration) (*GrpcBroadcastClient, error) {
	g := client.NewGrpcClientWithTimeout(nodeAddress, timeout)
	if err := g.Start(grpc.WithTransportCredentials(insecure.NewCredentials())); err != nil {
		return nil, fmt.Errorf("tronbroadcast: connecting to TRON node %q: %w", nodeAddress, err)
	}
	return &GrpcBroadcastClient{grpc: g}, nil
}

// CurrentBlockReference fetches a fresh txbuild.BlockReference from
// this client's own node -- GetNowBlockCtx, the same real gRPC call
// every TRON wallet uses to pick a recent reference block before
// building a transaction. Timestamp/Expiration come from the wall
// clock, with a 2-minute expiration window (TRON's own tolerance).
func (c *GrpcBroadcastClient) CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error) {
	block, err := c.grpc.GetNowBlockCtx(ctx)
	if err != nil {
		return txbuild.BlockReference{}, fmt.Errorf("tronbroadcast: fetching current TRON block: %w", err)
	}
	if len(block.Blockid) != 32 {
		return txbuild.BlockReference{}, fmt.Errorf("tronbroadcast: current TRON block returned a %d-byte blockid, want 32", len(block.Blockid))
	}
	var hash [32]byte
	copy(hash[:], block.Blockid)

	now := time.Now().UTC()
	return txbuild.BlockReference{
		BlockNumber: block.BlockHeader.RawData.Number,
		BlockHash:   hash,
		Timestamp:   now,
		Expiration:  now.Add(2 * time.Minute),
	}, nil
}

// Broadcast submits tx and returns its txID on success. A false Result
// is translated into a Go error here rather than silently returning an
// empty txid. The txid itself is, by TRON's own convention, verified
// against a live node while building C5: SHA256(raw_data), computed
// here directly from tx.RawData exactly as txbuild.Digest already does
// from BuildTransfer's own output.
func (c *GrpcBroadcastClient) Broadcast(ctx context.Context, tx *core.Transaction) (string, error) {
	ret, err := c.grpc.BroadcastCtx(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("tronbroadcast: broadcasting to TRON node: %w", err)
	}
	if !ret.Result {
		return "", fmt.Errorf("tronbroadcast: TRON node rejected the broadcast: %s: %s", ret.Code, ret.Message)
	}

	rawData, err := proto.Marshal(tx.RawData)
	if err != nil {
		return "", fmt.Errorf("tronbroadcast: computing txid after a successful broadcast: %w", err)
	}
	digest := txbuild.Digest(rawData)
	return hex.EncodeToString(digest[:]), nil
}

// BroadcastSigned rebuilds the full, broadcast-ready core.Transaction
// from unsignedTx (txbuild.BuildTransfer's own return value) and the
// 65-byte compact signature S1 returned, then broadcasts it -- keeps
// gotron-sdk's own core.Transaction type out of internal/orchestrate's
// consumer interface, mirroring dispatcher/internal/dispatch/broadcast.go's
// own reconstructTransaction, folded into this client rather than kept
// as a package-level function (relayd has no separate attempt-tracking
// layer of its own to own this step instead).
func (c *GrpcBroadcastClient) BroadcastSigned(ctx context.Context, unsignedTx []byte, signature [65]byte) (string, error) {
	raw := &core.TransactionRaw{}
	if err := proto.Unmarshal(unsignedTx, raw); err != nil {
		return "", fmt.Errorf("tronbroadcast: unmarshaling raw_data: %w", err)
	}
	tx := &core.Transaction{RawData: raw, Signature: [][]byte{signature[:]}}
	return c.Broadcast(ctx, tx)
}

// FinalityReader implements finality checking against a real TRON
// node's solidity (SR-confirmed) HTTP surface -- the same
// GET /walletsolidity/gettransactioninfobyid?value=<txid> primitive
// tronwatcher/internal/chain.FinalityClient also uses, verified live
// against api.trongrid.io.
type FinalityReader struct {
	baseURL string
	http    *http.Client
}

// NewFinalityReader returns a FinalityReader for baseURL (e.g.
// "https://api.trongrid.io").
func NewFinalityReader(baseURL string) *FinalityReader {
	return &FinalityReader{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

type transactionInfoResponse struct {
	ID string `json:"id"`
}

// IsFinal reports whether tronTxID has reached SR finality.
func (r *FinalityReader) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.baseURL+"/walletsolidity/gettransactioninfobyid?value="+tronTxID, nil)
	if err != nil {
		return false, fmt.Errorf("tronbroadcast: building finality request: %w", err)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("tronbroadcast: checking finality for %s: %w", tronTxID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("tronbroadcast: reading finality response for %s: %w", tronTxID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("tronbroadcast: TRON node returned %d checking finality for %s", resp.StatusCode, tronTxID)
	}

	var info transactionInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return false, fmt.Errorf("tronbroadcast: decoding finality response for %s: %w", tronTxID, err)
	}
	return info.ID == tronTxID, nil
}
