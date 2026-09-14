module s1

go 1.27.0

require (
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0
	github.com/go-chi/chi/v5 v5.3.2
	github.com/jackc/pgx/v5 v5.10.0
	github.com/pressly/goose/v3 v3.28.0
	github.com/prometheus/client_golang v1.23.2
	// Test-only, same as depositwatcher's own identical dependency (see
	// that module's go.mod comment): a fixture generator for
	// internal/kmssign/bscdeposit_test.go's own cross-validation against
	// an independent BIP32 implementation, never imported by anything
	// that ships.
	github.com/tyler-smith/go-bip32 v1.0.0
	golang.org/x/crypto v0.55.0
)

require (
	// Transitive, via go-bip32 above -- test-only for the same reason.
	github.com/FactomProject/basen v0.0.0-20150613233007-fe3947df716e // indirect
	github.com/FactomProject/btcutilecc v0.0.0-20130527213604-d3a63a5752ec // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
