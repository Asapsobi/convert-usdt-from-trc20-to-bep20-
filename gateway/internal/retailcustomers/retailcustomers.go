// Package retailcustomers is Model D's own B2C identity: an individual
// end-user account, email + password, entirely separate from
// internal/customers' own B2B partner-account identity space (no API
// key, no webhook URL, no rate-limit override -- a retail customer logs
// in with a password, a B2B customer never does). See
// docs/01-strategy/model-d-model-f-product-separation.md's own 14 Sep
// 2026 decision for why this exists as a second, parallel identity
// space rather than a field bolted onto customers.
package retailcustomers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"gateway/internal/db"
)

// Status is one retail customer's own account status.
type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
)

// RetailCustomer is one row of the retail_customers table --
// PasswordHash only, never the raw password (see Register's own doc
// comment).
type RetailCustomer struct {
	ID           int64
	Email        string
	PasswordHash string
	Status       Status
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

var (
	// ErrEmailTaken means Register was called with an email already
	// registered -- a real conflict, distinct from a validation error.
	ErrEmailTaken = errors.New("retailcustomers: email is already registered")
	// ErrInvalidCredentials is Authenticate's own result for a wrong
	// email OR a wrong password -- deliberately the SAME error for both
	// (never "no such email" vs "wrong password" as distinct results),
	// so a caller can't enumerate registered emails by probing this
	// endpoint's own error message.
	ErrInvalidCredentials = errors.New("retailcustomers: invalid email or password")
	// ErrNotFound means no retail customer matches the given id.
	ErrNotFound = errors.New("retailcustomers: no such retail customer")
	// ErrSuspended means Authenticate found valid credentials for an
	// account that is not currently active.
	ErrSuspended = errors.New("retailcustomers: account is suspended")
)

// Store is the retail_customers table's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// bcryptCost is intentionally the library default (10), not tuned up or
// down -- this is a real login-form password (low entropy, human
// chosen), the opposite case from customers.HashAPIKey's own "already
// 256 bits of randomness, a slow hash would be pointless" reasoning:
// here a slow, salted hash is exactly the right tool, and the default
// cost is a reasonable, well-reviewed starting point rather than a
// number invented for this codebase.
const bcryptCost = bcrypt.DefaultCost

// Register creates a new active retail customer with a bcrypt-hashed
// password. email is lowercased and trimmed before storage/lookup, so
// "User@Example.com" and "user@example.com" are the same account --
// never two.
func (s *Store) Register(ctx context.Context, email, password string) (RetailCustomer, error) {
	email = normalizeEmail(email)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return RetailCustomer{}, fmt.Errorf("retailcustomers: hashing password: %w", err)
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO retail_customers (email, password_hash, status)
		VALUES ($1, $2, $3)
		RETURNING id, email, password_hash, status, created_at, updated_at
	`, email, string(hash), string(StatusActive))
	rc, err := scanRetailCustomer(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return RetailCustomer{}, ErrEmailTaken
		}
		return RetailCustomer{}, fmt.Errorf("retailcustomers: registering %q: %w", email, err)
	}
	return rc, nil
}

// Authenticate verifies email/password and returns the matching
// account. Constant-shape on failure -- a nonexistent email still runs
// bcrypt against a fixed dummy hash before returning
// ErrInvalidCredentials, so this call takes roughly the same time
// whether the email exists or not, closing the timing side-channel a
// naive "look up first, bail early if not found" implementation would
// open.
func (s *Store) Authenticate(ctx context.Context, email, password string) (RetailCustomer, error) {
	email = normalizeEmail(email)
	row := s.pool.QueryRow(ctx, `
		SELECT id, email, password_hash, status, created_at, updated_at
		FROM retail_customers WHERE email = $1
	`, email)
	rc, err := scanRetailCustomer(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
			return RetailCustomer{}, ErrInvalidCredentials
		}
		return RetailCustomer{}, fmt.Errorf("retailcustomers: looking up %q: %w", email, err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(rc.PasswordHash), []byte(password)); err != nil {
		return RetailCustomer{}, ErrInvalidCredentials
	}
	if rc.Status != StatusActive {
		return RetailCustomer{}, ErrSuspended
	}
	return rc, nil
}

// dummyHash is a real bcrypt hash of a fixed, unrelated string -- used
// only to give Authenticate's own "email not found" path the same
// bcrypt-comparison cost as its "email found, password checked" path
// (see Authenticate's own doc comment).
const dummyHash = "$2a$10$C6UzMDM.H6dfI/f/IKcEeO9L.d8vpV.nHXzcQwG1z3P.bLQeVfmMe"

// Get fetches a retail customer by id.
func (s *Store) Get(ctx context.Context, id int64) (RetailCustomer, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, email, password_hash, status, created_at, updated_at
		FROM retail_customers WHERE id = $1
	`, id)
	rc, err := scanRetailCustomer(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RetailCustomer{}, ErrNotFound
		}
		return RetailCustomer{}, fmt.Errorf("retailcustomers: fetching %d: %w", id, err)
	}
	return rc, nil
}

// Suspend transitions a retail customer to suspended -- idempotent,
// same posture as customers.Store.Suspend.
func (s *Store) Suspend(ctx context.Context, id int64) error {
	return s.setStatus(ctx, id, StatusSuspended)
}

// Reactivate transitions a retail customer back to active.
func (s *Store) Reactivate(ctx context.Context, id int64) error {
	return s.setStatus(ctx, id, StatusActive)
}

func (s *Store) setStatus(ctx context.Context, id int64, status Status) error {
	tag, err := s.pool.Exec(ctx, `UPDATE retail_customers SET status = $1, updated_at = now() WHERE id = $2`, string(status), id)
	if err != nil {
		return fmt.Errorf("retailcustomers: setting status for %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

type scannable interface {
	Scan(dest ...any) error
}

func scanRetailCustomer(row scannable) (RetailCustomer, error) {
	var rc RetailCustomer
	var status string
	if err := row.Scan(&rc.ID, &rc.Email, &rc.PasswordHash, &status, &rc.CreatedAt, &rc.UpdatedAt); err != nil {
		return RetailCustomer{}, err
	}
	rc.Status = Status(status)
	return rc, nil
}
