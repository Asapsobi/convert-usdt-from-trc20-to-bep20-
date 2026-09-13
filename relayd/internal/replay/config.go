package replay

import "time"

// Config controls one replay run. Mirrors dispatcher's/gateway's own
// identical Config: relayd has a real external dependency to point at (a
// real, already-running ledgerd) -- starting it is this run's caller's
// job (an operator or a CI script), not this package's, same reasoning
// every prior component's own cmd/replay gives. Every other external
// boundary (the upstream swap vendor, S1's SigningService, both chains'
// broadcast/finality, the alert channel) is an in-process fake,
// scenario-local -- no run needs a second real service beyond ledgerd.
type Config struct {
	Seed          int64
	LedgerBaseURL string
	LedgerToken   string
}

// DefaultConfig seeds from the current time, so two runs without an
// explicit seed still differ.
func DefaultConfig() Config {
	return Config{Seed: time.Now().UnixNano()}
}
