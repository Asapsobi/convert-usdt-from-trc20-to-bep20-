package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ledgerFixture drives a real, already-running C1 as a test fixture --
// the error-returning mirror of relayd/internal/testledger (the
// *testing.T-bound helper this package can't use from cmd/replay/main.go,
// an ordinary binary, not a go test), same posture as
// dispatcher/internal/replay/ledgerfixture.go and every sibling
// component's own identical file.
type ledgerFixture struct {
	baseURL string
	token   string
	http    *http.Client
	pool    *pgxpool.Pool
}

func newLedgerFixture(baseURL, token string, pool *pgxpool.Pool) *ledgerFixture {
	return &ledgerFixture{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}, pool: pool}
}

func (f *ledgerFixture) do(ctx context.Context, method, path, idempotencyKey string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

func (f *ledgerFixture) waitHealthy(ctx context.Context, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/healthz", nil)
		resp, err := f.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("ledger fixture: %s never became healthy within %s", f.baseURL, deadline)
}

// fixtureOrder is the subset of C1's order resource this fixture decodes.
type fixtureOrder struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
	CustomerID string `json:"customer_id"`
	State      string `json:"state"`
	Version    int32  `json:"version"`
}

// createRelayOrder posts POST /v1/orders with tier=RELAY -- mirroring
// internal/testledger.CreateRelayOrder/CreateRelayOrderBEP20ToTRC20
// exactly (same fixed amounts: 100 in, 99.7 out, 0.3 fee), parametrized
// by direction since a replay scenario needs both.
func (f *ledgerFixture) createRelayOrder(ctx context.Context, externalID, customerID string, trc20ToBEP20 bool) (fixtureOrder, error) {
	now := time.Now().UTC()
	body := map[string]any{
		"external_id": externalID, "customer_id": customerID, "tier": "RELAY",
		"amount_in": "100.000000", "amount_out": "99.700000", "fee_units": "0.300000", "network_fee_units": "0.000000",
		"quoted_at": now, "quote_expires_at": now.Add(10 * time.Minute),
	}
	if trc20ToBEP20 {
		body["recipient_address"] = "0x00000000000000000000000000000000000001"
		body["amount_in_asset"] = "USDT_TRC20"
		body["amount_out_asset"] = "USDT_BEP20"
	} else {
		body["recipient_address"] = "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj"
		body["amount_in_asset"] = "USDT_BEP20"
		body["amount_out_asset"] = "USDT_TRC20"
	}

	status, respBody, err := f.do(ctx, http.MethodPost, "/v1/orders", "replay:create:"+externalID, body)
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusCreated {
		return fixtureOrder{}, fmt.Errorf("POST /v1/orders for %s: status %d: %s", externalID, status, respBody)
	}
	var o fixtureOrder
	if err := json.Unmarshal(respBody, &o); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding order response: %w: %s", err, respBody)
	}
	return o, nil
}

// advanceToScreened walks a freshly created RELAY order through funded
// and into screened, mirroring internal/testledger.AdvanceToScreened/
// AdvanceToScreenedWithSender/AdvanceToScreenedBEP20ToTRC20 -- one
// function, parametrized, instead of three, since this fixture has no
// *testing.T to attach three near-identical methods to. senderAddress
// may be empty (no sender_address recorded -- fine for scenarios that
// never need to refund).
func (f *ledgerFixture) advanceToScreened(ctx context.Context, order fixtureOrder, inAsset, senderAddress string) (fixtureOrder, error) {
	relayLegAccount := "asset:relay:leg:" + strconv.FormatInt(order.ID, 10)
	liability := "liability:customer:" + order.CustomerID + ":" + inAsset
	if err := f.createAccount(ctx, relayLegAccount, "ASSET", inAsset); err != nil {
		return fixtureOrder{}, err
	}
	if err := f.createAccount(ctx, liability, "LIABILITY", inAsset); err != nil {
		return fixtureOrder{}, err
	}

	now := time.Now().UTC()
	fundBody := map[string]any{
		"to_state": "funded", "expected_version": order.Version, "reason": "replay_deposit_final", "occurred_at": now,
		"entry": map[string]any{
			"entry_type": "deposit_final", "occurred_at": now,
			"lines": []map[string]any{
				{"account_code": relayLegAccount, "asset": inAsset, "amount": "100.000000"},
				{"account_code": liability, "asset": inAsset, "amount": "-100.000000"},
			},
		},
	}
	if senderAddress != "" {
		fundBody["sender_address"] = senderAddress
	}
	status, respBody, err := f.do(ctx, http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "replay:fund:"+order.ExternalID, fundBody)
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("funding %s: status %d: %s", order.ExternalID, status, respBody)
	}
	var funded fixtureOrder
	if err := json.Unmarshal(respBody, &funded); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding funded order response: %w: %s", err, respBody)
	}

	status, respBody, err = f.do(ctx, http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "replay:screen:"+order.ExternalID, map[string]any{
		"to_state": "screened", "expected_version": funded.Version, "reason": "screening_pass", "occurred_at": now,
	})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("screening %s: status %d: %s", order.ExternalID, status, respBody)
	}
	var screened fixtureOrder
	if err := json.Unmarshal(respBody, &screened); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding screened order response: %w: %s", err, respBody)
	}
	return screened, nil
}

// advanceToHeld walks a freshly created RELAY order through funded and
// into held -- mirroring what C3's own automatic Hold verdict does
// (funded->held, RequiresEntry: false), the starting point for the
// manual-reject scenario (scenarioManuallyRejectedHoldGetsRefunded).
func (f *ledgerFixture) advanceToHeld(ctx context.Context, order fixtureOrder, inAsset, senderAddress string) (fixtureOrder, error) {
	relayLegAccount := "asset:relay:leg:" + strconv.FormatInt(order.ID, 10)
	liability := "liability:customer:" + order.CustomerID + ":" + inAsset
	if err := f.createAccount(ctx, relayLegAccount, "ASSET", inAsset); err != nil {
		return fixtureOrder{}, err
	}
	if err := f.createAccount(ctx, liability, "LIABILITY", inAsset); err != nil {
		return fixtureOrder{}, err
	}

	now := time.Now().UTC()
	fundBody := map[string]any{
		"to_state": "funded", "expected_version": order.Version, "reason": "replay_deposit_final", "occurred_at": now,
		"entry": map[string]any{
			"entry_type": "deposit_final", "occurred_at": now,
			"lines": []map[string]any{
				{"account_code": relayLegAccount, "asset": inAsset, "amount": "100.000000"},
				{"account_code": liability, "asset": inAsset, "amount": "-100.000000"},
			},
		},
	}
	if senderAddress != "" {
		fundBody["sender_address"] = senderAddress
	}
	status, respBody, err := f.do(ctx, http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "replay:fund:"+order.ExternalID, fundBody)
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("funding %s: status %d: %s", order.ExternalID, status, respBody)
	}
	var funded fixtureOrder
	if err := json.Unmarshal(respBody, &funded); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding funded order response: %w: %s", err, respBody)
	}

	status, respBody, err = f.do(ctx, http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "replay:hold:"+order.ExternalID, map[string]any{
		"to_state": "held", "expected_version": funded.Version, "reason": "screening_hold_flagged", "occurred_at": now,
	})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("holding %s: status %d: %s", order.ExternalID, status, respBody)
	}
	var held fixtureOrder
	if err := json.Unmarshal(respBody, &held); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding held order response: %w: %s", err, respBody)
	}
	return held, nil
}

// rejectHeld walks a held RELAY order into refunded, posting entry as
// C1's own "entry" transition field -- mirroring exactly what
// screening/internal/holds.go's own Reject does over HTTP in a real
// deployment.
func (f *ledgerFixture) rejectHeld(ctx context.Context, order fixtureOrder, entry map[string]any) (fixtureOrder, error) {
	status, respBody, err := f.do(ctx, http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "replay:reject:"+order.ExternalID, map[string]any{
		"to_state": "refunded", "expected_version": order.Version, "reason": "manual_reject", "occurred_at": time.Now().UTC(),
		"entry": entry,
	})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("rejecting held order %s: status %d: %s", order.ExternalID, status, respBody)
	}
	var refunded fixtureOrder
	if err := json.Unmarshal(respBody, &refunded); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding refunded order response: %w: %s", err, respBody)
	}
	return refunded, nil
}

func (f *ledgerFixture) createAccount(ctx context.Context, code, accountType, asset string) error {
	normalSide := 1
	if accountType == "LIABILITY" || accountType == "REVENUE" || accountType == "EQUITY" {
		normalSide = -1
	}
	_, err := f.pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	return err
}

// accountBalance sums journal_lines.amount_units for the account named
// code -- 0 (not an error) for an account that was never opened, the
// correct answer for FINAL ASSERTION purposes ("this account nets to
// zero" is trivially true of an account that never existed).
func (f *ledgerFixture) accountBalance(ctx context.Context, code string) (int64, error) {
	var balance int64
	err := f.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(jl.amount_units), 0)
		FROM journal_lines jl
		JOIN accounts a ON a.id = jl.account_id
		WHERE a.code = $1
	`, code).Scan(&balance)
	return balance, err
}

func (f *ledgerFixture) getOrderState(ctx context.Context, externalID string) (string, error) {
	var state string
	err := f.pool.QueryRow(ctx, `SELECT state FROM orders WHERE external_id = $1`, externalID).Scan(&state)
	return state, err
}
