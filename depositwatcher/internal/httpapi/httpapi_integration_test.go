//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package httpapi_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/tyler-smith/go-bip32"

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/db"
	"depositwatcher/internal/httpapi"
)

const (
	testToken = "test-token"
	testActor = "test-actor"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	// The pool limit and cooldowns are exercised by their own tests; here
	// every order simply gets a free wallet, as with one wallet per order.
	if _, err := sqlDB.Exec(`UPDATE pool_settings SET max_wallets = 1000000, cooldown_after_use = '0', cooldown_after_expiry = '0'`); err != nil {
		t.Fatalf("relaxing pool settings for tests: %v", err)
	}

	pgxPool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pgxPool.Close)

	master, err := bip32.NewMasterKey([]byte("httpapi integration test fixture -- never use"))
	if err != nil {
		t.Fatalf("generating test master key: %v", err)
	}
	if err := addresses.Configure(master.PublicKey().B58Serialize()); err != nil {
		t.Fatalf("configuring addresses: %v", err)
	}
	return &db.Pool{Pool: pgxPool}
}

var seq int64
var seqMu sync.Mutex

func uniqueOrderID() int64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return time.Now().UnixNano() + seq
}

func newTestServer(t *testing.T, pool *db.Pool) *httptest.Server {
	srv, _ := newTestServerAndServer(t, pool)
	return srv
}

// newTestServerAndServer is newTestServer plus the underlying
// *httpapi.Server itself, for the one test (TestMetrics_ExposesNamedCounters)
// that needs to reach the Metrics NewRouter builds.
func newTestServerAndServer(t *testing.T, pool *db.Pool) (*httptest.Server, *httpapi.Server) {
	t.Helper()
	s := &httpapi.Server{
		Pool: pool,
		Auth: httpapi.AuthConfig{Tokens: map[string]string{testToken: testActor}},
	}
	router := httpapi.NewRouter(s) // populates s.Metrics as a side effect
	return httptest.NewServer(router), s
}

// call is a small HTTP helper: method/path/idempotencyKey/body in,
// status/decoded-body out. Every test in this file goes through it so a
// missing Authorization header is never accidentally the reason a test
// passes or fails.
func call(t *testing.T, srv *httptest.Server, method, path, idempotencyKey string, body any, out any) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decoding response for %s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

type addressDTO struct {
	Address       string `json:"address"`
	OrderID       int64  `json:"order_id"`
	ExternalID    string `json:"external_id"`
	CustomerID    string `json:"customer_id"`
	Status        string `json:"status"`
	RetiredReason string `json:"retired_reason"`
}

type errorDTO struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func postAddressBody(orderID int64, externalID, customerID string) map[string]any {
	now := time.Now().UTC().Truncate(time.Second)
	return map[string]any{
		"order_id": orderID, "external_id": externalID, "customer_id": customerID,
		"quoted_at": now, "quote_expires_at": now.Add(90 * time.Second),
	}
}

// ---------------------------------------------------------------------
// Auth and idempotency
// ---------------------------------------------------------------------

func TestUnauthenticated_Returns401(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/addresses/1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/addresses/1: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHealthzAndReadyz_Unauthenticated(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	for _, path := range []string{"/healthz", "/readyz"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestPostAddress_MissingIdempotencyKey_Returns400(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	orderID := uniqueOrderID()
	var errBody errorDTO
	status := call(t, srv, http.MethodPost, "/v1/addresses", "", postAddressBody(orderID, "ext", "cust"), &errBody)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if errBody.Error.Code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", errBody.Error.Code)
	}
}

// ---------------------------------------------------------------------
// Addresses
// ---------------------------------------------------------------------

func TestPostAddress_IdempotentOnOrderID_Returns200Both(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	orderID := uniqueOrderID()
	body := postAddressBody(orderID, fmt.Sprintf("ext-%d", orderID), "cust-1")

	var first addressDTO
	status := call(t, srv, http.MethodPost, "/v1/addresses", "key-1", body, &first)
	if status != http.StatusOK {
		t.Fatalf("first call: status = %d, want 200", status)
	}
	if first.Address == "" {
		t.Fatal("first call: address is empty")
	}

	var second addressDTO
	status = call(t, srv, http.MethodPost, "/v1/addresses", "key-2", body, &second)
	if status != http.StatusOK {
		t.Fatalf("second call: status = %d, want 200 (not 201-then-200)", status)
	}
	if second.Address != first.Address {
		t.Fatalf("second call returned a different address: %s vs %s", second.Address, first.Address)
	}
}

func TestGetAddress_NotFound(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	var errBody errorDTO
	status := call(t, srv, http.MethodGet, fmt.Sprintf("/v1/addresses/%d", uniqueOrderID()), "", nil, &errBody)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if errBody.Error.Code != "order_not_found" {
		t.Fatalf("error code = %q, want order_not_found", errBody.Error.Code)
	}
}

func TestRetireAddress_ThenGetReflectsIt(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	orderID := uniqueOrderID()
	var assigned addressDTO
	call(t, srv, http.MethodPost, "/v1/addresses", "key-1", postAddressBody(orderID, fmt.Sprintf("ext-%d", orderID), "cust-1"), &assigned)

	var retired addressDTO
	status := call(t, srv, http.MethodPost, fmt.Sprintf("/v1/addresses/%d/retire", orderID), "key-2",
		map[string]any{"reason": "expired"}, &retired)
	if status != http.StatusOK {
		t.Fatalf("retire: status = %d, want 200", status)
	}
	if retired.Status != "RETIRED" {
		t.Fatalf("retire: status field = %q, want RETIRED", retired.Status)
	}

	var got addressDTO
	call(t, srv, http.MethodGet, fmt.Sprintf("/v1/addresses/%d", orderID), "", nil, &got)
	if got.Status != "RETIRED" {
		t.Fatalf("GET after retire: status field = %q, want RETIRED", got.Status)
	}

	// A second retire is a legitimate "retry after an uncertain outcome"
	// case, same as internal/addresses.MarkFunded's own documented
	// behavior: migration 0002's trigger only fires on an ACTUAL status
	// change (WHEN OLD.status IS DISTINCT FROM NEW.status), so
	// RETIRED -> RETIRED never reaches it at all and just re-affirms the
	// same status rather than erroring.
	var retriedAgain addressDTO
	status = call(t, srv, http.MethodPost, fmt.Sprintf("/v1/addresses/%d/retire", orderID), "key-3",
		map[string]any{"reason": "expired again"}, &retriedAgain)
	if status != http.StatusOK {
		t.Fatalf("second retire: status = %d, want 200 (idempotent, per addresses.Retire's own design)", status)
	}
	if retriedAgain.Status != "RETIRED" {
		t.Fatalf("second retire: status field = %q, want RETIRED", retriedAgain.Status)
	}
}

// ---------------------------------------------------------------------
// Orphaned deposits
// ---------------------------------------------------------------------

type orphanedDepositDTO struct {
	ID                    int64  `json:"id"`
	Amount                string `json:"amount"`
	Resolution            string `json:"resolution"`
	ResolvedBy            string `json:"resolved_by"`
	OrderStateAtDetection string `json:"order_state_at_detection"`
}

// seedOrphanedDeposit inserts a row directly (there is no POST route for
// creating one -- only C2.8's own internal detection path does that);
// this test file only needs one to exist to exercise GET/resolve.
func seedOrphanedDeposit(t *testing.T, pool *db.Pool, txHash string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO orphaned_deposits (order_id, external_id, tx_hash, log_index, amount, detected_at, order_state_at_detection)
		VALUES ($1, $2, $3, 0, $4, now(), $5)
		RETURNING id
	`, uniqueOrderID(), "ext-orphaned", txHash, 3000_000000, "expired").Scan(&id)
	if err != nil {
		t.Fatalf("seeding orphaned deposit: %v", err)
	}
	return id
}

func TestGetOrphanedDeposits_ListsAndFiltersByResolved(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	txHash := fmt.Sprintf("0xhttpapi-list-%d", time.Now().UnixNano())
	id := seedOrphanedDeposit(t, pool, txHash)

	var listed struct {
		Deposits []orphanedDepositDTO `json:"deposits"`
	}
	status := call(t, srv, http.MethodGet, "/v1/orphaned-deposits", "", nil, &listed)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var found *orphanedDepositDTO
	for i := range listed.Deposits {
		if listed.Deposits[i].ID == id {
			found = &listed.Deposits[i]
		}
	}
	if found == nil {
		t.Fatal("GET /v1/orphaned-deposits did not include the seeded row")
	}
	if found.Amount != "3000.000000" {
		t.Fatalf("amount = %q, want a decimal string \"3000.000000\", never a JSON number", found.Amount)
	}

	var unresolvedOnly struct {
		Deposits []orphanedDepositDTO `json:"deposits"`
	}
	call(t, srv, http.MethodGet, "/v1/orphaned-deposits?resolved=false", "", nil, &unresolvedOnly)
	inList := false
	for _, d := range unresolvedOnly.Deposits {
		if d.ID == id {
			inList = true
		}
	}
	if !inList {
		t.Fatal("?resolved=false did not include an unresolved row")
	}

	var resolvedOnly struct {
		Deposits []orphanedDepositDTO `json:"deposits"`
	}
	call(t, srv, http.MethodGet, "/v1/orphaned-deposits?resolved=true", "", nil, &resolvedOnly)
	for _, d := range resolvedOnly.Deposits {
		if d.ID == id {
			t.Fatal("?resolved=true included an unresolved row")
		}
	}
}

func TestResolveOrphanedDeposit_SetsResolvedByFromActor(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	id := seedOrphanedDeposit(t, pool, fmt.Sprintf("0xhttpapi-resolve-%d", time.Now().UnixNano()))

	var resolved orphanedDepositDTO
	status := call(t, srv, http.MethodPost, fmt.Sprintf("/v1/orphaned-deposits/%d/resolve", id), "key-1",
		map[string]any{"resolution": "manually refunded off-chain"}, &resolved)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if resolved.Resolution != "manually refunded off-chain" {
		t.Fatalf("resolution = %q, want set", resolved.Resolution)
	}
	if resolved.ResolvedBy != testActor {
		t.Fatalf("resolved_by = %q, want the authenticated actor %q (never a body field)", resolved.ResolvedBy, testActor)
	}

	var errBody errorDTO
	status = call(t, srv, http.MethodPost, fmt.Sprintf("/v1/orphaned-deposits/%d/resolve", id), "key-2",
		map[string]any{"resolution": "a different resolution"}, &errBody)
	if status != http.StatusConflict {
		t.Fatalf("second resolve: status = %d, want 409", status)
	}
	if errBody.Error.Code != "already_resolved" {
		t.Fatalf("second resolve: error code = %q, want already_resolved", errBody.Error.Code)
	}
}

func TestResolveOrphanedDeposit_NotFound(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	var errBody errorDTO
	status := call(t, srv, http.MethodPost, "/v1/orphaned-deposits/999999999/resolve", "key-1",
		map[string]any{"resolution": "n/a"}, &errBody)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if errBody.Error.Code != "orphaned_deposit_not_found" {
		t.Fatalf("error code = %q, want orphaned_deposit_not_found", errBody.Error.Code)
	}
}

// ---------------------------------------------------------------------
// System
// ---------------------------------------------------------------------

func TestGetProviders_NotConfigured_Returns503(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	var errBody errorDTO
	status := call(t, srv, http.MethodGet, "/v1/system/providers", "", nil, &errBody)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if errBody.Error.Code != "system_component_not_ready" {
		t.Fatalf("error code = %q, want system_component_not_ready", errBody.Error.Code)
	}
}

func TestMetrics_ExposesNamedCounters(t *testing.T) {
	pool := testPool(t)
	srv, s := newTestServerAndServer(t, pool)
	defer srv.Close()

	// watcher_reports_to_ledger_total is a CounterVec: prometheus emits
	// nothing for it at all until some label value has been touched at
	// least once. Touching it here proves the hook actually reaches the
	// registered metric (httpapi.Metrics implementing
	// ledgerclient.MetricsRecorder), not just that the vec was declared.
	s.Metrics.ReportedToLedger("ok")

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatalf("reading /metrics body: %v", err)
	}
	text := body.String()

	for _, name := range []string{
		"watcher_candidates_detected_total",
		"watcher_candidates_finalized_total",
		"watcher_reports_to_ledger_total",
		"watcher_orphaned_deposits_total",
		"watcher_provider_agreement_failures_total",
		"watcher_cursor_lag_blocks",
	} {
		if !bytes.Contains(body.Bytes(), []byte(name)) {
			t.Errorf("/metrics does not mention %s\n--- body ---\n%s", name, text)
		}
	}
}

func TestGetInvariants_NoChainOrTracker_OmitsFields(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	var got map[string]any
	status := call(t, srv, http.MethodGet, "/v1/system/invariants", "", nil, &got)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(got) != 0 {
		t.Fatalf("invariants with no ChainPool/Tracker configured = %v, want an empty object (all fields omitted)", got)
	}
}

func TestGetCursor_ReadsSeededRow(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	var got map[string]any
	status := call(t, srv, http.MethodGet, "/v1/system/cursor", "", nil, &got)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if _, ok := got["last_scanned"]; !ok {
		t.Errorf("response missing last_scanned: %v", got)
	}
	if _, ok := got["updated_at"]; !ok {
		t.Errorf("response missing updated_at: %v", got)
	}
}

func TestPostCursor_MissingIdempotencyKey_Returns400(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	body := map[string]any{"last_scanned": 100, "last_candidate_scanned": 50, "reason": "test"}
	var errBody errorDTO
	status := call(t, srv, http.MethodPost, "/v1/system/cursor", "", body, &errBody)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if errBody.Error.Code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", errBody.Error.Code)
	}
}

func TestPostCursor_RejectsMissingReason(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	body := map[string]any{"last_scanned": 100, "last_candidate_scanned": 50}
	var got map[string]any
	status := call(t, srv, http.MethodPost, "/v1/system/cursor", "idem-1", body, &got)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %v", status, got)
	}
}

func TestPostCursor_RejectsCandidateAheadOfScanned(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	body := map[string]any{"last_scanned": 100, "last_candidate_scanned": 200, "reason": "test"}
	var got map[string]any
	status := call(t, srv, http.MethodPost, "/v1/system/cursor", "idem-2", body, &got)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (candidate ahead of scanned must be rejected), body = %v", status, got)
	}
}

func TestPostCursor_ThenGetReflectsIt(t *testing.T) {
	pool := testPool(t)
	srv := newTestServer(t, pool)
	defer srv.Close()

	body := map[string]any{"last_scanned": 500, "last_candidate_scanned": 400, "reason": "test fast-forward"}
	var posted map[string]any
	status := call(t, srv, http.MethodPost, "/v1/system/cursor", "idem-3", body, &posted)
	if status != http.StatusOK {
		t.Fatalf("POST status = %d, want 200, body = %v", status, posted)
	}

	var got map[string]any
	status = call(t, srv, http.MethodGet, "/v1/system/cursor", "", nil, &got)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", status)
	}
	if int64(got["last_scanned"].(float64)) != 500 {
		t.Errorf("last_scanned = %v, want 500", got["last_scanned"])
	}
	if int64(got["last_candidate_scanned"].(float64)) != 400 {
		t.Errorf("last_candidate_scanned = %v, want 400", got["last_candidate_scanned"])
	}
}
