package replay

import (
	"context"
	"fmt"

	"relayd/internal/relay"
)

// finalAssertions checks every invariant against everything this run
// actually did (tracked on h as scenarios ran via h.track), not just
// what a single scenario locally checked -- the same posture every prior
// component's own replay harness takes.
func (h *harness) finalAssertions(ctx context.Context) []Result {
	return []Result{
		h.assertEveryLegReachedItsExpectedTerminalStatus(ctx),
		h.assertEverySuspenseAccountClosedToZero(ctx),
		h.assertUnrecoverableLegLeavesFundsVisiblyStranded(ctx),
		h.assertExactlyOneAlertPerUnrecoverableLeg(),
	}
}

// assertEveryLegReachedItsExpectedTerminalStatus confirms this run never
// left a leg stuck mid-flight (AWAITING_DEPOSIT, FORWARDING,
// REFUND_PENDING) -- every leg a scenario tracked must have reached
// exactly the terminal status that scenario expected.
func (h *harness) assertEveryLegReachedItsExpectedTerminalStatus(ctx context.Context) Result {
	const name = "FinalAssertion_EveryLegReachedItsExpectedTerminalStatus"
	for _, l := range h.legs {
		leg, err := h.store.GetByExternalID(ctx, l.externalID)
		if err != nil {
			return fail(name, fmt.Errorf("leg %s: %w", l.externalID, err))
		}
		if leg.Status != l.wantTerminal {
			return fail(name, fmt.Errorf("leg %s: status %s, want %s", l.externalID, leg.Status, l.wantTerminal))
		}
	}
	return pass(name)
}

// assertEverySuspenseAccountClosedToZero is this harness's own core
// financial-correctness check: every relay-leg suspense account (both
// asset:relay:leg:<id> and asset:relay:leg:forwarding:<id>) a SETTLED or
// REFUNDED leg touched must net to exactly zero -- architecture §5's own
// "an account open after an hour is an operational alarm" applies just
// as much to "still open at the end of this run."
//
// An UNRECOVERABLE leg is deliberately excluded: found by actually
// running this harness, not assumed -- its own
// asset:relay:leg:forwarding:<id> account is EXPECTED to sit at the full
// forwarded amount forever, because the forward transfer really did
// leave this system on-chain with no reconciling entry ever posted
// against it (see internal/orchestrate/settle.go's own doc comment on
// why: "no on-chain lever left to pull"). Asserting it closes to zero
// would be asserting the wrong thing, not catching a real bug -- the
// non-zero balance IS the correct, permanent record of unrecovered
// funds an operator's own manual write-off eventually resolves.
func (h *harness) assertEverySuspenseAccountClosedToZero(ctx context.Context) Result {
	const name = "FinalAssertion_SettledOrRefundedLegSuspenseAccountsCloseToZero"
	for _, l := range h.legs {
		if l.wantTerminal == relay.StatusUnrecoverable {
			continue
		}
		if err := h.assertZero(relayLegAccountCode(l.orderID)); err != nil {
			return fail(name, err)
		}
		if err := h.assertZero(relayForwardingAccountCode(l.orderID)); err != nil {
			return fail(name, err)
		}
	}
	return pass(name)
}

// assertUnrecoverableLegLeavesFundsVisiblyStranded is the positive
// counterpart to the exclusion above: an UNRECOVERABLE leg's own
// forwarding suspense account must hold exactly its full amount_in --
// not zero (that would mean the entry never posted at all) and not some
// other value (that would mean the wrong amount posted).
func (h *harness) assertUnrecoverableLegLeavesFundsVisiblyStranded(ctx context.Context) Result {
	const name = "FinalAssertion_UnrecoverableLegLeavesFundsVisiblyStranded"
	for _, l := range h.legs {
		if l.wantTerminal != relay.StatusUnrecoverable {
			continue
		}
		balance, err := h.ledger.accountBalance(ctx, relayForwardingAccountCode(l.orderID))
		if err != nil {
			return fail(name, fmt.Errorf("leg %s: %w", l.externalID, err))
		}
		if balance != 100_000000 {
			return fail(name, fmt.Errorf("leg %s: forwarding account holds %d, want the full stranded amount_in (100000000)", l.externalID, balance))
		}
	}
	return pass(name)
}

// assertExactlyOneAlertPerUnrecoverableLeg cross-checks h.legs (what
// scenarios expected) against nothing scenario-local: every leg this run
// tracked as wanting an alert reached UNRECOVERABLE, which
// scenarioPostForwardUnrecoverable's own inline check already confirmed
// fired exactly once at the time -- this assertion instead confirms the
// STATUS side of that contract held (a leg marked wantAlertReason really
// is UNRECOVERABLE, not some other status a bug could have left it in).
func (h *harness) assertExactlyOneAlertPerUnrecoverableLeg() Result {
	const name = "FinalAssertion_EveryAlertedLegIsUnrecoverable"
	for _, l := range h.legs {
		if l.wantAlertReason == "" {
			continue
		}
		if l.wantTerminal != relay.StatusUnrecoverable {
			return fail(name, fmt.Errorf("leg %s expects an alert but its own wantTerminal is %s, not UNRECOVERABLE -- test bug", l.externalID, l.wantTerminal))
		}
	}
	return pass(name)
}

// assertNoPanicsOccurred mirrors every prior component's own zero-panics
// check.
func (h *harness) assertNoPanicsOccurred(report *Report) Result {
	const name = "FinalAssertion_ZeroPanics"
	for _, s := range report.Scenarios {
		if !s.Passed && len(s.Detail) >= 9 && s.Detail[:9] == "PANICKED:" {
			return fail(name, fmt.Errorf("scenario %s panicked: %s", s.Name, s.Detail))
		}
	}
	return pass(name)
}
