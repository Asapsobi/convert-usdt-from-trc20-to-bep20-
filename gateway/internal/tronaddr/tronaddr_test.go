package tronaddr_test

import (
	"errors"
	"testing"

	"gateway/internal/tronaddr"
)

// validTronAddress is a real, well-formed TRON base58check address --
// the same USDT-TRC20 contract address dispatcher/internal/txbuild's
// own USDTContractAddress constant uses, chosen here specifically
// because it's already independently verified real (that package's own
// doc comment: "Verified live: TronGrid's own transaction history for
// this address shows real, current USDT transfer activity"), not
// because this test cares about USDT's own contract -- it's just a
// guaranteed-correct base58check TRON address to validate against,
// with zero risk of a hand-typed checksum being wrong.
const validTronAddress = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

func TestValidate_AcceptsARealTronAddress(t *testing.T) {
	if err := tronaddr.Validate(validTronAddress); err != nil {
		t.Fatalf("Validate(%q) = %v, want nil", validTronAddress, err)
	}
}

func TestValidate_RejectsEmpty(t *testing.T) {
	err := tronaddr.Validate("")
	if !errors.Is(err, tronaddr.ErrInvalidAddress) {
		t.Fatalf("Validate(\"\") = %v, want ErrInvalidAddress", err)
	}
}

func TestValidate_RejectsEVMAddress(t *testing.T) {
	// The exact class of mistake that surfaced this whole requirement:
	// a BSC/EVM-format address pasted where a TRC20 destination belongs.
	err := tronaddr.Validate("0xAE2166bd7901Ea67c1E2Bc4179418fC228108F07")
	if !errors.Is(err, tronaddr.ErrInvalidAddress) {
		t.Fatalf("Validate(EVM address) = %v, want ErrInvalidAddress", err)
	}
}

func TestValidate_RejectsMalformedStrings(t *testing.T) {
	cases := []string{
		"not-an-address",
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6",   // one char short
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6tX", // one char long
		"1R7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",  // wrong leading char (not T)
		"   ",
		"T0000000000000000000000000000000", // right shape, garbage payload
	}
	for _, c := range cases {
		if err := tronaddr.Validate(c); !errors.Is(err, tronaddr.ErrInvalidAddress) {
			t.Errorf("Validate(%q) = %v, want ErrInvalidAddress", c, err)
		}
	}
}

func TestValidate_RejectsCorruptedChecksum(t *testing.T) {
	// Right length, right leading character, right alphabet -- but the
	// last character of a real, valid address is mutated, which changes
	// the decoded payload's own trailing byte and therefore invalidates
	// the base58check checksum without changing the string's length or
	// superficial shape. This is the case a prefix-only or length-only
	// check could never catch, and the whole reason this package wraps a
	// real base58check decoder instead of a regex.
	mutated := validTronAddress[:len(validTronAddress)-1] + "x"
	if mutated == validTronAddress {
		t.Fatal("test setup bug: mutation did not change the address")
	}
	if err := tronaddr.Validate(mutated); !errors.Is(err, tronaddr.ErrInvalidAddress) {
		t.Errorf("Validate(%q) (corrupted checksum) = %v, want ErrInvalidAddress", mutated, err)
	}
}

func TestValidate_RejectsWrongVersionByte(t *testing.T) {
	// A base58check-valid Bitcoin mainnet address (version byte 0x00,
	// not TRON's 0x41) -- exercises the version-byte check specifically,
	// distinct from a checksum failure or a length failure.
	if err := tronaddr.Validate("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"); !errors.Is(err, tronaddr.ErrInvalidAddress) {
		t.Error("Validate(a valid Bitcoin address) = nil, want ErrInvalidAddress -- wrong network version byte")
	}
}
