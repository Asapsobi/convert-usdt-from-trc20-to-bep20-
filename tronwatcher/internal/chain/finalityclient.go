package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FinalityClient checks whether a TRON transaction has reached SR/
// solidity confirmation -- copied from
// dispatcher/internal/dispatch/tronchain.go's own HTTPFinalityReader
// (verified live against api.trongrid.io while building C5: GET
// /walletsolidity/gettransactioninfobyid?value=<txid> returns a
// non-empty body with a matching "id" field once and only once a
// transaction has reached solidity state). Duplicated rather than
// imported: separate Go modules, no shared internal package, same
// convention as every other service in this repo.
type FinalityClient struct {
	baseURL string
	http    *http.Client
}

// NewFinalityClient returns a FinalityClient for baseURL (e.g.
// "https://api.trongrid.io").
func NewFinalityClient(baseURL string) *FinalityClient {
	return &FinalityClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

type transactionInfoResponse struct {
	ID string `json:"id"`
}

// IsFinal reports whether tronTxID has reached SR/solidity finality.
func (c *FinalityClient) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/walletsolidity/gettransactioninfobyid?value="+tronTxID, nil)
	if err != nil {
		return false, fmt.Errorf("chain: building finality request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("chain: checking finality for %s: %w", tronTxID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("chain: reading finality response for %s: %w", tronTxID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("chain: TRON node returned %d checking finality for %s", resp.StatusCode, tronTxID)
	}

	var info transactionInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return false, fmt.Errorf("chain: decoding finality response for %s: %w", tronTxID, err)
	}
	return info.ID == tronTxID, nil
}
