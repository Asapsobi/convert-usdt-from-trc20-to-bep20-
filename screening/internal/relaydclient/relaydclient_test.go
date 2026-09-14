package relaydclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"screening/internal/relaydclient"
)

func TestGetRefundEntry_DecodesRealResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/relay-legs/ext-1/refund-entry" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("unexpected Authorization header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"entry_type":"relay_refund","occurred_at":"2026-09-14T00:00:00Z","lines":[` +
			`{"account_code":"liability:customer:cust-1:USDT_TRC20","asset":"USDT_TRC20","amount":"100.000000"},` +
			`{"account_code":"asset:relay:leg:5","asset":"USDT_TRC20","amount":"-100.000000"}]}`))
	}))
	defer srv.Close()

	client := relaydclient.New(srv.URL, "tok")
	entry, err := client.GetRefundEntry(context.Background(), "ext-1")
	if err != nil {
		t.Fatalf("GetRefundEntry: %v", err)
	}
	if entry.EntryType != "relay_refund" {
		t.Errorf("entry_type = %s, want relay_refund", entry.EntryType)
	}
	if len(entry.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(entry.Lines))
	}
	if entry.Lines[0].AccountCode != "liability:customer:cust-1:USDT_TRC20" || entry.Lines[0].Amount != "100.000000" {
		t.Errorf("unexpected first line: %+v", entry.Lines[0])
	}
}

func TestGetRefundEntry_404MapsToErrNotARelayLeg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"relay: no such leg: external_id ext-1"}`))
	}))
	defer srv.Close()

	client := relaydclient.New(srv.URL, "tok")
	_, err := client.GetRefundEntry(context.Background(), "ext-1")
	if !errors.Is(err, relaydclient.ErrNotARelayLeg) {
		t.Fatalf("expected ErrNotARelayLeg, got %v", err)
	}
}

func TestGetRefundEntry_409MapsToErrLegNotEligible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"driver: this leg is not currently eligible for a refund entry: order state is screened, want held"}`))
	}))
	defer srv.Close()

	client := relaydclient.New(srv.URL, "tok")
	_, err := client.GetRefundEntry(context.Background(), "ext-1")
	if !errors.Is(err, relaydclient.ErrLegNotEligible) {
		t.Fatalf("expected ErrLegNotEligible, got %v", err)
	}
}

func TestGetRefundEntry_OtherStatusIsARealError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"driver: fetching order: some transient failure"}`))
	}))
	defer srv.Close()

	client := relaydclient.New(srv.URL, "tok")
	_, err := client.GetRefundEntry(context.Background(), "ext-1")
	if err == nil {
		t.Fatal("expected an error for a 502 response")
	}
	if errors.Is(err, relaydclient.ErrNotARelayLeg) || errors.Is(err, relaydclient.ErrLegNotEligible) {
		t.Fatalf("a 502 must never be misreported as one of the two expected-outcome sentinels, got %v", err)
	}
}
