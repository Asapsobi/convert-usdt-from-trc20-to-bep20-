// Command seed-slot-key registers one payout slot's own key -- the same
// class of gap dispatcher/cmd/seed-slot closed on C5's side: S1's own
// HTTP boundary (internal/httpapi) has no POST /v1/slots route either,
// so a slot key only ever came into existence via slots.Store.Register,
// called directly by Go code (tests, mainly). This mirrors
// ledger/cmd/seed-console's own precedent -- a small, typed Go tool
// calling the real internal package directly, run once by an operator,
// not a manual SQL statement and not a new production HTTP endpoint.
//
// Wires the exact same KMSClient construction s1d itself uses
// (S1_KMS_CLIENT/S1_KMS_FAKE_SEED) so the key this registers is the same
// one s1d will actually sign with later -- kmsKeyID is just a label the
// fake client derives a deterministic keypair from; any string works,
// but it must be the SAME string used consistently for this slot (a
// different label derives a DIFFERENT key/address).
//
// Usage:
//
//	S1_DATABASE_URL=... S1_KMS_CLIENT=fake S1_KMS_FAKE_SEED=... \
//	  seed-slot-key -id 1 -key-id slot-1
//
// Safe to run more than once for the same -id: slots.Store.Register
// itself is NOT idempotent (a second Register for an existing slot_id or
// kms_key_id errors, slots.ErrDuplicateSlot), so this command catches
// that specific error and reports the slot as it already exists instead
// of failing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"s1/internal/db"
	"s1/internal/kmssign"
	"s1/internal/slots"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.Int("id", 0, "this slot's own numeric id (required, > 0)")
	keyID := flag.String("key-id", "", "the KMS key id backing this slot (required) -- any string for S1_KMS_CLIENT=fake, but must stay stable for this slot")
	flag.Parse()

	if *id <= 0 {
		return fmt.Errorf("seed-slot-key: -id is required and must be > 0")
	}
	if *keyID == "" {
		return fmt.Errorf("seed-slot-key: -key-id is required")
	}

	ctx := context.Background()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	kmsClient, err := kmsClientFromEnv()
	if err != nil {
		return err
	}
	slotStore := slots.NewStore(pool, kmssign.NewWrapper(kmsClient))

	key, err := slotStore.Register(ctx, *id, *keyID)
	if err != nil {
		if !errors.Is(err, slots.ErrDuplicateSlot) {
			return fmt.Errorf("seed-slot-key: registering slot %d: %w", *id, err)
		}
		existing, getErr := slotStore.Get(ctx, *id)
		if getErr != nil {
			return fmt.Errorf("seed-slot-key: slot %d already registered, but fetching it failed: %w", *id, getErr)
		}
		if existing.KMSKeyID != *keyID {
			return fmt.Errorf("seed-slot-key: slot %d is already registered with a DIFFERENT kms_key_id (%s) than requested (%s) -- refusing to proceed",
				*id, existing.KMSKeyID, *keyID)
		}
		fmt.Printf("ok   slot %d already registered, kms_key_id=%s, tron_address=%s, status=%s\n",
			existing.SlotID, existing.KMSKeyID, existing.TronAddress, existing.Status)
		return nil
	}

	fmt.Printf("ok   slot %d registered, kms_key_id=%s, tron_address=%s, status=%s\n",
		key.SlotID, key.KMSKeyID, key.TronAddress, key.Status)
	return nil
}

// kmsClientFromEnv mirrors cmd/s1d's own function exactly -- see that
// file's own doc comment for why S1_KMS_CLIENT has no default.
func kmsClientFromEnv() (kmssign.KMSClient, error) {
	switch os.Getenv("S1_KMS_CLIENT") {
	case "fake":
		seed := int64(1)
		if raw := os.Getenv("S1_KMS_FAKE_SEED"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("seed-slot-key: S1_KMS_FAKE_SEED: %w", err)
			}
			seed = parsed
		}
		return kmssign.NewFakeKMSClient(seed), nil
	case "privy":
		appID := os.Getenv("PRIVY_APP_ID")
		appSecret := os.Getenv("PRIVY_APP_SECRET")
		if appID == "" || appSecret == "" {
			return nil, errors.New("seed-slot-key: S1_KMS_CLIENT=privy requires PRIVY_APP_ID and PRIVY_APP_SECRET to both be set")
		}
		return kmssign.NewPrivyKMSClient(appID, appSecret), nil
	case "":
		return nil, errors.New("seed-slot-key: S1_KMS_CLIENT is not set (must match whatever s1d itself is configured with)")
	default:
		return nil, fmt.Errorf("seed-slot-key: S1_KMS_CLIENT=%q is not a recognized KMS client (\"fake\" or \"privy\")", os.Getenv("S1_KMS_CLIENT"))
	}
}
