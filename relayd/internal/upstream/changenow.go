// ChangeNOW (changenow.io) is the second real vendor behind
// upstream.MultiProvider's own best-rate routing (router.go) --
// chosen alongside FixedFloat specifically so a single vendor's outage
// or a bad rate never blocks or overcharges a customer. R2's decision
// record (docs/01-strategy/model-f-relay-findings.md) has the full
// evaluation: ChangeNOW's own public API Terms of Use (§2.1/§3.3) grant
// an explicit license to build and distribute a third-party application
// that integrates with their Service -- exactly relayd's own model
// (integrate, never resell the API itself), the same criterion (d)
// FixedFloat's own commercial-key clause satisfied.
//
// Unlike FixedFloat, ChangeNOW's v1 API needs no request signing -- the
// API key travels as a path segment on write/read calls, a materially
// simpler auth model. See NewChangeNowProvider's own doc comment for
// what's independently confirmed here vs. inferred from third-party
// wrappers (the official Postman docs are JS-rendered and could not be
// fetched directly while building this).
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"relayd/internal/money"
)

const changeNowBaseURL = "https://api.changenow.io/v1"

// changeNowQuoteValidFor mirrors fixedFloatQuoteValidFor's own reasoning
// exactly: ChangeNOW's own estimate endpoint carries no expiration of
// its own (no order exists yet), so this is this file's own
// conservative window, not a vendor guarantee -- architecture doc §6's
// own re-quote-at-forward-time step is the real protection.
const changeNowQuoteValidFor = 60 * time.Second

// ChangeNowConfig holds this account's own real API key and currency-
// code mapping. USDTTRC20Ccy/USDTBEP20Ccy are config, not constants, for
// the same reason FixedFloatConfig's own equivalents are: inferred from
// public ChangeNOW pages and a third-party Go client's own endpoint
// list, never independently confirmed against a live GET /v1/currencies
// response while building this integration -- verify them there before
// routing real orders.
type ChangeNowConfig struct {
	APIKey       string
	USDTTRC20Ccy string // e.g. "usdttrc20" -- verify against GET /v1/currencies
	USDTBEP20Ccy string // e.g. "usdtbsc" -- verify against GET /v1/currencies
	HTTPClient   *http.Client
}

// ChangeNowProvider implements SwapProvider against ChangeNOW's real v1
// API.
type ChangeNowProvider struct {
	cfg     ChangeNowConfig
	http    *http.Client
	baseURL string
}

// NewChangeNowProvider validates cfg and returns a ChangeNowProvider.
// Endpoint paths below (/exchange-amount/{amount}/{from}_{to},
// POST/GET /transactions/...) are reconstructed from a real third-party
// Go client's own documented call list (github.com/crypdex/go-changenow),
// not the official Postman collection (JS-rendered, could not be
// fetched while building this) -- flagged the same way this file's own
// currency-code fields are: verify against a real sandbox/production
// call before trusting this in front of real money, per this whole
// project's own "verify before trusting a spec" discipline (see e.g.
// docs/03-build/c5-payout-dispatcher-build-prompts.md's own identical
// caveat about C4's real route paths).
func NewChangeNowProvider(cfg ChangeNowConfig) (*ChangeNowProvider, error) {
	missing := []string{}
	if cfg.APIKey == "" {
		missing = append(missing, "APIKey")
	}
	if cfg.USDTTRC20Ccy == "" {
		missing = append(missing, "USDTTRC20Ccy")
	}
	if cfg.USDTBEP20Ccy == "" {
		missing = append(missing, "USDTBEP20Ccy")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("upstream: changenow: missing required config field(s): %s", strings.Join(missing, ", "))
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &ChangeNowProvider{cfg: cfg, http: httpClient, baseURL: changeNowBaseURL}, nil
}

func (p *ChangeNowProvider) overrideBaseURLForTest(url string) {
	p.baseURL = url
}

func (p *ChangeNowProvider) ccyCode(asset money.Asset) (string, error) {
	switch asset {
	case money.USDT_TRC20:
		return p.cfg.USDTTRC20Ccy, nil
	case money.USDT_BEP20:
		return p.cfg.USDTBEP20Ccy, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedAsset, string(asset))
	}
}

func (p *ChangeNowProvider) assetFromCcy(code string) (money.Asset, error) {
	switch code {
	case p.cfg.USDTTRC20Ccy:
		return money.USDT_TRC20, nil
	case p.cfg.USDTBEP20Ccy:
		return money.USDT_BEP20, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedAsset, code)
	}
}

// get performs a GET against p.baseURL+path and decodes a successful
// JSON response into out.
func (p *ChangeNowProvider) get(ctx context.Context, path string, out any) error {
	return p.do(ctx, http.MethodGet, path, nil, out)
}

func (p *ChangeNowProvider) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("upstream: changenow: encoding request body: %w", err)
		}
		reader = strings.NewReader(string(b))
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("upstream: changenow: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("upstream: changenow: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("upstream: changenow: reading response for %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope changeNowErrorEnvelope
		_ = json.Unmarshal(respBody, &envelope)
		return &APIError{Code: resp.StatusCode, Msg: envelope.errorMessage()}
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("upstream: changenow: %w: decoding response for %s %s: %v", ErrMalformedResponse, method, path, err)
		}
	}
	return nil
}

// changeNowErrorEnvelope is ChangeNOW's own error response shape --
// field names inferred from the same third-party client's own error
// handling, not independently confirmed; errorMessage falls back to a
// generic message rather than ever returning an empty one.
type changeNowErrorEnvelope struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (e changeNowErrorEnvelope) errorMessage() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Error != "" {
		return e.Error
	}
	return "changenow: unrecognized error response"
}

type changeNowEstimateResponse struct {
	EstimatedAmount float64 `json:"estimatedAmount"`
}

// Quote implements SwapProvider via GET /exchange-amount/{amount}/{from}_{to}.
func (p *ChangeNowProvider) Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error) {
	fromCcy, err := p.ccyCode(pair.From)
	if err != nil {
		return Quote{}, err
	}
	toCcy, err := p.ccyCode(pair.To)
	if err != nil {
		return Quote{}, err
	}
	amountStr, err := money.Format(amountIn)
	if err != nil {
		return Quote{}, fmt.Errorf("upstream: changenow: formatting quote amount: %w", err)
	}

	path := fmt.Sprintf("/exchange-amount/%s/%s_%s?api_key=%s",
		url.PathEscape(amountStr), url.PathEscape(fromCcy), url.PathEscape(toCcy), url.QueryEscape(p.cfg.APIKey))

	var resp changeNowEstimateResponse
	if err := p.get(ctx, path, &resp); err != nil {
		return Quote{}, err
	}
	if resp.EstimatedAmount <= 0 {
		return Quote{}, fmt.Errorf("upstream: changenow: %w: non-positive estimated amount %v", ErrMalformedResponse, resp.EstimatedAmount)
	}

	amountOut, err := amountFromFloat(resp.EstimatedAmount, pair.To)
	if err != nil {
		return Quote{}, fmt.Errorf("upstream: changenow: %w: converting estimated amount: %v", ErrMalformedResponse, err)
	}

	now := time.Now().UTC()
	return Quote{
		ProviderName: "changenow",
		Pair:         pair,
		AmountIn:     amountIn,
		AmountOut:    amountOut,
		QuotedAt:     now,
		ValidUntil:   now.Add(changeNowQuoteValidFor),
	}, nil
}

type changeNowCreateRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Address string `json:"address"`
	Amount  string `json:"amount"`
}

type changeNowCreateResponse struct {
	ID               string  `json:"id"`
	PayinAddress     string  `json:"payinAddress"`
	PayoutAddress    string  `json:"payoutAddress"`
	FromCurrency     string  `json:"fromCurrency"`
	ToCurrency       string  `json:"toCurrency"`
	AmountExpectedTo float64 `json:"amountExpectedTo"`
}

// CreateOrder implements SwapProvider via POST /transactions/{api_key}.
func (p *ChangeNowProvider) CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error) {
	fromCcy, err := p.ccyCode(pair.From)
	if err != nil {
		return SwapOrder{}, err
	}
	toCcy, err := p.ccyCode(pair.To)
	if err != nil {
		return SwapOrder{}, err
	}
	amountStr, err := money.Format(amountIn)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: changenow: formatting create-order amount: %w", err)
	}

	path := "/transactions/" + url.PathEscape(p.cfg.APIKey)
	var resp changeNowCreateResponse
	err = p.do(ctx, http.MethodPost, path, changeNowCreateRequest{
		From: fromCcy, To: toCcy, Address: destinationAddress, Amount: amountStr,
	}, &resp)
	if err != nil {
		return SwapOrder{}, err
	}
	if resp.ID == "" || resp.PayinAddress == "" {
		return SwapOrder{}, fmt.Errorf("upstream: changenow: %w: create-order response missing id or payin address", ErrMalformedResponse)
	}

	amountOutExpected, err := amountFromFloat(resp.AmountExpectedTo, pair.To)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: changenow: %w: converting expected payout: %v", ErrMalformedResponse, err)
	}

	return SwapOrder{
		ProviderName:       "changenow",
		ProviderOrderID:    resp.ID,
		DepositAddress:     resp.PayinAddress,
		DestinationAddress: destinationAddress,
		Status:             StatusAwaitingDeposit,
		AmountIn:           amountIn,
		AmountOutExpected:  amountOutExpected,
		AmountOutActual:    nil,
		CreatedAt:          time.Now().UTC(),
	}, nil
}

type changeNowStatusResponse struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	PayinAddress  string  `json:"payinAddress"`
	PayoutAddress string  `json:"payoutAddress"`
	FromCurrency  string  `json:"fromCurrency"`
	ToCurrency    string  `json:"toCurrency"`
	AmountSend    float64 `json:"amountSend"`
	AmountReceive float64 `json:"amountReceive"`
}

// statusFromChangeNow maps ChangeNOW's own status field onto SwapStatus.
// Values are reconstructed from their own public help-center articles
// (support.changenow.io), not the API docs directly (unreachable while
// building this) -- "waiting"/"new" both map to AwaitingDeposit since
// public sources use both interchangeably for the pre-deposit state;
// "verifying" (an occasional manual-review step some vendors insert)
// maps to Confirming, the closest existing SwapStatus for "still in
// progress, not yet actionable." "refunded" maps to Failed, the same
// posture FixedFloat's own EMERGENCY status uses -- settle.go's own
// UNRECOVERABLE escalation is the correct outcome either way. Verify
// this mapping against a real GET /transactions/{id}/{api_key}
// response before trusting it in production.
func statusFromChangeNow(status string) SwapStatus {
	switch status {
	case "new", "waiting":
		return StatusAwaitingDeposit
	case "confirming", "verifying":
		return StatusConfirming
	case "exchanging":
		return StatusExchanging
	case "sending":
		return StatusSending
	case "finished":
		return StatusComplete
	case "failed", "refunded":
		return StatusFailed
	default:
		return StatusFailed
	}
}

// GetOrder implements SwapProvider via GET /transactions/{id}/{api_key}.
// Unlike FixedFloat, ChangeNOW's own status check needs only the id --
// no second per-order security token -- so ProviderOrderID round-trips
// unpacked, the same simple opaque-string contract SwapProvider already
// assumes for the common case.
func (p *ChangeNowProvider) GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error) {
	path := fmt.Sprintf("/transactions/%s/%s", url.PathEscape(providerOrderID), url.PathEscape(p.cfg.APIKey))
	var resp changeNowStatusResponse
	if err := p.get(ctx, path, &resp); err != nil {
		return SwapOrder{}, err
	}

	fromAsset, err := p.assetFromCcy(resp.FromCurrency)
	if err != nil {
		return SwapOrder{}, err
	}
	toAsset, err := p.assetFromCcy(resp.ToCurrency)
	if err != nil {
		return SwapOrder{}, err
	}
	amountIn, err := amountFromFloat(resp.AmountSend, fromAsset)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: changenow: %w: converting amount_send: %v", ErrMalformedResponse, err)
	}
	amountOutExpected, err := amountFromFloat(resp.AmountReceive, toAsset)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: changenow: %w: converting amount_receive: %v", ErrMalformedResponse, err)
	}

	status := statusFromChangeNow(resp.Status)
	var amountOutActual *money.Amount
	if status == StatusComplete {
		amountOutActual = &amountOutExpected
	}

	return SwapOrder{
		ProviderName:       "changenow",
		ProviderOrderID:    resp.ID,
		DepositAddress:     resp.PayinAddress,
		DestinationAddress: resp.PayoutAddress,
		Status:             status,
		AmountIn:           amountIn,
		AmountOutExpected:  amountOutExpected,
		AmountOutActual:    amountOutActual,
		CreatedAt:          time.Now().UTC(),
	}, nil
}

// amountFromFloat converts a vendor's own JSON float amount into a
// money.Amount via its decimal string representation -- never via
// direct float math on Units -- so this goes through the exact same
// ParseDecimal precision/decimals enforcement every other amount in
// this module does, rather than accumulating float64 rounding error
// directly into an on-chain-shaped integer.
func amountFromFloat(f float64, asset money.Asset) (money.Amount, error) {
	decimals, err := asset.Decimals()
	if err != nil {
		return money.Amount{}, err
	}
	s := strconv.FormatFloat(f, 'f', decimals, 64)
	return money.ParseDecimal(s, asset)
}
