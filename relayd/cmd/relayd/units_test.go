package main

import "testing"

func TestOptionalUnits(t *testing.T) {
	for raw, want := range map[string]int64{"": 0, "0.00001": 10_000_000_000_000, "0.0005": 500_000_000_000_000, "1": 1_000_000_000_000_000_000} {
		t.Setenv("RELAYD_TEST_UNITS", raw)
		if got, err := optionalUnits("RELAYD_TEST_UNITS", 18); err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", raw, got, err, want)
		}
	}
	t.Setenv("RELAYD_TEST_UNITS", "0.1")
	if got, err := optionalUnits("RELAYD_TEST_UNITS", 6); err != nil || got != 100_000 {
		t.Errorf("0.1 TRX: got %d, %v; want 100000 sun", got, err)
	}
	for _, bad := range []string{"0", "-1", "abc", "0.0000001"} {
		t.Setenv("RELAYD_TEST_UNITS", bad)
		if _, err := optionalUnits("RELAYD_TEST_UNITS", 6); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
