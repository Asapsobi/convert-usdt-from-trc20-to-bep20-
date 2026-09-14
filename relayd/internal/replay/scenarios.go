// This file's own SCENARIO MIX originally differed from
// docs/03-build/model-f-relay-build-prompts.md's own R6 wording ("a
// screening hold that resolves to refund, an upstream failed before the
// forward transfer") because R5 initially shipped only a first slice
// (automatic refund-by-timeout, no manual HELD->REFUNDED path). The
// manual path is now wired too (screening/internal/holds.go's own
// RelayAwareRefundEntryBuilder, calling relayd's GET .../refund-entry) --
// scenarioManuallyRejectedHoldGetsRefunded below exercises it for real,
// restoring the build-prompts doc's own original scenario. "Upstream
// failed before the forward transfer" still has no distinct code path in
// this implementation, though: relayd only ever learns an upstream
// order's status by polling it AFTER its own forward transfer confirms
// (advanceForwardedOne), so a pre-forward vendor failure is,
// today, indistinguishable from any other reason the forward attempt
// never completes, and is covered by the SAME stuck-forwarding-timeout
// mechanism scenarioStuckForwardingTimeoutRefund exercises. That one
// remaining gap is also recorded in README.md's own Model F status
// table.
package replay

import (
	"errors"
	"fmt"
	"time"

	"relayd/internal/money"
	"relayd/internal/relay"
	"relayd/internal/upstream"
)

// scenarioFullHappyPath_TRC20ToBEP20 drives one relay leg through the
// entire happy flow: AWAITING_DEPOSIT -> FORWARDING -> FORWARDED ->
// SETTLED, against a real ledgerd, asserting the real double-entry
// outcome internal/orchestrate's own package doc comment describes.
func (h *harness) scenarioFullHappyPath_TRC20ToBEP20() Result {
	const name = "FullHappyPath_TRC20ToBEP20"
	externalID := h.nextID("happy-trc")
	customerID := h.nextID("cust")

	order, err := h.ledger.createRelayOrder(h.ctx, externalID, customerID, true)
	if err != nil {
		return fail(name, err)
	}
	screened, err := h.ledger.advanceToScreened(h.ctx, order, "USDT_TRC20", "")
	if err != nil {
		return fail(name, err)
	}
	if _, err := h.store.Create(h.ctx, relay.Leg{
		ExternalID: externalID, OrderID: screened.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: customerID, DestinationAddress: "0xcustomer-bep20-address", DepositAddress: "Trelayd-fixture-deposit-address",
		AmountIn: money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}, AmountOutExpected: money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		return fail(name, err)
	}

	so := h.newOrchestrator(h.nextID("provider"), 0)
	so.upstream.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")

	if err := h.runTicksUntil(so.orch, 3, func() (bool, error) {
		st, err := h.legStatus(externalID)
		return st == relay.StatusForwarded, err
	}); err != nil {
		return fail(name, err)
	}

	leg, err := h.store.GetByExternalID(h.ctx, externalID)
	if err != nil {
		return fail(name, err)
	}
	if leg.UpstreamOrderID == nil {
		return fail(name, errors.New("expected an upstream_order_id to be recorded"))
	}
	if err := so.upstream.SetOrderStatus(*leg.UpstreamOrderID, upstream.StatusComplete,
		ptrAmount(money.Amount{Asset: money.USDT_BEP20, Units: 99_650000})); err != nil {
		return fail(name, err)
	}

	if err := h.runTicksUntil(so.orch, 3, func() (bool, error) {
		st, err := h.legStatus(externalID)
		return st == relay.StatusSettled, err
	}); err != nil {
		return fail(name, err)
	}

	if err := h.assertZero(customerLiabilityCode(customerID, "USDT_TRC20")); err != nil {
		return fail(name, err)
	}
	if err := h.assertZero(relayForwardingAccountCode(screened.ID)); err != nil {
		return fail(name, err)
	}
	h.track(trackedLeg{externalID: externalID, orderID: screened.ID, wantTerminal: relay.StatusSettled})
	return pass(name)
}

// scenarioFullHappyPath_BEP20ToTRC20 is the mirror direction: the
// customer deposits USDT_BEP20, relayd's forward leg is a real
// evmtx-built ERC20 transfer.
func (h *harness) scenarioFullHappyPath_BEP20ToTRC20() Result {
	const name = "FullHappyPath_BEP20ToTRC20"
	externalID := h.nextID("happy-bep")
	customerID := h.nextID("cust")

	order, err := h.ledger.createRelayOrder(h.ctx, externalID, customerID, false)
	if err != nil {
		return fail(name, err)
	}
	screened, err := h.ledger.advanceToScreened(h.ctx, order, "USDT_BEP20", "")
	if err != nil {
		return fail(name, err)
	}
	if _, err := h.store.Create(h.ctx, relay.Leg{
		ExternalID: externalID, OrderID: screened.ID, Direction: relay.BEP20ToTRC20,
		CustomerID: customerID, DestinationAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", DepositAddress: "0xrelayd-fixture-deposit-address",
		AmountIn: money.Amount{Asset: money.USDT_BEP20, Units: 100_000000}, AmountOutExpected: money.Amount{Asset: money.USDT_TRC20, Units: 99_700000},
	}); err != nil {
		return fail(name, err)
	}

	so := h.newOrchestrator(h.nextID("provider"), 0)
	so.upstream.ForceDepositAddress("0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf")

	if err := h.runTicksUntil(so.orch, 3, func() (bool, error) {
		st, err := h.legStatus(externalID)
		return st == relay.StatusForwarded, err
	}); err != nil {
		return fail(name, err)
	}
	if so.evmChain.broadcasts != 1 {
		return fail(name, fmt.Errorf("expected exactly 1 EVM broadcast, got %d", so.evmChain.broadcasts))
	}

	leg, err := h.store.GetByExternalID(h.ctx, externalID)
	if err != nil {
		return fail(name, err)
	}
	if leg.UpstreamOrderID == nil {
		return fail(name, errors.New("expected an upstream_order_id to be recorded"))
	}
	if err := so.upstream.SetOrderStatus(*leg.UpstreamOrderID, upstream.StatusComplete,
		ptrAmount(money.Amount{Asset: money.USDT_TRC20, Units: 99_650000})); err != nil {
		return fail(name, err)
	}

	if err := h.runTicksUntil(so.orch, 3, func() (bool, error) {
		st, err := h.legStatus(externalID)
		return st == relay.StatusSettled, err
	}); err != nil {
		return fail(name, err)
	}

	if err := h.assertZero(customerLiabilityCode(customerID, "USDT_BEP20")); err != nil {
		return fail(name, err)
	}
	if err := h.assertZero(relayForwardingAccountCode(screened.ID)); err != nil {
		return fail(name, err)
	}
	h.track(trackedLeg{externalID: externalID, orderID: screened.ID, wantTerminal: relay.StatusSettled})
	return pass(name)
}

// scenarioStuckForwardingTimeoutRefund forces the forward leg's own
// signature request to fail permanently, simulating a leg that can never
// broadcast its forward transfer -- R5's own automatic refund path
// should reverse relay_forward_start, post relay_refund, and broadcast a
// real refund transfer back to the ORIGINAL depositor's address. See
// this file's own top-of-file doc comment for why this scenario stands
// in for both of the build-prompts doc's original refund-triggering
// scenarios.
func (h *harness) scenarioStuckForwardingTimeoutRefund() Result {
	const name = "StuckForwardingLegAutomaticallyRefunded"
	externalID := h.nextID("refund")
	customerID := h.nextID("cust")
	senderAddress := "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj" // real, valid, checksummed -- txbuild validates it for real

	order, err := h.ledger.createRelayOrder(h.ctx, externalID, customerID, true)
	if err != nil {
		return fail(name, err)
	}
	screened, err := h.ledger.advanceToScreened(h.ctx, order, "USDT_TRC20", senderAddress)
	if err != nil {
		return fail(name, err)
	}
	if _, err := h.store.Create(h.ctx, relay.Leg{
		ExternalID: externalID, OrderID: screened.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: customerID, DestinationAddress: "0xcustomer-bep20-address", DepositAddress: "Trelayd-fixture-deposit-address",
		AmountIn: money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}, AmountOutExpected: money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		return fail(name, err)
	}

	so := h.newOrchestrator(h.nextID("provider"), time.Millisecond)
	so.upstream.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")
	so.signer.ForceError("relayd:sign:"+externalID, errors.New("replay: simulated permanent signing failure"))

	if err := h.runTicksUntil(so.orch, 4, func() (bool, error) {
		st, err := h.legStatus(externalID)
		return st == relay.StatusRefunded, err
	}); err != nil {
		return fail(name, err)
	}

	leg, err := h.store.GetByExternalID(h.ctx, externalID)
	if err != nil {
		return fail(name, err)
	}
	if leg.RefundTxID == nil || *leg.RefundTxID == "" {
		return fail(name, errors.New("expected a refund_tx_id to be recorded"))
	}
	if so.chain.broadcasts != 1 {
		return fail(name, fmt.Errorf("expected exactly 1 TRC20 broadcast (the refund), got %d", so.chain.broadcasts))
	}

	if err := h.assertZero(customerLiabilityCode(customerID, "USDT_TRC20")); err != nil {
		return fail(name, err)
	}
	if err := h.assertZero(relayLegAccountCode(screened.ID)); err != nil {
		return fail(name, err)
	}
	if err := h.assertZero(relayForwardingAccountCode(screened.ID)); err != nil {
		return fail(name, err)
	}
	h.track(trackedLeg{externalID: externalID, orderID: screened.ID, wantTerminal: relay.StatusRefunded})
	return pass(name)
}

// scenarioManuallyRejectedHoldGetsRefunded exercises the manual
// HELD->REFUNDED path: a real held order, refunded exactly the way
// screening/internal/holds.go's own Reject flow does in a real
// deployment (fetch the entry from driver.BuildRefundEntry -- what
// relayd's own GET .../refund-entry endpoint serves -- then post
// held->refunded to C1 with it), then picked up by
// internal/orchestrate's own startExternallyRefundedLegs and carried
// through REFUND_PENDING to REFUNDED with a real on-chain broadcast.
// This restores the build-prompts doc's own original R6 scenario (see
// this file's own top-of-file doc comment).
func (h *harness) scenarioManuallyRejectedHoldGetsRefunded() Result {
	const name = "ManuallyRejectedHoldGetsRefunded"
	externalID := h.nextID("manual-reject")
	customerID := h.nextID("cust")
	senderAddress := "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj"

	order, err := h.ledger.createRelayOrder(h.ctx, externalID, customerID, true)
	if err != nil {
		return fail(name, err)
	}
	held, err := h.ledger.advanceToHeld(h.ctx, order, "USDT_TRC20", senderAddress)
	if err != nil {
		return fail(name, err)
	}
	if _, err := h.store.Create(h.ctx, relay.Leg{
		ExternalID: externalID, OrderID: held.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: customerID, DestinationAddress: "0xcustomer-bep20-address", DepositAddress: "Trelayd-fixture-deposit-address",
		AmountIn: money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}, AmountOutExpected: money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		return fail(name, err)
	}

	// screening's own job in a real deployment -- reproduced directly via
	// the same Driver method its own HTTP call wraps, since this harness
	// has no real screend process to call through (see Config's own doc
	// comment: every boundary besides C1 is an in-process fake here).
	entry, err := h.refundEntryDriver().BuildRefundEntry(h.ctx, externalID)
	if err != nil {
		return fail(name, err)
	}
	entryLines := make([]map[string]any, len(entry.Lines))
	for i, l := range entry.Lines {
		entryLines[i] = map[string]any{"account_code": l.AccountCode, "asset": l.Asset, "amount": l.Amount}
	}
	refunded, err := h.ledger.rejectHeld(h.ctx, held, map[string]any{
		"entry_type": entry.EntryType, "occurred_at": entry.OccurredAt, "lines": entryLines,
	})
	if err != nil {
		return fail(name, err)
	}
	if refunded.State != "refunded" {
		return fail(name, fmt.Errorf("expected C1 order state refunded, got %s", refunded.State))
	}

	so := h.newOrchestrator(h.nextID("provider"), 0)

	if err := h.runTicksUntil(so.orch, 3, func() (bool, error) {
		st, err := h.legStatus(externalID)
		return st == relay.StatusRefunded, err
	}); err != nil {
		return fail(name, err)
	}

	leg, err := h.store.GetByExternalID(h.ctx, externalID)
	if err != nil {
		return fail(name, err)
	}
	if leg.RefundTxID == nil || *leg.RefundTxID == "" {
		return fail(name, errors.New("expected a refund_tx_id to be recorded"))
	}

	if err := h.assertZero(customerLiabilityCode(customerID, "USDT_TRC20")); err != nil {
		return fail(name, err)
	}
	if err := h.assertZero(relayLegAccountCode(held.ID)); err != nil {
		return fail(name, err)
	}
	h.track(trackedLeg{externalID: externalID, orderID: held.ID, wantTerminal: relay.StatusRefunded})
	return pass(name)
}

// scenarioPostForwardUnrecoverable drives a leg to FORWARDED (a real
// broadcast succeeds), then has the upstream platform report FAILED --
// this system's own forward transfer already confirmed on-chain by then,
// so no refund is possible: the leg must land UNRECOVERABLE and a real
// alert must fire exactly once.
func (h *harness) scenarioPostForwardUnrecoverable() Result {
	const name = "PostForwardUpstreamFailureLandsUnrecoverableAndAlerts"
	externalID := h.nextID("unrecoverable")
	customerID := h.nextID("cust")

	order, err := h.ledger.createRelayOrder(h.ctx, externalID, customerID, true)
	if err != nil {
		return fail(name, err)
	}
	screened, err := h.ledger.advanceToScreened(h.ctx, order, "USDT_TRC20", "")
	if err != nil {
		return fail(name, err)
	}
	if _, err := h.store.Create(h.ctx, relay.Leg{
		ExternalID: externalID, OrderID: screened.ID, Direction: relay.TRC20ToBEP20,
		CustomerID: customerID, DestinationAddress: "0xcustomer-bep20-address", DepositAddress: "Trelayd-fixture-deposit-address",
		AmountIn: money.Amount{Asset: money.USDT_TRC20, Units: 100_000000}, AmountOutExpected: money.Amount{Asset: money.USDT_BEP20, Units: 99_700000},
	}); err != nil {
		return fail(name, err)
	}

	so := h.newOrchestrator(h.nextID("provider"), 0)
	so.upstream.ForceDepositAddress("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")

	if err := h.runTicksUntil(so.orch, 3, func() (bool, error) {
		st, err := h.legStatus(externalID)
		return st == relay.StatusForwarded, err
	}); err != nil {
		return fail(name, err)
	}

	leg, err := h.store.GetByExternalID(h.ctx, externalID)
	if err != nil {
		return fail(name, err)
	}
	if leg.UpstreamOrderID == nil {
		return fail(name, errors.New("expected an upstream_order_id to be recorded"))
	}
	if err := so.upstream.SetOrderStatus(*leg.UpstreamOrderID, upstream.StatusFailed, nil); err != nil {
		return fail(name, err)
	}

	if err := h.runTicksUntil(so.orch, 3, func() (bool, error) {
		st, err := h.legStatus(externalID)
		return st == relay.StatusUnrecoverable, err
	}); err != nil {
		return fail(name, err)
	}

	fired := so.alerter.Fired()
	if len(fired) != 1 {
		return fail(name, fmt.Errorf("expected exactly 1 alert fired, got %d", len(fired)))
	}
	if fired[0].ExternalID != externalID {
		return fail(name, fmt.Errorf("alert fired for %q, want %q", fired[0].ExternalID, externalID))
	}

	h.track(trackedLeg{externalID: externalID, orderID: screened.ID, wantTerminal: relay.StatusUnrecoverable, wantAlertReason: "relay_leg_unrecoverable"})
	return pass(name)
}

func customerLiabilityCode(customerID, asset string) string {
	return fmt.Sprintf("liability:customer:%s:%s", customerID, asset)
}
func relayLegAccountCode(orderID int64) string { return fmt.Sprintf("asset:relay:leg:%d", orderID) }
func relayForwardingAccountCode(orderID int64) string {
	return fmt.Sprintf("asset:relay:leg:forwarding:%d", orderID)
}

func (h *harness) assertZero(accountCode string) error {
	balance, err := h.ledger.accountBalance(h.ctx, accountCode)
	if err != nil {
		return fmt.Errorf("reading balance for %s: %w", accountCode, err)
	}
	if balance != 0 {
		return fmt.Errorf("expected %s to close to 0, got %d", accountCode, balance)
	}
	return nil
}

func ptrAmount(a money.Amount) *money.Amount { return &a }
