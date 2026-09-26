// Package orchestrate is relayd's own driving loop and ledger-entry
// logic -- the direct sibling of dispatcher/internal/orchestrate, but
// for a zero-float relay leg rather than a pre-funded payout. Mirrors
// that package's own two-independent-phases-per-tick shape (RunLoop/
// RunTick, per-row error isolation). Unlike that package, every on-chain
// transfer is recorded in internal/transfers before it is signed and
// sent, so a restart resumes it instead of forgetting it (transfer.go).
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

	"relayd/internal/alert"
	"relayd/internal/energy"
	"relayd/internal/ledgerclient"
	"relayd/internal/money"
	"relayd/internal/pricing"
	"relayd/internal/relay"
	"relayd/internal/signing"
	"relayd/internal/sweeps"
	"relayd/internal/transfers"
	"relayd/internal/tronbroadcast"
	"relayd/internal/txbuild"
	"relayd/internal/upstream"
	"relayd/internal/watcherclient"
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
	AccountBalance(ctx context.Context, code string) (money.Amount, error)
	Transition(ctx context.Context, externalID, toState string, expectedVersion int32, reason string, occurredAt time.Time, idempotencyKey string) (ledgerclient.Order, error)
	PostEntry(ctx context.Context, entryType string, occurredAt time.Time, lines []ledgerclient.EntryLine, metadata map[string]any, idempotencyKey string) (int64, error)
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
	// RequestDepositSweepSignature signs FROM a specific per-order BSC
	// deposit key (index, a BIP32 child derived the same way
	// depositwatcher's own address book derives the matching address)
	// rather than relayd's own shared slot key -- what
	// advanceForwardingOneBEP20 uses to sign a BEP20_TO_TRC20 leg's
	// forward transfer, since that transfer must come from the
	// customer's own actual deposit address, never the slot.
	RequestDepositSweepSignature(ctx context.Context, index uint32, digest [32]byte, estimatedUSD float64, idempotencyKey string) (signing.SigningRequest, error)
	// RequestTronDepositSweepSignature is RequestDepositSweepSignature's
	// own TRON-deposit counterpart -- what advanceForwardingOneTRC20 uses
	// to sign a TRC20_TO_BEP20 leg's forward transfer from the customer's
	// own actual TRON deposit address, never the slot.
	RequestTronDepositSweepSignature(ctx context.Context, index uint32, digest [32]byte, estimatedUSD float64, idempotencyKey string) (signing.SigningRequest, error)
}

// DepositAddressLookup is the narrow slice of *watcherclient.Client this
// package needs for the BEP20_TO_TRC20 forward-signing cross-check
// (advanceForwardingOneBEP20): an independent, live re-fetch of a leg's
// own deposit address/derivation index from depositwatcher's own real
// address book, verified against the leg's own locally-recorded values
// before ever requesting a signature -- defense in depth, since
// RequestDepositSweepSignature itself performs no such check (it will
// derive-and-sign for whatever index it's given).
type DepositAddressLookup interface {
	GetAddress(ctx context.Context, orderID int64) (watcherclient.Address, error)
}

// TRC20Broadcaster is this package's own path to a real TRON node for
// the TRC20-direction forward leg.
type TRC20Broadcaster interface {
	CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error)
	BroadcastSigned(ctx context.Context, unsignedTx []byte, signature [65]byte) (txid string, err error)
	// TokenBalance is holder's USDT-TRC20 balance in raw on-chain units --
	// checked before any transfer is built, so relayd never signs one
	// the sender can't pay.
	TokenBalance(ctx context.Context, holder string) (*big.Int, error)
	// AccountResources is holder's activation, energy, bandwidth, and TRX.
	AccountResources(ctx context.Context, holder string) (tronbroadcast.Resources, error)
	// EstimateTransferEnergy is the exact energy a USDT transfer of raw
	// units from from to to would use, simulated on the node.
	EstimateTransferEnergy(ctx context.Context, from, to string, raw *big.Int) (int64, error)
}

// TRC20FinalityChecker checks TRC20 forward-transfer finality AND
// execution success -- CheckExecution is the one this package's own
// advanceForwardingOneTRC20/advanceRefundPendingOneTRC20 actually rely
// on before ever marking a leg forwarded/refunded: a real, live
// broadcast this package accepted (BroadcastSigned returned a txid, no
// error) still executed with receipt.result "OUT_OF_ENERGY", moving zero
// funds -- being accepted into a block is not the same fact as having
// succeeded, and nothing in this package checked the difference before
// this interface gained CheckExecution.
type TRC20FinalityChecker interface {
	IsFinal(ctx context.Context, tronTxID string) (bool, error)
	// CheckExecution reports whether tronTxID has reached finality
	// (final) and, only meaningful when final is true, whether its own
	// on-chain execution actually succeeded (success). failureReason is
	// the chain's own specific verdict (e.g. "OUT_OF_ENERGY") when final
	// is true and success is false.
	CheckExecution(ctx context.Context, tronTxID string) (final, success bool, failureReason string, err error)
}

// EVMBroadcaster is this package's own path to a real BSC node for the
// BEP20-direction forward leg -- relayd/internal/evmbroadcast.Client's
// own shape.
type EVMBroadcaster interface {
	CurrentNonce(ctx context.Context, address string) (uint64, error)
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	Broadcast(ctx context.Context, signed *types.Transaction) (txHash string, err error)
	// ConfirmedNonce is address's nonce as of the latest block (mined
	// transactions only) -- once it passes a sent transaction's nonce,
	// that nonce has been used.
	ConfirmedNonce(ctx context.Context, address string) (uint64, error)
	// TransactionMined reports whether txHash has a receipt at all
	// (successful or reverted), final or not.
	TransactionMined(ctx context.Context, txHash string) (bool, error)
	// TokenBalance is holder's USDT-BEP20 balance in raw on-chain units.
	TokenBalance(ctx context.Context, holder string) (*big.Int, error)
	// NativeBalance is holder's BNB balance in wei -- what pays its gas.
	NativeBalance(ctx context.Context, holder string) (*big.Int, error)
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

	// ForwardingTimeout bounds how long a leg may sit AWAITING_DEPOSIT
	// (its C1 order already screened, but upstream.CreateOrder has never
	// once succeeded) or FORWARDING (CreateOrder succeeded, but the
	// on-chain forward transfer has never once broadcast/confirmed)
	// before internal/orchestrate/refund.go gives up and refunds the
	// deposit back to its own sender instead of retrying forever. Money
	// only ever moves once past this timeout: refund.go's own conditional
	// DB updates and C1 transitions make a late success/late refund race
	// safe either way (see refund.go's own doc comment), so this is an
	// operational tuning knob, not a correctness-critical value -- but no
	// default is hardcoded here regardless, matching every other
	// real-money-shaped value in this Config.
	ForwardingTimeout time.Duration

	// StaleLegAlertAfter is the stale-relay-leg reconciliation alarm's own
	// threshold (docs/02-architecture/model-f-relay-architecture.md §5's
	// own "an account open after an hour is an operational alarm") --
	// zero (the default if unset) disables the alarm entirely, an
	// explicit opt-in like ForwardingTimeout. Purely observational: it
	// fires an alert (internal/alert), once per leg, and takes no
	// automatic action -- see reconcile.go's own doc comment for exactly
	// which statuses and why.
	StaleLegAlertAfter time.Duration

	// DepositGrace is how long past a leg's deposit deadline relayd waits
	// before expiring it unpaid and releasing its wallet (a customer's
	// payment can be slow to confirm). Zero disables expiry.
	DepositGrace time.Duration

	// GasTopUpWei / TRXTopUpSun are the least a treasury top-up sends to a
	// deposit wallet short of BNB (gas) or TRX (activation, bandwidth).
	// Zero uses the defaults (0.0005 BNB, 2 TRX).
	GasTopUpWei int64
	TRXTopUpSun int64

	// SweepToBSC / SweepToTRON are where swept profit goes on each chain.
	// Empty means the treasury slot's own address (SlotEVMAddress /
	// SlotAddress). Set from the environment only, never through the admin
	// API: redirecting profit must take access to the server itself.
	SweepToBSC  string
	SweepToTRON string

	// Treasuries are every treasury wallet -- S1 slots whose addresses pay
	// deposit wallets' gas and TRX. The first is the primary (the SlotID
	// slot); a top-up comes from whichever can pay for it. Empty means the
	// SlotID slot alone.
	Treasuries []Treasury
}

// Treasury is one treasury wallet: an S1 slot and its address on each
// chain.
type Treasury struct {
	SlotID      int
	EVMAddress  string // BSC
	TronAddress string // TRON
}

// Address is t's address on chain.
func (t Treasury) Address(chain transfers.Chain) string {
	if chain == transfers.BSC {
		return t.EVMAddress
	}
	return t.TronAddress
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
	Alert       alert.Alerter
	// BEP20DepositWatcher is depositwatcher's own real address book,
	// consulted only by advanceForwardingOneBEP20's own defense-in-depth
	// cross-check before ever requesting a per-order deposit-sweep
	// signature -- see DepositAddressLookup's own doc comment.
	BEP20DepositWatcher DepositAddressLookup
	// TronDepositWatcher is BEP20DepositWatcher's own TRC20-direction
	// counterpart -- tronwatcher's own real address book, consulted by
	// advanceForwardingOneTRC20's own identical defense-in-depth
	// cross-check.
	TronDepositWatcher DepositAddressLookup
	// Transfers is the durable record of every forward and refund
	// transaction -- see internal/transfers. Nothing about an in-flight
	// transfer lives only in memory.
	Transfers *transfers.Store
	// Pricing is the admin-managed pricing, used for a leg created before
	// legs snapshotted their own pricing (see pricingFor). Optional.
	Pricing *pricing.Store
	// Sweeps records the profit in each deposit wallet and its sweeps to
	// the treasury (sweep.go). Optional: nil disables sweeping.
	Sweeps *sweeps.Store
	// EVMPayouts and TRONPayouts check a vendor's payout on the chain the
	// customer is paid on before a leg is marked complete (payout.go).
	// Optional: nil takes the vendor's word for that chain.
	EVMPayouts  EVMPayoutReader
	TRONPayouts TRONPayoutReader
	Cfg         Config

	mu               sync.Mutex
	preflightAlerted map[string]string // externalID|reason -> last detail already alerted on
	lastSweepScan    time.Time
	releaseRetryAt   map[string]time.Time // externalID -> when its failed wallet release may be retried
}

// New wires an Orchestrator.
func New(store *relay.Store, ledger LedgerClient, up upstream.SwapProvider, energyClient EnergyClient,
	signer SigningService, chain TRC20Broadcaster, finality TRC20FinalityChecker,
	evmChain EVMBroadcaster, evmFinality EVMFinalityChecker, alerter alert.Alerter,
	bep20DepositWatcher DepositAddressLookup, tronDepositWatcher DepositAddressLookup, cfg Config) *Orchestrator {
	return &Orchestrator{
		Store: store, Ledger: ledger, Upstream: up, Energy: energyClient, Signing: signer,
		Chain: chain, Finality: finality, EVMChain: evmChain, EVMFinality: evmFinality, Alert: alerter,
		BEP20DepositWatcher: bep20DepositWatcher, TronDepositWatcher: tronDepositWatcher, Cfg: cfg,
		Transfers:        transfers.NewStore(store.DB()),
		preflightAlerted: make(map[string]string),
		releaseRetryAt:   make(map[string]time.Time),
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
