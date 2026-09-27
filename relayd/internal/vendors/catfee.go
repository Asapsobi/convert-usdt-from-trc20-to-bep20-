package vendors

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CatFee rents TRON energy through CatFee's API (https://api.catfee.io),
// paid from the account's prepaid TRX balance. Ported from
// energybroker/internal/provider/catfee.go, which was proven against the
// live API (separate Go modules, no shared package).
type CatFee struct {
	baseURL, apiKey, apiSecret string
	http                       *http.Client
}

const (
	catfeeBaseURL     = "https://api.catfee.io"
	catfeeMinQuantity = 65_000 // CatFee's own minimum energy order
	catfeeDuration    = "1h"   // the delegation lasts an hour; leftover energy is reused by later transfers
)

// NewCatFee builds a CatFee client; baseURL "" means the real API.
func NewCatFee(apiKey, apiSecret, baseURL string) (*CatFee, error) {
	if apiKey == "" || apiSecret == "" {
		return nil, errors.New("vendors: CatFee needs both an API key and secret")
	}
	if baseURL == "" {
		baseURL = catfeeBaseURL
	}
	return &CatFee{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, apiSecret: apiSecret,
		http: &http.Client{Timeout: 20 * time.Second}}, nil
}

// MinUnits implements EnergyVendor.
func (c *CatFee) MinUnits() int64 { return catfeeMinQuantity }

type catfeeEnvelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	SubCode string          `json:"sub_code"`
	SubMsg  string          `json:"sub_msg"`
	Data    json.RawMessage `json:"data"`
}

// do signs and sends one request: HMAC-SHA256 over timestamp + method +
// path-with-query, base64, in CF-ACCESS-SIGN.
func (c *CatFee) do(ctx context.Context, method, path string, query url.Values) (json.RawMessage, error) {
	requestPath := path
	if len(query) > 0 {
		requestPath += "?" + query.Encode()
	}
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(timestamp + method + requestPath))

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, nil)
	if err != nil {
		return nil, fmt.Errorf("catfee: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("CF-ACCESS-KEY", c.apiKey)
	req.Header.Set("CF-ACCESS-SIGN", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("CF-ACCESS-TIMESTAMP", timestamp)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catfee: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("catfee: reading %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("catfee: %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	var env catfeeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("catfee: decoding %s %s (HTTP %d): %w", method, path, resp.StatusCode, err)
	}
	if env.Code != 0 {
		// CatFee answered and refused: a rejection, not an outage.
		return nil, &EnergyRejection{fmt.Errorf("catfee: %s %s: code=%d msg=%q sub_code=%q sub_msg=%q",
			method, path, env.Code, env.Msg, env.SubCode, env.SubMsg)}
	}
	return env.Data, nil
}

// QuoteSun implements EnergyVendor.
func (c *CatFee) QuoteSun(ctx context.Context, units int64) (int64, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/estimate", url.Values{
		"quantity": {strconv.FormatInt(units, 10)}, "duration": {catfeeDuration},
	})
	if err != nil {
		return 0, err
	}
	var totalSun int64
	if err := json.Unmarshal(data, &totalSun); err != nil {
		return 0, fmt.Errorf("catfee: decoding /v1/estimate: %w", err)
	}
	return totalSun, nil
}

// Account implements EnergyAccountReader. CatFee reports the balance in
// sun.
func (c *CatFee) Account(ctx context.Context) (EnergyAccount, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/account", nil)
	if err != nil {
		return EnergyAccount{}, err
	}
	var acct struct {
		Balance         int64  `json:"balance"`
		RechargeAddress string `json:"recharge_address"`
	}
	if err := json.Unmarshal(data, &acct); err != nil {
		return EnergyAccount{}, fmt.Errorf("catfee: decoding /v1/account: %w", err)
	}
	return EnergyAccount{BalanceSun: acct.Balance, TopUpAddress: acct.RechargeAddress}, nil
}

// Rent implements EnergyVendor.
func (c *CatFee) Rent(ctx context.Context, target string, units int64) (Rental, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/order", url.Values{
		"quantity": {strconv.FormatInt(units, 10)}, "receiver": {target}, "duration": {catfeeDuration},
	})
	if err != nil {
		return Rental{}, err
	}
	var order struct {
		ID           string `json:"id"`
		PayAmountSun int64  `json:"pay_amount_sun"`
	}
	if err := json.Unmarshal(data, &order); err != nil {
		return Rental{}, fmt.Errorf("catfee: decoding /v1/order: %w", err)
	}
	if order.ID == "" {
		return Rental{}, errors.New("catfee: /v1/order returned no order id")
	}
	return Rental{OrderID: order.ID, CostSun: order.PayAmountSun}, nil
}
