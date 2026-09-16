//go:build integration

// The Design B (async finality) real-lifecycle E2E gate: the one test in
// this codebase that drives the ENTIRE real chain --
//
//	deterministic fake RPC providers (asyncSimChain, this package's own
//	fixture, async_e2e_fixture_integration_test.go)
//	-> a real chain.Pool
//	-> the real chain.RunIngestionLoop + candidates.RunLoop background
//	   loops (not called directly -- the real ticker-driven lifecycle)
//	-> runTick's own real WATCHER_FINALITY_MODE=async equivalent
//	   (candidates.Config.AsyncFinality=true), the exact mode-switch
//	   conditional cmd/watcherd wires from WATCHER_FINALITY_MODE/
//	   WATCHER_ALLOW_ASYNC_FINALITY
//	-> the real finality.Tracker.CheckFinalityAsync
//	-> the real, unmodified finalize()/drop() lifecycle
//	-> the real ledgerclient.Client.ReportDepositFinal
//	-> a real, isolated ledgerd + Postgres
//
// Every existing Design-B test either drives CheckFinalityAsync directly
// against a hand-written fake (internal/finality/async_driver_test.go) or
// proves the real chain.Pool -> real ledgerd seam with manually-triggered
// ticks (internal/finality/async_integration_test.go). Neither exercises
// candidates.RunLoop/runTick itself, or C2's own real candidate-detection
// path (ScanRange -> addresses.GetByAddress -> chain.ParseTransferLog ->
// chain.ClassifyAgainstOrder -> OnLogObserved) feeding Design B. This
// file closes that gap -- it is the CI-safe, deterministic sibling to the
// real-provider observation this session ran separately by hand (no
// internet dependency, no real BSC, no signing, no broadcasting, no real
// funds; every RPC provider here is a local httptest.Server).
//
// Async finality is enabled ONLY inside this test's own in-process
// finality.Tracker/candidates.Config -- nothing here touches
// WATCHER_FINALITY_MODE, restarts any running service, or comes anywhere
// near Model D's own :18082 process.
//
// Requires a real, reachable Postgres 16 instance (WATCHER_TEST_DATABASE_URL,
// LEDGER_TEST_DATABASE_URL) AND a sibling checkout of the ledger module at
// ../../../ledger. Run via `go test -tags integration ./internal/candidates/...`.
package candidates_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5/pgxpool"

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/candidates"
	"depositwatcher/internal/chain"
	"depositwatcher/internal/db"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/ledgerclient"
	"depositwatcher/internal/money"
	"depositwatcher/internal/orphaned"
)

// loopInterval is deliberately tiny -- both background loops (ingestion
// and the candidate/finality loop) tick every 20ms, so a scenario's own
// pollUntil calls converge in well under a second of real wall-clock
// time without ever needing a hand-picked fixed sleep matched to
// production's own 3s DefaultInterval. See this file's own top comment
// for why real time is used at all here (candidates.RunLoop has no
// injectable clock) -- this is that "absolutely necessary" minimum, not
// a stand-in for determinism: every assertion below waits for an
// observable real effect (an order's own state, a recorded disagreement,
// a call count), never a fixed "sleep and hope" duration.
const loopInterval = 20 * time.Millisecond

// asyncE2EHarness bundles everything shared across all ten scenarios: one
// real chain.Pool (backed by three deterministic fake nodes), one real
// finality.Tracker wired with the REAL ledgerclient.Client (never a
// fake -- this is the entire point of this test), one real, running
// ledgerd, and the two real background loops actually driving all of it,
// started once and left running for the whole test, exactly like a real
// watcherd process.
type asyncE2EHarness struct {
	t *testing.T

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	watcherPool   *pgxpool.Pool // raw pool -- for addresses.Assign/orphaned.Record/raw SQL fixture setup, all of which take db.Queryer
	watcherDBPool *db.Pool      // the SAME underlying connection, wrapped -- RunLoop/RunIngestionLoop need this concrete type
	ledgerPool    *pgxpool.Pool
	ledgerFix     *asyncE2ELedgerFixture
	ledgerClnt    *ledgerclient.Client

	chain   *asyncSimChain
	tracker *finality.Tracker

	mu            sync.Mutex
	heightSeq     uint64
	idSeq         int64
	finalCalls    map[string]int    // external_id -> number of real OnFinal calls
	disagreements map[string]string // external_id -> first recorded disagreement note
	reorgReports  map[string]bool   // external_id -> ReportReorg was called for it
}

var (
	asyncE2EContract = common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
	asyncE2ETopic    = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
)

func newAsyncE2EHarness(t *testing.T) *asyncE2EHarness {
	t.Helper()
	watcherPool := freshIsolatedWatcherPoolForAsyncE2E(t)

	ledgerDBURL := osEnvOrSkip(t, "LEDGER_TEST_DATABASE_URL")
	ledgerPool := startAsyncE2ELedgerd(t, ledgerDBURL)

	h := &asyncE2EHarness{
		t: t, watcherPool: watcherPool, watcherDBPool: &db.Pool{Pool: watcherPool}, ledgerPool: ledgerPool,
		ledgerFix:     newAsyncE2ELedgerFixture(),
		ledgerClnt:    ledgerclient.New(asyncE2ELedgerBaseURL, asyncE2ELedgerToken),
		chain:         newAsyncSimChain(t),
		finalCalls:    make(map[string]int),
		disagreements: make(map[string]string),
		reorgReports:  make(map[string]bool),
		heightSeq:     5_000_000, // far past anything ingestion's own default cursor (0) would ever legitimately reach on its own
	}

	tracker, err := finality.New(finality.Config{
		ContractAddress:         asyncE2EContract,
		TransferTopic:           asyncE2ETopic,
		OnFinal:                 h.onFinal,
		ReorgReporter:           reorgReporterFuncE2E(h.reportReorg),
		OrphanedDepositRecorder: orphanedRecorderFuncE2E(h.recordOrphaned),
		OnDisagreement:          h.onDisagreement,
		AsyncMinAgreement:       2,
	})
	if err != nil {
		t.Fatalf("finality.New: %v", err)
	}
	h.tracker = tracker

	ctx, cancel := context.WithCancel(context.Background())
	h.ctx, h.cancel = ctx, cancel

	// The real ingestion loop -- required so chain.LastScannedHeight
	// (what candidates.runTick gates its own ScanRange calls on) ever
	// advances at all. Untouched, unmodified: exactly what cmd/watcherd's
	// own engine.run starts alongside candidates.RunLoop.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		_ = chain.RunIngestionLoop(ctx, h.chain.pool, h.watcherDBPool, chain.IngestionConfig{Interval: loopInterval})
	}()

	// The real candidate loop, with AsyncFinality=true -- the exact
	// production mode-switch (internal/candidates/loop.go's own runTick)
	// that cmd/watcherd reaches only through the WATCHER_FINALITY_MODE=
	// async + WATCHER_ALLOW_ASYNC_FINALITY=true double gate. Enabled here
	// ONLY inside this test's own in-process Config -- no env var, no
	// running service, nothing outside this test process is affected.
	cfg := candidates.Config{
		ContractAddress: asyncE2EContract, TransferTopic: asyncE2ETopic,
		DustFloor: 1_000000, AsyncFinality: true,
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		_ = candidates.RunLoop(ctx, h.chain.pool, h.watcherDBPool, h.ledgerClnt, h.tracker, cfg, loopInterval)
	}()

	t.Cleanup(func() {
		cancel()
		h.wg.Wait()
	})

	return h
}

func osEnvOrSkip(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set; skipping integration test", name)
	}
	return v
}

func (h *asyncE2EHarness) onFinal(ctx context.Context, c finality.Candidate) error {
	err := h.ledgerClnt.ReportDepositFinal(ctx, c)
	h.mu.Lock()
	h.finalCalls[c.ExternalID]++
	h.mu.Unlock()
	return err
}

type reorgReporterFuncE2E func(ctx context.Context, externalID, originalEntryKey string) error

func (f reorgReporterFuncE2E) ReportReorg(ctx context.Context, externalID, originalEntryKey string) error {
	return f(ctx, externalID, originalEntryKey)
}

func (h *asyncE2EHarness) reportReorg(ctx context.Context, externalID, originalEntryKey string) error {
	err := h.ledgerClnt.ReportReorg(ctx, externalID, originalEntryKey)
	if err == nil {
		h.mu.Lock()
		h.reorgReports[externalID] = true
		h.mu.Unlock()
	}
	return err
}

type orphanedRecorderFuncE2E func(ctx context.Context, c finality.Candidate, c1Error error) error

func (f orphanedRecorderFuncE2E) RecordOrphanedDeposit(ctx context.Context, c finality.Candidate, c1Error error) error {
	return f(ctx, c, c1Error)
}

func (h *asyncE2EHarness) recordOrphaned(ctx context.Context, c finality.Candidate, c1Error error) error {
	state := "unknown"
	if order, err := h.ledgerClnt.GetOrder(ctx, c.ExternalID); err == nil {
		state = order.State
	}
	return orphaned.Record(ctx, h.watcherPool, orphaned.Deposit{
		OrderID: c.OrderID, ExternalID: c.ExternalID, TxHash: c.TxHash.Hex(), LogIndex: int(c.LogIndex),
		Amount: int64(c.Amount), DetectedAt: time.Now().UTC(), OrderStateAtDetection: state,
	})
}

func (h *asyncE2EHarness) onDisagreement(c finality.Candidate, note string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.disagreements[c.ExternalID]; !ok {
		h.disagreements[c.ExternalID] = note
	}
}

func (h *asyncE2EHarness) finalCallCount(externalID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finalCalls[externalID]
}

func (h *asyncE2EHarness) disagreementNote(externalID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	note, ok := h.disagreements[externalID]
	return note, ok
}

func (h *asyncE2EHarness) reorgReported(externalID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reorgReports[externalID]
}

func (h *asyncE2EHarness) nextHeight() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heightSeq += 100 // generous headroom so no two scenarios' own detection/finality re-checks ever touch a shared height
	return h.heightSeq
}

func (h *asyncE2EHarness) nextID(prefix string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.idSeq++
	return fmt.Sprintf("async-e2e-%s-%d-%d", prefix, time.Now().UnixNano(), h.idSeq)
}

// newTxHash returns a fresh, never-repeated synthetic tx hash.
func (h *asyncE2EHarness) newTxHash() common.Hash {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.idSeq++
	return common.BigToHash(big.NewInt(time.Now().UnixNano() + h.idSeq))
}

type e2eOrder struct {
	ExternalID string
	OrderID    int64
	CustomerID string
	Address    addresses.Address
}

// prepareOrder creates a REAL C1 order and its two ledger accounts, then
// assigns a REAL, address-book-registered watched address for it via
// addresses.Assign -- the same production function ScanRange's own
// candidate detection path (addresses.GetByAddress) reads from. Nothing
// about the resulting Candidate is hand-built: it only ever exists once
// a Transfer log to this exact address is fed through the real chain and
// picked up by the real ScanRange/OnLogObserved call chain.
func (h *asyncE2EHarness) prepareOrder(t *testing.T, amountIn string) e2eOrder {
	t.Helper()
	externalID := h.nextID("order")
	customerID := h.nextID("cust")
	order, err := h.ledgerFix.createOrder(h.ctx, externalID, customerID, amountIn)
	if err != nil {
		t.Fatalf("creating order: %v", err)
	}
	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custAcc := "liability:customer:" + customerID + ":USDT_BEP20"
	if err := h.ledgerFix.createAccount(h.ctx, h.ledgerPool, depositAcc, "ASSET", "USDT_BEP20", 1); err != nil {
		t.Fatalf("creating deposit account: %v", err)
	}
	if err := h.ledgerFix.createAccount(h.ctx, h.ledgerPool, custAcc, "LIABILITY", "USDT_BEP20", -1); err != nil {
		t.Fatalf("creating customer account: %v", err)
	}
	now := time.Now().UTC()
	addr, err := addresses.Assign(h.ctx, h.watcherPool, order.ID, externalID, customerID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("assigning address: %v", err)
	}
	return e2eOrder{ExternalID: externalID, OrderID: order.ID, CustomerID: customerID, Address: addr}
}

func addressTopicE2E(addr addresses.Address) common.Hash {
	return common.BytesToHash(common.HexToAddress(string(addr)).Bytes())
}

// transferLogE2E builds a well-formed Transfer log carrying minorUnits
// (this service's 6-decimal convention), rescaled to the real contract's
// 18 on-chain decimals -- the inverse of chain.ParseTransferLog's own
// rescale.
func transferLogE2E(height uint64, txHash, toTopic common.Hash, index uint, minorUnits money.Amount) types.Log {
	raw := new(big.Int).Mul(big.NewInt(int64(minorUnits)), big.NewInt(1_000_000_000_000))
	data := make([]byte, 32)
	raw.FillBytes(data)
	return types.Log{
		Address: asyncE2EContract, Topics: []common.Hash{asyncE2ETopic, common.Hash{}, toTopic},
		Data: data, TxHash: txHash, Index: index,
	}
}

// seedIngestionCursor nudges BOTH ingestion_cursor.last_scanned and its
// own last_candidate_scanned column to height-1 (never backward --
// GREATEST guards against racing an already-further-along cursor) so the
// real, already-running chain.RunIngestionLoop AND candidates.RunLoop's
// own catch-up scan each only have to walk the single new height this
// scenario just committed, instead of trying to walk from wherever they
// happen to be all the way up to a height thousands of blocks ahead --
// exactly the same bootstrap technique cmd/watcherd's own
// bootstrapCursorIfFresh uses for a freshly deployed watcher (engine.go),
// applied here once per scenario instead of once at process start.
//
// Seeding ONLY last_scanned is not enough: candidates.runTick's own
// catch-up scan is separately capped at maxBlocksPerTick=10 per tick
// (candidates/loop.go), sized for a real chain's ~1 block/second growth,
// not this suite's own scenario-to-scenario height jumps (allocated far
// apart so no two scenarios' own detection/finality re-checks ever touch
// a shared height) -- without also seeding last_candidate_scanned, C2's
// own candidate detection would need hundreds of thousands of ticks to
// ever catch up to a freshly seeded last_scanned that far ahead.
func (h *asyncE2EHarness) seedIngestionCursor(t *testing.T, height uint64) {
	t.Helper()
	_, err := h.watcherPool.Exec(h.ctx,
		`UPDATE ingestion_cursor SET last_scanned = GREATEST(last_scanned, $1), updated_at = now() WHERE id = 1`,
		int64(height-1))
	if err != nil {
		t.Fatalf("seeding ingestion cursor: %v", err)
	}
	current, found, err := chain.LastCandidateScannedHeight(h.ctx, h.watcherPool)
	if err != nil {
		t.Fatalf("reading candidate scan cursor: %v", err)
	}
	if !found || current < height-1 {
		if err := chain.SetCandidateScannedHeight(h.ctx, h.watcherPool, height-1); err != nil {
			t.Fatalf("seeding candidate scan cursor: %v", err)
		}
	}
}

// waitOrderState polls the REAL ledger (over real HTTP, against the real
// running ledgerd) until externalID's order reaches want, or fails the
// test after timeout.
func (h *asyncE2EHarness) waitOrderState(t *testing.T, externalID, want string, timeout time.Duration) {
	t.Helper()
	var last string
	ok := pollUntil(t, timeout, func() bool {
		o, err := h.ledgerFix.getOrder(h.ctx, externalID)
		if err != nil {
			return false
		}
		last = o.State
		return o.State == want
	})
	if !ok {
		t.Fatalf("order %s never reached state %q (last seen: %q)", externalID, want, last)
	}
}

func (h *asyncE2EHarness) orderState(t *testing.T, externalID string) string {
	t.Helper()
	o, err := h.ledgerFix.getOrder(h.ctx, externalID)
	if err != nil {
		t.Fatalf("getOrder(%s): %v", externalID, err)
	}
	return o.State
}

// commitCandidateAndWaitDetected creates a real order+address, commits a
// matching Transfer log identically to every provider (detection is
// always a routine, agreed event -- divergence is introduced afterward,
// per this file's own top comment), seeds both cursors so the real
// background loops pick it up on their very next tick, and waits for the
// real ScanRange -> OnLogObserved path to have tracked it.
//
// The observable here is tracker.PendingCount() increasing, not the
// order's own state: a freshly created order already starts "quoted"
// (createOrder's own initial state, before any deposit activity at all),
// so polling for orderState=="quoted" would be vacuously true the instant
// the order exists and would never actually prove detection happened --
// PendingCount only increases once the real OnLogObserved call fires.
func (h *asyncE2EHarness) commitCandidateAndWaitDetected(t *testing.T, amount money.Amount) (order e2eOrder, height uint64, txHash common.Hash, at time.Time) {
	t.Helper()
	order = h.prepareOrder(t, "3000.000000")
	height = h.nextHeight()
	txHash = h.newTxHash()
	at = time.Now()
	pendingBefore := h.tracker.PendingCount()
	log := transferLogE2E(height, txHash, addressTopicE2E(order.Address), 0, amount)
	h.chain.commitToAll(height, at, []types.Log{log})
	h.seedIngestionCursor(t, height)
	if !pollUntil(t, 5*time.Second, func() bool { return h.tracker.PendingCount() > pendingBefore }) {
		t.Fatalf("order %s: real candidate detection never happened (PendingCount never increased)", order.ExternalID)
	}
	if got := h.orderState(t, order.ExternalID); got != "quoted" {
		t.Fatalf("order %s state = %q immediately after detection, want quoted (0-conf detection must never itself settle anything)", order.ExternalID, got)
	}
	return order, height, txHash, at
}

// ---------------------------------------------------------------------
// The ten required scenarios. Each drives the SAME shared, already-
// running real candidates.RunLoop/chain.RunIngestionLoop pair (started
// once in newAsyncE2EHarness) -- no scenario calls CheckFinalityAsync,
// runTick, ScanRange, or OnLogObserved directly; every effect below is
// produced purely by mutating the deterministic fake chain and waiting
// for the real, already-running background loops to react to it, exactly
// as a real watcherd would.
// ---------------------------------------------------------------------

// 1: 2-of-3 quorum across different real ticks -- provider A confirms
// alone first (no settlement), provider B independently confirms the
// same (height, hash, facts) afterward (settlement).
func (h *asyncE2EHarness) scenarioTwoOfThreeQuorum(t *testing.T) {
	order, height, _, _ := h.commitCandidateAndWaitDetected(t, 3000_000000)

	nodeA := h.chain.nodeByName("A")
	nodeA.setFinalized(height)
	if !holdsFor(150*time.Millisecond, func() bool { return h.orderState(t, order.ExternalID) == "quoted" }) {
		t.Fatalf("order settled after only ONE provider confirmed -- a 2-of-3 quorum must never be satisfied by a single provider")
	}

	nodeB := h.chain.nodeByName("B")
	nodeB.setFinalized(height)
	h.waitOrderState(t, order.ExternalID, "funded", 5*time.Second)
}

// 2: no early settlement -- its own fresh candidate, confirmed by
// exactly one provider, must not fund the order.
func (h *asyncE2EHarness) scenarioNoEarlySettlement(t *testing.T) {
	order, height, _, _ := h.commitCandidateAndWaitDetected(t, 3000_000000)

	nodeA := h.chain.nodeByName("A")
	nodeA.setFinalized(height)
	if !holdsFor(200*time.Millisecond, func() bool { return h.orderState(t, order.ExternalID) == "quoted" }) {
		t.Fatalf("order funded before quorum was reached")
	}

	// Resolve it cleanly (bring in B) rather than leaving an unresolved
	// candidate parked in t.pending for the rest of the run.
	nodeB := h.chain.nodeByName("B")
	nodeB.setFinalized(height)
	h.waitOrderState(t, order.ExternalID, "funded", 5*time.Second)
}

// 3: same height, different block hash between two providers -- a
// terminal DISAGREEMENT_ALERT, never a settlement.
func (h *asyncE2EHarness) scenarioSameHeightDifferentHash(t *testing.T) {
	order, height, txHash, baseAt := h.commitCandidateAndWaitDetected(t, 3000_000000)
	log := transferLogE2E(height, txHash, addressTopicE2E(order.Address), 0, 3000_000000)

	// Diverge node B's own header at this height: a different block
	// timestamp produces a genuinely different computed hash, while
	// keeping the SAME log content (facts agree; only block identity
	// disagrees).
	nodeB := h.chain.nodeByName("B")
	nodeB.commitBlock(height, baseAt.Add(1*time.Hour), []types.Log{log})

	nodeA := h.chain.nodeByName("A")
	nodeA.setFinalized(height)
	nodeB.setFinalized(height)

	if !pollUntil(t, 5*time.Second, func() bool { _, ok := h.disagreementNote(order.ExternalID); return ok }) {
		t.Fatalf("expected a DISAGREEMENT_ALERT for a same-height/different-hash observation, none recorded")
	}
	if got := h.orderState(t, order.ExternalID); got != "quoted" {
		t.Fatalf("order state = %q after a hash disagreement, want quoted (must never settle)", got)
	}
}

// 4: same block hash, different decoded PRESENT facts between two
// providers -- also a terminal DISAGREEMENT_ALERT, never a settlement.
func (h *asyncE2EHarness) scenarioSameHashDifferentFacts(t *testing.T) {
	order, height, txHash, at := h.commitCandidateAndWaitDetected(t, 3000_000000)

	// Diverge node B's own logs at this height: the SAME timestamp (so
	// the computed hash is identical -- both providers agree on WHICH
	// block they mean) but a genuinely different decoded amount.
	differentAmountLog := transferLogE2E(height, txHash, addressTopicE2E(order.Address), 0, 9999_000000)
	nodeB := h.chain.nodeByName("B")
	nodeB.commitBlock(height, at, []types.Log{differentAmountLog})

	nodeA := h.chain.nodeByName("A")
	nodeA.setFinalized(height)
	nodeB.setFinalized(height)

	if !pollUntil(t, 5*time.Second, func() bool { _, ok := h.disagreementNote(order.ExternalID); return ok }) {
		t.Fatalf("expected a DISAGREEMENT_ALERT for a same-hash/different-facts observation, none recorded")
	}
	if got := h.orderState(t, order.ExternalID); got != "quoted" {
		t.Fatalf("order state = %q after a facts disagreement, want quoted (must never settle)", got)
	}
}

// 5: PRESENT (from two providers) + ABSENT (from a third, at the SAME
// hash) -- not a contradiction; the PRESENT quorum must still win.
func (h *asyncE2EHarness) scenarioPresentPlusAbsentSameHash(t *testing.T) {
	order, height, _, at := h.commitCandidateAndWaitDetected(t, 3000_000000)

	// Node B independently re-queries and finds NOTHING at this exact
	// height -- SAME hash as A/C (same timestamp), empty logs: indexer
	// lag, not a fork.
	nodeB := h.chain.nodeByName("B")
	nodeB.commitBlock(height, at, nil)

	nodeA := h.chain.nodeByName("A")
	nodeC := h.chain.nodeByName("C")
	nodeA.setFinalized(height)
	nodeB.setFinalized(height)
	nodeC.setFinalized(height)

	h.waitOrderState(t, order.ExternalID, "funded", 5*time.Second)
	if _, ok := h.disagreementNote(order.ExternalID); ok {
		t.Fatalf("PRESENT+ABSENT at the same hash must NOT be recorded as a disagreement")
	}
}

// 6: two independent, corroborated ABSENT observations reach quorum --
// ASYNC_NOT_FOUND. The candidate is dropped and never funded.
func (h *asyncE2EHarness) scenarioAsyncNotFound(t *testing.T) {
	order, height, _, at := h.commitCandidateAndWaitDetected(t, 3000_000000)
	pendingAfterDetection := h.tracker.PendingCount()

	nodeA := h.chain.nodeByName("A")
	nodeB := h.chain.nodeByName("B")
	nodeA.commitBlock(height, at, nil)
	nodeB.commitBlock(height, at, nil)
	nodeA.setFinalized(height)
	nodeB.setFinalized(height)

	if !pollUntil(t, 5*time.Second, func() bool { return h.tracker.PendingCount() < pendingAfterDetection }) {
		t.Fatalf("candidate was never dropped (ASYNC_NOT_FOUND never reached)")
	}
	if !holdsFor(200*time.Millisecond, func() bool { return h.orderState(t, order.ExternalID) != "funded" }) {
		t.Fatalf("a dropped (ASYNC_NOT_FOUND) candidate must never be funded")
	}
	if got := h.orderState(t, order.ExternalID); got != "quoted" {
		t.Fatalf("order state = %q, want quoted (dropped, never funded)", got)
	}
}

// 7: a provider fails outright (total RPC failure, every call errors)
// and contributes no vote either way; once it recovers, quorum is
// reached correctly.
func (h *asyncE2EHarness) scenarioRPCFailureThenRecovery(t *testing.T) {
	order, height, _, _ := h.commitCandidateAndWaitDetected(t, 3000_000000)

	nodeA := h.chain.nodeByName("A")
	nodeB := h.chain.nodeByName("B")
	nodeA.setDark(true) // total RPC failure: every call errors, including FinalizedFrom itself
	nodeB.setFinalized(height)

	if !holdsFor(200*time.Millisecond, func() bool { return h.orderState(t, order.ExternalID) == "quoted" }) {
		t.Fatalf("order settled from a single provider (B) while the other (A) was failing -- a failure must never count as a vote either way")
	}

	nodeA.setDark(false)
	nodeA.setFinalized(height)
	h.waitOrderState(t, order.ExternalID, "funded", 5*time.Second)
}

// 8: the chain reorgs BEFORE any provider ever independently confirms
// the candidate -- every provider's first-ever re-verification of this
// height sees the SAME, already-reorged (empty) content, converging to
// corroborated absence (ASYNC_NOT_FOUND) rather than a cross-provider
// disagreement (nothing was ever confirmed PRESENT for anyone to
// disagree with) -- distinct from scenario 3, where providers actively
// disagree with EACH OTHER.
func (h *asyncE2EHarness) scenarioReorgBeforeQuorum(t *testing.T) {
	order, height, _, at := h.commitCandidateAndWaitDetected(t, 3000_000000)
	pendingAfterDetection := h.tracker.PendingCount()

	reorgAt := at.Add(2 * time.Hour)
	h.chain.commitToAll(height, reorgAt, nil)

	nodeA := h.chain.nodeByName("A")
	nodeB := h.chain.nodeByName("B")
	nodeA.setFinalized(height)
	nodeB.setFinalized(height)

	if !pollUntil(t, 5*time.Second, func() bool { return h.tracker.PendingCount() < pendingAfterDetection }) {
		t.Fatalf("candidate was never dropped after a pre-quorum reorg")
	}
	if !holdsFor(200*time.Millisecond, func() bool { return h.orderState(t, order.ExternalID) != "funded" }) {
		t.Fatalf("a reorged-away-before-quorum candidate must never be funded")
	}
	if got := h.orderState(t, order.ExternalID); got != "quoted" {
		t.Fatalf("order state = %q, want quoted (dropped by pre-quorum reorg, never funded)", got)
	}
}

// 9: quorum is reached and the candidate finalizes first; the chain THEN
// reorgs it away -- the EXISTING, unmodified checkPostFinalReorgs (called
// explicitly by CheckFinalityAsync) must still catch it.
func (h *asyncE2EHarness) scenarioReorgAfterQuorum(t *testing.T) {
	order, height, _, at := h.commitCandidateAndWaitDetected(t, 3000_000000)

	nodeA := h.chain.nodeByName("A")
	nodeB := h.chain.nodeByName("B")
	nodeA.setFinalized(height)
	nodeB.setFinalized(height)
	h.waitOrderState(t, order.ExternalID, "funded", 5*time.Second)

	if h.reorgReported(order.ExternalID) {
		t.Fatalf("ReportReorg fired before any reorg happened")
	}

	// Post-final reorg: the joint LogsAt (what checkPostFinalReorgs
	// re-verifies every already-finalized candidate against) stops
	// finding this deposit at its own height.
	nodeA.commitBlock(height, at, nil)
	nodeB.commitBlock(height, at, nil)

	if !pollUntil(t, 5*time.Second, func() bool { return h.reorgReported(order.ExternalID) }) {
		t.Fatalf("the EXISTING post-finalization reorg protection never fired under Design B")
	}
}

// 10: after quorum, many further real ticks run against unchanged
// provider state -- OnFinal must be called exactly once, ever.
func (h *asyncE2EHarness) scenarioNoDoubleFinalization(t *testing.T) {
	order, height, _, _ := h.commitCandidateAndWaitDetected(t, 3000_000000)

	nodeA := h.chain.nodeByName("A")
	nodeB := h.chain.nodeByName("B")
	nodeA.setFinalized(height)
	nodeB.setFinalized(height)
	h.waitOrderState(t, order.ExternalID, "funded", 5*time.Second)

	if got := h.finalCallCount(order.ExternalID); got != 1 {
		t.Fatalf("OnFinal called %d times immediately after quorum, want exactly 1", got)
	}

	// Let many more real ticks run -- both providers' own state is
	// unchanged, so every subsequent tick re-observes the identical,
	// already-finalized candidate.
	time.Sleep(300 * time.Millisecond)

	if got := h.finalCallCount(order.ExternalID); got != 1 {
		t.Fatalf("OnFinal called %d times after many additional real ticks, want still exactly 1 (no double-finalization)", got)
	}
}

// TestAsyncE2E_DesignB_RealLifecycle is the Design B real-lifecycle E2E
// gate -- see this file's own top comment for the full chain it proves.
// One shared harness (one real chain.Pool, one real finality.Tracker,
// one real, already-running candidates.RunLoop + chain.RunIngestionLoop
// pair, one real ledgerd) drives all ten scenarios sequentially, each
// against its own freshly created order/candidate at its own
// never-reused height -- exactly like a real watcherd handling many
// deposits over its own continuous lifetime, not ten isolated processes.
func TestAsyncE2E_DesignB_RealLifecycle(t *testing.T) {
	h := newAsyncE2EHarness(t)

	t.Run("1_TwoOfThreeQuorumAcrossDifferentTicks", h.scenarioTwoOfThreeQuorum)
	t.Run("2_NoEarlySettlement", h.scenarioNoEarlySettlement)
	t.Run("3_SameHeightDifferentHash_DisagreementAlert", h.scenarioSameHeightDifferentHash)
	t.Run("4_SameHashDifferentFacts_DisagreementAlert", h.scenarioSameHashDifferentFacts)
	t.Run("5_PresentPlusAbsentSameHash_NotAContradiction", h.scenarioPresentPlusAbsentSameHash)
	t.Run("6_AsyncNotFound_Dropped", h.scenarioAsyncNotFound)
	t.Run("7_RPCFailureThenRecovery", h.scenarioRPCFailureThenRecovery)
	t.Run("8_ReorgBeforeQuorum", h.scenarioReorgBeforeQuorum)
	t.Run("9_ReorgAfterQuorum_ExistingProtectionStillRuns", h.scenarioReorgAfterQuorum)
	t.Run("10_NoDoubleFinalization", h.scenarioNoDoubleFinalization)
}
