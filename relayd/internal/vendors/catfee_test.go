package vendors

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Account reads the prepaid balance (in sun) and the top-up address from
// GET /v1/account, signed like every other CatFee call.
func TestCatFee_Account(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/account" {
			t.Errorf("got %s %s, want GET /v1/account", r.Method, r.URL.Path)
		}
		for _, h := range []string{"CF-ACCESS-KEY", "CF-ACCESS-SIGN", "CF-ACCESS-TIMESTAMP"} {
			if r.Header.Get(h) == "" {
				t.Errorf("missing %s header", h)
			}
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"wallet":"x","balance":1111450,"balanceTrx":1111450,"balance_usdt":0,"recharge_address":"TTopUpAddressForTheCatFeeAccount1"}}`))
	}))
	defer srv.Close()

	c, err := NewCatFee("key", "secret", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.BalanceSun != 1_111_450 || acct.TopUpAddress != "TTopUpAddressForTheCatFeeAccount1" {
		t.Fatalf("got %+v", acct)
	}
}

// A refusal from CatFee is a rejection, not an outage.
func TestCatFee_AccountRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":401,"msg":"invalid signature"}`))
	}))
	defer srv.Close()

	c, _ := NewCatFee("key", "secret", srv.URL)
	_, err := c.Account(context.Background())
	var rejection *EnergyRejection
	if !errors.As(err, &rejection) {
		t.Fatalf("got %v, want an EnergyRejection", err)
	}
}
