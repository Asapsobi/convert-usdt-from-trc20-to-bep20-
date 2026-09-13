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
	"relayd/internal/energy"
	"relayd/internal/ledgerclient"
	"relayd/internal/orchestrate"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
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
	return fmt.Sprintf("%x", digest[:8]), nil
}

// fakeFinality is never actually consulted by internal/orchestrate today
// (a forward leg is marked FORWARDED on a successful broadcast response,
// not gated on a separate finality check -- confirmed by grep, not
// assumed), but Orchestrator still needs something satisfying
// TRC20FinalityChecker/EVMFinalityChecker to construct.
type fakeFinality struct{}

func (fakeFinality) IsFinal(ctx context.Context, txID string) (bool, error) { return true, nil }

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

// scenarioOrchestrator bundles a fresh Orchestrator with its own fakes,
// so a scenario can both drive RunTick and inspect/force behavior on the
// exact fakes that Orchestrator is using.
type scenarioOrchestrator struct {
	orch     *orchestrate.Orchestrator
	upstream *upstream.MockProvider
	signer   *signing.FakeSigningService
	chain    *fakeChain
	evmChain *fakeEVMChain
	alerter  *fakeAlerter
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

	orch := orchestrate.New(h.store, h.client, up, fakeEnergy{}, signer, chain, fakeFinality{}, evmChain, fakeFinality{}, alerter,
		orchestrate.Config{
			SlotID: 1, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", SlotEVMAddress: "0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf",
			EnergyPerTransferUnits: 65000, ForwardingTimeout: forwardingTimeout,
		})

	return &scenarioOrchestrator{orch: orch, upstream: up, signer: signer, chain: chain, evmChain: evmChain, alerter: alerter}
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

func (h *harness) legStatus(externalID string) (relay.Status, error) {
	leg, err := h.store.GetByExternalID(h.ctx, externalID)
	if err != nil {
		return "", err
	}
	return leg.Status, nil
}
