// R2's own decision record (docs/01-strategy/model-f-relay-findings.md)
// names FixedFloat (ff.io) as the first real upstream vendor. This file
// is R4: the real SwapProvider implementation against FixedFloat's own
// API v2 (https://ff.io/en/api), replacing PlaceholderProvider for any
// deployment with UPSTREAM_PROVIDER=fixedfloat set.
//
// Every call uses their "fixed" order type, not "float" -- Quote's own
// ValidUntil field (upstream.go's own doc comment: "the PROVIDER's own
// lock window") only makes sense against a rate FixedFloat itself has
// committed to, and architecture doc §6's own "shorten the lock" /
// "re-quote at forward-time" mitigations both assume a real lock exists
// to shorten, not an indicative float that moves under us regardless.
//
// One real API-shape wrinkle this file works around: FixedFloat's own
// GetOrder equivalent (POST /order) needs BOTH the 6-character order id
// AND a separate per-order security token neither of which alone is
// enough to look up status -- but upstream.SwapProvider.GetOrder takes
// one opaque providerOrderID string, matching every other call site in
// this module (settle.go's own advanceForwardedLeg does
// o.Upstream.GetOrder(ctx, *leg.UpstreamOrderID), never anything else).
// Changing that interface would ripple through relay.Leg's own
// persisted UpstreamOrderID column and every orchestrate/replay call
// site for a problem only this one vendor has. Instead, CreateOrder
// packs "id|token" into the single ProviderOrderID string it returns
// (relay_legs.upstream_order_id is an unbounded text column -- see
// migrations/0002_relay_legs.sql -- so this costs nothing), and GetOrder
// splits it back apart. See packOrderRef/unpackOrderRef below.
package upstream

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"relayd/internal/money"
)

const fixedFloatBaseURL = "https://ff.io/api/v2"

// ErrUnsupportedAsset is returned for any money.Asset this provider has
// no FixedFloat currency-code mapping for -- TRX/BNB (relayd's own gas
// assets, never a swap leg) are the expected case, not a bug.
var ErrUnsupportedAsset = errors.New("upstream: fixedfloat: unsupported asset")

// fixedFloatQuoteValidFor is this file's own conservative interpretation
// of how long a Quote() preview should be trusted before re-checking --
// FixedFloat's own POST /price response (unlike POST /create) carries no
// expiration field of its own, since no order exists yet to expire. The
// real protection is architecture doc §6's own re-quote-at-forward-time
// step, not this window; this only bounds how stale a customer-facing
// quote (driver.go's own postRelayLeg) is allowed to look before that
// happens.
const fixedFloatQuoteValidFor = 60 * time.Second

// FixedFloatConfig holds this account's own real credentials and
// affiliate settings. Deliberately has no defaults for APIKey/APISecret/
// USDTTRC20Code/USDTBEP20Code -- the same "no hardcoded defaults for
// anything real-money-shaped" discipline every other credential in this
// repo follows (see cmd/relayd's own upstreamProviderFromEnv). The two
// currency-code fields exist as config, not constants, because they were
// never independently confirmed against a real GET /api/v2/ccies
// response while building this integration (only inferred from public
// FixedFloat pages and a third-party vendor's own matching convention)
// -- whoever wires UPSTREAM_PROVIDER=fixedfloat for real must verify
// them against that endpoint first and set them explicitly, rather than
// this file silently guessing a ticker for money that's about to move.
type FixedFloatConfig struct {
	APIKey       string
	APISecret    string
	RefCode      string // affiliate/referral code tied to this account's own commission rate; optional
	USDTTRC20Ccy string // FixedFloat's own currency code for USDT on TRC20, e.g. "USDTTRC" -- verify against GET /api/v2/ccies
	USDTBEP20Ccy string // FixedFloat's own currency code for USDT on BEP20/BSC, e.g. "USDTBSC" -- verify against GET /api/v2/ccies
	HTTPClient   *http.Client
}

// FixedFloatProvider implements SwapProvider against the real FixedFloat
// API v2.
type FixedFloatProvider struct {
	cfg     FixedFloatConfig
	http    *http.Client
	baseURL string
}

// NewFixedFloatProvider validates cfg and returns a FixedFloatProvider.
// Fails loud on any missing field rather than falling back to a zero
// value that would silently sign requests with an empty secret or quote
// against an empty currency code.
func NewFixedFloatProvider(cfg FixedFloatConfig) (*FixedFloatProvider, error) {
	missing := []string{}
	if cfg.APIKey == "" {
		missing = append(missing, "APIKey")
	}
	if cfg.APISecret == "" {
		missing = append(missing, "APISecret")
	}
	if cfg.USDTTRC20Ccy == "" {
		missing = append(missing, "USDTTRC20Ccy")
	}
	if cfg.USDTBEP20Ccy == "" {
		missing = append(missing, "USDTBEP20Ccy")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("upstream: fixedfloat: missing required config field(s): %s", strings.Join(missing, ", "))
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &FixedFloatProvider{cfg: cfg, http: httpClient, baseURL: fixedFloatBaseURL}, nil
}

// overrideBaseURLForTest points this provider at a local httptest server
// instead of the real ff.io -- test-only, never called from production
// wiring (cmd/relayd's own upstreamProviderFromEnv always leaves
// baseURL at its NewFixedFloatProvider default).
func (p *FixedFloatProvider) overrideBaseURLForTest(url string) {
	p.baseURL = url
}

// ccyCode maps relayd's own money.Asset to FixedFloat's own currency
// code. Only the two assets Pair ever actually carries (USDT_TRC20,
// USDT_BEP20) are supported -- ErrUnsupportedAsset otherwise, never a
// guessed/empty ticker sent to a real vendor.
func (p *FixedFloatProvider) ccyCode(asset money.Asset) (string, error) {
	switch asset {
	case money.USDT_TRC20:
		return p.cfg.USDTTRC20Ccy, nil
	case money.USDT_BEP20:
		return p.cfg.USDTBEP20Ccy, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedAsset, string(asset))
	}
}

// assetFromCcy is ccyCode's own inverse, used to interpret FixedFloat's
// own response currency codes back into a money.Asset.
func (p *FixedFloatProvider) assetFromCcy(code string) (money.Asset, error) {
	switch code {
	case p.cfg.USDTTRC20Ccy:
		return money.USDT_TRC20, nil
	case p.cfg.USDTBEP20Ccy:
		return money.USDT_BEP20, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedAsset, code)
	}
}

// packOrderRef/unpackOrderRef: see this file's own top-of-file doc
// comment for why FixedFloat's real API needs both an id and a security
// token to check status, and why that's packed into SwapProvider's own
// single opaque ProviderOrderID string rather than changing the
// interface. "|" is FixedFloat's own documented id/token alphabet-safe
// separator: both id and token are alphanumeric, per their own API
// examples, so this never collides with real values.
func packOrderRef(id, token string) string {
	return id + "|" + token
}

func unpackOrderRef(ref string) (id, token string, err error) {
	parts := strings.SplitN(ref, "|", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("upstream: fixedfloat: malformed provider order ref %q", ref)
	}
	return parts[0], parts[1], nil
}

// sign computes FixedFloat's own required X-API-SIGN header: HMAC-SHA256
// over the exact JSON body bytes sent, keyed by the account's own API
// secret, hex-encoded. Per their own API docs: an empty body signs the
// empty string.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ffErrorBody is FixedFloat's own envelope for every response, success
// or failure: code 0 means success, any other code is an error with msg
// carrying the human-readable reason.
type ffEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// APIError is a structured error response from FixedFloat.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("upstream: fixedfloat returned code %d: %s", e.Code, e.Msg)
}

// do posts body (marshaled to JSON) to FixedFloat's own path, signs it,
// and unmarshals a successful envelope's data into out. Never retries --
// callers (Quote/CreateOrder/GetOrder) are themselves retried by
// relayd's own orchestrate loop's per-tick, per-leg isolation, the same
// "outer loop retries, inner client doesn't" discipline
// dispatcher/internal/energy's own Client follows.
func (p *FixedFloatProvider) do(ctx context.Context, path string, body any, out any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("upstream: fixedfloat: encoding request body: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("upstream: fixedfloat: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-API-KEY", p.cfg.APIKey)
	req.Header.Set("X-API-SIGN", sign(p.cfg.APISecret, payload))

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("upstream: fixedfloat: POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("upstream: fixedfloat: reading response for POST %s: %w", path, err)
	}

	var envelope ffEnvelope
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("upstream: fixedfloat: %w: decoding envelope for POST %s: %v", ErrMalformedResponse, path, err)
	}
	if envelope.Code != 0 {
		return &APIError{Code: envelope.Code, Msg: envelope.Msg}
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("upstream: fixedfloat: %w: decoding data for POST %s: %v", ErrMalformedResponse, path, err)
		}
	}
	return nil
}

type ffPriceRequest struct {
	Type      string `json:"type"`
	FromCcy   string `json:"fromCcy"`
	ToCcy     string `json:"toCcy"`
	Direction string `json:"direction"`
	Amount    string `json:"amount"`
	RefCode   string `json:"refcode,omitempty"`
}

type ffPriceSide struct {
	Code   string `json:"code"`
	Amount string `json:"amount"`
}

type ffPriceData struct {
	From   ffPriceSide `json:"from"`
	To     ffPriceSide `json:"to"`
	Errors []string    `json:"errors"`
}

// Quote implements SwapProvider via POST /api/v2/price.
func (p *FixedFloatProvider) Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error) {
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
		return Quote{}, fmt.Errorf("upstream: fixedfloat: formatting quote amount: %w", err)
	}

	var data ffPriceData
	err = p.do(ctx, "/price", ffPriceRequest{
		Type: "fixed", FromCcy: fromCcy, ToCcy: toCcy, Direction: "from", Amount: amountStr, RefCode: p.cfg.RefCode,
	}, &data)
	if err != nil {
		return Quote{}, err
	}
	if len(data.Errors) > 0 {
		return Quote{}, fmt.Errorf("upstream: fixedfloat: price unavailable for pair: %s", strings.Join(data.Errors, ", "))
	}

	amountOut, err := money.ParseDecimal(data.To.Amount, pair.To)
	if err != nil {
		return Quote{}, fmt.Errorf("upstream: fixedfloat: %w: parsing quoted amount %q: %v", ErrMalformedResponse, data.To.Amount, err)
	}

	now := time.Now().UTC()
	return Quote{
		ProviderName: "fixedfloat",
		Pair:         pair,
		AmountIn:     amountIn,
		AmountOut:    amountOut,
		QuotedAt:     now,
		ValidUntil:   now.Add(fixedFloatQuoteValidFor),
	}, nil
}

type ffCreateRequest struct {
	Type      string `json:"type"`
	FromCcy   string `json:"fromCcy"`
	ToCcy     string `json:"toCcy"`
	Direction string `json:"direction"`
	Amount    string `json:"amount"`
	ToAddress string `json:"toAddress"`
	RefCode   string `json:"refcode,omitempty"`
}

type ffOrderSide struct {
	Code    string `json:"code"`
	Address string `json:"address"`
	Amount  string `json:"amount"`
}

type ffOrderData struct {
	ID     string      `json:"id"`
	Token  string      `json:"token"`
	Type   string      `json:"type"`
	Status string      `json:"status"`
	From   ffOrderSide `json:"from"`
	To     ffOrderSide `json:"to"`
}

// statusFromFixedFloat maps FixedFloat's own order.status values onto
// SwapStatus. Their own lifecycle (NEW -> PENDING -> EXCHANGE -> WITHDRAW
// -> DONE, or EXPIRED/EMERGENCY) is finer-grained in the middle than
// relayd needs -- everything from PENDING (deposit seen, unconfirmed)
// through WITHDRAW (their own payout broadcasting) is, from relayd's own
// perspective, just "the vendor has it and hasn't finished or failed
// yet," the same abstraction MockProvider's own StatusExchanging/
// StatusSending already collapse to for a fake vendor. EMERGENCY (the
// vendor's own manual-intervention state, e.g. amount mismatch) maps to
// StatusFailed rather than a new SwapStatus value -- settle.go's own
// advanceForwardedLeg already treats StatusFailed as "escalate to
// UNRECOVERABLE," the correct outcome for a forward transfer FixedFloat
// itself flagged as needing human review.
func statusFromFixedFloat(status string) SwapStatus {
	switch status {
	case "NEW":
		return StatusAwaitingDeposit
	case "PENDING":
		return StatusConfirming
	case "EXCHANGE":
		return StatusExchanging
	case "WITHDRAW":
		return StatusSending
	case "DONE":
		return StatusComplete
	case "EXPIRED":
		return StatusExpired
	case "EMERGENCY":
		return StatusFailed
	default:
		return StatusFailed
	}
}

func (p *FixedFloatProvider) toSwapOrder(data ffOrderData, destinationAddress string) (SwapOrder, error) {
	fromAsset, err := p.assetFromCcy(data.From.Code)
	if err != nil {
		return SwapOrder{}, err
	}
	toAsset, err := p.assetFromCcy(data.To.Code)
	if err != nil {
		return SwapOrder{}, err
	}
	amountIn, err := money.ParseDecimal(data.From.Amount, fromAsset)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: fixedfloat: %w: parsing order amount_in %q: %v", ErrMalformedResponse, data.From.Amount, err)
	}
	amountOutExpected, err := money.ParseDecimal(data.To.Amount, toAsset)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: fixedfloat: %w: parsing order amount_out %q: %v", ErrMalformedResponse, data.To.Amount, err)
	}

	status := statusFromFixedFloat(data.Status)
	var amountOutActual *money.Amount
	if status == StatusComplete {
		amountOutActual = &amountOutExpected
	}

	return SwapOrder{
		ProviderName:       "fixedfloat",
		ProviderOrderID:    packOrderRef(data.ID, data.Token),
		DepositAddress:     data.From.Address,
		DestinationAddress: destinationAddress,
		Status:             status,
		AmountIn:           amountIn,
		AmountOutExpected:  amountOutExpected,
		AmountOutActual:    amountOutActual,
		CreatedAt:          time.Now().UTC(),
	}, nil
}

// CreateOrder implements SwapProvider via POST /api/v2/create.
func (p *FixedFloatProvider) CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error) {
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
		return SwapOrder{}, fmt.Errorf("upstream: fixedfloat: formatting create-order amount: %w", err)
	}

	var data ffOrderData
	err = p.do(ctx, "/create", ffCreateRequest{
		Type: "fixed", FromCcy: fromCcy, ToCcy: toCcy, Direction: "from", Amount: amountStr,
		ToAddress: destinationAddress, RefCode: p.cfg.RefCode,
	}, &data)
	if err != nil {
		return SwapOrder{}, err
	}
	return p.toSwapOrder(data, destinationAddress)
}

type ffStatusRequest struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// GetOrder implements SwapProvider via POST /api/v2/order. providerOrderID
// must be a value this same provider's own CreateOrder returned (see
// this file's own top-of-file doc comment on packOrderRef) -- any other
// string fails with a malformed-ref error, never silently queries
// FixedFloat with a garbage id/token pair.
func (p *FixedFloatProvider) GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error) {
	id, token, err := unpackOrderRef(providerOrderID)
	if err != nil {
		return SwapOrder{}, err
	}

	var data ffOrderData
	if err := p.do(ctx, "/order", ffStatusRequest{ID: id, Token: token}, &data); err != nil {
		return SwapOrder{}, err
	}
	// GetOrder has no destinationAddress of its own to echo back --
	// relayd's own caller (settle.go) already knows it from the leg row
	// and never reads SwapOrder.DestinationAddress off a GetOrder result,
	// only off the original CreateOrder result (see relay.Leg's own
	// DestinationAddress field, set once at leg creation).
	return p.toSwapOrder(data, "")
}
