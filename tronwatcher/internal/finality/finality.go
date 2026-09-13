// Package finality tracks TRON deposit candidates from detection to
// solidity (SR-confirmed) finality.
//
// This is deliberately NOT a port of depositwatcher/internal/finality's
// full apparatus. That package's design is built around BSC's
// finality-TAG model (poll a "finalized" height, promote everything at
// or below it, then keep re-verifying every already-finalized candidate
// forever against a fresh log query, to catch the BEP-126 finality tag
// itself being violated). TRON's finality primitive is different in
// kind, not just in RPC shape: solidity confirmation
// (chain.FinalityClient.IsFinal) answers per-TRANSACTION, directly --
// there is no "agreed height" to poll and no separate log-presence
// re-check to run, because the finality check IS the direct
// confirmation, not a proxy for one.
//
// What this package deliberately does NOT do, by an explicit decision
// this session (not an oversight): re-verify already-finalized
// candidates on an ongoing basis the way depositwatcher's own
// checkPostFinalReorgs does. TRON's DPoS solidity confirmation is
// designed to be irreversible in practice, and this project has no
// known real-world case of a transaction reversing after solidity
// confirmation to defend against. If that assumption ever turns out to
// be wrong, add the equivalent re-verification loop here -- this
// comment is the flag for that decision, not a claim it can never
// happen.
package finality

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"tronwatcher/internal/chain"
	"tronwatcher/internal/money"
)

// ErrPermanentFailure is a sentinel a FinalHandler wraps its own error
// in to mean "retrying this exact call can never succeed" -- mirroring
// depositwatcher/internal/finality's own sentinel and its ledgerclient
// usage (illegal_transition, idempotency_conflict).
var ErrPermanentFailure = errors.New("finality: permanent failure, do not retry")

// ErrOrphanedDeposit is the narrower of the two ErrPermanentFailure
// cases: a deposit finalized on-chain for an order C1 no longer
// considers open.
var ErrOrphanedDeposit = errors.New("finality: order no longer open for this deposit")

// DefaultStalePendingCeiling mirrors depositwatcher's own default --
// crossing it means the finality checker or the chain itself is in
// trouble, never a reason to force-finalize.
const DefaultStalePendingCeiling = 5 * time.Minute

// DepositFinalIdempotencyKeyPrefix is this service's own prefix for a
// deposit_final entry's idempotency key -- "tronwatcher", not
// "watcher", so a key collision with depositwatcher's own C1 calls is
// structurally impossible even though both services may credit
// suspense/treasury accounts on the same C1 instance.
const DepositFinalIdempotencyKeyPrefix = "tronwatcher:deposit_final:"

// DepositFinalIdempotencyKey builds this service's own key:
// "tronwatcher:deposit_final:<tx_id>". Unlike
// depositwatcher/internal/finality's own tx_hash:log_index shape, a
// TRC20 transfer's idempotency key needs only the transaction id --
// TRON transactions overwhelmingly carry exactly one TRC20 transfer
// each (there is no EVM-style multi-log-per-tx concern this service
// needs to disambiguate against for a simple wallet-to-wallet deposit).
func DepositFinalIdempotencyKey(txID string) string {
	return fmt.Sprintf("%s%s", DepositFinalIdempotencyKeyPrefix, txID)
}

// ObservedTransfer is everything OnTransferObserved needs about a
// single TRC20 transfer already fetched (via chain.Pool.ScanAddress --
// 2-provider agreement), parsed (chain.ParseTransferValue), and
// classified against its order (chain.ClassifyAgainstOrder).
type ObservedTransfer struct {
	TxID           string
	BlockTimestamp time.Time // chain time, never wall-clock
	OrderID        int64
	ExternalID     string
	CustomerID     string
	Amount         money.Amount
	// SenderAddress is the transfer's own `from` -- C3 (screening) needs
	// it and has no chain access of its own, mirroring
	// depositwatcher/internal/finality.ObservedLog's identical field.
	SenderAddress string
}

func (o ObservedTransfer) key() string { return o.TxID }

// Candidate is a tracked deposit: observed and classified, not yet
// final.
type Candidate struct {
	ObservedTransfer
	Classification chain.Classification
	DetectedAt     time.Time

	alertedStale bool
}

// FinalHandler is invoked exactly once for each candidate the moment it
// finalizes. If it returns an error, the candidate is left pending
// rather than marked final -- C1's own idempotency-key convention makes
// retrying safe.
type FinalHandler func(ctx context.Context, c Candidate) error

// StalePendingHandler is invoked once -- not on every tick -- the first
// time a still-pending candidate crosses Config.StalePendingCeiling.
type StalePendingHandler func(c Candidate, pending time.Duration)

// OrphanedDepositRecorder persists a deposit C1 no longer has an open
// order for -- internal/orphaned.Record is the real store this backs
// onto, mirroring depositwatcher's own interface exactly.
type OrphanedDepositRecorder interface {
	RecordOrphanedDeposit(ctx context.Context, c Candidate, c1Error error) error
}

// FinalityChecker answers whether a TRON transaction has reached
// SR/solidity confirmation -- chain.FinalityClient is the real
// implementation. Behind an interface so this package's own tracking
// logic is testable without a live TRON node for every run.
type FinalityChecker interface {
	IsFinal(ctx context.Context, tronTxID string) (bool, error)
}

// MetricsRecorder mirrors depositwatcher/internal/finality's own
// interface -- optional, nil means no metrics, never a panic.
type MetricsRecorder interface {
	CandidateDetected()
	CandidateFinalized()
}

// Config controls a Tracker's alerting and dependencies.
type Config struct {
	OnFinal                 FinalHandler
	OnStalePending          StalePendingHandler // optional
	OrphanedDepositRecorder OrphanedDepositRecorder
	Metrics                 MetricsRecorder // optional

	// StalePendingCeiling defaults to DefaultStalePendingCeiling if <= 0.
	StalePendingCeiling time.Duration

	// Now defaults to time.Now; overridable so tests can drive the
	// stale-pending ceiling without actually sleeping for it.
	Now func() time.Time
}

func (t *Tracker) recordDetected() {
	if t.cfg.Metrics != nil {
		t.cfg.Metrics.CandidateDetected()
	}
}

func (t *Tracker) recordFinalized() {
	if t.cfg.Metrics != nil {
		t.cfg.Metrics.CandidateFinalized()
	}
}

// Tracker holds every deposit candidate observed but not yet finalized,
// entirely in process memory -- same accepted restart-loses-in-flight
// gap depositwatcher/internal/finality's own Tracker documents, for the
// same reason: no persistence chunk in this pass's own scope.
type Tracker struct {
	cfg Config

	mu      sync.Mutex
	pending map[string]*Candidate
}

// New validates cfg and returns a ready-to-use Tracker.
func New(cfg Config) (*Tracker, error) {
	if cfg.OnFinal == nil {
		return nil, errors.New("finality: Config.OnFinal must be set")
	}
	if cfg.OrphanedDepositRecorder == nil {
		return nil, errors.New("finality: Config.OrphanedDepositRecorder must be set")
	}
	if cfg.StalePendingCeiling <= 0 {
		cfg.StalePendingCeiling = DefaultStalePendingCeiling
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Tracker{
		cfg:     cfg,
		pending: make(map[string]*Candidate),
	}, nil
}

// OnTransferObserved fires deposit.detected -- advisory only, never
// touching C1 -- and, for a trackable classification, starts tracking
// the transfer toward finality. Idempotent on TxID: a duplicate
// delivery is a no-op, never a second detected event or candidate.
func (t *Tracker) OnTransferObserved(ctx context.Context, transfer ObservedTransfer, classification chain.Classification) error {
	key := transfer.key()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pending[key] != nil {
		return nil // duplicate delivery -- no-op, not a second event
	}

	if classification == chain.ZeroValue {
		slog.Warn("finality: zero-value transfer to a watched address (anomaly, not a deposit)",
			"tx_id", transfer.TxID, "order_id", transfer.OrderID)
		return nil
	}

	slog.Info("finality: deposit.detected (advisory, not yet solidity-confirmed)",
		"tx_id", transfer.TxID, "order_id", transfer.OrderID, "amount", transfer.Amount, "classification", classification)

	t.pending[key] = &Candidate{ObservedTransfer: transfer, Classification: classification, DetectedAt: t.cfg.Now()}
	t.recordDetected()
	return nil
}

// CheckFinality calls checker.IsFinal for every pending candidate and
// finalizes (calling Config.OnFinal exactly once) any that have reached
// solidity confirmation. Intended to be called on a ticker; wiring that
// ticker is cmd/tronwatcherd's job, matching depositwatcher's own
// convention.
func (t *Tracker) CheckFinality(ctx context.Context, checker FinalityChecker) {
	t.checkStalePending()

	for _, c := range t.snapshotPending() {
		final, err := checker.IsFinal(ctx, c.TxID)
		if err != nil {
			slog.Error("finality: checking solidity confirmation failed, will retry next tick",
				"tx_id", c.TxID, "error", err)
			continue
		}
		if !final {
			continue
		}
		t.finalize(ctx, c)
	}
}

func (t *Tracker) snapshotPending() []*Candidate {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Candidate, 0, len(t.pending))
	for _, c := range t.pending {
		out = append(out, c)
	}
	return out
}

func (t *Tracker) checkStalePending() {
	if t.cfg.OnStalePending == nil {
		return
	}
	now := t.cfg.Now()

	t.mu.Lock()
	var stale []*Candidate
	for _, c := range t.pending {
		if !c.alertedStale && now.Sub(c.DetectedAt) > t.cfg.StalePendingCeiling {
			c.alertedStale = true
			stale = append(stale, c)
		}
	}
	t.mu.Unlock()

	for _, c := range stale {
		t.cfg.OnStalePending(*c, now.Sub(c.DetectedAt))
	}
}

func (t *Tracker) finalize(ctx context.Context, c *Candidate) {
	key := c.key()
	if err := t.cfg.OnFinal(ctx, *c); err != nil {
		if errors.Is(err, ErrPermanentFailure) {
			if errors.Is(err, ErrOrphanedDeposit) {
				if handleErr := t.HandleUnreportable(ctx, *c, err); handleErr != nil {
					slog.Error("finality: HandleUnreportable failed, will retry next tick",
						"tx_id", c.TxID, "error", handleErr)
					return
				}
			} else {
				slog.Error("finality: OnFinal permanently failed, dropping from tracking",
					"tx_id", c.TxID, "order_id", c.OrderID, "external_id", c.ExternalID, "error", err)
			}
			t.mu.Lock()
			delete(t.pending, key)
			t.mu.Unlock()
			return
		}
		slog.Error("finality: OnFinal handler failed, will retry next tick",
			"tx_id", c.TxID, "error", err)
		return
	}
	t.mu.Lock()
	delete(t.pending, key)
	t.mu.Unlock()
	t.recordFinalized()
}

// HandleUnreportable records c as an orphaned deposit: a candidate that
// finalized on-chain but whose order C1 no longer considers open.
func (t *Tracker) HandleUnreportable(ctx context.Context, c Candidate, c1Error error) error {
	if err := t.cfg.OrphanedDepositRecorder.RecordOrphanedDeposit(ctx, c, c1Error); err != nil {
		return fmt.Errorf("finality: recording orphaned deposit for %s: %w", c.ExternalID, err)
	}
	slog.Error("finality: ORPHANED DEPOSIT -- a finalized deposit's order is no longer open; recorded for manual reconciliation",
		"tx_id", c.TxID, "order_id", c.OrderID, "external_id", c.ExternalID, "amount", c.Amount, "c1_error", c1Error)
	return nil
}

// PendingCount reports how many candidates are currently tracked but
// not yet final -- exported for tests and operator visibility.
func (t *Tracker) PendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// OldestPendingDetectedAt returns the DetectedAt of the longest-pending
// candidate, for operator visibility.
func (t *Tracker) OldestPendingDetectedAt() (detectedAt time.Time, found bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.pending {
		if !found || c.DetectedAt.Before(detectedAt) {
			detectedAt = c.DetectedAt
			found = true
		}
	}
	return detectedAt, found
}
