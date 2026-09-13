package chain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProvider_ScanTRC20Transfers_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("only_to"); got != "true" {
			t.Errorf("expected only_to=true, got %q", got)
		}
		resp := tronGridTRC20Response{
			Success: true,
		}
		resp.Data = append(resp.Data, struct {
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
		}{
			TransactionID:  "tx1",
			From:           "TFromAddress",
			To:             "TToAddress",
			Type:           "Transfer",
			Value:          "1000000",
			BlockTimestamp: 1700000000000,
		})
		resp.Data[0].TokenInfo.Address = USDTTRC20ContractAddress
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewProvider(Config{Name: "test", BaseURL: srv.URL})
	transfers, err := p.ScanTRC20Transfers(context.Background(), "TWatchedAddress", USDTTRC20ContractAddress, 0)
	if err != nil {
		t.Fatalf("ScanTRC20Transfers: %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(transfers))
	}
	tr := transfers[0]
	if tr.TxID != "tx1" || tr.From != "TFromAddress" || tr.To != "TToAddress" {
		t.Errorf("unexpected transfer: %+v", tr)
	}
	if tr.ValueRaw.Int64() != 1000000 {
		t.Errorf("expected value 1000000, got %s", tr.ValueRaw)
	}
}

func TestProvider_ScanTRC20Transfers_FiltersNonTransferType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"success":true,"data":[{"transaction_id":"tx1","type":"Approval","value":"1","from":"a","to":"b","token_info":{"address":"` + USDTTRC20ContractAddress + `"}}],"meta":{}}`
		w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewProvider(Config{Name: "test", BaseURL: srv.URL})
	transfers, err := p.ScanTRC20Transfers(context.Background(), "addr", USDTTRC20ContractAddress, 0)
	if err != nil {
		t.Fatalf("ScanTRC20Transfers: %v", err)
	}
	if len(transfers) != 0 {
		t.Fatalf("expected non-Transfer events to be filtered, got %d", len(transfers))
	}
}

func TestProvider_ScanTRC20Transfers_Paginates(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fingerprint := r.URL.Query().Get("fingerprint")
		if fingerprint == "" {
			w.Write([]byte(`{"success":true,"data":[{"transaction_id":"tx1","type":"Transfer","value":"1","from":"a","to":"b","block_timestamp":1,"token_info":{"address":"` + USDTTRC20ContractAddress + `"}}],"meta":{"fingerprint":"page2"}}`))
			return
		}
		if fingerprint == "page2" {
			w.Write([]byte(`{"success":true,"data":[{"transaction_id":"tx2","type":"Transfer","value":"2","from":"a","to":"b","block_timestamp":2,"token_info":{"address":"` + USDTTRC20ContractAddress + `"}}],"meta":{}}`))
			return
		}
		t.Fatalf("unexpected fingerprint %q", fingerprint)
	}))
	defer srv.Close()

	p := NewProvider(Config{Name: "test", BaseURL: srv.URL})
	transfers, err := p.ScanTRC20Transfers(context.Background(), "addr", USDTTRC20ContractAddress, 0)
	if err != nil {
		t.Fatalf("ScanTRC20Transfers: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 paginated calls, got %d", calls)
	}
	if len(transfers) != 2 {
		t.Fatalf("expected 2 transfers across both pages, got %d", len(transfers))
	}
}

func TestProvider_ScanTRC20Transfers_RejectsSuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":false,"data":[]}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{Name: "test", BaseURL: srv.URL})
	if _, err := p.ScanTRC20Transfers(context.Background(), "addr", USDTTRC20ContractAddress, 0); err == nil {
		t.Fatal("expected an error for success=false")
	}
}

func TestProvider_ScanTRC20Transfers_RejectsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{Name: "test", BaseURL: srv.URL})
	if _, err := p.ScanTRC20Transfers(context.Background(), "addr", USDTTRC20ContractAddress, 0); err == nil {
		t.Fatal("expected an error for HTTP 500")
	}
}

func TestProvider_ScanTRC20Transfers_RejectsMalformedValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":[{"transaction_id":"tx1","type":"Transfer","value":"not-a-number","from":"a","to":"b","token_info":{"address":"` + USDTTRC20ContractAddress + `"}}],"meta":{}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{Name: "test", BaseURL: srv.URL})
	if _, err := p.ScanTRC20Transfers(context.Background(), "addr", USDTTRC20ContractAddress, 0); err == nil {
		t.Fatal("expected an error for a non-numeric value field")
	}
}

func TestProvider_SendsAPIKeyHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("TRON-PRO-API-KEY"); got != "secret-key" {
			t.Errorf("expected TRON-PRO-API-KEY header, got %q", got)
		}
		w.Write([]byte(`{"success":true,"data":[],"meta":{}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{Name: "test", BaseURL: srv.URL, APIKey: "secret-key"})
	if _, err := p.ScanTRC20Transfers(context.Background(), "addr", USDTTRC20ContractAddress, 0); err != nil {
		t.Fatalf("ScanTRC20Transfers: %v", err)
	}
}
