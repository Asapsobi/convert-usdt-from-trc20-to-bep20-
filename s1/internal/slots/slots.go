// Package slots is S1.2: S1's own key registry -- which KMS key backs
// which TRON payout slot, and that slot's own derived address. Distinct
// from C5's own slot-identity registry (caps, rotation, tx counts) --
// see this package's own migration comment for why the two never share a
// table or a module.
package slots

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"s1/internal/db"
)

// Status is one slot key row's own lifecycle state.
type Status string

const (
	StatusActive  Status = "ACTIVE"
	StatusRetired Status = "RETIRED"
)

// SlotKey is one row of s1_slot_keys.
type SlotKey struct {
	SlotID      int
	KMSKeyID    string
	TronAddress string
	PublicKey   [33]byte
	Status      Status
	CreatedAt   time.Time
	RetiredAt   *time.Time
}

// ErrDuplicateSlot means slotID or kmsKeyID is already registered --
// Register never overwrites an existing row.
var ErrDuplicateSlot = errors.New("slots: slot id or KMS key id is already registered")

// ErrSlotNotFound means no row exists for the given slot id.
var ErrSlotNotFound = errors.New("slots: no such slot")

// ErrAlreadyRetired guards Retire against a redundant call -- distinct
// from the DB-level trigger that would reject a real illegal transition,
// this is the friendlier, typed result for the specific "already exactly
// where you wanted it" case.
var ErrAlreadyRetired = errors.New("slots: slot is already retired")

// PublicKeyGetter is the one call this package needs from
// internal/kmssign -- kmssign.Wrapper's real implementation, or a fake
// for testing. Defined here (the consumer), matching this project's
// established convention.
type PublicKeyGetter interface {
	GetPublicKey(ctx context.Context, keyID string) ([33]byte, error)
}

// Store is this package's own entry point.
type Store struct {
	pool      *db.Pool
	pubKeyGet PublicKeyGetter
}

// NewStore wires a Store.
func NewStore(pool *db.Pool, pubKeyGet PublicKeyGetter) *Store {
	return &Store{pool: pool, pubKeyGet: pubKeyGet}
}

// Register creates slotID's own key registry row: fetches kmsKeyID's
// public key (the one and only place in this package a TRON address is
// computed from a public key -- C5's own slot registry is told the
// resulting address, never derives it independently, so the two
// registries cannot silently disagree), and inserts it ACTIVE. Rejects a
// duplicate slot_id or kms_key_id -- one key per slot, permanently, for
// as long as that row stays ACTIVE.
func (s *Store) Register(ctx context.Context, slotID int, kmsKeyID string) (SlotKey, error) {
	compressed, err := s.pubKeyGet.GetPublicKey(ctx, kmsKeyID)
	if err != nil {
		return SlotKey{}, fmt.Errorf("slots: fetching public key for KMS key %s: %w", kmsKeyID, err)
	}
	pub, err := secp256k1.ParsePubKey(compressed[:])
	if err != nil {
		return SlotKey{}, fmt.Errorf("slots: parsing public key for KMS key %s: %w", kmsKeyID, err)
	}
	address := deriveTronAddress(pub)

	row := s.pool.QueryRow(ctx, `
		INSERT INTO s1_slot_keys (slot_id, kms_key_id, tron_address, public_key, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING slot_id, kms_key_id, tron_address, public_key, status, created_at, retired_at
	`, slotID, kmsKeyID, address, compressed[:], string(StatusActive))

	key, err := scanSlotKey(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return SlotKey{}, ErrDuplicateSlot
		}
		return SlotKey{}, fmt.Errorf("slots: registering slot %d: %w", slotID, err)
	}
	return key, nil
}

// Retire transitions slotID from ACTIVE to RETIRED -- one-way, enforced
// again at the DB level (this package's own migration), this Go-level
// check exists only to return the friendlier ErrAlreadyRetired for the
// specific already-retired case rather than surfacing the trigger's own
// raw exception text.
func (s *Store) Retire(ctx context.Context, slotID int) error {
	existing, err := s.Get(ctx, slotID)
	if err != nil {
		return err
	}
	if existing.Status == StatusRetired {
		return ErrAlreadyRetired
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE s1_slot_keys SET status = $1, retired_at = now() WHERE slot_id = $2
	`, string(StatusRetired), slotID)
	if err != nil {
		return fmt.Errorf("slots: retiring slot %d: %w", slotID, err)
	}
	return nil
}

// EVMAddress derives slotID's own EVM-format address (BSC included) --
// the SAME secp256k1 key its TronAddress already comes from, just the
// EIP-55 checksum encoding instead of TRON's base58check. See evm.go's
// own doc comment for why this needs no new key, no new custody model,
// and no new stored column: SlotKey.PublicKey already holds everything
// this derivation needs.
func (s *Store) EVMAddress(ctx context.Context, slotID int) (string, error) {
	key, err := s.Get(ctx, slotID)
	if err != nil {
		return "", fmt.Errorf("slots: resolving EVM address for slot %d: %w", slotID, err)
	}
	addr, err := DeriveEVMAddress(key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("slots: resolving EVM address for slot %d: %w", slotID, err)
	}
	return addr, nil
}

// Get fetches one slot key row by id.
func (s *Store) Get(ctx context.Context, slotID int) (SlotKey, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT slot_id, kms_key_id, tron_address, public_key, status, created_at, retired_at
		FROM s1_slot_keys WHERE slot_id = $1
	`, slotID)
	key, err := scanSlotKey(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SlotKey{}, ErrSlotNotFound
		}
		return SlotKey{}, fmt.Errorf("slots: fetching slot %d: %w", slotID, err)
	}
	return key, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanSlotKey(row scannable) (SlotKey, error) {
	var k SlotKey
	var pubKey []byte
	var status string
	if err := row.Scan(&k.SlotID, &k.KMSKeyID, &k.TronAddress, &pubKey, &status, &k.CreatedAt, &k.RetiredAt); err != nil {
		return SlotKey{}, err
	}
	k.Status = Status(status)
	copy(k.PublicKey[:], pubKey)
	return k, nil
}
