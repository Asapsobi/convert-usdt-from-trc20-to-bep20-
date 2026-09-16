// Package finality implements the C2 build spec's §B confirmation and
// finality policy: finality-tag-based as the primary, money-critical
// mechanism, with a depth-based rolling window (internal/chain's
// RunIngestionLoop, C2.3) retained underneath it purely for pre-final
// reorg observability. See the build spec's "Read this second" section
// for why: BSC's Fermi hard fork (14 Jan 2026) cut block time to ~0.45s
// and chain-level finality (BEP-126, exposed as the standard
// eth_getBlockByNumber("finalized") tag) to ~1s, making a hand-picked
// confirmation-depth policy both stale and strictly weaker than trusting
// the chain's own consensus commitment to a block.
package finality

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/money"
)

// ErrPermanentFailure is a sentinel a FinalHandler wraps its own error in
// to mean "retrying this exact call can never succeed" -- C2.7's
// ledgerclient.ReportDepositFinal wraps it for C1's illegal_transition
// (the order left quoted before C2 got here -- C2.8's job, not a retry
// case) and idempotency_conflict (a structural bug in this service's own
// key construction, not a transient condition). finalize treats an error
// wrapping this differently from any other FinalHandler failure: dropped
// from tracking instead of retried next tick, since retrying a call that
// cannot succeed only delays noticing it needs a person, not a retry.
var ErrPermanentFailure = errors.New("finality: permanent failure, do not retry")

// ErrOrphanedDeposit is the narrower of the two ErrPermanentFailure
// cases (C2.7's illegal_transition specifically, wrapped alongside
// ErrPermanentFailure, never alone): a deposit finalized on-chain for an
// order C1 no longer considers open. finalize routes an error wrapping
// this to HandleUnreportable (C2.8) instead of just logging and
// dropping -- this is real customer money that needs a human, not a
// structural bug in this service's own code (that's what
// ErrPermanentFailure alone, without this, still means).
var ErrOrphanedDeposit = errors.New("finality: order no longer open for this deposit")

// DefaultStalePendingCeiling is the build spec's own example: "orders of
// magnitude beyond the ~1s current expectation" for the finalized tag to
// catch up to a candidate. Crossing it means the provider pool or the
// chain itself is in trouble -- it is never a reason to force-finalize.
const DefaultStalePendingCeiling = 5 * time.Minute

// candidateKey is invariant 2's own idempotency-key shape
// ("tx_hash:log_index"), split into its two parts so it can be used
// directly as a map key; formatted into that exact string only where a
// string is actually needed (C2.7's Idempotency-Key header, not built
// here).
type candidateKey struct {
	TxHash   common.Hash
	LogIndex uint
}

// ObservedLog is everything OnLogObserved needs about a single Transfer
// log that has already been fetched (via chain.Pool.LogsAt -- 2-provider
// agreement on content, invariant 5), parsed (chain.ParseTransferLog),
// and classified against its order (chain.ClassifyAgainstOrder).
//
// This is a concrete struct rather than the build spec's own bare "log"
// parameter for OnLogObserved: a finality decision needs to know which
// order to credit once final (OrderID/ExternalID/CustomerID) and the
// exact chain time invariant 7 requires (BlockTime, never wall-clock),
// neither of which chain.ParseTransferLog alone produces. Resolving
// those -- which requires the address book, C2.1 -- is the caller's job,
// done once, before this package ever sees the log.
type ObservedLog struct {
	TxHash     common.Hash
	LogIndex   uint
	Height     uint64
	BlockTime  time.Time // UTC block timestamp -- never wall-clock (invariant 7)
	OrderID    int64
	ExternalID string
	CustomerID string
	Amount     money.Amount
	// SenderAddress is the Transfer log's `from`, EIP-55 checksummed
	// (chain.ParseTransferLog + common.Address.Hex(), never re-derived
	// here). C3 (screening) needs it and has no chain access of its own
	// -- this is the one hop of that trip this package carries; C2.7's
	// ledgerclient.ReportDepositFinal is what actually forwards it to
	// C1. See docs/03-build/c3-screening-build-prompts.md's "Read this
	// first" in the ledger repo's own build-prompts doc for the full
	// path this closes.
	SenderAddress string
}

func (o ObservedLog) key() candidateKey { return candidateKey{o.TxHash, o.LogIndex} }

// DepositFinalIdempotencyKeyPrefix is invariant 2's own prefix for a
// deposit_final entry's idempotency key. Exported so ledgerclient (the
// actual HTTP caller, C2.7 for the initial report and this package for
// the reorg report) can derive its own related key without duplicating
// the literal string.
const DepositFinalIdempotencyKeyPrefix = "watcher:deposit_final:"

// DepositFinalIdempotencyKey builds invariant 2's own key exactly:
// "watcher:deposit_final:<tx_hash>:<log_index>". This is the single place
// that format is defined -- C2.7's deposit_final report and this
// package's own post-final reorg report (which cites the original key
// back to C1) both call this, so the two can never drift apart.
func DepositFinalIdempotencyKey(txHash common.Hash, logIndex uint) string {
	return fmt.Sprintf("%s%s:%d", DepositFinalIdempotencyKeyPrefix, txHash.Hex(), logIndex)
}

// Candidate is a tracked deposit: observed and classified, not yet final
// (or, if a pre-final reorg removed it, never final at all).
type Candidate struct {
	ObservedLog
	Classification chain.Classification
	DetectedAt     time.Time

	alertedStale bool
	contradicted bool // set once a post-final reorg has been logged for this candidate -- see handlePostFinalReorg

	// asyncConfirmations is Design B's own per-provider confirmation
	// state (async.go, Phase 1) -- nil until CheckFinalityAsync first
	// touches this candidate (async_driver.go, Phase 2). Design A's own
	// code paths (OnLogObserved, CheckFinality, checkPostFinalReorgs)
	// never read or write this field, so they are completely unaffected
	// by its presence.
	asyncConfirmations *CandidateConfirmations
	// alertedDisagreement mirrors alertedStale's own "fire once" pattern
	// (see checkStalePending) for Design B's own DISAGREEMENT_ALERT --
	// Config.OnDisagreement is called exactly once per candidate, the
	// first tick its async confirmations enter that state, never again
	// on every subsequent tick it remains there.
	alertedDisagreement bool
}

// FinalHandler is invoked exactly once for each candidate the moment it
// finalizes. C2.7's real implementation reports it to C1; this package
// only decides *when*, never *how* -- per this chunk's own build spec
// ("this chunk defines when, not how it's transmitted").
//
// If FinalHandler returns an error, the candidate is left pending rather
// than marked final: C1's own idempotency-key convention (invariant 2)
// makes retrying safe -- a repeated report for the same
// tx_hash:log_index is a no-op on C1's side, never a double credit -- so
// retrying on the next tick is strictly safer than either silently
// dropping the report or asserting finality for a report that was never
// actually delivered.
type FinalHandler func(ctx context.Context, c Candidate) error

// StalePendingHandler is invoked once -- not on every tick -- the first
// time a still-pending candidate crosses Config.StalePendingCeiling.
type StalePendingHandler func(c Candidate, pending time.Duration)

// ReorgReporter posts a post-final reorg report to C1 -- C2.6's own
// ReportReorg(ctx, orderExternalID, originalEntryKey) error, per the
// build spec. Behind an interface not because the endpoint is
// unreachable (C1.11 closed that gap) but so this package's own
// detection/bookkeeping logic can be tested against a fake without a
// live C1 for every run; ledgerclient.Client is the real implementation.
//
// Mapping C1's response to scenario A/B and alerting at the right
// severity for each is this interface's own implementation's job (it
// has the HTTP response; this package does not) -- by the time
// ReportReorg returns nil here, that mapping and alerting has already
// happened.
type ReorgReporter interface {
	ReportReorg(ctx context.Context, externalID, originalEntryKey string) error
}

// OrphanedDepositRecorder persists a deposit C1 no longer has an open
// order for (C2.8's mechanism, gap #3's still-undecided policy) --
// internal/orphaned.Record is the real store this backs onto. Behind an
// interface for the same reason FinalHandler/ReorgReporter are:
// testable without a live database for every run.
//
// This package has no way to learn the order's current state itself
// (that requires a fresh C1 lookup, and finality deliberately never
// imports ledgerclient) -- a real implementation is expected to fetch it
// fresh at recording time (via GetOrder, best-effort, e.g. "unknown" if
// even that fails) rather than trust anything cached from before c1Error
// happened, which could already be stale by the time this runs.
type OrphanedDepositRecorder interface {
	RecordOrphanedDeposit(ctx context.Context, c Candidate, c1Error error) error
}

// Config controls a Tracker's chain-query target and alerting.
type Config struct {
	// ContractAddress and TransferTopic scope the re-verification LogsAt
	// query CheckFinality issues for every candidate becoming eligible.
	// Both are required -- a value this load-bearing must never be left
	// as an accidental zero value.
	ContractAddress common.Address
	TransferTopic   common.Hash

	OnFinal                 FinalHandler
	OnStalePending          StalePendingHandler // optional
	ReorgReporter           ReorgReporter
	OrphanedDepositRecorder OrphanedDepositRecorder

	// OnDisagreement is Design B's own alert hook (async_driver.go,
	// Phase 2) -- optional, nil means "log only" (CheckFinalityAsync
	// always logs at slog.Error regardless, matching
	// handlePostFinalReorg's own posture; this hook is for a caller that
	// wants real paging/alerting on top, the same relationship
	// OnStalePending already has to its own logging). Called exactly
	// once per candidate, never on every tick it remains in
	// DISAGREEMENT_ALERT. Never used by Design A's own CheckFinality.
	OnDisagreement func(c Candidate, note string)

	// AsyncMinAgreement is Design B's own quorum size, reusing the exact
	// same chain.Config.MinAgreement value the caller already configured
	// on the chain.Pool this Tracker is used with -- passed here too
	// (rather than read off the Pool internally) because
	// finality.Config is constructed independently of any particular
	// chain.Pool, mirroring ContractAddress/TransferTopic's own
	// "caller's job to keep in sync" posture. Required only if
	// CheckFinalityAsync is ever actually called; CheckFinality (Design
	// A) never reads this field.
	AsyncMinAgreement int

	// Metrics is optional -- nil means no metrics are recorded, never a
	// panic. C2.9's httpapi.Metrics implements this to drive
	// candidates_detected_total/candidates_finalized_total.
	Metrics MetricsRecorder

	// StalePendingCeiling defaults to DefaultStalePendingCeiling if <= 0.
	StalePendingCeiling time.Duration

	// Now defaults to time.Now; overridable so tests can drive the
	// stale-pending ceiling without actually sleeping for it.
	Now func() time.Time
}

// MetricsRecorder is how a Tracker reports the two counters C2.9's build
// spec names that only this package ever knows the moment of: a
// candidate becoming trackable, and one reaching finality. Behind an
// interface, optional, for the same reason every other Config dependency
// here is: testable without pulling in a metrics library for every run.
type MetricsRecorder interface {
	CandidateDetected()
	CandidateFinalized()
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

// Tracker holds every deposit candidate observed but not yet finalized
// (or dropped by a pre-final reorg), entirely in process memory. A
// restart loses in-flight candidates -- there is no persistence chunk in
// this package's own build spec, and inventing one here would be scope
// this chunk did not ask for. The real-world consequence (a deposit
// mid-tracking at restart time is never finalized until something
// re-observes it) is a genuine gap; flagged, not silently accepted.
type Tracker struct {
	cfg Config

	mu        sync.Mutex
	pending   map[candidateKey]*Candidate
	finalized map[candidateKey]*Candidate // invariant 4: once finalized, never re-tracked or re-finalized -- kept (not just a bool) so a later tick can re-verify it's still genuinely final

	// heightMonitor is Design B's own cross-candidate, cross-tick
	// monotonicity guard (async.go's ProviderHeightMonitor, Phase 1) --
	// one per Tracker, not per candidate, since a provider's own
	// finalized-height regression is a fact about that provider, not
	// about any single deposit. Lazily created on first use by
	// CheckFinalityAsync; nil (and never touched) for a Tracker that
	// only ever runs Design A's own CheckFinality.
	heightMonitor *ProviderHeightMonitor
}

// New validates cfg and returns a ready-to-use Tracker.
func New(cfg Config) (*Tracker, error) {
	if cfg.ContractAddress == (common.Address{}) {
		return nil, errors.New("finality: Config.ContractAddress must be set")
	}
	if cfg.TransferTopic == (common.Hash{}) {
		return nil, errors.New("finality: Config.TransferTopic must be set")
	}
	if cfg.OnFinal == nil {
		return nil, errors.New("finality: Config.OnFinal must be set")
	}
	if cfg.ReorgReporter == nil {
		return nil, errors.New("finality: Config.ReorgReporter must be set")
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
		cfg:       cfg,
		pending:   make(map[candidateKey]*Candidate),
		finalized: make(map[candidateKey]*Candidate),
	}, nil
}

// OnLogObserved fires deposit.detected -- 0-conf, advisory only, never
// touching C1 -- and, for a trackable classification, starts tracking
// log toward finality. Idempotent on (TxHash, LogIndex): a duplicate
// delivery of the same log (a provider replaying an event, or the
// ingestion loop re-observing a block it already processed) is a no-op,
// never a second detected event or a second candidate.
//
// ZeroValue is never tracked -- "legal on-chain, meaningless here," per
// the C2.4 build spec -- it is logged as an anomaly and nothing more.
// WrongToken never reaches here at all: a log that fails
// chain.ParseTransferLog never has an ObservedLog or Classification to
// pass in the first place.
func (t *Tracker) OnLogObserved(ctx context.Context, log ObservedLog, classification chain.Classification) error {
	key := log.key()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.finalized[key] != nil || t.pending[key] != nil {
		return nil // duplicate delivery -- no-op, not a second event
	}

	if classification == chain.ZeroValue {
		slog.Warn("finality: zero-value transfer to a watched address (anomaly, not a deposit)",
			"tx_hash", log.TxHash, "log_index", log.LogIndex, "order_id", log.OrderID)
		return nil
	}

	slog.Info("finality: deposit.detected (0-conf, advisory only)",
		"tx_hash", log.TxHash, "log_index", log.LogIndex, "height", log.Height,
		"order_id", log.OrderID, "amount", log.Amount, "classification", classification)

	t.pending[key] = &Candidate{ObservedLog: log, Classification: classification, DetectedAt: t.cfg.Now()}
	t.recordDetected()
	return nil
}

// CheckFinality polls pool.LatestFinalized, promotes every pending
// candidate at or below the agreed finalized height to final (calling
// Config.OnFinal exactly once for each), and separately re-verifies
// every ALREADY-finalized candidate still remembered, to catch a
// post-final reorg (C2.6): the chain's own BEP-126 finality being
// violated, an extraordinary event categorically different from the
// routine pre-final case above. Intended to be called on a ticker (e.g.
// every 2-3s); wiring that ticker is cmd/watcherd's job, not this
// function's, matching this chunk's own build spec.
//
// A candidate is re-verified against a fresh, 2-provider-agreed LogsAt
// query at its own height before being finalized -- never assumed final
// just because its height is now <= the finalized height. A log
// observed at height H can still have been reorged out between
// observation and now, before finality ever reached it (the routine,
// expected pre-final reorg case); re-querying is what turns that into
// silence rather than a false finalization. HeaderByNumber (primary-
// provider-only, C2.3) is never used for this: invariant 5 requires
// 2-provider agreement for every part of a finality decision, and
// LogsAt is the only primitive here that provides it.
func (t *Tracker) CheckFinality(ctx context.Context, pool *chain.Pool) error {
	finalHeight, _, err := pool.LatestFinalized(ctx)
	if err != nil {
		// Provider disagreement or insufficient participation:
		// finalization is withheld this tick, not granted on a
		// majority-of-one basis (invariant 5). The next tick tries
		// again; nothing is lost by waiting.
		return fmt.Errorf("finality: withholding finalization this tick: %w", err)
	}

	t.checkStalePending()

	byHeight := make(map[uint64][]*Candidate)
	for _, c := range t.readyCandidates(finalHeight) {
		byHeight[c.Height] = append(byHeight[c.Height], c)
	}

	for height, candidates := range byHeight {
		logs, err := pool.LogsAt(ctx, height, height, t.cfg.ContractAddress, [][]common.Hash{{t.cfg.TransferTopic}})
		if err != nil {
			// Can't re-verify this height this tick -- leave these
			// candidates pending rather than guessing either way.
			slog.Error("finality: re-verifying candidates at height failed, deferring to next tick",
				"height", height, "error", err)
			continue
		}
		present := make(map[candidateKey]bool, len(logs))
		for _, l := range logs {
			present[candidateKey{l.TxHash, l.Index}] = true
		}
		for _, c := range candidates {
			if !present[c.key()] {
				// Reorged out before finality reached it -- the
				// routine, expected case. Silence, not a reorg report
				// (C2.6's job is AFTER finality).
				slog.Info("finality: candidate no longer present at its height, dropping silently (pre-final reorg)",
					"tx_hash", c.TxHash, "log_index", c.LogIndex, "height", c.Height)
				t.drop(c.key())
				continue
			}
			t.finalize(ctx, c)
		}
	}

	t.checkPostFinalReorgs(ctx, pool)
	return nil
}

func (t *Tracker) readyCandidates(finalHeight uint64) []*Candidate {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ready []*Candidate
	for _, c := range t.pending {
		if c.Height <= finalHeight {
			ready = append(ready, c)
		}
	}
	return ready
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

func (t *Tracker) drop(key candidateKey) {
	t.mu.Lock()
	delete(t.pending, key)
	t.mu.Unlock()
}

func (t *Tracker) finalize(ctx context.Context, c *Candidate) {
	key := c.key()
	if err := t.cfg.OnFinal(ctx, *c); err != nil {
		if errors.Is(err, ErrPermanentFailure) {
			if errors.Is(err, ErrOrphanedDeposit) {
				// Real customer money, an order C1 no longer has open
				// for it: must be recorded, not merely logged, before
				// this candidate is allowed to stop being tracked. If
				// recording itself fails (e.g. a transient DB error),
				// leave it pending -- retrying HandleUnreportable next
				// tick is safe (Record is idempotent on tx_hash:log_index)
				// and strictly better than losing track of an orphaned
				// deposit because its OWN recording attempt happened to
				// fail once.
				if handleErr := t.HandleUnreportable(ctx, *c, err); handleErr != nil {
					slog.Error("finality: HandleUnreportable failed, will retry next tick",
						"tx_hash", c.TxHash, "log_index", c.LogIndex, "error", handleErr)
					return
				}
			} else {
				// A structural bug in this service's own code (e.g.
				// idempotency_conflict), not a customer-money case --
				// nothing to record, just needs a person to look at it.
				slog.Error("finality: OnFinal permanently failed, dropping from tracking",
					"tx_hash", c.TxHash, "log_index", c.LogIndex, "order_id", c.OrderID, "external_id", c.ExternalID, "error", err)
			}
			t.mu.Lock()
			delete(t.pending, key)
			t.mu.Unlock()
			return
		}
		slog.Error("finality: OnFinal handler failed, will retry next tick",
			"tx_hash", c.TxHash, "log_index", c.LogIndex, "error", err)
		return
	}
	t.mu.Lock()
	t.finalized[key] = c
	delete(t.pending, key)
	t.mu.Unlock()
	t.recordFinalized()
}

// HandleUnreportable records c as an orphaned deposit (C2.8's mechanism
// for gap #3): a candidate that finalized on-chain but whose order C1 no
// longer considers open, per c1Error (C2.7's illegal_transition
// response). Called from finalize the moment that failure is detected --
// exposed as its own method, matching this chunk's own build spec,
// rather than folded invisibly into finalize's private logic.
//
// This never retries the transition, never guesses at a resolution, and
// posts nothing else to C1 on its own initiative -- strictly a
// capture-and-surface mechanism until gap #3's actual business policy
// exists to drive one. The alert here is the loud, immediate signal;
// Config.OrphanedDepositRecorder (internal/orphaned) is what makes the
// row durable and visible via C2.9's HTTP surface afterward.
func (t *Tracker) HandleUnreportable(ctx context.Context, c Candidate, c1Error error) error {
	if err := t.cfg.OrphanedDepositRecorder.RecordOrphanedDeposit(ctx, c, c1Error); err != nil {
		return fmt.Errorf("finality: recording orphaned deposit for %s: %w", c.ExternalID, err)
	}
	slog.Error("finality: ORPHANED DEPOSIT -- a finalized deposit's order is no longer open; recorded for manual reconciliation",
		"tx_hash", c.TxHash, "log_index", c.LogIndex, "order_id", c.OrderID, "external_id", c.ExternalID,
		"amount", c.Amount, "c1_error", c1Error)
	return nil
}

// checkPostFinalReorgs re-verifies every remembered finalized candidate
// against a fresh LogsAt query at its own height -- the same
// invariant-5-compliant primitive readyCandidates' promotion uses above,
// applied to candidates already reported to C1. This grows without bound
// over the service's lifetime (every finalized deposit, forever, gets
// re-checked every tick) -- an accepted, documented cost at this
// system's real scale (~100 deposits/day, per component-map.md), not a
// design that would still be right at a much larger one.
// pool is AsyncChainQuerier (async_driver.go), not the concrete
// *chain.Pool -- widened so CheckFinalityAsync can call this shared
// method too, passing whatever satisfies the interface. A concrete
// *chain.Pool (what CheckFinality's own call below actually passes)
// trivially satisfies it, so this change is non-breaking for Design A.
func (t *Tracker) checkPostFinalReorgs(ctx context.Context, pool AsyncChainQuerier) {
	byHeight := make(map[uint64][]*Candidate)
	t.mu.Lock()
	for _, c := range t.finalized {
		byHeight[c.Height] = append(byHeight[c.Height], c)
	}
	t.mu.Unlock()
	if len(byHeight) == 0 {
		return
	}

	for height, candidates := range byHeight {
		logs, err := pool.LogsAt(ctx, height, height, t.cfg.ContractAddress, [][]common.Hash{{t.cfg.TransferTopic}})
		if err != nil {
			slog.Error("finality: re-verifying already-finalized candidates at height failed, will retry next tick",
				"height", height, "error", err)
			continue
		}
		present := make(map[candidateKey]bool, len(logs))
		for _, l := range logs {
			present[candidateKey{l.TxHash, l.Index}] = true
		}
		for _, c := range candidates {
			if !present[c.key()] {
				t.handlePostFinalReorg(ctx, c)
			}
		}
	}
}

// handlePostFinalReorg is called the moment a previously-finalized
// candidate's log is no longer present at its own height: the chain's
// own consensus finality was itself violated, categorically different
// from the routine pre-final case (invariant 4 -- finality, once
// asserted, is only ever reversed through this explicit, auditable
// path, never silently). Logs loudly exactly once (not every retry),
// then calls Config.ReorgReporter.ReportReorg with the exact
// idempotency key C1 was originally given (DepositFinalIdempotencyKey),
// retrying on the next tick if that call fails -- safe because a
// repeated report hits C1's own already_reversed path, never a double
// reversal.
func (t *Tracker) handlePostFinalReorg(ctx context.Context, c *Candidate) {
	key := c.key()

	t.mu.Lock()
	firstDetection := !c.contradicted
	c.contradicted = true
	t.mu.Unlock()

	if firstDetection {
		slog.Error("finality: POST-FINAL REORG DETECTED -- a previously finalized deposit is no longer present on chain; this should be near-impossible under BEP-126 finality",
			"tx_hash", c.TxHash, "log_index", c.LogIndex, "height", c.Height,
			"order_id", c.OrderID, "external_id", c.ExternalID)
	}

	originalEntryKey := DepositFinalIdempotencyKey(c.TxHash, c.LogIndex)
	if err := t.cfg.ReorgReporter.ReportReorg(ctx, c.ExternalID, originalEntryKey); err != nil {
		slog.Error("finality: ReportReorg failed, will retry next tick",
			"tx_hash", c.TxHash, "log_index", c.LogIndex, "external_id", c.ExternalID, "error", err)
		return
	}

	t.mu.Lock()
	delete(t.finalized, key)
	t.mu.Unlock()
}

// PendingCount reports how many candidates are currently tracked but not
// yet final -- exported for tests and operator visibility (e.g. a health
// endpoint), not used internally by this package itself.
func (t *Tracker) PendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// FinalizedCount reports how many finalized candidates are still being
// remembered and re-verified for a post-final reorg -- exported for
// tests and operator visibility, not used internally by this package.
func (t *Tracker) FinalizedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.finalized)
}

// OldestPendingDetectedAt returns the DetectedAt of the longest-pending
// candidate, for operator visibility (C2.9's GET /system/invariants,
// "oldest pending candidate age") -- computing the age itself from that
// timestamp is the caller's job, against its own clock, not this
// package's Config.Now (which exists for testability, not for serving
// operator-facing wall-clock reads).
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
