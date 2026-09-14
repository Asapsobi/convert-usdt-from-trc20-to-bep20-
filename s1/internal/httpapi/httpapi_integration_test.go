//go:build integration

// Requires a real, reachable Postgres 16 instance (S1_TEST_DATABASE_URL).
// Run via `make test-integration`.
package httpapi_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"s1/internal/db"
	"s1/internal/httpapi"
	"s1/internal/kmssign"
	"s1/internal/requests"
	"s1/internal/slots"
)

const (
	c5Token        = "c5-test-token"
	c5Actor        = "dispatcher"
	approverToken1 = "approver-test-token-1"
	approverActor1 = "alice"
	approverToken2 = "approver-test-token-2"
	approverActor2 = "bob"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("S1_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("S1_TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

// slotKeyGetterAdapter adapts *slots.Store to requests.SlotKeyGetter.
type slotKeyGetterAdapter struct{ store *slots.Store }

func (a slotKeyGetterAdapter) Get(ctx context.Context, slotID int) (requests.SlotKeyInfo, error) {
	key, err := a.store.Get(ctx, slotID)
	if err != nil {
		return requests.SlotKeyInfo{}, err
	}
	return requests.SlotKeyInfo{KMSKeyID: key.KMSKeyID, PublicKey: key.PublicKey, TronAddress: key.TronAddress}, nil
}

// testServer spins up the real router (httptest.NewServer) against the
// shared s1_test database, with a real Postgres-backed slots.Store and
// requests.Store, signing through a real kmssign.Wrapper over a
// FakeKMSClient -- mirroring every prior component's own testServer.
func testServer(t *testing.T, thresholdUSD float64) (baseURL string, pool *db.Pool) {
	t.Helper()
	dbURL := testDatabaseURL(t)

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir(t)); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	ctx := context.Background()
	rawPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(rawPool.Close)
	wrapped := &db.Pool{Pool: rawPool}

	if _, err := rawPool.Exec(ctx, `TRUNCATE signing_requests, signing_audit_log, signing_approvals, s1_slot_keys RESTART IDENTITY`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}

	fakeKMS := kmssign.NewFakeKMSClient(1)
	wrapper := kmssign.NewWrapper(fakeKMS)
	slotStore := slots.NewStore(wrapped, wrapper)
	if _, err := slotStore.Register(ctx, 1, "kms-key-slot-1"); err != nil {
		t.Fatalf("registering slot 1: %v", err)
	}

	signingStore := requests.NewStore(wrapped, slotKeyGetterAdapter{slotStore}, nil, wrapper, requests.Config{ApprovalThresholdUSD: thresholdUSD})

	server := &httpapi.Server{
		Pool:      wrapped,
		C5Auth:    httpapi.AuthConfig{Tokens: map[string]string{c5Token: c5Actor}},
		Approver:  httpapi.AuthConfig{Tokens: map[string]string{approverToken1: approverActor1, approverToken2: approverActor2}},
		Signing:   signingStore,
		BuildInfo: func() (string, string) { return "test", "test" },
	}
	router := httpapi.NewRouter(server)

	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)
	return ts.URL, wrapped
}

func doRequest(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshaling request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
}

type signingRequestResult struct {
	ID        int64   `json:"id"`
	Status    string  `json:"status"`
	SignedTx  *string `json:"signed_tx"`
	CreatedAt string  `json:"created_at"`
}

func TestPostSigningRequest_UnderThresholdSignsAndIsReadableImmediately(t *testing.T) {
	baseURL, _ := testServer(t, 10000)

	digest := hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/signing-requests", c5Token, map[string]any{
		"slot_id": 1, "digest": digest, "estimated_usd": 5000, "idempotency_key": "idem-http-1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var created signingRequestResult
	decodeInto(t, resp, &created)
	if created.Status != "SIGNED" {
		t.Fatalf("status = %q, want SIGNED", created.Status)
	}
	if created.SignedTx == nil || len(*created.SignedTx) != 130 { // 65 bytes hex-encoded
		t.Fatalf("signed_tx = %v, want a 130-char hex string", created.SignedTx)
	}

	getResp := doRequest(t, http.MethodGet, fmt.Sprintf("%s/v1/signing-requests/%d", baseURL, created.ID), c5Token, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
	}
	var fetched signingRequestResult
	decodeInto(t, getResp, &fetched)
	if fetched.Status != "SIGNED" || fetched.SignedTx == nil || *fetched.SignedTx != *created.SignedTx {
		t.Fatalf("fetched = %+v, want status SIGNED with the same signed_tx as at creation", fetched)
	}
}

func TestPostSigningRequest_OverThresholdRequiresTwoDistinctApprovers(t *testing.T) {
	baseURL, _ := testServer(t, 10000)

	digest := hex.EncodeToString(bytes.Repeat([]byte{8}, 32))
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/signing-requests", c5Token, map[string]any{
		"slot_id": 1, "digest": digest, "estimated_usd": 50000, "idempotency_key": "idem-http-2",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var created signingRequestResult
	decodeInto(t, resp, &created)
	if created.Status != "PENDING" {
		t.Fatalf("status = %q, want PENDING", created.Status)
	}

	approveURL := fmt.Sprintf("%s/v1/signing-requests/%d/approve", baseURL, created.ID)

	first := doRequest(t, http.MethodPost, approveURL, approverToken1, nil)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("1st approve status = %d, want 200", first.StatusCode)
	}
	var afterFirst signingRequestResult
	decodeInto(t, first, &afterFirst)
	if afterFirst.Status != "PENDING" {
		t.Fatalf("status after 1st approval = %q, want still PENDING", afterFirst.Status)
	}

	second := doRequest(t, http.MethodPost, approveURL, approverToken2, nil)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("2nd approve status = %d, want 200", second.StatusCode)
	}
	var afterSecond signingRequestResult
	decodeInto(t, second, &afterSecond)
	if afterSecond.Status != "SIGNED" {
		t.Fatalf("status after 2nd distinct approval = %q, want SIGNED", afterSecond.Status)
	}
}

func TestApprove_C5ScopedTokenCannotApprove(t *testing.T) {
	baseURL, _ := testServer(t, 10000)

	digest := hex.EncodeToString(bytes.Repeat([]byte{9}, 32))
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/signing-requests", c5Token, map[string]any{
		"slot_id": 1, "digest": digest, "estimated_usd": 50000, "idempotency_key": "idem-http-3",
	})
	var created signingRequestResult
	decodeInto(t, resp, &created)

	approveURL := fmt.Sprintf("%s/v1/signing-requests/%d/approve", baseURL, created.ID)
	blocked := doRequest(t, http.MethodPost, approveURL, c5Token, nil)
	if blocked.StatusCode == http.StatusOK {
		t.Fatal("a C5-scoped token was able to approve a signing request; want it rejected")
	}
	if blocked.StatusCode != http.StatusUnauthorized && blocked.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 401 or 403", blocked.StatusCode)
	}
}

func TestGetSlotAddress(t *testing.T) {
	baseURL, _ := testServer(t, 10000)

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/slots/1/address", c5Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		TronAddress string `json:"tron_address"`
	}
	decodeInto(t, resp, &got)
	if got.TronAddress == "" || got.TronAddress[0] != 'T' {
		t.Fatalf("tron_address = %q, want a real-looking TRON address", got.TronAddress)
	}
}

func TestHealthzReadyzUnauthenticated(t *testing.T) {
	baseURL, _ := testServer(t, 10000)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		resp := doRequest(t, http.MethodGet, baseURL+path, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 with no token", path, resp.StatusCode)
		}
	}
}

func TestGetSigningRequests_ListsOnlyPending(t *testing.T) {
	baseURL, _ := testServer(t, 10000)

	// Over threshold -- stays PENDING, should appear in the list.
	pendingDigest := hex.EncodeToString(bytes.Repeat([]byte{9}, 32))
	pendingResp := doRequest(t, http.MethodPost, baseURL+"/v1/signing-requests", c5Token, map[string]any{
		"slot_id": 1, "digest": pendingDigest, "estimated_usd": 50000, "idempotency_key": "idem-list-pending",
	})
	if pendingResp.StatusCode != http.StatusCreated {
		t.Fatalf("seeding pending request: status = %d, want 201", pendingResp.StatusCode)
	}
	var pending signingRequestResult
	decodeInto(t, pendingResp, &pending)
	if pending.Status != "PENDING" {
		t.Fatalf("seeded request status = %q, want PENDING", pending.Status)
	}

	// Under threshold -- signs immediately, should NOT appear in the list.
	signedDigest := hex.EncodeToString(bytes.Repeat([]byte{10}, 32))
	signedResp := doRequest(t, http.MethodPost, baseURL+"/v1/signing-requests", c5Token, map[string]any{
		"slot_id": 1, "digest": signedDigest, "estimated_usd": 5000, "idempotency_key": "idem-list-signed",
	})
	if signedResp.StatusCode != http.StatusCreated {
		t.Fatalf("seeding signed request: status = %d, want 201", signedResp.StatusCode)
	}
	var signed signingRequestResult
	decodeInto(t, signedResp, &signed)

	listResp := doRequest(t, http.MethodGet, baseURL+"/v1/signing-requests?status=pending", c5Token, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	var got struct {
		SigningRequests []struct {
			ID           int64   `json:"id"`
			SlotID       int     `json:"slot_id"`
			EstimatedUSD float64 `json:"estimated_usd"`
		} `json:"signing_requests"`
	}
	decodeInto(t, listResp, &got)

	foundPending, foundSigned := false, false
	for _, r := range got.SigningRequests {
		if r.ID == pending.ID {
			foundPending = true
			if r.EstimatedUSD != 50000 {
				t.Errorf("pending request estimated_usd = %v, want 50000", r.EstimatedUSD)
			}
		}
		if r.ID == signed.ID {
			foundSigned = true
		}
	}
	if !foundPending {
		t.Errorf("PENDING request %d not present in status=pending list", pending.ID)
	}
	if foundSigned {
		t.Errorf("SIGNED request %d wrongly present in status=pending list", signed.ID)
	}
}

func TestGetSigningRequests_RejectsUnsupportedStatus(t *testing.T) {
	baseURL, _ := testServer(t, 10000)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/signing-requests?status=signed", c5Token, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
