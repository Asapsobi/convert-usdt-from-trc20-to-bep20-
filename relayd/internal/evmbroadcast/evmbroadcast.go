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
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"relayd/internal/evmtx"
)

// ErrReverted wraps IsFinal's own returned error specifically when a
// transaction's receipt confirms it reverted on-chain (receipt.Status !=
// Successful) -- errors.Is(err, ErrReverted) is how a caller distinguishes
// that CONFIRMED outcome from every other error IsFinal can return (a
// transient RPC failure fetching the receipt or the finalized height,
// wrapped plainly, with no ErrReverted in its chain). This distinction
// matters for real money: a caller that treated any non-nil error as a
// confirmed failure would, on a merely transient error, abandon and
// rebuild a transaction that may still go on to succeed on its own --
// risking two real transfers landing for the same logical send.
var ErrReverted = errors.New("evmbroadcast: transaction reverted on-chain")

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

// ConfirmedNonce returns address's nonce as of the latest block, counting
// only mined transactions. Once it passes a sent transaction's nonce,
// that nonce has been used by some mined transaction.
func (c *Client) ConfirmedNonce(ctx context.Context, address string) (uint64, error) {
	nonce, err := c.eth.NonceAt(ctx, common.HexToAddress(address), nil)
	if err != nil {
		return 0, fmt.Errorf("evmbroadcast: fetching confirmed nonce for %s: %w", address, err)
	}
	return nonce, nil
}

// balanceOfSelector is keccak256("balanceOf(address)")[:4].
var balanceOfSelector = []byte{0x70, 0xa0, 0x82, 0x31}

// TokenBalance returns holder's USDT balance in raw on-chain units (18
// decimals), as of the latest block.
func (c *Client) TokenBalance(ctx context.Context, holder string) (*big.Int, error) {
	to := common.HexToAddress(evmtx.USDTContractAddress)
	data := append(append([]byte{}, balanceOfSelector...), common.LeftPadBytes(common.HexToAddress(holder).Bytes(), 32)...)
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("evmbroadcast: reading USDT balance of %s: %w", holder, err)
	}
	if len(out) != 32 {
		return nil, fmt.Errorf("evmbroadcast: USDT balanceOf(%s) returned %d bytes, want 32", holder, len(out))
	}
	return new(big.Int).SetBytes(out), nil
}

// TransactionMined reports whether txHash has a receipt at all --
// successful or reverted, final or not. False means no block this node
// knows of contains it.
func (c *Client) TransactionMined(ctx context.Context, txHash string) (bool, error) {
	_, err := c.eth.TransactionReceipt(ctx, common.HexToHash(txHash))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ethereum.NotFound) {
		return false, nil
	}
	return false, fmt.Errorf("evmbroadcast: fetching receipt for %s: %w", txHash, err)
}

// NativeBalance returns holder's BNB balance in wei, as of the latest block.
func (c *Client) NativeBalance(ctx context.Context, holder string) (*big.Int, error) {
	bal, err := c.eth.BalanceAt(ctx, common.HexToAddress(holder), nil)
	if err != nil {
		return nil, fmt.Errorf("evmbroadcast: reading BNB balance of %s: %w", holder, err)
	}
	return bal, nil
}

// SuggestGasPrice returns the node's own current suggested gas price.
func (c *Client) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	price, err := c.eth.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("evmbroadcast: suggesting gas price: %w", err)
	}
	return price, nil
}

// ChainID returns the connected node's own chain id -- 56 for BSC
// mainnet, anything else means relayd is pointed at the wrong network.
func (c *Client) ChainID(ctx context.Context) (*big.Int, error) {
	id, err := c.eth.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("evmbroadcast: eth_chainId: %w", err)
	}
	return id, nil
}

// decimalsSelector is keccak256("decimals()")[:4].
var decimalsSelector = []byte{0x31, 0x3c, 0xe5, 0x67}

// TokenDecimals reads token's own decimals() from the chain.
func (c *Client) TokenDecimals(ctx context.Context, token string) (uint8, error) {
	to := common.HexToAddress(token)
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: decimalsSelector}, nil)
	if err != nil {
		return 0, fmt.Errorf("evmbroadcast: calling decimals() on %s: %w", token, err)
	}
	value := new(big.Int).SetBytes(out)
	if len(out) != 32 || !value.IsUint64() || value.Uint64() > 255 {
		return 0, fmt.Errorf("evmbroadcast: decimals() on %s returned %x, not a uint8", token, out)
	}
	return uint8(value.Uint64()), nil
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
// Status == 0) is reported as an error wrapping ErrReverted, never
// silently treated as "not yet final" -- a caller polling this in a loop
// must not wait forever for a transaction that will never succeed. Every
// OTHER error this returns (fetching the receipt, fetching the finalized
// height) is a plain wrapped error with no ErrReverted in its chain --
// deliberately distinguishable via errors.Is, since a caller that cannot
// tell "confirmed reverted" apart from "transient RPC failure" would risk
// abandoning and rebuilding a transaction that may still go on to
// succeed on its own.
func (c *Client) IsFinal(ctx context.Context, txHash string) (bool, error) {
	receipt, err := c.eth.TransactionReceipt(ctx, common.HexToHash(txHash))
	if err != nil {
		if err == ethereum.NotFound {
			return false, nil // not yet mined -- not an error, just not final yet
		}
		return false, fmt.Errorf("evmbroadcast: fetching receipt for %s: %w", txHash, err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return false, fmt.Errorf("evmbroadcast: transaction %s reverted on-chain (status %d): %w", txHash, receipt.Status, ErrReverted)
	}

	finalizedHeight, err := c.latestFinalizedHeight(ctx)
	if err != nil {
		return false, err
	}
	return receipt.BlockNumber.Uint64() <= finalizedHeight, nil
}

// transferTopic is the ERC-20 Transfer(address,address,uint256) event.
var transferTopic = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

// TokenTransfer is one ERC-20 Transfer a transaction made.
type TokenTransfer struct {
	Token  string // the token contract
	To     string
	Amount *big.Int // raw on-chain units
}

// TokenTransfers reads the ERC-20 transfers txHash made and whether it is
// final -- how relayd checks a vendor's payout to a customer. A
// transaction not mined yet has no transfers and is not final; a
// reverted one is final and moved nothing.
func (c *Client) TokenTransfers(ctx context.Context, txHash string) ([]TokenTransfer, bool, error) {
	receipt, err := c.eth.TransactionReceipt(ctx, common.HexToHash(txHash))
	if err != nil {
		if err == ethereum.NotFound {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("evmbroadcast: fetching receipt for %s: %w", txHash, err)
	}
	finalizedHeight, err := c.latestFinalizedHeight(ctx)
	if err != nil {
		return nil, false, err
	}
	final := receipt.BlockNumber.Uint64() <= finalizedHeight
	if receipt.Status != types.ReceiptStatusSuccessful {
		return nil, final, nil
	}
	var out []TokenTransfer
	for _, lg := range receipt.Logs {
		if len(lg.Topics) != 3 || lg.Topics[0] != transferTopic {
			continue
		}
		out = append(out, TokenTransfer{Token: lg.Address.Hex(), To: common.BytesToAddress(lg.Topics[2].Bytes()).Hex(),
			Amount: new(big.Int).SetBytes(lg.Data)})
	}
	return out, final, nil
}
