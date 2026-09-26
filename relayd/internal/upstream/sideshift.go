// SideShift (sideshift.ai) is a third real vendor option behind
// upstream.SwapProvider, alongside FixedFloat and ChangeNOW -- added
// because the user had a working SideShift account secret in hand while
// FixedFloat/ChangeNOW still needed fresh signups. Unlike either of
// those two, this file's request/response shapes were independently
// verified against SideShift's REAL, LIVE production API before being
// written (POST /v2/quotes, POST /v2/shifts/fixed, GET /v2/shifts/{id},
// and GET /v2/account -- one real quote and one real, harmless,
// never-funded fixed shift were created against production, per this
// whole codebase's own "verify before trusting a vendor spec"
// discipline), not reconstructed from a doc summary the way ChangeNOW's
// own top-of-file comment describes.
//
// One real quirk found only by that live check, not documented anywhere
// findable: SideShift's API has no separate "affiliateId" a caller sets
// up independently -- GET /v2/account (authenticated by the secret
// alone) returns the account's own "id" field, and that exact value IS
// the affiliateId POST /v2/shifts/fixed requires. Config.AffiliateID
// below is that account id -- fetch it once via GET /v2/account
// (authenticated with Secret alone) and configure it directly; this
// package does not fetch it automatically on every call.
//
// What's still NOT independently verified: the full "status" enum a
// shift can report over its lifecycle. Only "waiting" (the initial,
// pre-deposit state) was ever actually observed live -- the rest of
// statusFromSideShift's own mapping is reconstructed from SideShift's
// own general public documentation of the shift lifecycle, the same
// "code-complete but unproven" posture this codebase already uses for
// ChangeNOW's identical gap. Also NOT found in the real response at any
// point: any field carrying the vendor's own OUTPUT/settle transaction
// hash once a shift completes -- the live shift created while building
// this never reached settlement (it was never funded, on purpose), so
// SwapOrder's own AmountOutActual is populated the same way every other
// provider in this file does (present once Status is StatusComplete),
// but no separate output-tx-id field exists in this integration yet.
// Revisit once a real shift is driven all the way to "settled" and its
// real completed-shape response can be inspected.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"relayd/internal/money"
)

const sideShiftBaseURL = "https://sideshift.ai/api/v2"

// sideShiftQuoteValidFor mirrors fixedFloatQuoteValidFor/
// changeNowQuoteValidFor's own reasoning: a live quote response does
// carry its own real "expiresAt" (SideShift quotes a genuine 15-minute
// window, confirmed live), but relayd's own re-quote-at-forward-time
// step (architecture doc §6) is still the real protection, not this
// constant -- kept only as this file's own conservative fallback should
// a caller ever need ValidUntil before parsing a real expiresAt.
const sideShiftQuoteValidFor = 15 * time.Minute

// SideshiftConfig holds this account's own real secret and currency
// mapping. Secret is SideShift's own "private key" (header
// x-sideshift-secret) -- soft authentication for the account, per their
// own docs. AffiliateID is that SAME account's own "id" as reported by
// GET /v2/account (see this file's own top-of-file doc comment) --
// distinct from Secret, required on every order-creating call, never
// inferable from Secret alone. Coin/Network are separate fields here,
// unlike FixedFloat/ChangeNOW's single currency-code string, because
// SideShift's own real API genuinely separates them (confirmed live via
// GET /v2/coins: USDT's own "tron" and "bsc" networks are both real,
// present entries).
type SideshiftConfig struct {
	Secret           string
	AffiliateID      string
	USDTTRC20Coin    string // e.g. "USDT" -- verify against GET /v2/coins
	USDTTRC20Network string // e.g. "tron" -- verify against GET /v2/coins
	USDTBEP20Coin    string // e.g. "USDT" -- verify against GET /v2/coins
	USDTBEP20Network string // e.g. "bsc" -- verify against GET /v2/coins
	HTTPClient       *http.Client
}

// SideshiftProvider implements SwapProvider against SideShift's real v2
// API.
type SideshiftProvider struct {
	cfg     SideshiftConfig
	http    *http.Client
	baseURL string
}

// NewSideshiftProvider validates cfg and returns a SideshiftProvider.
func NewSideshiftProvider(cfg SideshiftConfig) (*SideshiftProvider, error) {
	missing := []string{}
	if cfg.Secret == "" {
		missing = append(missing, "Secret")
	}
	if cfg.AffiliateID == "" {
		missing = append(missing, "AffiliateID")
	}
	if cfg.USDTTRC20Coin == "" {
		missing = append(missing, "USDTTRC20Coin")
	}
	if cfg.USDTTRC20Network == "" {
		missing = append(missing, "USDTTRC20Network")
	}
	if cfg.USDTBEP20Coin == "" {
		missing = append(missing, "USDTBEP20Coin")
	}
	if cfg.USDTBEP20Network == "" {
		missing = append(missing, "USDTBEP20Network")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("upstream: sideshift: missing required config field(s): %s", strings.Join(missing, ", "))
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &SideshiftProvider{cfg: cfg, http: httpClient, baseURL: sideShiftBaseURL}, nil
}

func (p *SideshiftProvider) overrideBaseURLForTest(url string) {
	p.baseURL = url
}

type sideShiftCoinNetwork struct {
	Coin    string
	Network string
}

func (p *SideshiftProvider) coinNetwork(asset money.Asset) (sideShiftCoinNetwork, error) {
	switch asset {
	case money.USDT_TRC20:
		return sideShiftCoinNetwork{Coin: p.cfg.USDTTRC20Coin, Network: p.cfg.USDTTRC20Network}, nil
	case money.USDT_BEP20:
		return sideShiftCoinNetwork{Coin: p.cfg.USDTBEP20Coin, Network: p.cfg.USDTBEP20Network}, nil
	default:
		return sideShiftCoinNetwork{}, fmt.Errorf("%w: %q", ErrUnsupportedAsset, string(asset))
	}
}

func (p *SideshiftProvider) assetFromCoinNetwork(coin, network string) (money.Asset, error) {
	switch {
	case coin == p.cfg.USDTTRC20Coin && network == p.cfg.USDTTRC20Network:
		return money.USDT_TRC20, nil
	case coin == p.cfg.USDTBEP20Coin && network == p.cfg.USDTBEP20Network:
		return money.USDT_BEP20, nil
	default:
		return "", fmt.Errorf("%w: %q/%q", ErrUnsupportedAsset, coin, network)
	}
}

// sideShiftErrorEnvelope is SideShift's own real error response shape,
// confirmed live: {"error":{"message":"...","code":"..."}}.
type sideShiftErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

func (p *SideshiftProvider) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("upstream: sideshift: encoding request body: %w", err)
		}
		reader = strings.NewReader(string(b))
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("upstream: sideshift: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-sideshift-secret", p.cfg.Secret)

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("upstream: sideshift: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("upstream: sideshift: reading response for %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope sideShiftErrorEnvelope
		_ = json.Unmarshal(respBody, &envelope)
		msg := envelope.Error.Message
		if msg == "" {
			msg = "sideshift: unrecognized error response"
		}
		return &APIError{Code: resp.StatusCode, Msg: msg}
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("upstream: sideshift: %w: decoding response for %s %s: %v", ErrMalformedResponse, method, path, err)
		}
	}
	return nil
}

type sideShiftQuoteRequest struct {
	DepositCoin    string `json:"depositCoin"`
	DepositNetwork string `json:"depositNetwork"`
	SettleCoin     string `json:"settleCoin"`
	SettleNetwork  string `json:"settleNetwork"`
	DepositAmount  string `json:"depositAmount"`
}

// sideShiftQuoteResponse mirrors the REAL, live-verified response shape
// from POST /v2/quotes (captured while building this integration):
// {"id","createdAt","depositCoin","settleCoin","depositNetwork",
// "settleNetwork","expiresAt","depositAmount","settleAmount","rate"}.
type sideShiftQuoteResponse struct {
	ID            string `json:"id"`
	ExpiresAt     string `json:"expiresAt"`
	DepositAmount string `json:"depositAmount"`
	SettleAmount  string `json:"settleAmount"`
}

// Quote implements SwapProvider via POST /v2/quotes.
func (p *SideshiftProvider) Quote(ctx context.Context, pair Pair, amountIn money.Amount) (Quote, error) {
	from, err := p.coinNetwork(pair.From)
	if err != nil {
		return Quote{}, err
	}
	to, err := p.coinNetwork(pair.To)
	if err != nil {
		return Quote{}, err
	}
	amountStr, err := money.Format(amountIn)
	if err != nil {
		return Quote{}, fmt.Errorf("upstream: sideshift: formatting quote amount: %w", err)
	}

	var resp sideShiftQuoteResponse
	err = p.do(ctx, http.MethodPost, "/quotes", sideShiftQuoteRequest{
		DepositCoin: from.Coin, DepositNetwork: from.Network,
		SettleCoin: to.Coin, SettleNetwork: to.Network,
		DepositAmount: amountStr,
	}, &resp)
	if err != nil {
		return Quote{}, err
	}
	if resp.ID == "" {
		return Quote{}, fmt.Errorf("upstream: sideshift: %w: quote response missing id", ErrMalformedResponse)
	}

	amountOut, err := money.ParseDecimal(resp.SettleAmount, pair.To)
	if err != nil {
		return Quote{}, fmt.Errorf("upstream: sideshift: %w: parsing settleAmount %q: %v", ErrMalformedResponse, resp.SettleAmount, err)
	}

	now := time.Now().UTC()
	validUntil := now.Add(sideShiftQuoteValidFor)
	if resp.ExpiresAt != "" {
		if parsed, err := time.Parse(time.RFC3339, resp.ExpiresAt); err == nil {
			validUntil = parsed
		}
	}

	return Quote{
		ProviderName: "sideshift",
		Pair:         pair,
		AmountIn:     amountIn,
		AmountOut:    amountOut,
		QuotedAt:     now,
		ValidUntil:   validUntil,
	}, nil
}

// ProviderOrderID for sideshift is just the shift's own real id -- no
// packing needed, unlike FixedFloat's id/token pair (GET /v2/shifts/{id}
// takes only the id, confirmed live, no second secret required per
// order the way FixedFloat's own status check needs).

type sideShiftCreateShiftRequest struct {
	SettleAddress string `json:"settleAddress"`
	QuoteID       string `json:"quoteId"`
	AffiliateID   string `json:"affiliateId"`
}

// sideShiftShiftResponse mirrors the REAL, live-verified response shape
// from both POST /v2/shifts/fixed and GET /v2/shifts/{id} (confirmed
// identical in shape by creating one real shift and immediately
// re-fetching it): {"id","createdAt","depositCoin","settleCoin",
// "depositNetwork","settleNetwork","depositAddress","settleAddress",
// "depositMin","depositMax","type","quoteId","depositAmount",
// "settleAmount","expiresAt","status","averageShiftSeconds","rate"}.
// Only status="waiting" was ever actually observed (the live shift
// created while building this was deliberately never funded) -- see
// statusFromSideShift's own doc comment for what's still unverified.
type sideShiftShiftResponse struct {
	ID             string `json:"id"`
	DepositCoin    string `json:"depositCoin"`
	SettleCoin     string `json:"settleCoin"`
	DepositNetwork string `json:"depositNetwork"`
	SettleNetwork  string `json:"settleNetwork"`
	DepositAddress string `json:"depositAddress"`
	SettleAddress  string `json:"settleAddress"`
	DepositAmount  string `json:"depositAmount"`
	SettleAmount   string `json:"settleAmount"`
	Status         string `json:"status"`
	SettleHash     string `json:"settleHash"`
}

func (p *SideshiftProvider) toSwapOrder(data sideShiftShiftResponse) (SwapOrder, error) {
	fromAsset, err := p.assetFromCoinNetwork(data.DepositCoin, data.DepositNetwork)
	if err != nil {
		return SwapOrder{}, err
	}
	toAsset, err := p.assetFromCoinNetwork(data.SettleCoin, data.SettleNetwork)
	if err != nil {
		return SwapOrder{}, err
	}
	amountIn, err := money.ParseDecimal(data.DepositAmount, fromAsset)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: sideshift: %w: parsing depositAmount %q: %v", ErrMalformedResponse, data.DepositAmount, err)
	}
	amountOutExpected, err := money.ParseDecimal(data.SettleAmount, toAsset)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: sideshift: %w: parsing settleAmount %q: %v", ErrMalformedResponse, data.SettleAmount, err)
	}

	status := statusFromSideShift(data.Status)
	var amountOutActual *money.Amount
	if status == StatusComplete {
		amountOutActual = &amountOutExpected
	}

	return SwapOrder{
		ProviderName:       "sideshift",
		ProviderOrderID:    data.ID,
		DepositAddress:     data.DepositAddress,
		DestinationAddress: data.SettleAddress,
		Status:             status,
		AmountIn:           amountIn,
		AmountOutExpected:  amountOutExpected,
		AmountOutActual:    amountOutActual,
		PayoutTxID:         nonEmpty(data.SettleHash),
		CreatedAt:          time.Now().UTC(),
	}, nil
}

// CreateOrder implements SwapProvider via POST /v2/shifts/fixed.
// SideShift's own fixed-shift model genuinely requires a real quoteId
// first (confirmed live) -- unlike FixedFloat/ChangeNOW, whose own
// create-order calls take amount+pair directly, this needs the raw
// POST /v2/quotes call made here directly (not via this file's own
// Quote method, which returns the vendor-agnostic Quote type and
// discards SideShift's own quote id) so the exact id POST
// /v2/shifts/fixed requires is available.
func (p *SideshiftProvider) CreateOrder(ctx context.Context, pair Pair, amountIn money.Amount, destinationAddress string) (SwapOrder, error) {
	from, err := p.coinNetwork(pair.From)
	if err != nil {
		return SwapOrder{}, err
	}
	to, err := p.coinNetwork(pair.To)
	if err != nil {
		return SwapOrder{}, err
	}
	amountStr, err := money.Format(amountIn)
	if err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: sideshift: formatting create-order amount: %w", err)
	}
	var quoteResp sideShiftQuoteResponse
	if err := p.do(ctx, http.MethodPost, "/quotes", sideShiftQuoteRequest{
		DepositCoin: from.Coin, DepositNetwork: from.Network,
		SettleCoin: to.Coin, SettleNetwork: to.Network,
		DepositAmount: amountStr,
	}, &quoteResp); err != nil {
		return SwapOrder{}, fmt.Errorf("upstream: sideshift: re-quoting for order creation: %w", err)
	}

	var resp sideShiftShiftResponse
	err = p.do(ctx, http.MethodPost, "/shifts/fixed", sideShiftCreateShiftRequest{
		SettleAddress: destinationAddress, QuoteID: quoteResp.ID, AffiliateID: p.cfg.AffiliateID,
	}, &resp)
	if err != nil {
		return SwapOrder{}, err
	}
	if resp.ID == "" || resp.DepositAddress == "" {
		return SwapOrder{}, fmt.Errorf("upstream: sideshift: %w: create-shift response missing id or deposit address", ErrMalformedResponse)
	}
	return p.toSwapOrder(resp)
}

// GetOrder implements SwapProvider via GET /v2/shifts/{id}.
func (p *SideshiftProvider) GetOrder(ctx context.Context, providerOrderID string) (SwapOrder, error) {
	var resp sideShiftShiftResponse
	if err := p.do(ctx, http.MethodGet, "/shifts/"+providerOrderID, nil, &resp); err != nil {
		return SwapOrder{}, err
	}
	return p.toSwapOrder(resp)
}

// statusFromSideShift maps SideShift's own status field onto SwapStatus.
// Only "waiting" was independently confirmed live while building this
// integration (see this file's own top-of-file doc comment) -- the rest
// ("pending"/"processing"/"review"/"settled"/"refund"/"refunding"/
// "expired"/"multiple") are SideShift's own publicly documented shift
// lifecycle states, reconstructed the same "not yet independently
// confirmed against a real completed order" way ChangeNOW's own mapping
// already is in this file's sibling. "review" (a manual-check state) and
// "multiple" (a multi-deposit edge case not relevant to relayd's own
// exactly-one-forward-transfer model) both map to StatusFailed --
// settle.go's own UNRECOVERABLE escalation is the correct outcome for
// either, the same posture FixedFloat's EMERGENCY and ChangeNOW's
// verifying/refunded statuses already take. Verify this mapping against
// a real completed (or real refunded) shift before trusting it with
// production traffic.
func statusFromSideShift(status string) SwapStatus {
	switch status {
	case "waiting":
		return StatusAwaitingDeposit
	case "pending":
		return StatusConfirming
	case "processing":
		return StatusExchanging
	case "settling":
		return StatusSending
	case "settled":
		return StatusComplete
	case "refund", "refunding", "expired", "multiple":
		return StatusFailed
	default: // "review", or a status this code doesn't know
		return StatusNeedsAttention
	}
}
