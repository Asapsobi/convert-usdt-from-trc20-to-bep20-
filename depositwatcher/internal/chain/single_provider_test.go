package chain

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestFinalizedFrom_Success(t *testing.T) {
	pool, a, _ := twoNodePool(t, 2)
	a.setFinalized(1000, hash(0xAA))

	height, h, err := pool.FinalizedFrom(context.Background(), "A")
	if err != nil {
		t.Fatalf("FinalizedFrom: %v", err)
	}
	if height != 1000 || h != hash(0xAA) {
		t.Fatalf("FinalizedFrom = (%d, %s), want (1000, %s)", height, h, hash(0xAA))
	}
}

func TestFinalizedFrom_TransportError(t *testing.T) {
	pool, a, _ := twoNodePool(t, 2)
	a.setFinalizedError("boom")

	_, _, err := pool.FinalizedFrom(context.Background(), "A")
	if err == nil {
		t.Fatal("expected an error when the provider's own finalized call fails")
	}
}

func TestFinalizedFrom_UnknownProvider(t *testing.T) {
	pool, _, _ := twoNodePool(t, 2)
	_, _, err := pool.FinalizedFrom(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected an error for a provider name this Pool was never configured with")
	}
}

func TestBlockHashAt_Success(t *testing.T) {
	pool, a, _ := twoNodePool(t, 2)
	a.setBlock(500, testHeader(500, hash(0x01)))

	got, err := pool.BlockHashAt(context.Background(), "A", 500)
	if err != nil {
		t.Fatalf("BlockHashAt: %v", err)
	}
	wantHeader := testHeader(500, hash(0x01))
	want := wantHeader.Hash()
	if got != want {
		t.Fatalf("BlockHashAt = %s, want %s (the real computed header hash)", got, want)
	}
}

func TestBlockHashAt_HeightNotFound(t *testing.T) {
	pool, _, _ := twoNodePool(t, 2)
	// No block registered at this height -- the fake node returns JSON
	// null, exactly like a real node would for a height it doesn't have.
	_, err := pool.BlockHashAt(context.Background(), "A", 999999)
	if err == nil {
		t.Fatal("expected an error for a height this provider has no block for")
	}
}

func TestBlockHashAt_UnknownProvider(t *testing.T) {
	pool, _, _ := twoNodePool(t, 2)
	_, err := pool.BlockHashAt(context.Background(), "nonexistent", 500)
	if err == nil {
		t.Fatal("expected an error for an unconfigured provider name")
	}
}

func TestLogsAtFrom_Success(t *testing.T) {
	pool, a, _ := twoNodePool(t, 2)
	a.setLogs([]types.Log{sampleLog(100, 0)})

	got, err := pool.LogsAtFrom(context.Background(), "A", 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if err != nil {
		t.Fatalf("LogsAtFrom: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 log, got %d", len(got))
	}
}

func TestLogsAtFrom_TransportError(t *testing.T) {
	pool, a, _ := twoNodePool(t, 2)
	a.setLogsError("rate limited")

	_, err := pool.LogsAtFrom(context.Background(), "A", 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if err == nil {
		t.Fatal("expected an error when the provider's own log query fails")
	}
}

func TestLogsAtFrom_UnknownProvider(t *testing.T) {
	pool, _, _ := twoNodePool(t, 2)
	_, err := pool.LogsAtFrom(context.Background(), "nonexistent", 100, 100,
		common.HexToAddress("0x55d398326f99059fF775485246999027B3197955"), nil)
	if err == nil {
		t.Fatal("expected an error for an unconfigured provider name")
	}
}

func TestProviderNames_ReturnsConfiguredProviders(t *testing.T) {
	pool, _, _, _ := threeNodePool(t, 2)
	got := pool.ProviderNames()
	if len(got) != 3 || got[0] != "A" || got[1] != "B" || got[2] != "C" {
		t.Fatalf("ProviderNames() = %v, want [A B C]", got)
	}
}
