// Design B's real per-provider query orchestration -- Phase 2. async.go
// (Phase 1) stays a pure decision core with no RPC of its own; this file
// is what actually calls chain.Pool's new single-provider primitives
// (chain/single_provider.go), decodes real responses into
// ProviderObservation values, and feeds them to that core. Nothing here
// duplicates Design A's own OnFinal/drop/orphaned-deposit/reorg logic --
// every terminal outcome routes through the SAME existing methods
// CheckFinality (finality.go) already uses, so Design B inherits their
// existing idempotency and safety guarantees for free rather than
// re-implementing them.
//
// Deliberately fully sequential (no goroutines across providers or
// candidates), unlike LatestFinalized/LogsAt's own parallel fan-out --
// a conservative choice for this phase: this system's own real volume
// (~4 deposits/hour peak) makes the performance cost negligible, and
// sequential execution avoids an entire class of concurrency bugs
// around lazily-initializing Candidate.asyncConfirmations and calling
// finalize/drop from multiple goroutines at once. Can be parallelized
// later, safely, once actually needed -- not needed for this phase.
package finality

import (
	"context"
	"log/slog"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"depositwatcher/internal/chain"
)

// AsyncChainQuerier is the slice of *chain.Pool's own methods Design B's
// orchestration needs -- both CheckFinalityAsync's own per-provider
// calls and the shared checkPostFinalReorgs (finality.go) it also
// drives (see CheckFinalityAsync's own doc comment for why that call is
// here at all). Behind an interface for the same reason every other
// Config dependency in this package already is (OnFinal/ReorgReporter/
// OrphanedDepositRecorder): testable without a real chain.Pool -- which
// itself needs real *ethclient.Client connections -- for every run.
// *chain.Pool already satisfies this exactly as-is; no adapter needed
// at the real call site (cmd/watcherd), and CheckFinality's own existing
// call to checkPostFinalReorgs with a concrete *chain.Pool keeps
// compiling unchanged (a concrete type trivially satisfies a matching
// interface).
type AsyncChainQuerier interface {
	ProviderNames() []string
	FinalizedFrom(ctx context.Context, providerName string) (height uint64, hash common.Hash, err error)
	BlockHashAt(ctx context.Context, providerName string, height uint64) (common.Hash, error)
	LogsAtFrom(ctx context.Context, providerName string, fromBlock, toBlock uint64, contractAddress common.Address, topics [][]common.Hash) ([]types.Log, error)
	LogsAt(ctx context.Context, fromBlock, toBlock uint64, contractAddress common.Address, topics [][]common.Hash) ([]types.Log, error)
}

// CheckFinalityAsync is Design B's own top-level tick entry point --
// candidates/loop.go's runTick calls this INSTEAD OF CheckFinality when
// WATCHER_FINALITY_MODE=async (cmd/watcherd), never both in the same
// tick. Config.AsyncMinAgreement, Config.ContractAddress, and
// Config.TransferTopic are all required (the same load-bearing values
// CheckFinality already requires for its own joint round); nothing here
// changes what a real deposit must look like to finalize, only how many
// separate, independent queries confirm it.
func (t *Tracker) CheckFinalityAsync(ctx context.Context, pool AsyncChainQuerier) error {
	t.mu.Lock()
	if t.heightMonitor == nil {
		t.heightMonitor = NewProviderHeightMonitor()
	}
	t.mu.Unlock()

	for _, providerName := range pool.ProviderNames() {
		if err := t.checkOneProviderAsync(ctx, pool, providerName); err != nil {
			slog.Error("finality: async check failed for provider, will retry next tick",
				"provider", providerName, "error", err)
			continue
		}
	}

	// The existing post-finalization reorg check is Design-agnostic: it
	// only ever reads t.finalized, never cares which mechanism put a
	// candidate there. Design A's own CheckFinality already calls this;
	// Design B must call it too, explicitly, since only one of the two
	// tick functions runs per deployment (candidates/loop.go's own
	// mode switch) -- without this call here, a Design-B deployment
	// would never re-verify its own finalized candidates at all.
	t.checkPostFinalReorgs(ctx, pool)
	return nil
}

// checkOneProviderAsync performs steps 1-2 (this provider's own
// finalized-height check, monotonicity-guarded) once, then steps 3-6
// (independent block-hash + log re-query + decode + record) once per
// currently-pending candidate that provider is eligible to check. A
// FRESH snapshot of t.pending is taken here, not reused across
// providers within the same tick, so a candidate an earlier provider
// already finalized or dropped THIS tick is correctly excluded from a
// later provider's own inner loop.
func (t *Tracker) checkOneProviderAsync(ctx context.Context, pool AsyncChainQuerier, providerName string) error {
	height, _, err := pool.FinalizedFrom(ctx, providerName)
	if err != nil {
		return err // rule 1: transport failure -- no vote, retried next tick
	}

	t.mu.Lock()
	monitor := t.heightMonitor
	t.mu.Unlock()
	if ok := monitor.Check(providerName, height); !ok {
		slog.Warn("finality: provider's own finalized height regressed -- excluded this tick, not trusted",
			"provider", providerName, "reported_height", height)
		return nil // excluded, not an error to retry differently -- next tick tries again
	}

	for _, candidate := range t.pendingSnapshot() {
		if candidate.Height > height {
			continue // this provider hasn't reached this candidate's own target height yet
		}
		t.checkOneCandidateAsync(ctx, pool, providerName, candidate)
	}
	return nil
}

// pendingSnapshot returns the current pending candidates as a slice,
// safe to range over without holding t.mu for the duration -- mirrors
// readyCandidates' own identical locking shape (finality.go).
func (t *Tracker) pendingSnapshot() []*Candidate {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Candidate, 0, len(t.pending))
	for _, c := range t.pending {
		out = append(out, c)
	}
	return out
}

// isStillPending reports whether key is still in t.pending right now --
// checked immediately before acting on a candidate so a candidate
// already finalized/dropped by an earlier provider THIS SAME tick is
// never processed again (idempotence/no-double-finalization, verified
// by TestAsyncDriver_NoDoubleFinalization).
func (t *Tracker) isStillPending(key candidateKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.pending[key]
	return ok
}

// checkOneCandidateAsync performs ProviderObservation's own two required
// checks (steps 3-4) against ONE provider for ONE candidate: an
// independent historical block-hash lookup at the candidate's own
// target height, and an independent log re-query at that same height.
// Every value in the resulting ProviderObservation comes from THIS
// provider's own two calls -- never inferred, never borrowed from
// another provider's own result (the invariant the whole design exists
// to enforce). On success, records the observation and acts on
// whatever status comes back; on any transport error, records nothing
// (rule 1) and simply returns, logged, retried next tick.
func (t *Tracker) checkOneCandidateAsync(ctx context.Context, pool AsyncChainQuerier, providerName string, candidate *Candidate) {
	key := candidate.key()
	if !t.isStillPending(key) {
		return // an earlier provider this same tick already finalized or dropped it
	}

	blockHash, err := pool.BlockHashAt(ctx, providerName, candidate.Height)
	if err != nil {
		slog.Error("finality: async block-hash check failed, will retry next tick",
			"provider", providerName, "tx_hash", candidate.TxHash, "log_index", candidate.LogIndex, "height", candidate.Height, "error", err)
		return
	}

	logs, err := pool.LogsAtFrom(ctx, providerName, candidate.Height, candidate.Height,
		t.cfg.ContractAddress, [][]common.Hash{{t.cfg.TransferTopic}})
	if err != nil {
		slog.Error("finality: async log re-query failed, will retry next tick",
			"provider", providerName, "tx_hash", candidate.TxHash, "log_index", candidate.LogIndex, "height", candidate.Height, "error", err)
		return
	}

	obs := ProviderObservation{Height: candidate.Height, BlockHash: blockHash, OrderID: candidate.OrderID}
	for _, l := range logs {
		if l.TxHash != candidate.TxHash || l.Index != candidate.LogIndex {
			continue
		}
		// Independently decode THIS provider's own returned log --
		// never trust the candidate's own already-recorded Amount for
		// what this provider itself is reporting; a provider that
		// returns matching (tx_hash, log_index) identifiers but altered
		// Data would otherwise slip through unnoticed (the "lies about
		// historical content" adversarial case).
		_, _, amount, parseErr := chain.ParseTransferLog(l)
		if parseErr != nil {
			// A matching (tx_hash, log_index) that fails to parse is an
			// INVALID observation from this provider, not evidence of
			// absence -- conflating the two would let a one-off decode
			// glitch (a truncated field, a malformed proxy response) on
			// an otherwise-present deposit count toward ASYNC_NOT_FOUND
			// exactly as if this provider had genuinely looked and found
			// nothing. Same posture as a transport error above: no vote
			// recorded at all this tick, logged, retried next tick --
			// never RecordConfirmed with Present:false for this case.
			slog.Error("finality: async log re-query returned a matching tx_hash/log_index that fails to parse as a Transfer -- invalid observation, NOT recorded as absent, will retry next tick",
				"provider", providerName, "tx_hash", candidate.TxHash, "log_index", candidate.LogIndex, "height", candidate.Height, "error", parseErr)
			return
		}
		obs.Present = true
		obs.Amount = amount
		break
	}

	if !t.isStillPending(key) {
		// Nothing else touches candidate state concurrently (this driver
		// is deliberately sequential -- see this file's own top-of-file
		// doc comment), so this can only be true if the candidate was
		// already resolved earlier in THIS SAME provider's own inner
		// loop -- structurally unreachable today, since each candidate
		// appears at most once per pendingSnapshot(). Checked anyway,
		// defensively, before ever recording an observation or calling
		// finalize/drop below.
		return
	}

	if candidate.asyncConfirmations == nil {
		candidate.asyncConfirmations = NewCandidateConfirmations(t.cfg.AsyncMinAgreement)
	}
	status := candidate.asyncConfirmations.RecordConfirmed(providerName, obs)

	switch status {
	case AsyncQuorumReached:
		t.finalize(ctx, candidate)
	case AsyncNotFound:
		slog.Info("finality: async quorum of providers independently confirm this deposit never became final -- dropping (routine, not an alert)",
			"tx_hash", candidate.TxHash, "log_index", candidate.LogIndex, "height", candidate.Height)
		t.drop(key)
	case AsyncDisagreementAlert:
		if !candidate.alertedDisagreement {
			candidate.alertedDisagreement = true
			note := candidate.asyncConfirmations.DisagreementNote()
			slog.Error("finality: ASYNC DISAGREEMENT -- providers report genuinely different canonical history for this deposit; held pending human review, never auto-finalized or auto-dropped",
				"tx_hash", candidate.TxHash, "log_index", candidate.LogIndex, "height", candidate.Height,
				"order_id", candidate.OrderID, "external_id", candidate.ExternalID, "detail", note)
			if t.cfg.OnDisagreement != nil {
				t.cfg.OnDisagreement(*candidate, note)
			}
		}
	case AsyncPending:
		// nothing to do -- retried next tick
	}
}
