package chain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFinalityClient_IsFinal_MatchingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("value"); got != "tx123" {
			t.Errorf("expected value=tx123, got %q", got)
		}
		w.Write([]byte(`{"id":"tx123"}`))
	}))
	defer srv.Close()

	c := NewFinalityClient(srv.URL)
	final, err := c.IsFinal(context.Background(), "tx123")
	if err != nil {
		t.Fatalf("IsFinal: %v", err)
	}
	if !final {
		t.Error("expected final=true for a matching id")
	}
}

func TestFinalityClient_IsFinal_EmptyBodyNotYetFinal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewFinalityClient(srv.URL)
	final, err := c.IsFinal(context.Background(), "tx123")
	if err != nil {
		t.Fatalf("IsFinal: %v", err)
	}
	if final {
		t.Error("expected final=false for an empty response (not yet solidity-confirmed)")
	}
}

func TestFinalityClient_IsFinal_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewFinalityClient(srv.URL)
	if _, err := c.IsFinal(context.Background(), "tx123"); err == nil {
		t.Fatal("expected an error for HTTP 500")
	}
}
