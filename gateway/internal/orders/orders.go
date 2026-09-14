// Package orders is C6.3/C6.4's own bookkeeping: gateway_orders, the
// row that exists precisely to close the real partial-failure window
// between C1's order creation and C2's address assignment -- see
// c6-api-gateway-build-prompts.md's own "Read this second".
package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gateway/internal/db"
)

// GatewayOrder is one row of gateway_orders. Exactly one of CustomerID
// (B2B partner) / RetailCustomerID (B2C end-user) is ever set -- mirrors
// the table's own CHECK constraint (migration 0009), same split
// quotes.Quote uses.
type GatewayOrder struct {
	ExternalID              string
	CustomerID              *int64
	RetailCustomerID        *int64
	QuoteID                 int64
	C1OrderID               int64
	C1OrderCreated          bool
	C2AddressAssigned       bool
	DepositAddress          *string
	AddressPendingAlertedAt *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// ErrNotFound means no gateway_orders row exists for the given
// external_id.
var ErrNotFound = errors.New("orders: no such order")

// OwnerLabel renders o's own owner as a short string for C2's own
// customer_id metadata field (informational only, not a security
// boundary) -- "retail:"-prefixed for a B2C order so its id space never
// collides with a same-numbered B2B customer_id when read back off C2's
// own row.
func (o GatewayOrder) OwnerLabel() string {
	if o.CustomerID != nil {
		return fmt.Sprintf("%d", *o.CustomerID)
	}
	if o.RetailCustomerID != nil {
		return fmt.Sprintf("retail:%d", *o.RetailCustomerID)
	}
	return "unknown"
}

// Store is the gateway_orders table's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts a new gateway_orders row -- always with
// c1_order_created=true, since this is only ever called after C1's own
// POST /v1/orders has already succeeded (step 4 of the choreography,
// c6-api-gateway-build-prompts.md C6.3). Idempotent on external_id: a
// second Create for the same external_id returns the existing row
// unchanged rather than erroring, the same "the row, not
// application-level locking, makes concurrent callers agree" pattern
// this whole project uses everywhere a claim can race.
func (s *Store) Create(ctx context.Context, externalID string, customerID, quoteID, c1OrderID int64) (GatewayOrder, error) {
	return s.create(ctx, externalID, &customerID, nil, quoteID, c1OrderID)
}

// CreateForRetail is Create's own counterpart for a B2C end-user
// (internal/retailcustomers), writing retail_customer_id instead of
// customer_id -- otherwise identical.
func (s *Store) CreateForRetail(ctx context.Context, externalID string, retailCustomerID, quoteID, c1OrderID int64) (GatewayOrder, error) {
	return s.create(ctx, externalID, nil, &retailCustomerID, quoteID, c1OrderID)
}

func (s *Store) create(ctx context.Context, externalID string, customerID, retailCustomerID *int64, quoteID, c1OrderID int64) (GatewayOrder, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO gateway_orders (external_id, customer_id, retail_customer_id, quote_id, c1_order_id, c1_order_created, c2_address_assigned)
		VALUES ($1, $2, $3, $4, $5, true, false)
		ON CONFLICT (external_id) DO NOTHING
		RETURNING external_id, customer_id, retail_customer_id, quote_id, c1_order_id, c1_order_created, c2_address_assigned, deposit_address, address_pending_alerted_at, created_at, updated_at
	`, externalID, customerID, retailCustomerID, quoteID, c1OrderID)
	o, err := scanOrder(row)
	if err == nil {
		return o, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return GatewayOrder{}, fmt.Errorf("orders: creating %q: %w", externalID, err)
	}
	return s.Get(ctx, externalID)
}

// Get fetches a gateway_orders row by external_id.
func (s *Store) Get(ctx context.Context, externalID string) (GatewayOrder, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT external_id, customer_id, retail_customer_id, quote_id, c1_order_id, c1_order_created, c2_address_assigned, deposit_address, address_pending_alerted_at, created_at, updated_at
		FROM gateway_orders WHERE external_id = $1
	`, externalID)
	o, err := scanOrder(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GatewayOrder{}, ErrNotFound
		}
		return GatewayOrder{}, fmt.Errorf("orders: fetching %q: %w", externalID, err)
	}
	return o, nil
}

// MarkAddressAssigned records that C2 successfully assigned
// depositAddress for externalID -- idempotent (setting the same address
// twice is a harmless no-op update, never an error), matching C2's own
// POST /v1/addresses idempotency this call is downstream of.
func (s *Store) MarkAddressAssigned(ctx context.Context, externalID, depositAddress string) (GatewayOrder, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE gateway_orders SET c2_address_assigned = true, deposit_address = $1, updated_at = now()
		WHERE external_id = $2
		RETURNING external_id, customer_id, retail_customer_id, quote_id, c1_order_id, c1_order_created, c2_address_assigned, deposit_address, address_pending_alerted_at, created_at, updated_at
	`, depositAddress, externalID)
	o, err := scanOrder(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GatewayOrder{}, ErrNotFound
		}
		return GatewayOrder{}, fmt.Errorf("orders: marking %q address assigned: %w", externalID, err)
	}
	return o, nil
}

// MarkAddressPendingAlerted claims this row's one-time pending-address
// alert: the UPDATE only touches a row whose address_pending_alerted_at
// is still NULL, so concurrent reconciliation loop instances (or two
// ticks racing) agree on "already alerted" via the row itself, not
// application-level locking -- the same claim pattern this whole
// project uses everywhere a race can double-fire a side effect.
// Reports whether THIS call was the one that claimed it.
func (s *Store) MarkAddressPendingAlerted(ctx context.Context, externalID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE gateway_orders SET address_pending_alerted_at = now()
		WHERE external_id = $1 AND address_pending_alerted_at IS NULL
	`, externalID)
	if err != nil {
		return false, fmt.Errorf("orders: marking %q address-pending alerted: %w", externalID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListPendingAddress returns every row still missing its C2 address,
// older than minAge (C6.4's own short grace period so this loop never
// races a POST /v1/orders request still mid-flight), oldest first.
func (s *Store) ListPendingAddress(ctx context.Context, minAge time.Duration) ([]GatewayOrder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT external_id, customer_id, retail_customer_id, quote_id, c1_order_id, c1_order_created, c2_address_assigned, deposit_address, address_pending_alerted_at, created_at, updated_at
		FROM gateway_orders
		WHERE c2_address_assigned = false AND created_at < $1
		ORDER BY created_at ASC
	`, time.Now().UTC().Add(-minAge))
	if err != nil {
		return nil, fmt.Errorf("orders: listing pending-address rows: %w", err)
	}
	defer rows.Close()

	var out []GatewayOrder
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanOrder(row scanRow) (GatewayOrder, error) {
	var o GatewayOrder
	if err := row.Scan(&o.ExternalID, &o.CustomerID, &o.RetailCustomerID, &o.QuoteID, &o.C1OrderID, &o.C1OrderCreated, &o.C2AddressAssigned,
		&o.DepositAddress, &o.AddressPendingAlertedAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return GatewayOrder{}, err
	}
	return o, nil
}
