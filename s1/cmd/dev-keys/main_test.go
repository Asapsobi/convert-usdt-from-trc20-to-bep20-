package main

import (
	"strconv"
	"testing"

	"s1/internal/kmssign"
)

// What dev-keys prints is exactly what s1d accepts at startup.
func TestGenerate_S1AcceptsTheKeys(t *testing.T) {
	keys, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range keys {
		got[kv[0]] = kv[1]
	}
	if seed, err := strconv.ParseInt(got["S1_KMS_FAKE_SEED"], 10, 64); err != nil || seed < 0 {
		t.Fatalf("S1_KMS_FAKE_SEED %q is not a positive int64: %v", got["S1_KMS_FAKE_SEED"], err)
	}
	if _, err := kmssign.NewBSCDepositKeys(got["S1_BSC_DEPOSIT_XPRV"], got["S1_BSC_DEPOSIT_XPUB"]); err != nil {
		t.Fatalf("BSC deposit keys rejected: %v", err)
	}
	if _, err := kmssign.NewTronDepositKeys(got["S1_TRON_DEPOSIT_XPRV"], got["S1_TRON_DEPOSIT_XPUB"]); err != nil {
		t.Fatalf("TRON deposit keys rejected: %v", err)
	}
	if got["S1_BSC_DEPOSIT_XPRV"] == got["S1_TRON_DEPOSIT_XPRV"] {
		t.Fatal("BSC and TRON deposit wallets share one key")
	}

	again, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	if again[1][1] == got["S1_BSC_DEPOSIT_XPRV"] {
		t.Fatal("two runs produced the same key")
	}
}
