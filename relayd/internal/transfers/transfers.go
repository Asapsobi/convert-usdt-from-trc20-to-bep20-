// Package transfers is the durable record of every on-chain transfer
// relayd builds for a relay leg -- the forward to the vendor, or a refund
// back to the depositor. An attempt is written here before it is signed
// and its signature before it is sent, so a restart at any point resumes
// the exact transaction that might already be on its way instead of
// building a second one that spends the same deposit.
//
// The one rule every caller relies on: at most one attempt per leg and
// purpose is open (BUILT, SIGNED or BROADCAST) at a time, enforced by a
// partial unique index. A new transaction is only built once every
// earlier one has provably confirmed, failed, or been dropped.
package transfers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"relayd/internal/db"
	"relayd/internal/money"
)

// Purpose is why a transfer is being sent.
type Purpose string

const (
	Forward  Purpose = "FORWARD"   // deposit address -> vendor's deposit address
	Refund   Purpose = "REFUND"    // deposit address -> the original depositor
	GasTopUp Purpose = "GAS_TOPUP" // treasury -> a BSC deposit wallet short of BNB for gas
	TRXTopUp Purpose = "TRX_TOPUP" // treasury -> a TRON deposit wallet: activates it, pays its bandwidth
	Sweep    Purpose = "SWEEP"     // deposit wallet -> treasury
)

// Chain is where a transfer is sent.
type Chain string

const (
	BSC  Chain = "BSC"
	TRON Chain = "TRON"
)

// Status is an attempt's lifecycle stage.
type Status string

const (
	// StatusBuilt: unsigned and persisted. It cannot land on-chain, so it
	// can be abandoned at any time.
	StatusBuilt Status = "BUILT"
	// StatusSigned: signed and persisted; it may or may not have reached
	// a node. From here on it might land, so it is never abandoned --
	// only resolved by what the chain says.
	StatusSigned Status = "SIGNED"
	// StatusBroadcast: at least one node accepted it.
	StatusBroadcast Status = "BROADCAST"
	// StatusConfirmed: executed successfully and final.
	StatusConfirmed Status = "CONFIRMED"
	// StatusFailed: included on-chain but its execution failed (a revert,
	// OUT_OF_ENERGY) -- the funds did not move.
	StatusFailed Status = "FAILED"
	// StatusDropped: provably can never land (a TRON transaction past its
	// expiration, or an EVM nonce consumed by a different transaction).
	StatusDropped Status = "DROPPED"
	// StatusAbandoned: a BUILT attempt that was never signed and sent.
	StatusAbandoned Status = "ABANDONED"
)

// Open reports whether s still holds the leg's single open-attempt slot.
func (s Status) Open() bool {
	return s == StatusBuilt || s == StatusSigned || s == StatusBroadcast
}

// MayLand reports whether an attempt in s could still move funds.
func (s Status) MayLand() bool {
	return s == StatusSigned || s == StatusBroadcast
}

// Attempt is a transfer_attempts row.
type Attempt struct {
	ID                 int64
	ExternalID         string
	Purpose            Purpose
	Chain              Chain
	FromAddress        string
	ToAddress          string
	Amount             money.Amount
	Status             Status
	UnsignedTx         []byte
	Digest             [32]byte
	Signature          *[65]byte
	TxHash             *string
	EVMNonce           *uint64
	TronExpiresAt      *time.Time
	BroadcastCount     int
	LastBroadcastAt    *time.Time
	LastBroadcastError *string
	NonceConsumedSince *time.Time
	StuckAlertedAt     *time.Time
	FailureReason      *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

var (
	// ErrOpenAttemptExists means Create was refused because the leg
	// already has an open attempt for that purpose.
	ErrOpenAttemptExists = errors.New("transfers: an open attempt already exists for this leg and purpose")
	// ErrNotInExpectedStatus means a Mark call's status guard didn't match.
	ErrNotInExpectedStatus = errors.New("transfers: attempt is not in the expected status for this transition")
)

// Store is transfer_attempts' entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

const selectSQL = `
	SELECT id, external_id, purpose, chain, from_address, to_address, amount, asset, status,
		unsigned_tx, digest, signature, tx_hash, evm_nonce, tron_expires_at,
		broadcast_count, last_broadcast_at, last_broadcast_error, nonce_consumed_since,
		stuck_alerted_at, failure_reason, created_at, updated_at
	FROM transfer_attempts`

// Create inserts a in StatusBuilt. It returns ErrOpenAttemptExists if the
// leg already has an open attempt for a.Purpose.
func (s *Store) Create(ctx context.Context, a Attempt) (Attempt, error) {
	var nonce *int64
	if a.EVMNonce != nil {
		v := int64(*a.EVMNonce)
		nonce = &v
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO transfer_attempts
			(external_id, purpose, chain, from_address, to_address, amount, asset, status,
			 unsigned_tx, digest, tx_hash, evm_nonce, tron_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`,
		a.ExternalID, string(a.Purpose), string(a.Chain), a.FromAddress, a.ToAddress, a.Amount.Units, string(a.Amount.Asset),
		string(StatusBuilt), a.UnsignedTx, a.Digest[:], a.TxHash, nonce, a.TronExpiresAt)
	var id int64
	if err := row.Scan(&id); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "transfer_attempts_one_open" {
			return Attempt{}, fmt.Errorf("%w: %s %s", ErrOpenAttemptExists, a.ExternalID, a.Purpose)
		}
		return Attempt{}, fmt.Errorf("transfers: creating %s attempt for %s: %w", a.Purpose, a.ExternalID, err)
	}
	return s.Get(ctx, id)
}

// Get fetches one attempt by id.
func (s *Store) Get(ctx context.Context, id int64) (Attempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx, selectSQL+` WHERE id = $1`, id))
	if err != nil {
		return Attempt{}, fmt.Errorf("transfers: fetching attempt %d: %w", id, err)
	}
	return a, nil
}

// Open returns the leg's open attempt for purpose, if any.
func (s *Store) Open(ctx context.Context, externalID string, purpose Purpose) (Attempt, bool, error) {
	return s.one(ctx, `WHERE external_id = $1 AND purpose = $2 AND status IN ('BUILT', 'SIGNED', 'BROADCAST')`, externalID, purpose)
}

// Confirmed returns the leg's confirmed attempt for purpose, if any.
func (s *Store) Confirmed(ctx context.Context, externalID string, purpose Purpose) (Attempt, bool, error) {
	return s.one(ctx, `WHERE external_id = $1 AND purpose = $2 AND status = 'CONFIRMED'`, externalID, purpose)
}

func (s *Store) one(ctx context.Context, where, externalID string, purpose Purpose) (Attempt, bool, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx, selectSQL+" "+where, externalID, string(purpose)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, false, nil
	}
	if err != nil {
		return Attempt{}, false, fmt.Errorf("transfers: fetching %s attempt for %s: %w", purpose, externalID, err)
	}
	return a, true, nil
}

// ListOpen returns every open attempt for any of purposes, oldest first.
func (s *Store) ListOpen(ctx context.Context, purposes ...Purpose) ([]Attempt, error) {
	names := make([]string, len(purposes))
	for i, p := range purposes {
		names[i] = string(p)
	}
	rows, err := s.pool.Query(ctx, selectSQL+`
		WHERE purpose = ANY($1) AND status IN ('BUILT', 'SIGNED', 'BROADCAST') ORDER BY id`, names)
	if err != nil {
		return nil, fmt.Errorf("transfers: listing open attempts: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("transfers: listing open attempts: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// OpenFrom returns an open attempt sent from address, if any. Only one
// transaction is ever open per sending address: two would race on its
// nonce (BSC) and its balance (both chains).
func (s *Store) OpenFrom(ctx context.Context, address string) (Attempt, bool, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx, selectSQL+`
		WHERE from_address = $1 AND status IN ('BUILT', 'SIGNED', 'BROADCAST') ORDER BY id LIMIT 1`, address))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, false, nil
	}
	if err != nil {
		return Attempt{}, false, fmt.Errorf("transfers: finding an open attempt from %s: %w", address, err)
	}
	return a, true, nil
}

// CountConfirmedWithPrefix counts confirmed attempts for purpose whose job
// key starts with prefix -- how many top-up rounds a job has used.
func (s *Store) CountConfirmedWithPrefix(ctx context.Context, prefix string, purpose Purpose) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM transfer_attempts WHERE starts_with(external_id, $1) AND purpose = $2 AND status = 'CONFIRMED'
	`, prefix, string(purpose)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("transfers: counting confirmed %s attempts for %s*: %w", purpose, prefix, err)
	}
	return n, nil
}

// LastConfirmedWithPrefix is when the most recent confirmed attempt for
// purpose whose job key starts with prefix was confirmed.
func (s *Store) LastConfirmedWithPrefix(ctx context.Context, prefix string, purpose Purpose) (time.Time, bool, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(updated_at) FROM transfer_attempts WHERE starts_with(external_id, $1) AND purpose = $2 AND status = 'CONFIRMED'
	`, prefix, string(purpose)).Scan(&t)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("transfers: reading the last confirmed %s for %s*: %w", purpose, prefix, err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}

// CountFailed counts the leg's attempts for purpose that executed
// on-chain and failed.
func (s *Store) CountFailed(ctx context.Context, externalID string, purpose Purpose) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM transfer_attempts WHERE external_id = $1 AND purpose = $2 AND status = 'FAILED'
	`, externalID, string(purpose)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("transfers: counting failed %s attempts for %s: %w", purpose, externalID, err)
	}
	return n, nil
}

// ListRelated returns every attempt for job plus the treasury top-ups
// that paid for them, oldest first -- a job's full on-chain footprint.
func (s *Store) ListRelated(ctx context.Context, job string) ([]Attempt, error) {
	rows, err := s.pool.Query(ctx, selectSQL+`
		WHERE external_id = $1
			OR starts_with(external_id, 'gas_topup:' || $1 || ':')
			OR starts_with(external_id, 'trx_topup:' || $1 || ':')
		ORDER BY id`, job)
	if err != nil {
		return nil, fmt.Errorf("transfers: listing attempts related to %s: %w", job, err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("transfers: listing attempts related to %s: %w", job, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListForLeg returns every attempt for externalID, oldest first.
func (s *Store) ListForLeg(ctx context.Context, externalID string) ([]Attempt, error) {
	rows, err := s.pool.Query(ctx, selectSQL+` WHERE external_id = $1 ORDER BY id`, externalID)
	if err != nil {
		return nil, fmt.Errorf("transfers: listing attempts for %s: %w", externalID, err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("transfers: listing attempts for %s: %w", externalID, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkSigned records the signature and the transaction hash it produces.
// This is the commit point: from here on the attempt may land on-chain.
func (s *Store) MarkSigned(ctx context.Context, id int64, signature [65]byte, txHash string) error {
	return s.transition(ctx, id, StatusSigned, []Status{StatusBuilt},
		`signature = $4, tx_hash = $5`, signature[:], txHash)
}

// RecordBroadcast notes one send of attempt id. A nil sendErr means a
// node accepted it, which moves a SIGNED attempt to BROADCAST.
func (s *Store) RecordBroadcast(ctx context.Context, id int64, sendErr error) error {
	var errText *string
	if sendErr != nil {
		t := sendErr.Error()
		errText = &t
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE transfer_attempts
		SET broadcast_count = broadcast_count + 1, last_broadcast_at = now(), last_broadcast_error = $2,
			status = CASE WHEN $2::text IS NULL AND status = 'SIGNED' THEN 'BROADCAST' ELSE status END,
			updated_at = now()
		WHERE id = $1 AND status IN ('SIGNED', 'BROADCAST')
	`, id, errText)
	if err != nil {
		return fmt.Errorf("transfers: recording broadcast of attempt %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: attempt %d is no longer SIGNED or BROADCAST", ErrNotInExpectedStatus, id)
	}
	return nil
}

// MarkConfirmed resolves a sent attempt as executed successfully.
func (s *Store) MarkConfirmed(ctx context.Context, id int64) error {
	return s.transition(ctx, id, StatusConfirmed, []Status{StatusSigned, StatusBroadcast}, ``)
}

// MarkFailed resolves a sent attempt as executed-and-failed (no funds moved).
func (s *Store) MarkFailed(ctx context.Context, id int64, reason string) error {
	return s.transition(ctx, id, StatusFailed, []Status{StatusSigned, StatusBroadcast}, `failure_reason = $4`, reason)
}

// MarkDropped resolves a sent attempt as provably never landing.
func (s *Store) MarkDropped(ctx context.Context, id int64, reason string) error {
	return s.transition(ctx, id, StatusDropped, []Status{StatusSigned, StatusBroadcast}, `failure_reason = $4`, reason)
}

// MarkAbandoned closes a BUILT (never signed and sent) attempt.
func (s *Store) MarkAbandoned(ctx context.Context, id int64, reason string) error {
	return s.transition(ctx, id, StatusAbandoned, []Status{StatusBuilt}, `failure_reason = $4`, reason)
}

// SetNonceConsumedSince records (or, with nil, clears) when the chain was
// first seen past this EVM attempt's nonce without its own receipt.
func (s *Store) SetNonceConsumedSince(ctx context.Context, id int64, at *time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE transfer_attempts SET nonce_consumed_since = $2, updated_at = now() WHERE id = $1`, id, at); err != nil {
		return fmt.Errorf("transfers: updating nonce_consumed_since on attempt %d: %w", id, err)
	}
	return nil
}

// MarkStuckAlerted claims the one-time "sent but not landing" alert for
// attempt id; fired is true only for the first caller.
func (s *Store) MarkStuckAlerted(ctx context.Context, id int64) (fired bool, err error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE transfer_attempts SET stuck_alerted_at = now() WHERE id = $1 AND stuck_alerted_at IS NULL
	`, id)
	if err != nil {
		return false, fmt.Errorf("transfers: marking attempt %d stuck-alerted: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// transition moves attempt id to to from one of from, applying extra SET
// clauses (whose parameters start at $4). Re-applying a transition that
// already happened is a no-op success.
func (s *Store) transition(ctx context.Context, id int64, to Status, from []Status, extra string, args ...any) error {
	fromText := make([]string, len(from))
	for i, f := range from {
		fromText[i] = string(f)
	}
	set := `status = $2, updated_at = now()`
	if extra != "" {
		set += ", " + extra
	}
	params := append([]any{id, string(to), fromText}, args...)
	tag, err := s.pool.Exec(ctx, `UPDATE transfer_attempts SET `+set+` WHERE id = $1 AND status = ANY($3)`, params...)
	if err != nil {
		return fmt.Errorf("transfers: marking attempt %d %s: %w", id, to, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	current, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.Status == to {
		return nil
	}
	return fmt.Errorf("%w: attempt %d is %s, cannot become %s", ErrNotInExpectedStatus, id, current.Status, to)
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanAttempt(row scanRow) (Attempt, error) {
	var a Attempt
	var purpose, chain, status, asset string
	var digest, signature []byte
	var nonce *int64
	err := row.Scan(&a.ID, &a.ExternalID, &purpose, &chain, &a.FromAddress, &a.ToAddress, &a.Amount.Units, &asset, &status,
		&a.UnsignedTx, &digest, &signature, &a.TxHash, &nonce, &a.TronExpiresAt,
		&a.BroadcastCount, &a.LastBroadcastAt, &a.LastBroadcastError, &a.NonceConsumedSince,
		&a.StuckAlertedAt, &a.FailureReason, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Attempt{}, err
	}
	a.Purpose, a.Chain, a.Status = Purpose(purpose), Chain(chain), Status(status)
	a.Amount.Asset = money.Asset(asset)
	copy(a.Digest[:], digest)
	if signature != nil {
		var sig [65]byte
		copy(sig[:], signature)
		a.Signature = &sig
	}
	if nonce != nil {
		v := uint64(*nonce)
		a.EVMNonce = &v
	}
	return a, nil
}
