module tronwatcher

go 1.27.0

require (
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0
	github.com/go-chi/chi/v5 v5.3.2
	github.com/jackc/pgx/v5 v5.10.0
	github.com/pressly/goose/v3 v3.28.0
	github.com/prometheus/client_golang v1.16.0

	// TEST-ONLY. Used solely by internal/addresses/derive_test.go as a
	// fixture generator (a real, valid xpub to derive against) and as an
	// independent cross-validation oracle for this package's own BIP32
	// CKDpub math -- never imported by any non-test file. See
	// internal/addresses/bip32.go's top-of-file comment for why the
	// package's own BIP32 derivation is implemented directly against
	// secp256k1 rather than through this.
	github.com/tyler-smith/go-bip32 v1.0.0
	golang.org/x/crypto v0.55.0
)

require (
	github.com/FactomProject/basen v0.0.0-20150613233007-fe3947df716e // indirect
	github.com/FactomProject/btcutilecc v0.0.0-20130527213604-d3a63a5752ec // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.4 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.44.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
