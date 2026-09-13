// Package chain scans TRON for inbound TRC20 transfers to this
// service's own watched addresses.
//
// This is NOT a port of depositwatcher/internal/chain: TRON has no
// eth_getLogs equivalent -- no generic "give me every Transfer event
// emitted by contract C between blocks A and B" JSON-RPC call the way
// every EVM chain exposes. What TRON's own indexers (TronGrid, and
// TronGrid-compatible full-node gateways) expose instead is a
// per-ADDRESS TRC20 transfer history endpoint
// (GET /v1/accounts/{address}/transactions/trc20), so this package
// watches per-address, not per-contract-then-filter. See
// energybroker/internal/buffer/trongrid.go for the sibling REST-client
// pattern this package's own Provider follows (base URL default,
// optional TRON-PRO-API-KEY header, timeout).
package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is TRON's own public full-node API gateway -- the same
// host dispatcher/internal/dispatch/tronchain.go's own HTTPFinalityReader
// already uses, and the same host energybroker/internal/buffer/trongrid.go
// already uses for on-chain verification.
const DefaultBaseURL = "https://api.trongrid.io"

// Config configures a Provider.
type Config struct {
	Name       string // for error messages and cross-check logging, e.g. "trongrid-primary"
	BaseURL    string // DefaultBaseURL if empty
	APIKey     string // sent as TRON-PRO-API-KEY if set; optional
	HTTPClient *http.Client
}

// Provider is one TronGrid-compatible REST endpoint this service scans
// against. Two independently-operated Providers (e.g. TronGrid itself
// plus a second TronGrid-compatible gateway) are cross-checked by Pool,
// mirroring depositwatcher/internal/chain.Pool's own 2-provider
// agreement discipline (invariant 5) -- the specific query shape here
// differs (per-address REST, not per-contract eth_getLogs), but the
// "never trust a single provider's own say-so for money" posture does
// not.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewProvider returns a Provider for cfg.
func NewProvider(cfg Config) *Provider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Provider{
		name:    cfg.Name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  cfg.APIKey,
		http:    client,
	}
}

// Name reports this provider's own configured name.
func (p *Provider) Name() string { return p.name }

// Transfer is one inbound TRC20 transfer TronGrid reported for a watched
// address, still in the token's own on-chain units (see
// classify.go's ParseTransferValue for the rescale into
// money.Amount) -- unparsed here so Pool can cross-check two providers'
// raw responses byte-for-field before either is trusted.
type Transfer struct {
	TxID            string
	From            string
	To              string
	ValueRaw        *big.Int
	BlockTimestamp  time.Time
	ContractAddress string
}

// tronGridTRC20Response is TronGrid's own
// GET /v1/accounts/{address}/transactions/trc20 response shape -- field
// names and casing confirmed against TRON's own official API reference
// (developers.tron.network).
type tronGridTRC20Response struct {
	Data []struct {
		TransactionID string `json:"transaction_id"`
		TokenInfo     struct {
			Address  string `json:"address"`
			Decimals int    `json:"decimals"`
		} `json:"token_info"`
		BlockTimestamp int64  `json:"block_timestamp"` // milliseconds since epoch
		From           string `json:"from"`
		To             string `json:"to"`
		Type           string `json:"type"`
		Value          string `json:"value"`
	} `json:"data"`
	Success bool `json:"success"`
	Meta    struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"meta"`
}

// ScanTRC20Transfers fetches inbound TRC20 transfers to address for
// contractAddress (the USDT-TRC20 contract in production), with
// block_timestamp >= minTimestampMs, filtered to Transfer-type events
// only_to this address (TronGrid's own only_to query parameter -- never
// fetch outbound transfers, this service never needs them and they would
// only bloat the response). Paginates via TronGrid's own cursor
// ("fingerprint"), following it until exhausted; a real watched address
// sees at most a handful of deposits, so this is not expected to loop
// more than once or twice in practice.
func (p *Provider) ScanTRC20Transfers(ctx context.Context, address, contractAddress string, minTimestampMs int64) ([]Transfer, error) {
	var out []Transfer
	fingerprint := ""

	for {
		page, next, err := p.fetchPage(ctx, address, contractAddress, minTimestampMs, fingerprint)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" || len(page) == 0 {
			break
		}
		fingerprint = next
	}
	return out, nil
}

func (p *Provider) fetchPage(ctx context.Context, address, contractAddress string, minTimestampMs int64, fingerprint string) ([]Transfer, string, error) {
	q := url.Values{}
	q.Set("only_to", "true")
	q.Set("only_confirmed", "true")
	q.Set("contract_address", contractAddress)
	q.Set("min_timestamp", strconv.FormatInt(minTimestampMs, 10))
	q.Set("limit", "200")
	q.Set("order_by", "block_timestamp,asc")
	if fingerprint != "" {
		q.Set("fingerprint", fingerprint)
	}

	reqURL := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?%s", p.baseURL, address, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("chain: building request for %s: %w", p.name, err)
	}
	req.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", p.apiKey)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("chain: %s: scanning %s: %w", p.name, address, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("chain: %s: reading response for %s: %w", p.name, address, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("chain: %s: scanning %s: HTTP %d: %s", p.name, address, resp.StatusCode, string(body))
	}

	var parsed tronGridTRC20Response
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", fmt.Errorf("chain: %s: decoding response for %s: %w", p.name, address, err)
	}
	if !parsed.Success {
		return nil, "", fmt.Errorf("chain: %s: TronGrid reported success=false for %s", p.name, address)
	}

	out := make([]Transfer, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.Type != "Transfer" {
			continue // defensive: only_to already scopes this, but never trust a provider's own filtering alone
		}
		value, ok := new(big.Int).SetString(d.Value, 10)
		if !ok {
			return nil, "", fmt.Errorf("chain: %s: transfer %s has a non-numeric value %q", p.name, d.TransactionID, d.Value)
		}
		out = append(out, Transfer{
			TxID:            d.TransactionID,
			From:            d.From,
			To:              d.To,
			ValueRaw:        value,
			BlockTimestamp:  time.UnixMilli(d.BlockTimestamp).UTC(),
			ContractAddress: d.TokenInfo.Address,
		})
	}
	return out, parsed.Meta.Fingerprint, nil
}
