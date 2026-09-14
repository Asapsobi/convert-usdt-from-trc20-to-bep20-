package holds

import (
	"context"
	"errors"
	"fmt"

	"screening/internal/ledgerclient"
	"screening/internal/relaydclient"
)

// RelaydRefundEntryClient is the one call RelayAwareRefundEntryBuilder
// needs from relayd -- *relaydclient.Client's real implementation, or a
// fake for testing. Declared here, consumer-side, matching
// Releaser/Rejecter's own identical convention in this file (holds.go
// already imports screening/internal/ledgerclient for OrderRef, so
// importing relaydclient here for its own RefundEntry type is the same
// established pattern, not a new one).
type RelaydRefundEntryClient interface {
	GetRefundEntry(ctx context.Context, externalID string) (relaydclient.RefundEntry, error)
}

// RelayAwareRefundEntryBuilder is this package's own real
// RefundEntryBuilder for RELAY-tier orders: relayd is the one owner in
// this codebase that both understands RELAY-tier's own account-code
// conventions and already constructs/signs/broadcasts the physical
// on-chain refund transfer (R5's own timeout-triggered refund path,
// relayd/internal/orchestrate/refund.go), so a manually-rejected
// screening hold on a RELAY order asks relayd for the correctly-shaped
// entry rather than this package guessing at RELAY's own conventions
// itself.
//
// For any OTHER tier (DIRECT/STANDARD/SWEEP -- Model D), relayd has no
// leg to report: GetRefundEntry returns relaydclient.ErrNotARelayLeg,
// and this builder falls back to StubRefundEntryBuilder's own
// ErrRefundEntryNotImplemented, UNCHANGED from today's behavior. No
// owner for the physical BEP20 refund exists yet for those tiers -- see
// StubRefundEntryBuilder's own doc comment, still accurate for them.
type RelayAwareRefundEntryBuilder struct {
	Relayd RelaydRefundEntryClient
}

// BuildRefundEntry implements RefundEntryBuilder.
func (b RelayAwareRefundEntryBuilder) BuildRefundEntry(ctx context.Context, order ledgerclient.OrderRef) (map[string]any, error) {
	entry, err := b.Relayd.GetRefundEntry(ctx, order.ExternalID)
	if err != nil {
		if errors.Is(err, relaydclient.ErrNotARelayLeg) {
			return StubRefundEntryBuilder{}.BuildRefundEntry(ctx, order)
		}
		return nil, fmt.Errorf("holds: asking relayd for a RELAY refund entry for %s: %w", order.ExternalID, err)
	}

	lines := make([]map[string]any, len(entry.Lines))
	for i, l := range entry.Lines {
		lines[i] = map[string]any{"account_code": l.AccountCode, "asset": l.Asset, "amount": l.Amount}
	}
	return map[string]any{
		"entry_type":  entry.EntryType,
		"occurred_at": entry.OccurredAt,
		"lines":       lines,
	}, nil
}
