// Package quotes is C6.2: issuing and persisting priced quotes, and
// (for C6.3) marking one consumed exactly once. A quote is computed by
// internal/pricing exactly once, at issuance -- this package never
// recomputes a price, only stores and later reads back what
// internal/pricing already decided.
package quotes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gateway/internal/db"
	"gateway/internal/money"
	"gateway/internal/pricing"
)

// Quote is one row of the quotes table. Exactly one of CustomerID
// (B2B partner) / RetailCustomerID (B2C end-user) is ever set -- mirrors
// the quotes table's own CHECK constraint (migration 0009). See
// docs/01-strategy/model-d-model-f-product-separation.md's own 14 Sep
// 2026 decision for why this table serves both owner kinds.
type Quote struct {
	ID                        int64
	CustomerID                *int64
	RetailCustomerID          *int64
	Tier                      pricing.Tier
	AmountIn                  money.Amount
	AmountOut                 money.Amount
	FeeUnits                  money.Amount
	NetworkFeeUnits           money.Amount
	RecipientAddress          string
	CreatedAt                 time.Time
	ExpiresAt                 time.Time
	ConsumedAt                *time.Time
	ConsumedByOrderExternalID *string
}

// Expired reports whether q's own lock window has passed as of now.
func (q Quote) Expired(now time.Time) bool { return !now.Before(q.ExpiresAt) }

// Consumed reports whether q has already been used by an order.
func (q Quote) Consumed() bool { return q.ConsumedAt != nil }

// ErrNotFound means no quote matches the given id (or it belongs to a
// different customer -- Get always scopes by customer_id, so a
// customer can never read another customer's own quote by guessing an
// id).
var ErrNotFound = errors.New("quotes: no such quote")

// ErrAlreadyConsumed means MarkConsumed found the row already consumed
// by a DIFFERENT order than the one asking -- a real conflict, not an
// idempotent replay (see MarkConsumed's own doc comment for the
// distinction).
var ErrAlreadyConsumed = errors.New("quotes: already consumed by a different order")

// Store is the quotes table's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Create issues and persists a new quote -- customerID's own priced
// request, expiring validity after now. Two calls with identical
// tier/amountIn/customerID still produce two independent rows with
// different ids and independent expiry, per C6.2's own acceptance
// criterion: no caching or reuse across requests.
func (s *Store) Create(ctx context.Context, customerID int64, priced pricing.Quote, recipientAddress string, now time.Time, validity time.Duration) (Quote, error) {
	return s.create(ctx, &customerID, nil, priced, recipientAddress, now, validity)
}

// CreateForRetail is Create's own counterpart for a B2C end-user
// (internal/retailcustomers), writing retail_customer_id instead of
// customer_id -- otherwise identical.
func (s *Store) CreateForRetail(ctx context.Context, retailCustomerID int64, priced pricing.Quote, recipientAddress string, now time.Time, validity time.Duration) (Quote, error) {
	return s.create(ctx, nil, &retailCustomerID, priced, recipientAddress, now, validity)
}

func (s *Store) create(ctx context.Context, customerID, retailCustomerID *int64, priced pricing.Quote, recipientAddress string, now time.Time, validity time.Duration) (Quote, error) {
	expiresAt := now.Add(validity)
	row := s.pool.QueryRow(ctx, `
		INSERT INTO quotes (customer_id, retail_customer_id, tier, amount_in, amount_out, fee_units, network_fee_units, recipient_address, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, customer_id, retail_customer_id, tier, amount_in, amount_out, fee_units, network_fee_units, recipient_address,
			created_at, expires_at, consumed_at, consumed_by_order_external_id
	`, customerID, retailCustomerID, string(priced.Tier), int64(priced.AmountIn), int64(priced.AmountOut), int64(priced.FeeUnits), int64(priced.NetworkFeeUnits),
		recipientAddress, now, expiresAt)
	q, err := scanQuote(row)
	if err != nil {
		return Quote{}, fmt.Errorf("quotes: creating: %w", err)
	}
	return q, nil
}

// Get fetches quote id, scoped to customerID -- a quote belonging to a
// different customer is reported as ErrNotFound, never as "exists but
// isn't yours," the same ownership-not-just-existence discipline C6.5's
// own acceptance criterion applies to order status.
func (s *Store) Get(ctx context.Context, id, customerID int64) (Quote, error) {
	return s.getScoped(ctx, id, "customer_id", customerID)
}

// GetForRetail is Get's own counterpart, scoped to retail_customer_id
// instead -- same "belongs to someone else is ErrNotFound, not a
// permission error" discipline.
func (s *Store) GetForRetail(ctx context.Context, id, retailCustomerID int64) (Quote, error) {
	return s.getScoped(ctx, id, "retail_customer_id", retailCustomerID)
}

func (s *Store) getScoped(ctx context.Context, id int64, ownerColumn string, ownerID int64) (Quote, error) {
	// ownerColumn is one of exactly two compile-time-known literals
	// (never caller/request-supplied), so building the query string with
	// it is safe -- the same posture as any other internal, fixed-choice
	// SQL fragment in this codebase.
	row := s.pool.QueryRow(ctx, `
		SELECT id, customer_id, retail_customer_id, tier, amount_in, amount_out, fee_units, network_fee_units, recipient_address,
			created_at, expires_at, consumed_at, consumed_by_order_external_id
		FROM quotes WHERE id = $1 AND `+ownerColumn+` = $2
	`, id, ownerID)
	q, err := scanQuote(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quote{}, ErrNotFound
		}
		return Quote{}, fmt.Errorf("quotes: fetching %d: %w", id, err)
	}
	return q, nil
}

// MarkConsumed atomically claims quote id for orderExternalID --
// guarded by "WHERE consumed_at IS NULL", the same claim-not-check
// pattern this project uses everywhere a race is possible (e.g.
// dispatch_attempts.Create's own UNIQUE-constraint claim). Three
// outcomes:
//   - The claim succeeds (this call was first): returns the now-consumed
//     row, nil error.
//   - The row was ALREADY consumed by this exact orderExternalID (a
//     genuine retry of the same choreography step, C6.3's own
//     idempotency requirement): returns the row as-is, nil error --
//     never an error for a caller replaying its own prior success.
//   - The row was already consumed by a DIFFERENT orderExternalID (two
//     genuinely different orders racing the same quote): returns
//     ErrAlreadyConsumed.
func (s *Store) MarkConsumed(ctx context.Context, id int64, orderExternalID string) (Quote, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE quotes SET consumed_at = now(), consumed_by_order_external_id = $1
		WHERE id = $2 AND consumed_at IS NULL
		RETURNING id, customer_id, retail_customer_id, tier, amount_in, amount_out, fee_units, network_fee_units, recipient_address,
			created_at, expires_at, consumed_at, consumed_by_order_external_id
	`, orderExternalID, id)
	q, err := scanQuote(row)
	if err == nil {
		return q, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, fmt.Errorf("quotes: marking %d consumed: %w", id, err)
	}

	existing, getErr := s.getByID(ctx, id)
	if getErr != nil {
		return Quote{}, getErr
	}
	if existing.ConsumedByOrderExternalID != nil && *existing.ConsumedByOrderExternalID == orderExternalID {
		return existing, nil // idempotent replay of this exact order's own prior claim
	}
	return Quote{}, ErrAlreadyConsumed
}

// GetByID fetches quote id unscoped by owner -- for internal callers
// that already hold a trusted row referencing this quote (e.g.
// internal/reconcile, iterating its own gateway_orders rows) rather
// than an untrusted customer-supplied id needing ownership enforcement.
// Customer-facing callers must use Get/GetForRetail instead.
func (s *Store) GetByID(ctx context.Context, id int64) (Quote, error) {
	return s.getByID(ctx, id)
}

func (s *Store) getByID(ctx context.Context, id int64) (Quote, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, customer_id, retail_customer_id, tier, amount_in, amount_out, fee_units, network_fee_units, recipient_address,
			created_at, expires_at, consumed_at, consumed_by_order_external_id
		FROM quotes WHERE id = $1
	`, id)
	q, err := scanQuote(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quote{}, ErrNotFound
		}
		return Quote{}, fmt.Errorf("quotes: fetching %d: %w", id, err)
	}
	return q, nil
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanQuote(row scanRow) (Quote, error) {
	var q Quote
	var tier string
	var amountIn, amountOut, feeUnits, networkFeeUnits int64
	if err := row.Scan(&q.ID, &q.CustomerID, &q.RetailCustomerID, &tier, &amountIn, &amountOut, &feeUnits, &networkFeeUnits, &q.RecipientAddress,
		&q.CreatedAt, &q.ExpiresAt, &q.ConsumedAt, &q.ConsumedByOrderExternalID); err != nil {
		return Quote{}, err
	}
	q.Tier = pricing.Tier(tier)
	q.AmountIn, q.AmountOut, q.FeeUnits, q.NetworkFeeUnits = money.Amount(amountIn), money.Amount(amountOut), money.Amount(feeUnits), money.Amount(networkFeeUnits)
	return q, nil
}
