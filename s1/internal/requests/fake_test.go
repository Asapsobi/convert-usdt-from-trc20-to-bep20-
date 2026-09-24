package requests

import (
	"context"
	"sync"
	"testing"
)

func TestFakeSigningService_UnderThresholdSignsImmediately(t *testing.T) {
	f := NewFakeSigningService(10000)
	req, err := f.RequestSignature(context.Background(), 1, [32]byte{1}, 5000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if req.Status != StatusSigned {
		t.Fatalf("Status = %s, want SIGNED", req.Status)
	}
	if req.SignedTx == ([65]byte{}) {
		t.Fatal("SignedTx is empty on a SIGNED request")
	}
}

func TestFakeSigningService_AtOrAboveThresholdStaysPending(t *testing.T) {
	f := NewFakeSigningService(10000)
	req, err := f.RequestSignature(context.Background(), 1, [32]byte{1}, 10000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if req.Status != StatusPending {
		t.Fatalf("Status = %s, want PENDING for a request at the threshold", req.Status)
	}
}

func TestFakeSigningService_IdempotentReplayReturnsTheSameRequest(t *testing.T) {
	f := NewFakeSigningService(10000)
	first, err := f.RequestSignature(context.Background(), 1, [32]byte{1}, 5000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature (1st): %v", err)
	}
	second, err := f.RequestSignature(context.Background(), 1, [32]byte{9, 9}, 999999, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature (2nd, replayed): %v", err)
	}
	if second.ID != first.ID || second.Status != first.Status || second.SignedTx != first.SignedTx {
		t.Fatalf("replayed request = %+v, want identical to first %+v", second, first)
	}
}

func TestFakeSigningService_ConcurrentSameIdempotencyKeyYieldsOneRequest(t *testing.T) {
	f := NewFakeSigningService(10000)
	const n = 50
	var wg sync.WaitGroup
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := f.RequestSignature(context.Background(), 1, [32]byte{1}, 5000, "idem-concurrent")
			if err != nil {
				t.Errorf("RequestSignature: %v", err)
				return
			}
			ids[i] = req.ID
		}(i)
	}
	wg.Wait()

	first := ids[0]
	for _, id := range ids {
		if id != first {
			t.Fatalf("concurrent calls with the same idempotency key produced different request ids: %v", ids)
		}
	}
}

func TestFakeSigningService_ApproveAndRejectFake(t *testing.T) {
	f := NewFakeSigningService(10000)
	pending, err := f.RequestSignature(context.Background(), 1, [32]byte{1}, 50000, "idem-pending")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if pending.Status != StatusPending {
		t.Fatalf("Status = %s, want PENDING", pending.Status)
	}

	if err := f.ApproveFake(pending.ID, 1, [32]byte{1}); err != nil {
		t.Fatalf("ApproveFake: %v", err)
	}
	got, err := f.GetSignature(context.Background(), pending.ID)
	if err != nil {
		t.Fatalf("GetSignature: %v", err)
	}
	if got.Status != StatusSigned {
		t.Fatalf("Status = %s, want SIGNED after approval", got.Status)
	}

	if err := f.ApproveFake(pending.ID, 1, [32]byte{1}); err != ErrRequestAlreadyResolved {
		t.Fatalf("second ApproveFake error = %v, want ErrRequestAlreadyResolved", err)
	}
}

func TestFakeSigningService_SlotAddress(t *testing.T) {
	f := NewFakeSigningService(10000)
	f.SetSlotAddress(1, "TFakeSlotAddress00000000000001")
	addr, err := f.SlotAddress(context.Background(), 1)
	if err != nil {
		t.Fatalf("SlotAddress: %v", err)
	}
	if addr != "TFakeSlotAddress00000000000001" {
		t.Fatalf("SlotAddress = %q, want the configured address", addr)
	}

	if _, err := f.SlotAddress(context.Background(), 2); err == nil {
		t.Fatal("SlotAddress for an unconfigured slot: want an error, got nil")
	}
}

func TestFakeSigningService_ProvisionTronDepositKeyIsIdempotent(t *testing.T) {
	f := NewFakeSigningService(10000)
	first, err := f.ProvisionTronDepositKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("ProvisionTronDepositKey (1st): %v", err)
	}
	if first.Index != 42 || first.Address == "" {
		t.Fatalf("ProvisionTronDepositKey (1st) = %+v, want a non-empty address for index 42", first)
	}

	second, err := f.ProvisionTronDepositKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("ProvisionTronDepositKey (2nd, retried): %v", err)
	}
	if second != first {
		t.Fatalf("retried ProvisionTronDepositKey(42) = %+v, want identical to first %+v", second, first)
	}

	other, err := f.ProvisionTronDepositKey(context.Background(), 43)
	if err != nil {
		t.Fatalf("ProvisionTronDepositKey (index 43): %v", err)
	}
	if other.Address == first.Address {
		t.Fatalf("different indices got the same fake address: %q", other.Address)
	}
}

func TestFakeSigningService_ProvisionBSCDepositKeyIsIdempotent(t *testing.T) {
	f := NewFakeSigningService(10000)
	first, err := f.ProvisionBSCDepositKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("ProvisionBSCDepositKey (1st): %v", err)
	}
	if first.Index != 42 || first.Address == "" {
		t.Fatalf("ProvisionBSCDepositKey (1st) = %+v, want a non-empty address for index 42", first)
	}

	second, err := f.ProvisionBSCDepositKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("ProvisionBSCDepositKey (2nd, retried): %v", err)
	}
	if second != first {
		t.Fatalf("retried ProvisionBSCDepositKey(42) = %+v, want identical to first %+v", second, first)
	}

	other, err := f.ProvisionBSCDepositKey(context.Background(), 43)
	if err != nil {
		t.Fatalf("ProvisionBSCDepositKey (index 43): %v", err)
	}
	if other.Address == first.Address {
		t.Fatalf("different indices got the same fake address: %q", other.Address)
	}
}
