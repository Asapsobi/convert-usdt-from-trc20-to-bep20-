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
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/client"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"relayd/internal/txbuild"
)

// SetAPIKey sends a TronGrid API key (TRON-PRO-API-KEY) with every call
// -- without one TronGrid rate-limits hard (HTTP 429).
func (c *GrpcBroadcastClient) SetAPIKey(key string) {
	_ = c.grpc.SetAPIKey(key)
}

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

// TokenBalance returns holder's USDT-TRC20 balance in raw on-chain units
// (6 decimals), via a constant (read-only) balanceOf call on the node.
// Written against TRC20CallCtx directly rather than the SDK's own
// TRC20ContractBalanceCtx, which indexes the node's result without
// checking it is there and would panic on an empty response.
func (c *GrpcBroadcastClient) TokenBalance(ctx context.Context, holder string) (*big.Int, error) {
	addr, err := address.Base58ToAddress(holder)
	if err != nil {
		return nil, fmt.Errorf("tronbroadcast: invalid TRON address %q: %w", holder, err)
	}
	// balanceOf(address): selector + the 20-byte address (the 0x41 TRON
	// prefix stripped) left-padded to one 32-byte word.
	word := hex.EncodeToString(common.LeftPadBytes(addr.Bytes()[1:], 32))
	result, err := c.grpc.TRC20CallCtx(ctx, "", txbuild.USDTContractAddress, "0x70a08231"+word, true, 0)
	if err != nil {
		return nil, fmt.Errorf("tronbroadcast: reading USDT balance of %s: %w", holder, err)
	}
	constant := result.GetConstantResult()
	if len(constant) != 1 || len(constant[0]) != 32 {
		return nil, fmt.Errorf("tronbroadcast: USDT balanceOf(%s) returned an unexpected result %x", holder, constant)
	}
	return new(big.Int).SetBytes(constant[0]), nil
}

// Resources is what a TRON account can spend without burning TRX.
type Resources struct {
	Exists     bool  // the account has been activated (received TRX at least once)
	Energy     int64 // available energy: delegated + staked, minus what's used
	Bandwidth  int64 // available bandwidth: free daily + staked, minus what's used
	BalanceSun int64 // TRX balance -- burned for whatever bandwidth is missing
}

// AccountResources reads addr's activation, energy, bandwidth, and TRX.
// An address that never received TRX has no account: Exists is false and
// it can't send anything until it is activated.
func (c *GrpcBroadcastClient) AccountResources(ctx context.Context, addr string) (Resources, error) {
	acct, err := c.grpc.GetAccountCtx(ctx, addr)
	if err != nil {
		if strings.Contains(err.Error(), "account not found") {
			return Resources{}, nil
		}
		return Resources{}, fmt.Errorf("tronbroadcast: reading account %s: %w", addr, err)
	}
	res, err := c.grpc.GetAccountResourceCtx(ctx, addr)
	if err != nil {
		return Resources{}, fmt.Errorf("tronbroadcast: reading resources of %s: %w", addr, err)
	}
	return Resources{
		Exists:     true,
		Energy:     max(0, res.GetEnergyLimit()-res.GetEnergyUsed()),
		Bandwidth:  max(0, res.GetFreeNetLimit()-res.GetFreeNetUsed()) + max(0, res.GetNetLimit()-res.GetNetUsed()),
		BalanceSun: acct.GetBalance(),
	}, nil
}

// EstimateTransferEnergy simulates a USDT transfer of raw units from
// from to to on the node and returns the energy it would use -- the
// exact figure, which is about twice as high when to has never held USDT
// (the contract stores a new balance slot).
func (c *GrpcBroadcastClient) EstimateTransferEnergy(ctx context.Context, from, to string, raw *big.Int) (int64, error) {
	toAddr, err := address.Base58ToAddress(to)
	if err != nil {
		return 0, fmt.Errorf("tronbroadcast: invalid TRON address %q: %w", to, err)
	}
	data := "0xa9059cbb" + hex.EncodeToString(common.LeftPadBytes(toAddr.Bytes()[1:], 32)) +
		hex.EncodeToString(common.LeftPadBytes(raw.Bytes(), 32))
	result, err := c.grpc.TRC20CallCtx(ctx, from, txbuild.USDTContractAddress, data, true, 0)
	if err != nil {
		return 0, fmt.Errorf("tronbroadcast: estimating the energy of a transfer from %s: %w", from, err)
	}
	if result.GetEnergyUsed() <= 0 {
		return 0, fmt.Errorf("tronbroadcast: the node estimated no energy for a transfer from %s", from)
	}
	return result.GetEnergyUsed(), nil
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
	apiKey  string
	http    *http.Client
}

// SetAPIKey sends a TronGrid API key (TRON-PRO-API-KEY) with every call:
// TronGrid rate-limits keyless calls, which would leave a sent TRON
// transfer unconfirmed.
func (r *FinalityReader) SetAPIKey(key string) { r.apiKey = key }

// NewFinalityReader returns a FinalityReader for baseURL (e.g.
// "https://api.trongrid.io").
func NewFinalityReader(baseURL string) *FinalityReader {
	return &FinalityReader{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// transactionInfoResponse captures both the existence check IsFinal
// already used (ID) and the execution outcome nothing in this package
// previously looked at: Receipt.Result is TRON's own canonical
// per-transaction execution verdict ("SUCCESS", or a specific failure
// reason like "OUT_OF_ENERGY", "REVERT", etc.) -- confirmed live: a
// broadcast this package's own Broadcast() accepted (ret.Result == true,
// meaning the NODE took the transaction into a block) still executed
// with receipt.result "OUT_OF_ENERGY", moving zero funds, while ID was
// already populated and non-empty. A transaction being included in a
// block is not the same fact as it having succeeded; this package's own
// IsFinal previously only checked the former.
type transactionInfoResponse struct {
	ID      string `json:"id"`
	Result  string `json:"result"` // present ("FAILED") on failure, absent on success -- not relied on; Receipt.Result is
	Receipt struct {
		Result string `json:"result"` // "SUCCESS", or the specific failure reason
	} `json:"receipt"`
	Log []struct {
		Address string   `json:"address"` // the contract, hex without the 41 prefix
		Topics  []string `json:"topics"`
		Data    string   `json:"data"`
	} `json:"log"`
}

func (r *FinalityReader) fetchTransactionInfo(ctx context.Context, tronTxID string) (transactionInfoResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.baseURL+"/walletsolidity/gettransactioninfobyid?value="+tronTxID, nil)
	if err != nil {
		return transactionInfoResponse{}, fmt.Errorf("tronbroadcast: building finality request: %w", err)
	}
	if r.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", r.apiKey)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return transactionInfoResponse{}, fmt.Errorf("tronbroadcast: checking finality for %s: %w", tronTxID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return transactionInfoResponse{}, fmt.Errorf("tronbroadcast: reading finality response for %s: %w", tronTxID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return transactionInfoResponse{}, fmt.Errorf("tronbroadcast: TRON node returned %d checking finality for %s", resp.StatusCode, tronTxID)
	}

	var info transactionInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return transactionInfoResponse{}, fmt.Errorf("tronbroadcast: decoding finality response for %s: %w", tronTxID, err)
	}
	return info, nil
}

// IsFinal reports whether tronTxID has reached SR finality -- existence
// only, same as before. Callers that need to know whether the
// transaction actually SUCCEEDED, not just that it was included, must
// use CheckExecution instead: a transaction reaching finality with a
// failed execution result is still "final" in the sense IsFinal answers
// (it will never be reorged away), it just never moved anything.
func (r *FinalityReader) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	info, err := r.fetchTransactionInfo(ctx, tronTxID)
	if err != nil {
		return false, err
	}
	return info.ID == tronTxID, nil
}

// CheckExecution reports whether tronTxID has reached finality AND, if
// so, whether its own on-chain execution actually succeeded --
// receipt.result == "SUCCESS" is the one signal this package trusts;
// anything else (a specific failure reason string, or an empty receipt
// before the transaction is even indexed) means the transaction moved no
// funds, no matter how it was broadcast or accepted. failureReason is
// TRON's own receipt.result string when success is false and final is
// true (e.g. "OUT_OF_ENERGY"), empty otherwise.
func (r *FinalityReader) CheckExecution(ctx context.Context, tronTxID string) (final, success bool, failureReason string, err error) {
	info, err := r.fetchTransactionInfo(ctx, tronTxID)
	if err != nil {
		return false, false, "", err
	}
	if info.ID != tronTxID {
		return false, false, "", nil
	}
	if info.Receipt.Result == "SUCCESS" {
		return true, true, "", nil
	}
	return true, false, info.Receipt.Result, nil
}

// TokenTransfer is one TRC-20 Transfer a transaction made.
type TokenTransfer struct {
	Token  string // the token contract, base58
	To     string // base58
	Amount *big.Int
}

// trc20TransferTopic is the Transfer(address,address,uint256) event.
const trc20TransferTopic = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// tronAddressFromHex turns a 20-byte hex address (a log's contract, or the
// last 20 bytes of a topic) into its base58 form.
func tronAddressFromHex(h string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(h, "0x"))
	if err != nil {
		return "", err
	}
	if len(raw) > 20 {
		raw = raw[len(raw)-20:]
	}
	if len(raw) != 20 {
		return "", fmt.Errorf("tronbroadcast: %q is not a 20-byte address", h)
	}
	return address.Address(append([]byte{0x41}, raw...)).String(), nil
}

// TokenTransfers reads the TRC-20 transfers tronTxID made, from the
// solidified (final) chain -- how relayd checks a vendor's payout to a
// customer. A transaction not yet solidified has no transfers and is not
// final; a failed one is final and moved nothing.
func (r *FinalityReader) TokenTransfers(ctx context.Context, tronTxID string) ([]TokenTransfer, bool, error) {
	info, err := r.fetchTransactionInfo(ctx, tronTxID)
	if err != nil {
		return nil, false, err
	}
	if info.ID != tronTxID {
		return nil, false, nil
	}
	if info.Receipt.Result != "SUCCESS" {
		return nil, true, nil
	}
	var out []TokenTransfer
	for _, lg := range info.Log {
		if len(lg.Topics) != 3 || strings.TrimPrefix(lg.Topics[0], "0x") != trc20TransferTopic {
			continue
		}
		token, err := tronAddressFromHex(lg.Address)
		if err != nil {
			continue
		}
		to, err := tronAddressFromHex(lg.Topics[2])
		if err != nil {
			continue
		}
		amount, ok := new(big.Int).SetString(strings.TrimPrefix(lg.Data, "0x"), 16)
		if !ok {
			continue
		}
		out = append(out, TokenTransfer{Token: token, To: to, Amount: amount})
	}
	return out, true, nil
}
