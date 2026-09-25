package replay

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"sync"
	"time"

	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5/pgxpool"

	"relayd/internal/alert"
	"relayd/internal/db"
	"relayd/internal/driver"
	"relayd/internal/energy"
	"relayd/internal/ledgerclient"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/tronbroadcast"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
)

// trackedLeg is one leg a scenario created, for the FINAL ASSERTIONS to
// check against afterward -- not just what a single scenario locally
// asserted.
type trackedLeg struct {
	externalID      string
	orderID         int64
	wantTerminal    relay.Status // the status this leg is expected to end this run in
	wantAlertReason string       // non-empty only for legs expected to fire an alert
}

// harness bundles everything every scenario needs: relayd's own
// Postgres-backed relay.Store and a real *ledgerclient.Client against a
// real, running C1 (this harness's own ledgerFixture and that same
// client), plus in-process fakes for every OTHER external boundary --
// the upstream swap vendor, S1's SigningService, both chains'
// broadcast/finality, and the alert channel. Each scenario gets its own
// fresh Orchestrator with its own fakes (a fresh upstream.MockProvider
// with a scenario-unique provider name in particular -- sharing one
// across scenarios would collide on relay_legs_upstream_order_unique,
// found directly by internal/orchestrate's own integration tests this
// session), the same "don't let one scenario's forced failure leak into
// another's" reasoning every prior component's own replay harness
// already settled on.
type harness struct {
	ctx  context.Context
	rng  *rand.Rand
	seed int64

	pool   *db.Pool // relayd's own database
	ledger *ledgerFixture
	client *ledgerclient.Client // real client against the real, running C1
	store  *relay.Store

	mu   sync.Mutex
	seq  int
	legs []trackedLeg
}

func newHarness(ctx context.Context, cfg Config, pool *db.Pool, ledgerPool *pgxpool.Pool) *harness {
	return &harness{
		ctx: ctx, rng: rand.New(rand.NewSource(cfg.Seed)), seed: cfg.Seed,
		pool:   pool,
		ledger: newLedgerFixture(cfg.LedgerBaseURL, cfg.LedgerToken, ledgerPool),
		client: ledgerclient.New(cfg.LedgerBaseURL, cfg.LedgerToken),
		store:  relay.NewStore(pool),
	}
}

// nextID returns a deterministic (seeded), collision-free identifier for
// this run -- external ids, customer ids, upstream provider names.
func (h *harness) nextID(prefix string) string {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	h.mu.Unlock()
	return fmt.Sprintf("replay-%s-%d-%d", prefix, h.seed, seq)
}

func (h *harness) track(l trackedLeg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.legs = append(h.legs, l)
}

// --- fakes: every external boundary except C1 ---

// fakeEnergy always confirms immediately -- mirrors every sibling
// orchestrator's own identical replay/test fake.
type fakeEnergy struct{}

func (fakeEnergy) Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (energy.Reservation, error) {
	return energy.Reservation{Status: "CONFIRMED", EnergyUnits: units}, nil
}

// fakeChain stands in for a real TRON node -- deterministic block
// reference; BroadcastSigned returns a stable txid derived from the
// unsigned bytes.
type fakeChain struct {
	mu         sync.Mutex
	broadcasts int
}

func (f *fakeChain) CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error) {
	return txbuild.BlockReference{
		BlockNumber: 1,
		BlockHash:   [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Timestamp:   time.Now().UTC(),
		Expiration:  time.Now().UTC().Add(2 * time.Minute),
	}, nil
}

func (f *fakeChain) BroadcastSigned(ctx context.Context, unsignedTx []byte, signature [65]byte) (string, error) {
	f.mu.Lock()
	f.broadcasts++
	f.mu.Unlock()
	digest := txbuild.Digest(unsignedTx)
	return fmt.Sprintf("%x", digest[:]), nil
}

// TokenBalance reports plenty of USDT for every holder -- balance
// shortfalls are exercised by tests that configure them explicitly.
func (f *fakeChain) TokenBalance(ctx context.Context, holder string) (*big.Int, error) {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil), nil
}

// AccountResources reports an activated account with plenty of energy,
// bandwidth, and TRX -- resource shortfalls are exercised by tests that
// configure them explicitly.
func (f *fakeChain) AccountResources(ctx context.Context, holder string) (tronbroadcast.Resources, error) {
	return tronbroadcast.Resources{Exists: true, Energy: 1_000_000_000, Bandwidth: 1_000_000, BalanceSun: 1_000_000_000}, nil
}

func (f *fakeChain) EstimateTransferEnergy(ctx context.Context, from, to string, raw *big.Int) (int64, error) {
	return 64_285, nil
}

// fakeFinality always reports a broadcast TRC20 transfer as final and
// successful -- this harness's own replay scenarios exercise the happy
// path, never a real "accepted but execution failed" outcome (see
// orchestrate.TRC20FinalityChecker's own doc comment for why that
// distinction now matters: CheckExecution is genuinely consulted before
// a TRC20 leg is ever marked forwarded).
type fakeFinality struct{}

func (fakeFinality) IsFinal(ctx context.Context, txID string) (bool, error) { return true, nil }

func (fakeFinality) CheckExecution(ctx context.Context, txID string) (final, success bool, failureReason string, err error) {
	return true, true, "", nil
}

// fakeEVMChain is fakeChain's own EVM-direction sibling.
type fakeEVMChain struct {
	mu         sync.Mutex
	broadcasts int
	nonce      uint64
}

func (f *fakeEVMChain) CurrentNonce(ctx context.Context, address string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nonce, nil
}

func (f *fakeEVMChain) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return big.NewInt(3_000_000_000), nil
}

func (f *fakeEVMChain) Broadcast(ctx context.Context, signed *gethtypes.Transaction) (string, error) {
	f.mu.Lock()
	f.broadcasts++
	f.nonce++
	f.mu.Unlock()
	return signed.Hash().Hex(), nil
}

// ConfirmedNonce never passes a sent transaction's nonce, so nothing is
// ever treated as dropped unless a test says so.
func (f *fakeEVMChain) ConfirmedNonce(ctx context.Context, address string) (uint64, error) {
	return 0, nil
}

func (f *fakeEVMChain) TransactionMined(ctx context.Context, txHash string) (bool, error) {
	return true, nil
}

// TokenBalance and NativeBalance report plenty of USDT and BNB for every
// holder -- balance shortfalls are exercised by tests that configure them
// explicitly.
func (f *fakeEVMChain) TokenBalance(ctx context.Context, holder string) (*big.Int, error) {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil), nil
}

func (f *fakeEVMChain) NativeBalance(ctx context.Context, holder string) (*big.Int, error) {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil), nil
}

// fakeAlerter captures every alert fired -- the harness's own record,
// checked by FinalAssertion_ExactlyOneAlertPerUnrecoverableLeg.
type fakeAlerter struct {
	mu    sync.Mutex
	fired []alert.Alert
}

func (f *fakeAlerter) Fire(ctx context.Context, a alert.Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fired = append(f.fired, a)
	return nil
}

func (f *fakeAlerter) Fired() []alert.Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]alert.Alert, len(f.fired))
	copy(out, f.fired)
	return out
}

// fakeBEP20DepositWatcher is depositwatcher's own real address book,
// stood in for -- advanceForwardingOneBEP20's own defense-in-depth
// cross-check calls GetAddress before ever requesting a deposit-sweep
// signature, so any scenario exercising a BEP20_TO_TRC20 leg must
// configure this consistently with that leg's own fixture
// DepositAddress/DepositDerivationIndex, matching what a real
// depositwatcher would report.
type fakeBEP20DepositWatcher struct {
	mu   sync.Mutex
	byID map[int64]watcherclient.Address
}

func newFakeBEP20DepositWatcher() *fakeBEP20DepositWatcher {
	return &fakeBEP20DepositWatcher{byID: make(map[int64]watcherclient.Address)}
}

// set configures orderID to resolve to address/index, as if a real
// depositwatcher had really assigned it.
func (f *fakeBEP20DepositWatcher) set(orderID int64, address string, index uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[orderID] = watcherclient.Address{Address: address, DerivationIndex: &index, OrderID: orderID}
}

func (f *fakeBEP20DepositWatcher) GetAddress(ctx context.Context, orderID int64) (watcherclient.Address, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.byID[orderID]
	if !ok {
		return watcherclient.Address{}, fmt.Errorf("fakeBEP20DepositWatcher: no address configured for order %d", orderID)
	}
	return addr, nil
}

// fakeTronDepositWatcher is fakeBEP20DepositWatcher's own TRC20-direction
// counterpart -- tronwatcher's own real address book, stood in for.
// advanceForwardingOneTRC20's own identical defense-in-depth cross-check
// calls GetAddress before ever requesting a TRON deposit-sweep
// signature, so any scenario exercising a TRC20_TO_BEP20 leg that
// reaches FORWARDING must configure this consistently with that leg's
// own fixture DepositAddress/DepositDerivationIndex.
type fakeTronDepositWatcher struct {
	mu   sync.Mutex
	byID map[int64]watcherclient.Address
}

func newFakeTronDepositWatcher() *fakeTronDepositWatcher {
	return &fakeTronDepositWatcher{byID: make(map[int64]watcherclient.Address)}
}

// set configures orderID to resolve to address/index, as if a real
// tronwatcher had really assigned it.
func (f *fakeTronDepositWatcher) set(orderID int64, address string, index uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[orderID] = watcherclient.Address{Address: address, DerivationIndex: &index, OrderID: orderID}
}

func (f *fakeTronDepositWatcher) GetAddress(ctx context.Context, orderID int64) (watcherclient.Address, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	addr, ok := f.byID[orderID]
	if !ok {
		return watcherclient.Address{}, fmt.Errorf("fakeTronDepositWatcher: no address configured for order %d", orderID)
	}
	return addr, nil
}

// scenarioOrchestrator bundles a fresh Orchestrator with its own fakes,
// so a scenario can both drive RunTick and inspect/force behavior on the
// exact fakes that Orchestrator is using.
type scenarioOrchestrator struct {
	orch                *orchestrate.Orchestrator
	upstream            *upstream.MockProvider
	signer              *signing.FakeSigningService
	chain               *fakeChain
	evmChain            *fakeEVMChain
	alerter             *fakeAlerter
	bep20DepositWatcher *fakeBEP20DepositWatcher
	tronDepositWatcher  *fakeTronDepositWatcher
}

// newOrchestrator builds a fresh Orchestrator wired with real C1 access
// (this harness's own client) and fresh, scenario-local fakes for
// everything else. forwardingTimeout is 0 (disabled) unless a scenario
// explicitly needs R5's own timeout-refund path.
func (h *harness) newOrchestrator(providerName string, forwardingTimeout time.Duration) *scenarioOrchestrator {
	up := upstream.NewMockProvider(providerName, h.rng.Int63())
	signer := signing.NewFakeSigningService()
	signer.SetSlotAddress(1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")
	signer.SetEVMAddress(1, "0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf")
	chain := &fakeChain{}
	evmChain := &fakeEVMChain{}
	alerter := &fakeAlerter{}
	bep20DepositWatcher := newFakeBEP20DepositWatcher()
	tronDepositWatcher := newFakeTronDepositWatcher()

	orch := orchestrate.New(h.store, h.client, up, fakeEnergy{}, signer, chain, fakeFinality{}, evmChain, fakeFinality{}, alerter,
		bep20DepositWatcher, tronDepositWatcher,
		orchestrate.Config{
			SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", SlotEVMAddress: "0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf",
			EnergyPerTransferUnits: 65000, ForwardingTimeout: forwardingTimeout,
		})

	return &scenarioOrchestrator{orch: orch, upstream: up, signer: signer, chain: chain, evmChain: evmChain, alerter: alerter, bep20DepositWatcher: bep20DepositWatcher, tronDepositWatcher: tronDepositWatcher}
}

// runTicksUntil ticks o up to maxTicks times, stopping early once check
// returns true -- the harness's own "drive real RunTick until this leg
// resolves, don't assume a fixed tick count" convention, matching
// internal/orchestrate's own integration tests' documented "a single
// tick can legitimately carry a leg through several phases at once."
func (h *harness) runTicksUntil(o *orchestrate.Orchestrator, maxTicks int, check func() (bool, error)) error {
	for i := 0; i < maxTicks; i++ {
		if err := o.RunTick(h.ctx); err != nil {
			return fmt.Errorf("RunTick %d: %w", i+1, err)
		}
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("did not reach the expected state within %d ticks", maxTicks)
}

// refundEntryDriver is a minimal Driver -- only the two fields
// BuildRefundEntry actually reads (Ledger, Store) -- standing in for
// what screening's own RelayAwareRefundEntryBuilder would call over
// HTTP in a real deployment (relayd's own GET .../refund-entry, which
// wraps this exact method). scenarioManuallyRejectedHoldGetsRefunded is
// the one scenario that needs it.
func (h *harness) refundEntryDriver() *driver.Driver {
	return &driver.Driver{Ledger: h.client, Store: h.store}
}

func (h *harness) legStatus(externalID string) (relay.Status, error) {
	leg, err := h.store.GetByExternalID(h.ctx, externalID)
	if err != nil {
		return "", err
	}
	return leg.Status, nil
}
