package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/ledgerclient"
)

const bscMainnetChainID = 56

// decimalsSelector is keccak256("decimals()")[:4].
var decimalsSelector = []byte{0x31, 0x3c, 0xe5, 0x67}

// runEngineSelfChecks refuses to start the chain-watching engine on
// anything definitely misconfigured, each of which otherwise fails
// silently later: providers on different (or the wrong) chain, a token
// whose real decimals differ from what every deposit amount is scaled
// by, or a ledger that rejects this service's token -- the last one
// means deposits are detected but never recorded, exactly how the
// first customer deposit went unrecorded. Unreachable dependencies only
// warn; the engine's own per-tick retries handle those.
func runEngineSelfChecks(providers []chain.Provider, contractAddress string, ledger *ledgerclient.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var fatal []string
	fail := func(format string, args ...any) { fatal = append(fatal, fmt.Sprintf(format, args...)) }
	warn := func(format string, args ...any) {
		slog.Warn("watcherd: startup check: " + fmt.Sprintf(format, args...))
	}

	chainIDs := map[string]*big.Int{}
	for _, p := range providers {
		id, err := p.Client.ChainID(ctx)
		if err != nil {
			warn("RPC provider %s unreachable, chain id not verified: %v", p.Name, err)
			continue
		}
		chainIDs[p.Name] = id
	}
	var first *big.Int
	for name, id := range chainIDs {
		if first == nil {
			first = id
		} else if id.Cmp(first) != 0 {
			fail("RPC providers disagree on the chain (%s reports chain id %s, another reports %s)", name, id, first)
		}
	}
	if first != nil && strings.EqualFold(contractAddress, defaultContractAddress) && first.Int64() != bscMainnetChainID {
		fail("watching mainnet USDT %s, but the RPC providers are on chain id %s, not BSC mainnet (%d)", contractAddress, first, bscMainnetChainID)
	}

	if len(providers) > 0 {
		to := common.HexToAddress(contractAddress)
		out, err := providers[0].Client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: decimalsSelector}, nil)
		decimals := new(big.Int).SetBytes(out)
		switch {
		case err != nil:
			warn("could not read decimals() of %s via %s: %v", contractAddress, providers[0].Name, err)
		case len(out) != 32:
			fail("%s returned %x for decimals(), not a uint8 -- is it really a token contract?", contractAddress, out)
		case decimals.Cmp(big.NewInt(chain.TokenDecimals)) != 0:
			fail("token %s reports %s decimals on-chain, but every deposit amount is scaled as %d decimals", contractAddress, decimals, chain.TokenDecimals)
		}
	}

	_, err := ledger.GetOrder(ctx, "watcherd-startup-token-probe")
	var apiErr *ledgerclient.APIError
	switch {
	case err == nil:
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden):
		fail("ledger rejected watcherd's token (HTTP %d) -- deposits would be detected but never recorded; check WATCHER_LEDGER_TOKEN", apiErr.Status)
	case errors.As(err, &apiErr):
	default:
		warn("ledger unreachable, token not verified: %v", err)
	}

	if len(fatal) > 0 {
		return fmt.Errorf("watcherd: startup self-checks failed:\n  - %s", strings.Join(fatal, "\n  - "))
	}
	slog.Info("watcherd: startup self-checks passed")
	return nil
}
