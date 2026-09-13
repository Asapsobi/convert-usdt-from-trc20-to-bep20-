package watcherclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAssignAddress_ParsesResponse(t *testing.T) {
	var gotIdempotencyKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/addresses" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		gotIdempotencyKey = r.Header.Get("Idempotency-Key")
		json.NewEncoder(w).Encode(addressResponse{
			Address: "Tdeposit-address", OrderID: 1, ExternalID: "ext-1", CustomerID: "cust-1", Status: "WATCHING",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	now := time.Now().UTC()
	addr, err := c.AssignAddress(context.Background(), 1, "ext-1", "cust-1", now, now.Add(90*time.Second), "idem-1")
	if err != nil {
		t.Fatalf("AssignAddress: %v", err)
	}
	if addr.Address != "Tdeposit-address" {
		t.Errorf("unexpected address: %s", addr.Address)
	}
	if gotIdempotencyKey != "idem-1" {
		t.Errorf("expected Idempotency-Key header to be forwarded, got %q", gotIdempotencyKey)
	}
}

func TestGetAddress_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "order_not_found"}})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	if _, err := c.GetAddress(context.Background(), 999); err == nil {
		t.Fatal("expected an error for a 404 response")
	}
}
