// Package evmbroadcast resolves the chain state internal/evmtx needs
// (nonce, gas price), broadcasts a signed BEP20 forward transfer to a
// real BSC node, and checks its own finality -- the EVM-direction
// sibling of internal/tronbroadcast. New code, not a port: no service
// in this repo has ever broadcast an EVM transaction before now (see
// internal/evmtx's own package doc comment on why this whole direction
// is genuinely new work, not reused).
//
// Finality is checked the SAME way depositwatcher/internal/chain's own
// LatestFinalized does -- BSC's real BEP-126 fast-finality "finalized"
// block tag (eth_getBlockByNumber("finalized", false)), not a
// hand-picked confirmation depth -- for the same reason that package's
// own doc comment gives: a chain-level finality commitment is both
// faster and strictly stronger than guessing a depth. Unlike
// depositwatcher's own Pool, this is single-provider: relayd's own
// forward-leg finality check has no architectural requirement for
// multi-provider agreement the way C2's real-money DEPOSIT detection
// does (docs/02-architecture/model-f-relay-architecture.md names no
// such requirement for the forward leg), matching the single-endpoint
// precedent internal/tronbroadcast/tronbroadcast.go's own
// FinalityReader already set for the TRC20 direction.
package evmbroadcast

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Client wraps a single real BSC node connection.
type Client struct {
	eth *ethclient.Client
}

// NewClient dials rpcURL (e.g. a real BSC RPC provider's own HTTPS
// endpoint).
func NewClient(ctx context.Context, rpcURL string) (*Client, error) {
	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("evmbroadcast: dialing %q: %w", rpcURL, err)
	}
	return &Client{eth: eth}, nil
}

// CurrentNonce returns address's own next usable nonce, including
// pending (not-yet-mined) transactions -- required so a retried
// forward-leg build never reuses a nonce a prior attempt already
// consumed.
func (c *Client) CurrentNonce(ctx context.Context, address string) (uint64, error) {
	nonce, err := c.eth.PendingNonceAt(ctx, common.HexToAddress(address))
	if err != nil {
		return 0, fmt.Errorf("evmbroadcast: fetching nonce for %s: %w", address, err)
	}
	return nonce, nil
}

// SuggestGasPrice returns the node's own current suggested gas price.
func (c *Client) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	price, err := c.eth.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("evmbroadcast: suggesting gas price: %w", err)
	}
	return price, nil
}

// Broadcast submits signed and returns its own transaction hash.
func (c *Client) Broadcast(ctx context.Context, signed *types.Transaction) (string, error) {
	if err := c.eth.SendTransaction(ctx, signed); err != nil {
		return "", fmt.Errorf("evmbroadcast: broadcasting: %w", err)
	}
	return signed.Hash().Hex(), nil
}

type finalizedBlockRPC struct {
	Number string `json:"number"`
}

// latestFinalizedHeight calls eth_getBlockByNumber("finalized", false)
// directly via the raw RPC client -- mirroring
// depositwatcher/internal/chain's own identical call exactly (same
// method, same params, same reasoning: the typed ethclient helper has
// no "finalized" tag constant of its own).
func (c *Client) latestFinalizedHeight(ctx context.Context) (uint64, error) {
	var result finalizedBlockRPC
	if err := c.eth.Client().CallContext(ctx, &result, "eth_getBlockByNumber", "finalized", false); err != nil {
		return 0, fmt.Errorf("evmbroadcast: eth_getBlockByNumber(finalized): %w", err)
	}
	height, ok := new(big.Int).SetString(trimHexPrefix(result.Number), 16)
	if !ok {
		return 0, fmt.Errorf("evmbroadcast: malformed finalized block number %q", result.Number)
	}
	return height.Uint64(), nil
}

func trimHexPrefix(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}

// IsFinal reports whether txHash has both landed in a mined block (a
// successful receipt) AND that block is at or below BSC's own current
// finalized height. A transaction that reverted on-chain (receipt
// Status == 0) is reported as an error, never silently treated as
// "not yet final" -- a caller polling this in a loop must not wait
// forever for a transaction that will never succeed.
func (c *Client) IsFinal(ctx context.Context, txHash string) (bool, error) {
	receipt, err := c.eth.TransactionReceipt(ctx, common.HexToHash(txHash))
	if err != nil {
		if err == ethereum.NotFound {
			return false, nil // not yet mined -- not an error, just not final yet
		}
		return false, fmt.Errorf("evmbroadcast: fetching receipt for %s: %w", txHash, err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return false, fmt.Errorf("evmbroadcast: transaction %s reverted on-chain (status %d)", txHash, receipt.Status)
	}

	finalizedHeight, err := c.latestFinalizedHeight(ctx)
	if err != nil {
		return false, err
	}
	return receipt.BlockNumber.Uint64() <= finalizedHeight, nil
}
