package orchestrate

import (
	"context"
	"testing"
	"time"

	"relayd/internal/money"
	"relayd/internal/transfers"
)

type verdictlessFinality struct{}

func (verdictlessFinality) IsFinal(ctx context.Context, txID string) (bool, error) { return true, nil }
func (verdictlessFinality) CheckExecution(ctx context.Context, txID string) (bool, bool, string, error) {
	return true, false, "", nil // solidified, receipt without a verdict
}

// A solidified TRX transfer (whose receipt never carries a verdict) is
// confirmed; a solidified USDT transfer without a verdict is never guessed.
func TestTronOutcome_SolidifiedTRXTransferIsConfirmed(t *testing.T) {
	ad := tronAdapter{o: &Orchestrator{Finality: verdictlessFinality{}}}
	hash, expires := "abc", time.Now().Add(time.Minute)
	trx := transfers.Attempt{TxHash: &hash, TronExpiresAt: &expires, Amount: money.Amount{Asset: assetTRX, Units: 1000}}
	if out, err := ad.outcome(context.Background(), trx, time.Now()); err != nil || out.state != outcomeConfirmed {
		t.Fatalf("a solidified TRX transfer must be confirmed, got %+v (%v)", out, err)
	}
	usdt := trx
	usdt.Amount = money.Amount{Asset: money.USDT_TRC20, Units: 1_000000}
	if out, err := ad.outcome(context.Background(), usdt, time.Now()); err != nil || out.state != outcomePending {
		t.Fatalf("a USDT transfer without a verdict must stay pending, got %+v (%v)", out, err)
	}
	if !alreadyOnChain(errString("tronbroadcast: broadcasting to TRON node: result error: Dup transaction.")) {
		t.Fatal(`"Dup transaction." means the node already has it`)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
