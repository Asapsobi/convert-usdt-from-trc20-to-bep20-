// Package orchestrate is relayd's own driving loop and ledger-entry
// logic -- the direct sibling of dispatcher/internal/orchestrate, but
// for a zero-float relay leg rather than a pre-funded payout. Mirrors
// that package's own two-independent-phases-per-tick shape (RunLoop/
// RunTick, per-row error isolation, an in-memory unsigned-tx cache with
// the same documented not-crash-safe limitation).
//
// # The ledger entries this package posts -- a real design decision,
// not fully specified by docs/02-architecture/model-f-relay-architecture.md
//
// Model D's own screened->dispatching entry (dispatcher's E2,
// buildConversionLines) moves value through position:corridor -- the
// pre-funded treasury Model F explicitly does not have ("never holds a
// net position," per docs/01-strategy/model-f-relay-findings.md's own
// verdict). So RELAY orders need their own entry shapes, designed here:
//
//   - screened -> dispatching ("relay_forward_start"): moves the full
//     amount_in from the relay-leg suspense account
//     (asset:relay:leg:<order_id>, opened by the deposit watcher's own
//     deposit_final entry) into a second, forwarding-specific suspense
//     account (asset:relay:leg:forwarding:<order_id>). No value is
//     created or destroyed -- this exists so C1's own audit trail
//     records exactly when this leg started forwarding, the same
//     auditability reason every OTHER state transition in this system
//     requires an entry, not just the ones that move real value.
//   - dispatching -> settled ("relay_settle"), posted only once the
//     upstream platform confirms completion: closes the customer's own
//     liability (fulfilled off-books -- the upstream platform pays the
//     customer's own destination wallet directly, not this system),
//     closes the forwarding suspense account by the amount actually
//     forwarded on-chain, and books the difference (the commission) as
//     revenue. Three lines, balanced:
//     DR liability:customer:<id>:<in_asset>        +amount_in
//     CR asset:relay:leg:forwarding:<order_id>      -forward_amount
//     CR revenue:relay_commission:<in_asset>        -fee_units
//     (forward_amount + fee_units == amount_in, by construction --
//     see forwardAmount's own doc comment.)
package orchestrate

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"relayd/internal/energy"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
)

// DefaultInterval mirrors dispatcher/internal/orchestrate's own default.
const DefaultInterval = 5 * time.Second

// LedgerClient is the narrow slice of *ledgerclient.Client this package
// needs -- declared here, consumer-side, so a fake satisfies it in
// tests without importing the real HTTP client.
type LedgerClient interface {
	ListOrdersByState(ctx context.Context, state, cursor string) ([]ledgerclient.OrderRef, string, error)
	GetOrder(ctx context.Context, externalID string) (ledgerclient.Order, error)
	EnsureAccount(ctx context.Context, code string, accountType ledgerclient.AccountType, asset string, idempotencyKey string) error
	TransitionWithEntry(ctx context.Context, externalID, toState string, expectedVersion int32, reason, entryType string, occurredAt time.Time, lines []ledgerclient.EntryLine, idempotencyKey string) (ledgerclient.Order, error)
}

// EnergyClient is the narrow slice of *energy.Client this package needs.
type EnergyClient interface {
	Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (energy.Reservation, error)
}

// SigningService is the narrow slice of *signing.Client this package
// needs -- *signing.Client and *signing.FakeSigningService both satisfy
// it with no adapter, mirroring dispatcher/internal/dispatch's own
// identical interface.
type SigningService interface {
	RequestSignature(ctx context.Context, slotID int, digest [32]byte, estimatedUSD float64, idempotencyKey string) (signing.SigningRequest, error)
}

// TRC20Broadcaster is this package's own path to a real TRON node for
// the TRC20-direction forward leg.
type TRC20Broadcaster interface {
	CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error)
	BroadcastSigned(ctx context.Context, unsignedTx []byte, signature [65]byte) (txid string, err error)
}

// TRC20FinalityChecker checks TRC20 forward-transfer finality.
type TRC20FinalityChecker interface {
	IsFinal(ctx context.Context, tronTxID string) (bool, error)
}

// EVMBroadcaster is this package's own path to a real BSC node for the
// BEP20-direction forward leg -- relayd/internal/evmbroadcast.Client's
// own shape.
type EVMBroadcaster interface {
	CurrentNonce(ctx context.Context, address string) (uint64, error)
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	Broadcast(ctx context.Context, signed *types.Transaction) (txHash string, err error)
}

// EVMFinalityChecker checks BEP20 forward-transfer finality.
type EVMFinalityChecker interface {
	IsFinal(ctx context.Context, txHash string) (bool, error)
}

// Config is this package's own required tuning -- no hardcoded defaults
// for real-money thresholds, matching every sibling orchestrator's own
// posture.
type Config struct {
	// SlotID identifies the S1 slot relayd signs every forward leg
	// with, on either chain -- ONE secp256k1 key, two address
	// encodings (see s1/internal/slots/evm.go's own doc comment), not
	// two separate keys. A DIFFERENT slot from any Model D payout slot
	// -- "smaller blast radius," per the architecture doc's own S1
	// entry -- an operational/seeding decision, not something this
	// package chooses.
	SlotID int
	// SlotAddress/SlotEVMAddress are that slot's own two address
	// encodings, resolved once at startup (cmd/relayd), not re-fetched
	// every tick.
	SlotAddress    string
	SlotEVMAddress string

	// EnergyPerTransferUnits is the TRON energy a single USDT-TRC20
	// transfer costs -- mirrors dispatcher's own identical, required (no
	// default) config value. Only consulted for the TRC20_TO_BEP20
	// direction; BEP20_TO_TRC20's own forward leg needs BNB gas instead,
	// resolved live via EVMBroadcaster.SuggestGasPrice, not a config
	// value (per architecture doc §2: "no C4 involvement" for that leg).
	EnergyPerTransferUnits int64

	// EnergyReservationWait bounds how long Reserve's own slow path is
	// allowed to retry before giving up.
	EnergyReservationWait time.Duration

	// EVMGasLimit overrides evmtx.DefaultGasLimit if nonzero.
	EVMGasLimit uint64
}

// Orchestrator bundles every dependency RunTick needs.
type Orchestrator struct {
	Store       *relay.Store
	Ledger      LedgerClient
	Upstream    upstream.SwapProvider
	Energy      EnergyClient
	Signing     SigningService
	Chain       TRC20Broadcaster
	Finality    TRC20FinalityChecker
	EVMChain    EVMBroadcaster
	EVMFinality EVMFinalityChecker
	Cfg         Config

	mu         sync.Mutex
	pending    map[string]pendingForward    // externalID -> cached unsigned TRC20 tx
	pendingEVM map[string]pendingEVMForward // externalID -> cached unsigned BEP20 tx
}

// pendingForward caches a TRC20 forward leg's unsigned bytes across
// ticks -- BlockReference is not deterministic, so this cannot simply
// be rebuilt from persisted inputs. Not persisted across a process
// restart: the same accepted, documented limitation
// dispatcher/internal/orchestrate's own Orchestrator.pending carries
// (see that package's own doc comment on dispatchOne). pendingEVM
// carries the identical limitation for the BEP20 direction, for the
// identical reason (nonce/gas price are resolved live, not
// deterministic from persisted inputs alone).
type pendingForward struct {
	unsignedTx []byte
}

// pendingEVMForward is pendingForward's own BEP20-direction analogue --
// the digest is cached alongside the unsigned transaction rather than
// recomputed from it every tick (evmtx.BuildTransfer is pure/deterministic
// so recomputing would also be correct, but caching is cheaper and avoids
// a second, redundant construction call on every tick a signature stays
// PENDING).
type pendingEVMForward struct {
	unsignedTx *types.Transaction
	digest     [32]byte
}

// New wires an Orchestrator.
func New(store *relay.Store, ledger LedgerClient, up upstream.SwapProvider, energyClient EnergyClient,
	signer SigningService, chain TRC20Broadcaster, finality TRC20FinalityChecker,
	evmChain EVMBroadcaster, evmFinality EVMFinalityChecker, cfg Config) *Orchestrator {
	return &Orchestrator{
		Store: store, Ledger: ledger, Upstream: up, Energy: energyClient, Signing: signer,
		Chain: chain, Finality: finality, EVMChain: evmChain, EVMFinality: evmFinality, Cfg: cfg,
		pending:    make(map[string]pendingForward),
		pendingEVM: make(map[string]pendingEVMForward),
	}
}

// upstreamPairFor maps a relay leg's own Direction to the
// upstream.Pair its Quote/CreateOrder calls need.
func upstreamPairFor(direction relay.Direction) upstream.Pair {
	if direction == relay.TRC20ToBEP20 {
		return upstream.Pair{From: money.USDT_TRC20, To: money.USDT_BEP20}
	}
	return upstream.Pair{From: money.USDT_BEP20, To: money.USDT_TRC20}
}

func customerAccountCode(customerID string, asset money.Asset) string {
	return fmt.Sprintf("liability:customer:%s:%s", customerID, asset)
}

func relayLegAccountCode(orderID int64) string {
	return fmt.Sprintf("asset:relay:leg:%d", orderID)
}

func relayLegForwardingAccountCode(orderID int64) string {
	return fmt.Sprintf("asset:relay:leg:forwarding:%d", orderID)
}

func commissionAccountCode(asset money.Asset) string {
	return fmt.Sprintf("revenue:relay_commission:%s", asset)
}

// commissionWalletAccountCode is a real, ongoing (not per-leg) asset
// account for commission relayd has collected but not yet swept out --
// see settle.go's own doc comment on why revenue:relay_commission's
// credit needs a real asset account backing it, not just a per-leg
// suspense account closing to a nonzero remainder.
func commissionWalletAccountCode(asset money.Asset) string {
	return fmt.Sprintf("asset:relay:commission_wallet:%s", asset)
}

// forwardAmount is amount_in minus the order's own fee_units -- "the
// forward transfer is (received - take)," per the build-prompts doc's
// own happy-flow step 8. Both operands share the in-asset (fee_units is
// stored, and here interpreted, in the SAME asset as amount_in -- see
// orchestrate.go's own package doc comment on why the commission is
// taken out of the deposit before forwarding, never out of the
// upstream's own payout).
func forwardAmount(amountIn, feeUnits money.Amount) (money.Amount, error) {
	return amountIn.Sub(feeUnits)
}
