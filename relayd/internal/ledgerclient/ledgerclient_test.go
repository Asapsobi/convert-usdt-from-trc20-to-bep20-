package ledgerclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"relayd/internal/money"
)

func TestCreateOrder_SendsExplicitAssetsAndParsesResponse(t *testing.T) {
	var gotReq postOrderRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/orders" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(orderResponse{
			ID: 1, ExternalID: "ext-1", CustomerID: "cust-1", Tier: "RELAY", State: "quoted",
			AmountIn: "100.000000", AmountOut: "99.700000", FeeUnits: "0.300000", NetworkFeeUnits: "0.000000",
			RecipientAddress: "0xdest", Version: 0,
			AmountInAsset: "USDT_TRC20", AmountOutAsset: "USDT_BEP20",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	amountIn := money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}
	amountOut := money.Amount{Asset: money.USDT_BEP20, Units: 99_700000}
	fee := money.Amount{Asset: money.USDT_BEP20, Units: 300000}
	networkFee := money.Amount{Asset: money.USDT_BEP20, Units: 0}
	now := time.Now().UTC()

	order, err := c.CreateOrder(context.Background(), "ext-1", "cust-1", amountIn, amountOut, fee, networkFee,
		"0xdest", now, now.Add(90*time.Second), "idem-1")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if gotReq.Tier != "RELAY" {
		t.Errorf("expected tier=RELAY, got %q", gotReq.Tier)
	}
	if gotReq.AmountInAsset != "USDT_TRC20" || gotReq.AmountOutAsset != "USDT_BEP20" {
		t.Errorf("expected explicit asset fields, got in=%q out=%q", gotReq.AmountInAsset, gotReq.AmountOutAsset)
	}
	if order.AmountIn.Asset != money.USDT_TRC20 || order.AmountOut.Asset != money.USDT_BEP20 {
		t.Errorf("expected parsed order to preserve the asset pairing, got in=%s out=%s", order.AmountIn.Asset, order.AmountOut.Asset)
	}
	if order.AmountIn.Units != 100_000000 {
		t.Errorf("expected amount_in 100_000000, got %d", order.AmountIn.Units)
	}
}

func TestGetOrder_ParsesMixedAssetPairCorrectly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(orderResponse{
			ID: 1, ExternalID: "ext-1", Tier: "RELAY", State: "screened",
			AmountIn: "50.000000", AmountOut: "49.850000", FeeUnits: "0.150000", NetworkFeeUnits: "0.000000",
			AmountInAsset: "USDT_BEP20", AmountOutAsset: "USDT_TRC20",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	order, err := c.GetOrder(context.Background(), "ext-1")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.AmountIn.Asset != money.USDT_BEP20 {
		t.Errorf("expected amount_in asset USDT_BEP20, got %s", order.AmountIn.Asset)
	}
	if order.AmountOut.Asset != money.USDT_TRC20 {
		t.Errorf("expected amount_out asset USDT_TRC20, got %s", order.AmountOut.Asset)
	}
	if order.FeeUnits.Asset != money.USDT_BEP20 {
		t.Errorf("expected fee_units asset to match amount_in (USDT_BEP20) for a RELAY order, got %s", order.FeeUnits.Asset)
	}
}

func TestGetOrder_NotFoundClassifiesToSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "order_not_found"}})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	_, err := c.GetOrder(context.Background(), "ext-missing")
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestEnsureAccount_SendsCorrectPayload(t *testing.T) {
	var got postAccountRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	if err := c.EnsureAccount(context.Background(), "asset:relay:leg:1", AccountAsset, "USDT_TRC20", "idem-key"); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	if got.Code != "asset:relay:leg:1" || got.Type != "ASSET" || got.Asset != "USDT_TRC20" {
		t.Errorf("unexpected request: %+v", got)
	}
}

func TestListOrdersByState_ParsesRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "screened" {
			t.Errorf("expected state=screened, got %q", got)
		}
		json.NewEncoder(w).Encode(listOrdersResp{
			Orders:     []listOrderResp{{ID: 1, ExternalID: "ext-1"}, {ID: 2, ExternalID: "ext-2"}},
			NextCursor: "cursor-2",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	refs, next, err := c.ListOrdersByState(context.Background(), "screened", "")
	if err != nil {
		t.Fatalf("ListOrdersByState: %v", err)
	}
	if len(refs) != 2 || next != "cursor-2" {
		t.Errorf("unexpected result: refs=%+v next=%q", refs, next)
	}
}

func TestTransitionWithEntry_PostsCorrectShape(t *testing.T) {
	var got postTransitionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/orders/ext-1/transitions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(orderResponse{
			ExternalID: "ext-1", Tier: "RELAY", State: "dispatching", Version: 2,
			AmountIn: "1.000000", AmountOut: "1.000000", FeeUnits: "0", NetworkFeeUnits: "0",
			AmountInAsset: "USDT_TRC20", AmountOutAsset: "USDT_BEP20",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	lines := []EntryLine{
		{AccountCode: "asset:relay:leg:1", Asset: "USDT_TRC20", Amount: money.Amount{Asset: money.USDT_TRC20, Units: -1_000000}},
		{AccountCode: "revenue:relay_commission", Asset: "USDT_TRC20", Amount: money.Amount{Asset: money.USDT_TRC20, Units: 1_000000}},
	}
	_, err := c.TransitionWithEntry(context.Background(), "ext-1", "dispatching", 1, "relay_forward", "relay_settle", time.Now().UTC(), lines, "idem-2")
	if err != nil {
		t.Fatalf("TransitionWithEntry: %v", err)
	}
	if got.ToState != "dispatching" || got.Entry == nil || len(got.Entry.Lines) != 2 {
		t.Errorf("unexpected request: %+v", got)
	}
}
