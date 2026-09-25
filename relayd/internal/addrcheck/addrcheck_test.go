package addrcheck

import (
	"errors"
	"testing"
)

func TestTRON(t *testing.T) {
	if err := TRON("TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj"); err != nil {
		t.Fatalf("valid address rejected: %v", err)
	}
	for _, bad := range []string{
		"",
		"TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdk", // one character off: checksum fails
		"0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf",
		"TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqc",
	} {
		if err := TRON(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: expected ErrInvalid, got %v", bad, err)
		}
	}
}

func TestEVM(t *testing.T) {
	for _, good := range []string{
		"0x4192cC99D3CB95573DcAF8dd76921476e0C7bCaF", // correct checksum
		"0x4192cc99d3cb95573dcaf8dd76921476e0c7bcaf", // all lower: no checksum to check
	} {
		if err := EVM(good); err != nil {
			t.Errorf("%q rejected: %v", good, err)
		}
	}
	for _, bad := range []string{
		"",
		"0x4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf", // mixed case, wrong checksum
		"0x0000000000000000000000000000000000000000",
		"TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		"4192cc99D3Cb95573dCaf8dD76921476E0c7bCAf",
		"0x4192cc99D3Cb95573dCaf8dD76921476E0c7bC",
	} {
		if err := EVM(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: expected ErrInvalid, got %v", bad, err)
		}
	}
}
