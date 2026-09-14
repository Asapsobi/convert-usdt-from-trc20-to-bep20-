// Package retailsessions is a retail customer's own login session:
// issued at login, presented as a bearer token on every subsequent
// request (`Authorization: Bearer rs_...`), revocable (logout), and
// real-expiring -- unlike internal/customers' own long-lived API key,
// this is shaped like a web session, matching what an end-user product
// actually needs, not a service-to-service credential repurposed for
// one.
package retailsessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gateway/internal/db"
)

// DefaultSessionDuration is how long a freshly issued session stays
// valid -- config in the sense that a future chunk could expose it, not
// exposed as one yet since nothing has asked for a different value. 30
// days: long enough that a retail customer isn't forced to re-login on
// every visit, short enough that a leaked, unrevoked token doesn't stay
// valid indefinitely.
const DefaultSessionDuration = 30 * 24 * time.Hour

// ErrInvalidOrExpired is Validate's own result for a token that doesn't
// match any session, or matches one that's expired or was revoked --
// deliberately the same error for all three cases, so a caller can't
// distinguish "never existed" from "expired" from "logged out" by
// probing this endpoint.
var ErrInvalidOrExpired = errors.New("retailsessions: invalid or expired session")

// Session is one row of the retail_sessions table.
type Session struct {
	ID               int64
	RetailCustomerID int64
	CreatedAt        time.Time
	ExpiresAt        time.Time
	RevokedAt        *time.Time
}

// Store is the retail_sessions table's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// generateToken returns a fresh, random, high-entropy session token --
// same 256-bit entropy and "rs_" (retail session) prefix convention as
// customers.GenerateAPIKey's own "sk_live_"/"sk_test_", so it's visually
// distinct from either in a support ticket or a pasted log line.
func generateToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("retailsessions: generating token: %w", err)
	}
	return "rs_" + hex.EncodeToString(raw), nil
}

// HashToken hashes a raw session token for storage/lookup -- SHA-256,
// not bcrypt, for the exact same reason customers.HashAPIKey isn't
// either: this token is already 256 bits of real randomness (unlike a
// human-chosen password), so a fast, indexed lookup is the right tool,
// not a slow hash meant to defend a low-entropy secret.
func HashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// Create issues a new session for retailCustomerID, valid for
// DefaultSessionDuration from now. Returns the session row and the raw
// token -- the only moment that raw value ever exists outside the
// caller's own memory, mirroring customers.Store.Create's own "shown
// once" discipline.
func (s *Store) Create(ctx context.Context, retailCustomerID int64, now time.Time) (Session, string, error) {
	rawToken, err := generateToken()
	if err != nil {
		return Session{}, "", err
	}
	hash := HashToken(rawToken)
	expiresAt := now.Add(DefaultSessionDuration)

	row := s.pool.QueryRow(ctx, `
		INSERT INTO retail_sessions (retail_customer_id, session_token_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, retail_customer_id, created_at, expires_at, revoked_at
	`, retailCustomerID, hash, now, expiresAt)
	sess, err := scanSession(row)
	if err != nil {
		return Session{}, "", fmt.Errorf("retailsessions: creating for retail customer %d: %w", retailCustomerID, err)
	}
	return sess, rawToken, nil
}

// Validate resolves rawToken to its still-valid session -- not expired,
// not revoked -- or ErrInvalidOrExpired. now is passed explicitly
// (never time.Now() inside this package) so a caller's own tests can
// exercise expiry deterministically, the same convention every other
// time-sensitive Store in this repo already follows.
func (s *Store) Validate(ctx context.Context, rawToken string, now time.Time) (Session, error) {
	hash := HashToken(rawToken)
	row := s.pool.QueryRow(ctx, `
		SELECT id, retail_customer_id, created_at, expires_at, revoked_at
		FROM retail_sessions WHERE session_token_hash = $1
	`, hash)
	sess, err := scanSession(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrInvalidOrExpired
		}
		return Session{}, fmt.Errorf("retailsessions: validating: %w", err)
	}
	if sess.RevokedAt != nil || !now.Before(sess.ExpiresAt) {
		return Session{}, ErrInvalidOrExpired
	}
	return sess, nil
}

// Revoke logs a session out -- idempotent (revoking an already-revoked
// or nonexistent session is a no-op, never an error, matching this
// project's established "idempotent by construction" convention for
// every terminal-state transition).
func (s *Store) Revoke(ctx context.Context, rawToken string) error {
	hash := HashToken(rawToken)
	if _, err := s.pool.Exec(ctx, `
		UPDATE retail_sessions SET revoked_at = now() WHERE session_token_hash = $1 AND revoked_at IS NULL
	`, hash); err != nil {
		return fmt.Errorf("retailsessions: revoking: %w", err)
	}
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanSession(row scannable) (Session, error) {
	var sess Session
	if err := row.Scan(&sess.ID, &sess.RetailCustomerID, &sess.CreatedAt, &sess.ExpiresAt, &sess.RevokedAt); err != nil {
		return Session{}, err
	}
	return sess, nil
}
