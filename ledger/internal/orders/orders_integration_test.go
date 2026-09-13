//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the C1.5 ship-gate proof.
//
// expectedTransitions below is written independently of the production
// transitionTable in transitions.go, copied straight from the build
// spec's table rather than imported from the package under test. The
// point of the exhaustive cross-product test is to check that
// orders.Transition's actual runtime behavior matches the SPEC, not that
// the unexported map agrees with itself.
package orders_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cryptorand "crypto/rand"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/orders"
)

var runID = randomHex()

func randomHex() string {
	b := make([]byte, 4)
	_, _ = cryptorand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0x0f]
	}
	return string(out)
}

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

func applyMigrations(t *testing.T, url string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(sqlDB, migrationsDir(t)))
}

// prepareTestPool does the setup every orders test needs regardless of
// how its pool was constructed: seed the fixed chart of accounts, wire up
// the halt cache Transition now requires (SetHaltCache is a package-level
// call, so every test process needs it, not just ones that touch halt
// directly), and reset system_state to unhalted. That last part matters
// even for tests that never mention halting: system_state is a single
// shared row in the persistent test database, so without resetting it, a
// halt left behind by an earlier reorg-scenario-B test (in this run or an
// earlier one) would make every halt-blocked transition in an unrelated
// test fail with ErrSystemHalted for reasons that test never caused.
func prepareTestPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	require.NoError(t, accounts.Seed(ctx, pool))
	cache, err := halt.NewCache(ctx, pool)
	require.NoError(t, err)
	orders.SetHaltCache(cache)
	_, err = pool.Exec(ctx, `
		UPDATE system_state
		SET halted = false, halt_reason = NULL, halt_detail = NULL, halted_at = NULL, halted_by = NULL
		WHERE id = 1
	`)
	require.NoError(t, err)
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testDatabaseURL(t)
	applyMigrations(t, url)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	prepareTestPool(t, ctx, pool)
	return pool
}

func testPoolWithMaxConns(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	url := testDatabaseURL(t)
	applyMigrations(t, url)

	cfg, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	cfg.MaxConns = maxConns

	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	prepareTestPool(t, ctx, pool)
	return pool
}

// freshIsolatedPool creates a throwaway database on the same cluster as
// LEDGER_TEST_DATABASE_URL and returns a pool for it, with cleanup
// registered. Needed by any test that manipulates system_state's halted
// flag more than transiently: go test parallelizes across packages by
// default, so a halt this test sets (or clears) against the shared
// ledger_test database can be observed -- or clobbered -- by another
// package's concurrently running test. prepareTestPool's own reset-at-
// start only protects against a halt left over from an EARLIER test; it
// can't protect against one set by a test running RIGHT NOW in a
// different process.
func freshIsolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseURL := testDatabaseURL(t)
	u, err := url.Parse(baseURL)
	require.NoError(t, err)
	adminURL := *u
	adminURL.Path = "/postgres"
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, adminURL.String())
	require.NoError(t, err)
	dbName := fmt.Sprintf("orders_isolated_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	adminPool.Close()

	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), adminURL.String())
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})

	freshURL := *u
	freshURL.Path = "/" + dbName
	applyMigrations(t, freshURL.String())

	pool, err := pgxpool.New(ctx, freshURL.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	prepareTestPool(t, ctx, pool)
	return pool
}

func withTx(t *testing.T, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) // no-op once committed

	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// runInTx is withTx without any *testing.T dependency, for use from
// worker goroutines, where require/t.Fatal are not safe to call.
func runInTx(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var extIDSeq, accountSeq, entrySeq int64

func newQuotedOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool) orders.Order {
	t.Helper()
	n := atomic.AddInt64(&extIDSeq, 1)
	o, err := orders.Create(ctx, pool, orders.CreateParams{
		ExternalID:       fmt.Sprintf("ext:%s:%s:%d", t.Name(), runID, n),
		CustomerID:       "cust-1",
		Tier:             orders.Standard,
		AmountIn:         money.Amount{Asset: money.USDT_BEP20, Units: 3000_000000},
		AmountOut:        money.Amount{Asset: money.USDT_TRC20, Units: 2990_700000},
		FeeUnits:         money.Amount{Asset: money.USDT_TRC20, Units: 7_500000},
		NetworkFeeUnits:  money.Amount{Asset: money.USDT_TRC20, Units: 1_800000},
		RecipientAddress: "T-recipient-address",
		QuotedAt:         time.Now(),
		QuoteExpiresAt:   time.Now().Add(90 * time.Second),
	})
	require.NoError(t, err)
	return o
}

func twoTRXAccounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	n := atomic.AddInt64(&accountSeq, 1)
	a := fmt.Sprintf("liability:customer:%s:%s:orderentry:%d:a", t.Name(), runID, n)
	b := fmt.Sprintf("liability:customer:%s:%s:orderentry:%d:b", t.Name(), runID, n)
	_, err := accounts.Create(ctx, pool, a, accounts.Liability, money.TRX)
	require.NoError(t, err)
	_, err = accounts.Create(ctx, pool, b, accounts.Liability, money.TRX)
	require.NoError(t, err)
	return a, b
}

func simpleEntry(t *testing.T, acc1, acc2 string) journal.EntryRequest {
	t.Helper()
	n := atomic.AddInt64(&entrySeq, 1)
	return journal.EntryRequest{
		IdempotencyKey: fmt.Sprintf("orders_test:%s:%s:%d", runID, t.Name(), n),
		EntryType:      "order_transition_test",
		Actor:          "test",
		OccurredAt:     time.Now(),
		Lines: []journal.Line{
			{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 1_000000}},
			{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -1_000000}},
		},
	}
}

func transitionRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orderID int64) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM order_transitions WHERE order_id = $1", orderID).Scan(&n))
	return n
}

// expectedRule and expectedTransitions are the independent, hand-copied
// version of the build spec's transition table (11 pairs, including
// which are halt-blocked). allStates and pathToState derive the walk
// needed to place a fresh order at any given state, purely from this
// same independent table.
type expectedRule struct {
	RequiresEntry bool
	HaltBlocked   bool
}

var expectedTransitions = map[orders.State]map[orders.State]expectedRule{
	orders.Quoted: {
		orders.Funded:  {RequiresEntry: true, HaltBlocked: false},
		orders.Expired: {RequiresEntry: false, HaltBlocked: false},
	},
	orders.Funded: {
		orders.Screened: {RequiresEntry: false, HaltBlocked: false},
		orders.Held:     {RequiresEntry: false, HaltBlocked: false},
		orders.Refunded: {RequiresEntry: true, HaltBlocked: true},
		orders.Quoted:   {RequiresEntry: true, HaltBlocked: false},
	},
	orders.Held: {
		orders.Screened: {RequiresEntry: false, HaltBlocked: false},
		orders.Refunded: {RequiresEntry: true, HaltBlocked: true},
	},
	orders.Screened: {
		orders.Dispatching: {RequiresEntry: true, HaltBlocked: true},
	},
	orders.Dispatching: {
		orders.Settled: {RequiresEntry: true, HaltBlocked: true},
		orders.Held:    {RequiresEntry: true, HaltBlocked: false},
	},
}

var allStates = []orders.State{
	orders.Quoted, orders.Funded, orders.Screened, orders.Dispatching,
	orders.Settled, orders.Held, orders.Refunded, orders.Expired,
}

var pathToState = map[orders.State][]orders.State{
	orders.Quoted:      nil,
	orders.Funded:      {orders.Funded},
	orders.Screened:    {orders.Funded, orders.Screened},
	orders.Held:        {orders.Funded, orders.Held},
	orders.Dispatching: {orders.Funded, orders.Screened, orders.Dispatching},
	orders.Settled:     {orders.Funded, orders.Screened, orders.Dispatching, orders.Settled},
	orders.Refunded:    {orders.Funded, orders.Refunded},
	orders.Expired:     {orders.Expired},
}

// advanceToState creates a fresh order and walks it, via real,
// legitimate Transition calls, from Quoted to target.
func advanceToState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, target orders.State) orders.Order {
	t.Helper()
	order := newQuotedOrder(t, ctx, pool)
	current := order.State
	version := order.Version

	for _, next := range pathToState[target] {
		rule, ok := expectedTransitions[current][next]
		require.Truef(t, ok, "no expected rule for %s -> %s while walking to %s", current, next, target)

		var entry *journal.EntryRequest
		if rule.RequiresEntry {
			acc1, acc2 := twoTRXAccounts(t, ctx, pool)
			e := simpleEntry(t, acc1, acc2)
			entry = &e
		}

		var updated orders.Order
		err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = orders.Transition(ctx, tx, order.ID, next, version, orders.TransitionParams{
				Actor:      "test",
				Reason:     "advance to " + string(target),
				OccurredAt: time.Now(),
				Entry:      entry,
			})
			return err
		})
		require.NoErrorf(t, err, "advancing %s -> %s while walking to %s", current, next, target)

		order = updated
		current = updated.State
		version = updated.Version
	}
	return order
}

// TestFullStateCrossProduct is C1.5's headline acceptance criterion: over
// the full 8x8 state cross product, exactly the 11 pairs in
// expectedTransitions succeed and the other 53 return ErrIllegalTransition
// while writing nothing -- no order_transitions row, no change to the
// order's own state or version. A settled order rejecting every further
// transition is a special case of this: Settled has zero legal outgoing
// pairs, so all 8 of its targets are exercised here as illegal.
func TestFullStateCrossProduct(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var legalCount, illegalCount int

	for _, from := range allStates {
		probe := advanceToState(t, ctx, pool, from)
		beforeState, beforeVersion := probe.State, probe.Version

		for _, to := range allStates {
			if _, ok := expectedTransitions[from][to]; ok {
				legalCount++
				continue // exercised for real, separately, below
			}
			illegalCount++

			before := transitionRowCount(t, ctx, pool, probe.ID)
			err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orders.Transition(ctx, tx, probe.ID, to, beforeVersion, orders.TransitionParams{
					Actor: "test", Reason: "illegal probe", OccurredAt: time.Now(),
				})
				return err
			})
			require.ErrorIsf(t, err, orders.ErrIllegalTransition, "%s -> %s should be illegal", from, to)

			after := transitionRowCount(t, ctx, pool, probe.ID)
			require.Equalf(t, before, after, "%s -> %s: illegal attempt wrote an order_transitions row", from, to)

			reloaded, err := orders.Get(ctx, pool, probe.ID)
			require.NoError(t, err)
			require.Equalf(t, beforeState, reloaded.State, "%s -> %s: illegal attempt changed order state", from, to)
			require.Equalf(t, beforeVersion, reloaded.Version, "%s -> %s: illegal attempt changed order version", from, to)
		}
	}

	require.Equal(t, 11, legalCount, "expected exactly 11 legal pairs in the 8x8 cross product")
	require.Equal(t, 53, illegalCount, "expected exactly 53 illegal pairs in the 8x8 cross product")

	for from, targets := range expectedTransitions {
		for to, rule := range targets {
			order := advanceToState(t, ctx, pool, from)

			var entry *journal.EntryRequest
			if rule.RequiresEntry {
				acc1, acc2 := twoTRXAccounts(t, ctx, pool)
				e := simpleEntry(t, acc1, acc2)
				entry = &e
			}

			var updated orders.Order
			err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				updated, err = orders.Transition(ctx, tx, order.ID, to, order.Version, orders.TransitionParams{
					Actor: "test", Reason: "legal transition test", OccurredAt: time.Now(), Entry: entry,
				})
				return err
			})
			require.NoErrorf(t, err, "%s -> %s should be legal", from, to)
			require.Equal(t, to, updated.State)
			require.Equal(t, order.Version+1, updated.Version)
		}
	}
}

// TestHaltBlocksExactlyTheDocumentedPairs closes a gap
// TestHaltBlocksOnlyHaltBlockedTransitions (halt_enforcement_integration_test.go)
// leaves open: that test proves the halt-check mechanism works for
// exactly one halt-blocked pair (screened -> dispatching) and one
// non-blocked pair (quoted -> funded). Nothing previously verified the
// other three documented halt-blocked pairs -- funded -> refunded,
// held -> refunded, dispatching -> settled -- are actually rejected
// while halted, nor that the remaining non-blocked pairs actually keep
// working. This drives all 11, against expectedTransitions' independent
// HaltBlocked bit (hand-copied from the build spec's table, not read
// back from transitionTable), so a wrong HaltBlocked flag in production
// code -- letting money leave while halted, or blocking something that
// must keep working while halted -- would fail here even if it agreed
// with itself everywhere else.
func TestHaltBlocksExactlyTheDocumentedPairs(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()

	// Every order is advanced to its `from` state, and every required
	// entry's accounts prepared, BEFORE the halt is ever set: reaching
	// Dispatching, for example, must itself cross screened ->
	// dispatching, which is halt-blocked, and doing that walk while
	// already halted would fail for a reason unrelated to what this
	// test checks. All 11 attempts then run against ONE halted window,
	// since halting only blocks the 4 documented pairs -- the other 7
	// are expected to keep working while halted, not just once it clears.
	type prepared struct {
		from, to orders.State
		rule     expectedRule
		order    orders.Order
		entry    *journal.EntryRequest
	}
	var cases []prepared
	for from, targets := range expectedTransitions {
		for to, rule := range targets {
			order := advanceToState(t, ctx, pool, from)

			var entry *journal.EntryRequest
			if rule.RequiresEntry {
				acc1, acc2 := twoTRXAccounts(t, ctx, pool)
				e := simpleEntry(t, acc1, acc2)
				entry = &e
			}
			cases = append(cases, prepared{from: from, to: to, rule: rule, order: order, entry: entry})
		}
	}

	require.NoError(t, halt.Set(ctx, pool, halt.SetParams{Reason: "TEST_HALT", Actor: "test"}))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE system_state SET halted = false WHERE id = 1`)
	})
	// The in-memory halt cache (C1.7) is deliberately at most 1 second
	// stale -- a fast rejection is only correct if a caller can tolerate
	// exactly this. Every order above was advanced (and, for Dispatching,
	// had the cache read at least once) before Set ran, so without this
	// wait the very first halt-blocked check below could still observe
	// a cached "not halted" read from moments earlier and wrongly succeed.
	time.Sleep(1100 * time.Millisecond)

	for _, c := range cases {
		err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orders.Transition(ctx, tx, c.order.ID, c.to, c.order.Version, orders.TransitionParams{
				Actor: "test", Reason: "halt-blocking exhaustive check", OccurredAt: time.Now(), Entry: c.entry,
			})
			return err
		})

		if c.rule.HaltBlocked {
			require.ErrorIsf(t, err, orders.ErrSystemHalted,
				"%s -> %s is documented halt-blocked but was not rejected while halted", c.from, c.to)
		} else {
			require.NoErrorf(t, err,
				"%s -> %s is documented NOT halt-blocked but was rejected while halted: %v", c.from, c.to, err)
		}
	}
}

func TestTransitionRequiringEntryWithoutOneFails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := advanceToState(t, ctx, pool, orders.Funded) // Funded -> Refunded requires an entry

	before := transitionRowCount(t, ctx, pool, order.ID)
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.Transition(ctx, tx, order.ID, orders.Refunded, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "no entry provided", OccurredAt: time.Now(),
		})
		return err
	})
	require.ErrorIs(t, err, orders.ErrEntryRequired)

	after := transitionRowCount(t, ctx, pool, order.ID)
	require.Equal(t, before, after, "must write nothing when a required entry is missing")

	reloaded, err := orders.Get(ctx, pool, order.ID)
	require.NoError(t, err)
	require.Equal(t, order.State, reloaded.State)
	require.Equal(t, order.Version, reloaded.Version)
}

func TestTransitionWithUnbalancedEntryFailsAndLeavesOrderUnchanged(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := advanceToState(t, ctx, pool, orders.Screened) // Screened -> Dispatching requires an entry

	acc1, acc2 := twoTRXAccounts(t, ctx, pool)
	badEntry := journal.EntryRequest{
		IdempotencyKey: fmt.Sprintf("orders_test:%s:%s:unbalanced", runID, t.Name()),
		EntryType:      "bad",
		Actor:          "test",
		OccurredAt:     time.Now(),
		Lines: []journal.Line{
			{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5}},
			{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -3}}, // imbalance
		},
	}

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.Transition(ctx, tx, order.ID, orders.Dispatching, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "unbalanced entry", OccurredAt: time.Now(), Entry: &badEntry,
		})
		return err
	})
	require.Error(t, err)
	require.ErrorIs(t, err, journal.ErrUnbalanced)

	reloaded, err := orders.Get(ctx, pool, order.ID)
	require.NoError(t, err)
	require.Equal(t, order.State, reloaded.State, "order state must be unchanged after a failed transition")
	require.Equal(t, order.Version, reloaded.Version, "order version must be unchanged after a failed transition")
}

// TestConcurrentTransitionSameVersionExactlyOneSucceeds is C1.5's
// concurrency acceptance criterion: 50 goroutines racing Transition with
// the same expectedVersion must produce exactly one success and 49
// ErrVersionConflict -- never a retry-in-place, never more than one
// winner.
func TestConcurrentTransitionSameVersionExactlyOneSucceeds(t *testing.T) {
	pool := testPoolWithMaxConns(t, 50)
	ctx := context.Background()
	order := advanceToState(t, ctx, pool, orders.Quoted) // Quoted -> Expired needs no entry

	const n = 50
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for g := 0; g < n; g++ {
		go func(g int) {
			defer wg.Done()
			errs[g] = runInTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orders.Transition(ctx, tx, order.ID, orders.Expired, order.Version, orders.TransitionParams{
					Actor: "test", Reason: "concurrency test", OccurredAt: time.Now(),
				})
				return err
			})
		}(g)
	}
	wg.Wait()

	var successCount, conflictCount int
	for i, err := range errs {
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, orders.ErrVersionConflict):
			conflictCount++
		default:
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	require.Equal(t, 1, successCount, "exactly one of 50 concurrent same-version transitions must succeed")
	require.Equal(t, n-1, conflictCount, "the other 49 must all get ErrVersionConflict")

	reloaded, err := orders.Get(ctx, pool, order.ID)
	require.NoError(t, err)
	require.Equal(t, orders.Expired, reloaded.State)
	require.Equal(t, order.Version+1, reloaded.Version)
}

// ---------------------------------------------------------------------
// sender_address (added for C3/screening -- see
// docs/03-build/c3-screening-build-prompts.md's "Read this first" and
// migrations/0009_orders_sender_address.sql).
// ---------------------------------------------------------------------

func TestTransition_SenderAddressSetOnFundedAndPersists(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := newQuotedOrder(t, ctx, pool)
	require.Nil(t, order.SenderAddress, "a freshly quoted order must have no sender_address yet")

	acc1, acc2 := twoTRXAccounts(t, ctx, pool)
	entry := simpleEntry(t, acc1, acc2)
	sender := "0xSenderAddress0000000000000000000000001"

	var updated orders.Order
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = orders.Transition(ctx, tx, order.ID, orders.Funded, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "fund", OccurredAt: time.Now(),
			Entry: &entry, SenderAddress: &sender,
		})
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, updated.SenderAddress)
	require.Equal(t, sender, *updated.SenderAddress)

	// Persisted, not just returned in-memory: a fresh read sees it too.
	reloaded, err := orders.GetByExternalID(ctx, pool, order.ExternalID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.SenderAddress)
	require.Equal(t, sender, *reloaded.SenderAddress)
}

func TestTransition_SenderAddressRejectedOnNonFundedTarget(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := newQuotedOrder(t, ctx, pool)
	sender := "0xSenderAddress0000000000000000000000002"

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.Transition(ctx, tx, order.ID, orders.Expired, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "not a funding transition", OccurredAt: time.Now(),
			SenderAddress: &sender,
		})
		return err
	})
	require.ErrorIs(t, err, orders.ErrInvalidParams)

	// Rejected before anything was written -- still Quoted, no sender_address.
	reloaded, err := orders.Get(ctx, pool, order.ID)
	require.NoError(t, err)
	require.Equal(t, orders.Quoted, reloaded.State)
	require.Nil(t, reloaded.SenderAddress)
}

// TestTransition_SenderAddressOverwrittenAfterReorgRefund covers the one
// path that can legitimately call the quoted->funded write twice for the
// same order: a reorg reverses funded->quoted (see transitionTable), and
// a later re-detected deposit -- possibly from a different sender than
// the one that got reorged out -- funds it again. The second value must
// win; this is not "immutable" in the sense of "never changes", only in
// the sense of "nothing but this one transition ever writes it".
func TestTransition_SenderAddressOverwrittenAfterReorgRefund(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := newQuotedOrder(t, ctx, pool)

	acc1, acc2 := twoTRXAccounts(t, ctx, pool)
	firstEntry := simpleEntry(t, acc1, acc2)
	firstSender := "0xFirstSender000000000000000000000000001"

	var funded orders.Order
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		funded, err = orders.Transition(ctx, tx, order.ID, orders.Funded, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "fund", OccurredAt: time.Now(),
			Entry: &firstEntry, SenderAddress: &firstSender,
		})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, firstSender, *funded.SenderAddress)

	// Reorg: funded -> quoted (reversal), requires an entry, not halt-blocked.
	acc3, acc4 := twoTRXAccounts(t, ctx, pool)
	reversalEntry := simpleEntry(t, acc3, acc4)
	var reverted orders.Order
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reverted, err = orders.Transition(ctx, tx, order.ID, orders.Quoted, funded.Version, orders.TransitionParams{
			Actor: "test", Reason: "reorg", OccurredAt: time.Now(),
			Entry: &reversalEntry,
		})
		return err
	})
	require.NoError(t, err)
	// The reversal itself never touches sender_address -- only a
	// quoted->funded transition ever writes this column.
	require.NotNil(t, reverted.SenderAddress)
	require.Equal(t, firstSender, *reverted.SenderAddress)

	acc5, acc6 := twoTRXAccounts(t, ctx, pool)
	secondEntry := simpleEntry(t, acc5, acc6)
	secondSender := "0xSecondSender00000000000000000000000002"
	var refunded orders.Order
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		refunded, err = orders.Transition(ctx, tx, order.ID, orders.Funded, reverted.Version, orders.TransitionParams{
			Actor: "test", Reason: "re-fund after reorg", OccurredAt: time.Now(),
			Entry: &secondEntry, SenderAddress: &secondSender,
		})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, secondSender, *refunded.SenderAddress, "the second funding event's sender must win")
}

// ---------------------------------------------------------------------
// ListByStateAfter / Cursor (added for C3/screening discovery -- §A of
// docs/03-build/c3-screening-build-prompts.md).
// ---------------------------------------------------------------------

func TestListByStateAfter_FiltersByStateAndPaginates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Three funded orders this test owns, created in a known order, plus
	// one quoted order that must never appear in a state=funded query.
	var funded []orders.Order
	for i := 0; i < 3; i++ {
		o := advanceToState(t, ctx, pool, orders.Funded)
		funded = append(funded, o)
		time.Sleep(2 * time.Millisecond) // force distinct updated_at for a stable order
	}
	quoted := newQuotedOrder(t, ctx, pool)

	// Page 1: limit 2.
	page1, err := orders.ListByStateAfter(ctx, pool, orders.Funded, nil, 2)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(page1), 1) // shared DB: other tests' funded orders may also be present, but ours must appear in order

	for _, o := range page1 {
		require.NotEqual(t, quoted.ID, o.ID, "a quoted order must never appear in a state=funded listing")
	}

	// Walk forward with the cursor until we've seen all three of ours,
	// or run out of pages -- proves cursor pagination advances and never
	// re-returns an already-seen row.
	seen := map[int64]bool{}
	var cursor *orders.Cursor
	for pages := 0; pages < 100; pages++ {
		page, err := orders.ListByStateAfter(ctx, pool, orders.Funded, cursor, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, o := range page {
			require.False(t, seen[o.ID], "order %d returned twice across pages", o.ID)
			seen[o.ID] = true
		}
		last := page[len(page)-1]
		cursor = &orders.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
	}
	for _, o := range funded {
		require.True(t, seen[o.ID], "order %d (state=funded) was never returned by any page", o.ID)
	}
}

// TestCreate_RelayTierBothDirectionsRoundTrip is Model F's own
// additive-migration proof, for BOTH directions -- migration 0010 added
// 'RELAY' to the real Postgres order_tier ENUM, migration 0011 added
// relay_direction (and its own tier-consistency CHECK constraint), and
// orders.Relay/allTiers plus CreateParams.validate's new branch made
// both a Go-side and a database-side reality. This is not a redundant
// check: an earlier version of this same change only fixed
// CreateParams.validate and missed that scanOrder/Create's own INSERT
// hard-coded the asset pairing with no way to record which direction a
// RELAY order actually is -- a real bug this exact round-trip assertion
// (not just "Create returns no error") is what would have caught, since
// the corruption only shows up on READ BACK, not on write.
//
// Everything else about a RELAY order (state machine, transitions,
// reconciliation) is intentionally untested here: orders.Transition
// never branches on Tier (confirmed by exhaustive grep before this tier
// was added), so the existing State-keyed coverage above already
// exercises a RELAY order's own transitions identically to every other
// tier's.
func TestCreate_RelayTierBothDirectionsRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	cases := []struct {
		name              string
		amountInAsset     money.Asset
		amountOutAsset    money.Asset
		wantRecipientAddr string
	}{
		{"TRC20 in, BEP20 out (the primary happy-flow example)", money.USDT_TRC20, money.USDT_BEP20, "0xcustomer-bep20-address"},
		{"BEP20 in, TRC20 out (the mirror direction)", money.USDT_BEP20, money.USDT_TRC20, "Tcustomer-trc20-address"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := atomic.AddInt64(&extIDSeq, 1)
			o, err := orders.Create(ctx, pool, orders.CreateParams{
				ExternalID: fmt.Sprintf("ext:%s:%s:%d", t.Name(), runID, n),
				CustomerID: "cust-relay-1",
				Tier:       orders.Relay,
				AmountIn:   money.Amount{Asset: tc.amountInAsset, Units: 100_000000},
				AmountOut:  money.Amount{Asset: tc.amountOutAsset, Units: 99_700000},
				// fee_units/network_fee_units are in amount_in's asset for
				// RELAY -- relayd withholds its commission from the
				// deposited asset before forwarding, since the upstream
				// vendor pays the customer's destination wallet directly.
				FeeUnits:         money.Amount{Asset: tc.amountInAsset, Units: 300000},
				NetworkFeeUnits:  money.Amount{Asset: tc.amountInAsset, Units: 0},
				RecipientAddress: tc.wantRecipientAddr,
				QuotedAt:         time.Now(),
				QuoteExpiresAt:   time.Now().Add(90 * time.Second),
			})
			require.NoError(t, err)
			require.Equal(t, orders.Relay, o.Tier)
			require.Equal(t, tc.amountInAsset, o.AmountIn.Asset, "Create's own returned order")
			require.Equal(t, tc.amountOutAsset, o.AmountOut.Asset, "Create's own returned order")

			// The real assertion: re-fetch from a FRESH query, proving the
			// direction was actually persisted and correctly reconstructed
			// by scanOrder, not just correct on the value Create happened
			// to return from its own RETURNING clause in the same call.
			fetched, err := orders.GetByExternalID(ctx, pool, o.ExternalID)
			require.NoError(t, err)
			require.Equal(t, orders.Relay, fetched.Tier)
			require.Equal(t, orders.Quoted, fetched.State)
			require.Equal(t, tc.amountInAsset, fetched.AmountIn.Asset, "re-fetched order's amount_in asset")
			require.Equal(t, tc.amountOutAsset, fetched.AmountOut.Asset, "re-fetched order's amount_out asset")
			require.Equal(t, tc.amountInAsset, fetched.FeeUnits.Asset, "re-fetched order's fee_units asset")
		})
	}
}

// TestCreate_RelayTierRejectsInvalidAssetPairing confirms
// CreateParams.validate's new branch actually rejects a same-asset (or
// otherwise nonsensical) pairing for RELAY, rather than only accepting
// the two legal combinations by accident.
func TestCreate_RelayTierRejectsInvalidAssetPairing(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	n := atomic.AddInt64(&extIDSeq, 1)

	_, err := orders.Create(ctx, pool, orders.CreateParams{
		ExternalID:       fmt.Sprintf("ext:%s:%s:%d", t.Name(), runID, n),
		CustomerID:       "cust-relay-1",
		Tier:             orders.Relay,
		AmountIn:         money.Amount{Asset: money.USDT_TRC20, Units: 100_000000},
		AmountOut:        money.Amount{Asset: money.USDT_TRC20, Units: 99_700000}, // same asset both sides -- invalid
		FeeUnits:         money.Amount{Asset: money.USDT_TRC20, Units: 300000},
		NetworkFeeUnits:  money.Amount{Asset: money.USDT_TRC20, Units: 0},
		RecipientAddress: "T-some-address",
		QuotedAt:         time.Now(),
		QuoteExpiresAt:   time.Now().Add(90 * time.Second),
	})
	require.ErrorIs(t, err, orders.ErrInvalidParams)
}

// TestCreate_NonRelayTierRejectsRelayDirectionLeakage is the mirror
// check: migration 0011's own orders_relay_direction_matches_tier CHECK
// constraint must reject relay_direction being set for a non-RELAY
// order at the database level, independent of the fact that Create's Go
// code never does this on its own -- defense in depth, same posture as
// every other cross-column invariant this schema enforces at the DB
// layer (e.g. 0002's normal_side_matches_type).
func TestCreate_NonRelayTierRejectsRelayDirectionLeakage(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	n := atomic.AddInt64(&extIDSeq, 1)

	_, err := pool.Exec(ctx, `
		INSERT INTO orders
			(external_id, customer_id, tier, state, amount_in, amount_out,
			 fee_units, network_fee_units, recipient_address, quoted_at, quote_expires_at,
			 version, relay_direction)
		VALUES ($1, $2, 'STANDARD', 'quoted', 100000000, 99700000, 300000, 0, $3, now(), now() + interval '90 seconds', 0, 'BEP20_TO_TRC20')
	`, fmt.Sprintf("ext:%s:%s:%d", t.Name(), runID, n), "cust-1", "T-some-address")
	require.Error(t, err, "a non-RELAY order with a non-NULL relay_direction must violate the CHECK constraint")
}
