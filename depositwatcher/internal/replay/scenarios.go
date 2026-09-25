package replay

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/chain"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/money"
	"depositwatcher/internal/orphaned"
)

func pass(name string) Result { return Result{Name: name, Passed: true} }
func fail(name string, err error) Result {
	return Result{Name: name, Passed: false, Detail: err.Error()}
}

// nextHeight hands out a fresh, never-repeated block height so scenarios
// never collide on the sim chain, regardless of run order.
func (h *harness) nextHeight() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	return uint64(1_000_000 + h.seq)
}

func (h *harness) newTxHash() common.Hash {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	return common.BigToHash(big.NewInt(time.Now().UnixNano() + h.seq))
}

func addressTopic(addr addresses.Address) common.Hash {
	return common.BytesToHash(common.HexToAddress(string(addr)).Bytes())
}

// newTransferLog builds a well-formed Transfer log carrying minorUnits
// (this service's 6-decimal convention), rescaled to the real contract's
// 18 on-chain decimals -- the inverse of chain.ParseTransferLog's own
// rescale.
func newTransferLog(height uint64, txHash, toTopic common.Hash, index uint, minorUnits money.Amount) types.Log {
	raw := new(big.Int).Mul(big.NewInt(int64(minorUnits)), big.NewInt(1_000_000_000_000))
	data := make([]byte, 32)
	raw.FillBytes(data)
	return types.Log{
		Address: contractAddress, Topics: []common.Hash{transferTopic, common.Hash{}, toTopic},
		Data: data, TxHash: txHash, Index: index,
	}
}

// preparedOrder is a real C1 order plus its C2 watched address, ready for
// a scenario to send a deposit to.
type preparedOrder struct {
	ExternalID      string
	OrderID         int64
	CustomerID      string
	DepositAccount  string
	CustomerAccount string
	Address         addresses.Address
}

func (h *harness) prepareOrder(amountIn string, quoteExpiresIn time.Duration) (preparedOrder, error) {
	externalID := h.nextExternalID("order")
	customerID := h.nextExternalID("cust")
	order, err := h.ledger.createOrder(h.ctx, externalID, customerID, amountIn, "1.000000", "0.000000", "0.000000", quoteExpiresIn)
	if err != nil {
		return preparedOrder{}, fmt.Errorf("creating order: %w", err)
	}

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custAcc := "liability:customer:" + customerID
	if err := h.ledger.createAccount(h.ctx, h.ledgerPool, depositAcc, "ASSET", "USDT_BEP20", 1); err != nil {
		return preparedOrder{}, fmt.Errorf("creating deposit account: %w", err)
	}
	if err := h.ledger.createAccount(h.ctx, h.ledgerPool, custAcc, "LIABILITY", "USDT_BEP20", -1); err != nil {
		return preparedOrder{}, fmt.Errorf("creating customer account: %w", err)
	}

	now := time.Now().UTC()
	addr, err := addresses.Assign(h.ctx, h.watcherPool, order.ID, externalID, customerID, now, now.Add(quoteExpiresIn))
	if err != nil {
		return preparedOrder{}, fmt.Errorf("assigning address: %w", err)
	}

	return preparedOrder{
		ExternalID: externalID, OrderID: order.ID, CustomerID: customerID,
		DepositAccount: depositAcc, CustomerAccount: custAcc, Address: addr,
	}, nil
}

// scenarioCleanDeposits: exact, over, and under amounts each reach
// finality via the finalized-tag path and fund their order. Dust never
// does: a payment below the dust floor (address poisoning, a stray
// transfer) is recorded as orphaned and leaves the order waiting for its
// real deposit.
func (h *harness) scenarioCleanDeposits() Result {
	const name = "CleanDeposits"
	cases := []struct {
		label string
		minor money.Amount
		want  chain.Classification
	}{
		{"exact", 3000_000000, chain.Exact},
		{"over", 3500_000000, chain.Overpay},
		{"under", 2000_000000, chain.Underpay},
		{"dust", 500_000, chain.Dust},
	}
	for _, tc := range cases {
		order, err := h.prepareOrder("3000.000000", time.Hour)
		if err != nil {
			return fail(name, fmt.Errorf("%s: %w", tc.label, err))
		}
		height := h.nextHeight()
		log := newTransferLog(height, h.newTxHash(), addressTopic(order.Address), 0, tc.minor)
		h.chain.commitBlockToAll(height, time.Now(), []types.Log{log})
		h.chain.finalizeAll(height)

		if err := h.tick(height, height); err != nil {
			return fail(name, fmt.Errorf("%s: tick: %w", tc.label, err))
		}

		updated, err := h.ledger.getOrder(h.ctx, order.ExternalID)
		if err != nil {
			return fail(name, fmt.Errorf("%s: %w", tc.label, err))
		}
		want := "funded"
		if tc.want == chain.Dust {
			want = "quoted"
		}
		if updated.State != want {
			return fail(name, fmt.Errorf("%s: order state = %q, want %s", tc.label, updated.State, want))
		}
	}
	return pass(name)
}

// scenarioDepositToRetiredAddress: a deposit landing on an address whose
// order is already terminal (retired) -- a late deposit, C2.8's own
// mechanism, never a finality candidate.
func (h *harness) scenarioDepositToRetiredAddress() Result {
	const name = "DepositToRetiredAddress"
	order, err := h.prepareOrder("3000.000000", time.Hour)
	if err != nil {
		return fail(name, err)
	}
	if err := addresses.Retire(h.ctx, h.watcherPool, order.OrderID, "settled"); err != nil {
		return fail(name, fmt.Errorf("retiring address: %w", err))
	}

	height := h.nextHeight()
	txHash := h.newTxHash()
	log := newTransferLog(height, txHash, addressTopic(order.Address), 0, 3000_000000)
	h.chain.commitBlockToAll(height, time.Now(), []types.Log{log})
	h.chain.finalizeAll(height)

	if err := h.tick(height, height); err != nil {
		return fail(name, err)
	}
	if got := h.tracker.PendingCount(); got != 0 {
		return fail(name, fmt.Errorf("PendingCount() = %d, want 0 -- a late deposit is never a candidate", got))
	}
	found, err := h.orphanedRowFor(txHash)
	if err != nil {
		return fail(name, err)
	}
	if found == nil {
		return fail(name, errors.New("no orphaned_deposits row was recorded for the late deposit"))
	}
	return pass(name)
}

// scenarioDepositAfterQuoteExpiry: a deposit that finalizes after C1 has
// independently moved the order to expired -- races the order's own
// expiry, and must be captured (orphaned, via the illegal_transition
// path), not retried in a loop and not silently dropped.
func (h *harness) scenarioDepositAfterQuoteExpiry() Result {
	const name = "DepositAfterQuoteExpiry"
	order, err := h.prepareOrder("3000.000000", time.Hour)
	if err != nil {
		return fail(name, err)
	}
	current, err := h.ledger.getOrder(h.ctx, order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if err := h.ledger.expireOrder(h.ctx, current); err != nil {
		return fail(name, fmt.Errorf("expiring order: %w", err))
	}

	height := h.nextHeight()
	txHash := h.newTxHash()
	log := newTransferLog(height, txHash, addressTopic(order.Address), 0, 3000_000000)
	h.chain.commitBlockToAll(height, time.Now(), []types.Log{log})
	h.chain.finalizeAll(height)

	if err := h.tick(height, height); err != nil {
		return fail(name, err)
	}
	if got := h.tracker.PendingCount(); got != 0 {
		return fail(name, fmt.Errorf("PendingCount() = %d, want 0 -- a permanently-failed report must be dropped, not retried forever", got))
	}
	attempts := h.finalAttemptsFor(order.ExternalID)
	if len(attempts) != 1 {
		return fail(name, fmt.Errorf("ReportDepositFinal was attempted %d times, want exactly 1 (no retry loop)", len(attempts)))
	}
	if !errors.Is(attempts[0].Err, finality.ErrOrphanedDeposit) {
		return fail(name, fmt.Errorf("attempt error = %v, want one wrapping finality.ErrOrphanedDeposit", attempts[0].Err))
	}
	found, err := h.orphanedRowFor(txHash)
	if err != nil {
		return fail(name, err)
	}
	if found == nil {
		return fail(name, errors.New("no orphaned_deposits row was recorded"))
	}
	if found.OrderStateAtDetection != "expired" {
		return fail(name, fmt.Errorf("OrderStateAtDetection = %q, want expired", found.OrderStateAtDetection))
	}
	return pass(name)
}

// scenarioPreFinalReorg: a candidate is detected, then its block is
// rewritten (the log disappears) BEFORE the finalized tag ever reaches
// it -- the routine, expected case. Must be silent: no report to C1,
// asserted here as the absence of any ReportDepositFinal attempt, not
// merely the absence of a successful one.
func (h *harness) scenarioPreFinalReorg() Result {
	const name = "PreFinalReorg"
	order, err := h.prepareOrder("3000.000000", time.Hour)
	if err != nil {
		return fail(name, err)
	}
	height := h.nextHeight()
	log := newTransferLog(height, h.newTxHash(), addressTopic(order.Address), 0, 3000_000000)
	h.chain.commitBlockToAll(height, time.Now(), []types.Log{log})

	// Detect it (finalized tag deliberately NOT advanced past height
	// yet), then rewrite history at that height on every node before it
	// ever gets the chance to.
	if err := h.tick(height-1, height); err != nil {
		return fail(name, err)
	}
	if got := h.tracker.PendingCount(); got != 1 {
		return fail(name, fmt.Errorf("PendingCount() after detection = %d, want 1", got))
	}

	h.chain.rewriteHeight(height, time.Now(), nil, 0, 1, 2) // all three nodes now agree: nothing at this height
	h.chain.finalizeAll(height)
	if err := h.tick(height, height); err != nil {
		return fail(name, err)
	}

	if got := h.tracker.PendingCount(); got != 0 {
		return fail(name, fmt.Errorf("PendingCount() after reorg = %d, want 0 -- must be dropped, not left pending", got))
	}
	if attempts := h.finalAttemptsFor(order.ExternalID); len(attempts) != 0 {
		return fail(name, fmt.Errorf("ReportDepositFinal was attempted %d times, want 0 -- a pre-final reorg must be silent", len(attempts)))
	}
	return pass(name)
}

// scenarioProviderDisagreement: injecting disagreement on the finalized
// tag itself must withhold finalization, never grant it on a
// majority-of-one basis.
func (h *harness) scenarioProviderDisagreement() Result {
	const name = "ProviderDisagreement"
	order, err := h.prepareOrder("3000.000000", time.Hour)
	if err != nil {
		return fail(name, err)
	}
	height := h.nextHeight()
	log := newTransferLog(height, h.newTxHash(), addressTopic(order.Address), 0, 3000_000000)
	h.chain.commitBlockToAll(height, time.Now(), []types.Log{log})

	if err := h.tick(height-1, height); err != nil {
		return fail(name, err)
	}

	// A genuine three-way disagreement, not just one slow provider: each
	// node commits its OWN version of height with a different timestamp
	// (part of the header, so each hashes differently), so no pair of 2
	// agrees on the same (height, hash) -- unlike two providers
	// genuinely agreeing on an earlier height while a third lags, which
	// is a case invariant 5 correctly allows to succeed, not disagreement.
	h.chain.nodes[0].commitBlock(height, time.Now(), nil)
	h.chain.nodes[1].commitBlock(height, time.Now().Add(time.Second), nil)
	h.chain.nodes[2].commitBlock(height, time.Now().Add(2*time.Second), nil)
	h.chain.finalizeOne(0, height)
	h.chain.finalizeOne(1, height)
	h.chain.finalizeOne(2, height)

	err = h.tick(height, height)
	if err == nil {
		return fail(name, errors.New("tick succeeded despite provider disagreement, want a withheld finalization"))
	}
	if !errors.Is(err, chain.ErrAmbiguousAgreement) && !errors.Is(err, chain.ErrNoAgreement) {
		return fail(name, fmt.Errorf("tick error = %v, want ErrNoAgreement or ErrAmbiguousAgreement", err))
	}
	if attempts := h.finalAttemptsFor(order.ExternalID); len(attempts) != 0 {
		return fail(name, fmt.Errorf("ReportDepositFinal was attempted %d times despite disagreement, want 0", len(attempts)))
	}
	return pass(name)
}

// scenarioProviderDark: a provider going dark for an extended period
// must show up as a health alert, and dropping below MinAgreement (2 of
// 3 dark) must hard-stall ingestion rather than silently trusting the
// one provider still alive.
func (h *harness) scenarioProviderDark() Result {
	const name = "ProviderDark"
	height := h.nextHeight()
	h.chain.commitBlockToAll(height, time.Now(), nil)
	h.chain.finalizeAll(height)

	h.chain.setDark(2, true)
	defer h.chain.setDark(2, false)

	// UnhealthyAfterConsecutiveFailures defaults to 3 -- drive enough
	// rounds for node C's streak to cross it. Two providers (A, B) still
	// agree, so these ticks succeed despite C being down.
	for i := 0; i < 4; i++ {
		if err := h.tick(height, height); err != nil {
			return fail(name, fmt.Errorf("tick %d with one provider dark: %w", i, err))
		}
	}
	var unhealthy bool
	for _, ph := range h.chain.pool.ProviderHealthSnapshot() {
		if ph.Name == "C" && !ph.Healthy {
			unhealthy = true
		}
	}
	if !unhealthy {
		return fail(name, errors.New("provider C never became unhealthy after repeated failures"))
	}

	// Now drop a SECOND provider -- only one of three left, below
	// MinAgreement(2). This must hard-stall, not silently trust B alone.
	h.chain.setDark(1, true)
	defer h.chain.setDark(1, false)

	err := h.tick(height, height)
	if err == nil {
		return fail(name, errors.New("tick succeeded with only one provider alive, want a hard stall"))
	}
	return pass(name)
}

// scenarioDuplicateLogDelivery: the same event, delivered twice (e.g. a
// provider replaying it, or the ingestion loop re-scanning a range it
// already covered) must be a no-op against C1, never a double report.
func (h *harness) scenarioDuplicateLogDelivery() Result {
	const name = "DuplicateLogDelivery"
	order, err := h.prepareOrder("3000.000000", time.Hour)
	if err != nil {
		return fail(name, err)
	}
	height := h.nextHeight()
	log := newTransferLog(height, h.newTxHash(), addressTopic(order.Address), 0, 3000_000000)
	h.chain.commitBlockToAll(height, time.Now(), []types.Log{log})
	h.chain.finalizeAll(height)

	// Scan the SAME range twice, as a restart re-observing an
	// already-handled block would.
	if err := h.tick(height, height); err != nil {
		return fail(name, fmt.Errorf("first tick: %w", err))
	}
	if err := h.tick(height, height); err != nil {
		return fail(name, fmt.Errorf("second tick: %w", err))
	}

	updated, err := h.ledger.getOrder(h.ctx, order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if updated.State != "funded" {
		return fail(name, fmt.Errorf("order state = %q, want funded", updated.State))
	}
	if attempts := h.finalAttemptsFor(order.ExternalID); len(attempts) != 1 {
		return fail(name, fmt.Errorf("ReportDepositFinal was attempted %d times for one duplicated delivery, want exactly 1", len(attempts)))
	}
	return pass(name)
}

// scenarioWrongTokenAndZeroValue: neither shape ever reaches
// classification or C1.
func (h *harness) scenarioWrongTokenAndZeroValue() Result {
	const name = "WrongTokenAndZeroValue"
	order, err := h.prepareOrder("3000.000000", time.Hour)
	if err != nil {
		return fail(name, err)
	}
	height := h.nextHeight()

	zeroValue := newTransferLog(height, h.newTxHash(), addressTopic(order.Address), 0, 0)
	wrongToken := types.Log{
		Address: contractAddress, Topics: []common.Hash{common.HexToHash("0xnotatransfer")},
		Data: make([]byte, 32), TxHash: h.newTxHash(), Index: 1,
	}
	h.chain.commitBlockToAll(height, time.Now(), []types.Log{zeroValue, wrongToken})
	h.chain.finalizeAll(height)

	if err := h.tick(height, height); err != nil {
		return fail(name, err)
	}
	if got := h.tracker.PendingCount(); got != 0 {
		return fail(name, fmt.Errorf("PendingCount() = %d, want 0", got))
	}
	updated, err := h.ledger.getOrder(h.ctx, order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if updated.State != "quoted" {
		return fail(name, fmt.Errorf("order state = %q, want unchanged (quoted) -- neither shape may ever reach C1", updated.State))
	}
	return pass(name)
}

// scenarioPostFinalReorg: a previously-finalized deposit's block is
// rewritten out from under it -- chain history violating its own prior
// finality, an extraordinary event this environment can only reproduce
// by deliberately misconfiguring the simulated finality gadget (there is
// no way to make this happen through normal simNode operation short of
// calling commitBlock again for an already-finalized height, which is
// exactly what this does). Must be detected and reported to C1's real
// reorg endpoint, exercising C1's own scenario A/B behavior for real.
func (h *harness) scenarioPostFinalReorg() Result {
	const name = "PostFinalReorg"
	order, err := h.prepareOrder("3000.000000", time.Hour)
	if err != nil {
		return fail(name, err)
	}
	height := h.nextHeight()
	log := newTransferLog(height, h.newTxHash(), addressTopic(order.Address), 0, 3000_000000)
	h.chain.commitBlockToAll(height, time.Now(), []types.Log{log})
	h.chain.finalizeAll(height)

	if err := h.tick(height, height); err != nil {
		return fail(name, fmt.Errorf("initial finalization: %w", err))
	}
	updated, err := h.ledger.getOrder(h.ctx, order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if updated.State != "funded" {
		return fail(name, fmt.Errorf("order state before the reorg = %q, want funded", updated.State))
	}

	// Deliberately misconfigure the simulated finality gadget: rewrite
	// the already-finalized height's content on every node, and re-set
	// their finalized tag past it -- what "BEP-126 finality was itself
	// violated" means for a controllable fake, since a real chain cannot
	// be asked to do this on schedule.
	h.chain.rewriteHeight(height, time.Now(), nil, 0, 1, 2)
	h.chain.finalizeAll(height + 1)

	if err := h.tick(height+1, height+1); err != nil {
		return fail(name, fmt.Errorf("post-reorg tick: %w", err))
	}

	h.mu.Lock()
	reported := append([]string(nil), h.reorgReports...)
	h.mu.Unlock()
	wasReported := false
	for _, ext := range reported {
		if ext == order.ExternalID {
			wasReported = true
		}
	}
	if !wasReported {
		return fail(name, errors.New("ReportReorg was never called for the post-final reorg"))
	}

	afterReorg, err := h.ledger.getOrder(h.ctx, order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if afterReorg.State != "quoted" {
		return fail(name, fmt.Errorf("order state after the reorg = %q, want quoted (scenario A -- the order never left "+
			"Funded before the reorg, so this is C1's own real scenario A, not a mock of it)", afterReorg.State))
	}
	return pass(name)
}

// orphanedRow is the subset of orphaned.Deposit scenarios check.
type orphanedRow struct {
	OrderStateAtDetection string
}

func (h *harness) orphanedRowFor(txHash common.Hash) (*orphanedRow, error) {
	deposits, err := orphaned.List(h.ctx, h.watcherPool, nil)
	if err != nil {
		return nil, err
	}
	for _, d := range deposits {
		if d.TxHash == txHash.Hex() {
			return &orphanedRow{OrderStateAtDetection: d.OrderStateAtDetection}, nil
		}
	}
	return nil, nil
}
