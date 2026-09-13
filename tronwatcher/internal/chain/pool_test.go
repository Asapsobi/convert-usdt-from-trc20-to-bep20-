package chain

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func trc20Server(t *testing.T, transfers []Transfer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := tronGridTRC20Response{Success: true}
		for _, tr := range transfers {
			var d struct {
				TransactionID string `json:"transaction_id"`
				TokenInfo     struct {
					Address  string `json:"address"`
					Decimals int    `json:"decimals"`
				} `json:"token_info"`
				BlockTimestamp int64  `json:"block_timestamp"`
				From           string `json:"from"`
				To             string `json:"to"`
				Type           string `json:"type"`
				Value          string `json:"value"`
			}
			d.TransactionID = tr.TxID
			d.From = tr.From
			d.To = tr.To
			d.Type = "Transfer"
			d.Value = tr.ValueRaw.String()
			d.BlockTimestamp = tr.BlockTimestamp.UnixMilli()
			d.TokenInfo.Address = tr.ContractAddress
			resp.Data = append(resp.Data, d)
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestNewPool_RejectsFewerThanTwoProviders(t *testing.T) {
	if _, err := NewPool(nil); err == nil {
		t.Fatal("expected ErrInsufficientProviders for 0 providers")
	}
	one := NewProvider(Config{Name: "solo"})
	if _, err := NewPool([]*Provider{one}); err == nil {
		t.Fatal("expected ErrInsufficientProviders for 1 provider")
	}
}

func TestPool_ScanAddress_AgreementIncluded(t *testing.T) {
	ts := time.UnixMilli(1700000000000).UTC()
	shared := Transfer{TxID: "tx1", From: "a", To: "b", ValueRaw: big.NewInt(1_000000), BlockTimestamp: ts, ContractAddress: USDTTRC20ContractAddress}

	srv1 := trc20Server(t, []Transfer{shared})
	defer srv1.Close()
	srv2 := trc20Server(t, []Transfer{shared})
	defer srv2.Close()

	pool, err := NewPool([]*Provider{
		NewProvider(Config{Name: "p1", BaseURL: srv1.URL}),
		NewProvider(Config{Name: "p2", BaseURL: srv2.URL}),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	transfers, err := pool.ScanAddress(context.Background(), "addr", USDTTRC20ContractAddress, 0)
	if err != nil {
		t.Fatalf("ScanAddress: %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("expected 1 agreed transfer, got %d", len(transfers))
	}
}

func TestPool_ScanAddress_DisagreementExcluded(t *testing.T) {
	ts := time.UnixMilli(1700000000000).UTC()
	onlyOnPrimary := Transfer{TxID: "tx-primary-only", From: "a", To: "b", ValueRaw: big.NewInt(5_000000), BlockTimestamp: ts, ContractAddress: USDTTRC20ContractAddress}

	srv1 := trc20Server(t, []Transfer{onlyOnPrimary})
	defer srv1.Close()
	srv2 := trc20Server(t, nil) // secondary never saw it
	defer srv2.Close()

	pool, err := NewPool([]*Provider{
		NewProvider(Config{Name: "p1", BaseURL: srv1.URL}),
		NewProvider(Config{Name: "p2", BaseURL: srv2.URL}),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	transfers, err := pool.ScanAddress(context.Background(), "addr", USDTTRC20ContractAddress, 0)
	if err != nil {
		t.Fatalf("ScanAddress: %v", err)
	}
	if len(transfers) != 0 {
		t.Fatalf("expected 0 transfers when providers disagree, got %d: %+v", len(transfers), transfers)
	}
}

func TestPool_ScanAddress_ValueMismatchExcluded(t *testing.T) {
	ts := time.UnixMilli(1700000000000).UTC()
	primary := Transfer{TxID: "tx1", From: "a", To: "b", ValueRaw: big.NewInt(1_000000), BlockTimestamp: ts, ContractAddress: USDTTRC20ContractAddress}
	secondaryDisagrees := Transfer{TxID: "tx1", From: "a", To: "b", ValueRaw: big.NewInt(999_000000), BlockTimestamp: ts, ContractAddress: USDTTRC20ContractAddress}

	srv1 := trc20Server(t, []Transfer{primary})
	defer srv1.Close()
	srv2 := trc20Server(t, []Transfer{secondaryDisagrees})
	defer srv2.Close()

	pool, err := NewPool([]*Provider{
		NewProvider(Config{Name: "p1", BaseURL: srv1.URL}),
		NewProvider(Config{Name: "p2", BaseURL: srv2.URL}),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	transfers, err := pool.ScanAddress(context.Background(), "addr", USDTTRC20ContractAddress, 0)
	if err != nil {
		t.Fatalf("ScanAddress: %v", err)
	}
	if len(transfers) != 0 {
		t.Fatalf("expected a value mismatch between providers to be excluded, got %d", len(transfers))
	}
}

func TestPool_ScanAddress_ProviderErrorPropagates(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv1.Close()
	srv2 := trc20Server(t, nil)
	defer srv2.Close()

	pool, err := NewPool([]*Provider{
		NewProvider(Config{Name: "p1", BaseURL: srv1.URL}),
		NewProvider(Config{Name: "p2", BaseURL: srv2.URL}),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	if _, err := pool.ScanAddress(context.Background(), "addr", USDTTRC20ContractAddress, 0); err == nil {
		t.Fatal("expected an error when a provider fails")
	}
}
