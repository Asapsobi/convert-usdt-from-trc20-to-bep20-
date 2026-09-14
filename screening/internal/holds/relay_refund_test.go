// Pure logic tests -- no Postgres needed, unlike this package's own
// holds_integration_test.go, since RelayAwareRefundEntryBuilder's only
// dependency is the RelaydRefundEntryClient interface, faked here.
package holds_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"screening/internal/holds"
	"screening/internal/ledgerclient"
	"screening/internal/relaydclient"
)

type fakeRelaydRefundEntryClient struct {
	entry relaydclient.RefundEntry
	err   error
}

func (f fakeRelaydRefundEntryClient) GetRefundEntry(ctx context.Context, externalID string) (relaydclient.RefundEntry, error) {
	return f.entry, f.err
}

func TestRelayAwareRefundEntryBuilder_ReturnsRelaydEntry(t *testing.T) {
	client := fakeRelaydRefundEntryClient{entry: relaydclient.RefundEntry{
		EntryType:  "relay_refund",
		OccurredAt: "2026-09-14T00:00:00Z",
		Lines: []relaydclient.RefundEntryLine{
			{AccountCode: "liability:customer:cust-1:USDT_TRC20", Asset: "USDT_TRC20", Amount: "100.000000"},
			{AccountCode: "asset:relay:leg:5", Asset: "USDT_TRC20", Amount: "-100.000000"},
		},
	}}
	builder := holds.RelayAwareRefundEntryBuilder{Relayd: client}

	entry, err := builder.BuildRefundEntry(context.Background(), ledgerclient.OrderRef{OrderID: 5, ExternalID: "ext-1"})
	if err != nil {
		t.Fatalf("BuildRefundEntry: %v", err)
	}
	if entry["entry_type"] != "relay_refund" {
		t.Errorf("entry_type = %v, want relay_refund", entry["entry_type"])
	}
	lines, ok := entry["lines"].([]map[string]any)
	if !ok {
		t.Fatalf("lines is %T, want []map[string]any", entry["lines"])
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0]["account_code"] != "liability:customer:cust-1:USDT_TRC20" || lines[0]["amount"] != "100.000000" {
		t.Errorf("unexpected first line: %+v", lines[0])
	}
	if lines[1]["account_code"] != "asset:relay:leg:5" || lines[1]["amount"] != "-100.000000" {
		t.Errorf("unexpected second line: %+v", lines[1])
	}
}

func TestRelayAwareRefundEntryBuilder_FallsBackToStubWhenNotARelayLeg(t *testing.T) {
	client := fakeRelaydRefundEntryClient{err: fmt.Errorf("%w: ext-1", relaydclient.ErrNotARelayLeg)}
	builder := holds.RelayAwareRefundEntryBuilder{Relayd: client}

	_, err := builder.BuildRefundEntry(context.Background(), ledgerclient.OrderRef{OrderID: 1, ExternalID: "ext-1"})
	if !errors.Is(err, holds.ErrRefundEntryNotImplemented) {
		t.Fatalf("expected ErrRefundEntryNotImplemented (the stub's own fallback), got %v", err)
	}
}

func TestRelayAwareRefundEntryBuilder_PropagatesOtherErrors(t *testing.T) {
	sentinel := errors.New("relayd is unreachable")
	client := fakeRelaydRefundEntryClient{err: sentinel}
	builder := holds.RelayAwareRefundEntryBuilder{Relayd: client}

	_, err := builder.BuildRefundEntry(context.Background(), ledgerclient.OrderRef{OrderID: 1, ExternalID: "ext-1"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the underlying error to propagate (never silently falling back to the stub for a real failure), got %v", err)
	}
	if errors.Is(err, holds.ErrRefundEntryNotImplemented) {
		t.Fatal("a real relayd failure must never be reported as ErrRefundEntryNotImplemented -- that would misleadingly imply 'no owner exists' rather than 'the owner is down'")
	}
}
