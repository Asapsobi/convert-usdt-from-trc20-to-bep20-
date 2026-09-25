// Package vendors is relayd's vendor registry: which vendors exist for
// each service, which an administrator has enabled and in what priority,
// which are healthy right now -- and the routers that pick a vendor per
// request and fail over to the next one (conversion.go, energy.go).
package vendors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"relayd/internal/db"
)

// Service is what a vendor provides.
type Service string

const (
	Conversion Service = "conversion" // swaps USDT across networks and pays the customer
	Energy     Service = "energy"     // rents TRON energy to a wallet
)

// Selection strategies an administrator can choose per service.
const (
	BestRate = "best_rate" // conversion: the vendor paying the customer the most
	Cheapest = "cheapest"  // energy: the vendor charging the least
	Priority = "priority"  // either: the administrator's priority order
)

// Vendor is one vendor's registry row.
type Vendor struct {
	Service             Service
	Name                string
	Enabled             bool
	Priority            int // lower is preferred
	ConsecutiveFailures int
	UnavailableUntil    *time.Time
	LastError           *string
	LastErrorAt         *time.Time
	LastSuccessAt       *time.Time
	UpdatedAt           time.Time
}

// Available reports whether v may be used at now: enabled, and not
// sitting out a failure back-off.
func (v Vendor) Available(now time.Time) bool {
	return v.Enabled && (v.UnavailableUntil == nil || !v.UnavailableUntil.After(now))
}

// backoff is how long a vendor sits out after its nth consecutive
// failure: 30s, 1m, 2m, 4m, ... capped at 10m.
func backoff(n int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < n && d < 10*time.Minute; i++ {
		d *= 2
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// Store is the registry.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

const vendorSelect = `
	SELECT service, name, enabled, priority, consecutive_failures, unavailable_until,
		last_error, last_error_at, last_success_at, updated_at
	FROM vendors`

func scanVendor(row interface{ Scan(...any) error }) (Vendor, error) {
	var v Vendor
	var service string
	err := row.Scan(&service, &v.Name, &v.Enabled, &v.Priority, &v.ConsecutiveFailures, &v.UnavailableUntil,
		&v.LastError, &v.LastErrorAt, &v.LastSuccessAt, &v.UpdatedAt)
	v.Service = Service(service)
	return v, err
}

// Register makes sure name is in the registry for service (enabled, at
// the default priority, when new). A vendor is registered at startup for
// every vendor whose credentials are configured.
func (s *Store) Register(ctx context.Context, service Service, name string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO vendors (service, name) VALUES ($1, $2) ON CONFLICT (service, name) DO NOTHING
	`, string(service), name); err != nil {
		return fmt.Errorf("vendors: registering %s vendor %s: %w", service, name, err)
	}
	return nil
}

// List returns every registered vendor for service, preferred first.
func (s *Store) List(ctx context.Context, service Service) ([]Vendor, error) {
	rows, err := s.pool.Query(ctx, vendorSelect+` WHERE service = $1 ORDER BY priority, name`, string(service))
	if err != nil {
		return nil, fmt.Errorf("vendors: listing %s vendors: %w", service, err)
	}
	defer rows.Close()
	var out []Vendor
	for rows.Next() {
		v, err := scanVendor(rows)
		if err != nil {
			return nil, fmt.Errorf("vendors: listing %s vendors: %w", service, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Get returns one vendor.
func (s *Store) Get(ctx context.Context, service Service, name string) (Vendor, error) {
	v, err := scanVendor(s.pool.QueryRow(ctx, vendorSelect+` WHERE service = $1 AND name = $2`, string(service), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Vendor{}, fmt.Errorf("vendors: no %s vendor named %q", service, name)
	}
	return v, err
}

// RecordSuccess returns a vendor to the pool.
func (s *Store) RecordSuccess(ctx context.Context, service Service, name string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE vendors SET consecutive_failures = 0, unavailable_until = NULL, last_success_at = now(), updated_at = now()
		WHERE service = $1 AND name = $2
	`, string(service), name)
	return err
}

// RecordFailure takes a vendor out of the pool for its next back-off.
func (s *Store) RecordFailure(ctx context.Context, service Service, name string, cause error) error {
	var failures int
	if err := s.pool.QueryRow(ctx, `
		UPDATE vendors SET consecutive_failures = consecutive_failures + 1, last_error = $3, last_error_at = now(), updated_at = now()
		WHERE service = $1 AND name = $2
		RETURNING consecutive_failures
	`, string(service), name, cause.Error()).Scan(&failures); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE vendors SET unavailable_until = now() + $3::interval WHERE service = $1 AND name = $2`,
		string(service), name, backoff(failures))
	return err
}

// SetEnabled lets an administrator take a vendor out of (or back into)
// use. Re-enabling also clears any failure back-off.
func (s *Store) SetEnabled(ctx context.Context, service Service, name string, enabled bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE vendors SET enabled = $3,
			unavailable_until = CASE WHEN $3 THEN NULL ELSE unavailable_until END,
			consecutive_failures = CASE WHEN $3 THEN 0 ELSE consecutive_failures END,
			updated_at = now()
		WHERE service = $1 AND name = $2
	`, string(service), name, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("vendors: no %s vendor named %q", service, name)
	}
	return nil
}

// SetPriority sets a vendor's priority (lower is preferred).
func (s *Store) SetPriority(ctx context.Context, service Service, name string, priority int) error {
	tag, err := s.pool.Exec(ctx, `UPDATE vendors SET priority = $3, updated_at = now() WHERE service = $1 AND name = $2`,
		string(service), name, priority)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("vendors: no %s vendor named %q", service, name)
	}
	return nil
}

// Strategy returns the administrator's selection strategy for service.
func (s *Store) Strategy(ctx context.Context, service Service) (string, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'vendor_selection'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultStrategy(service), nil
	}
	if err != nil {
		return "", fmt.Errorf("vendors: reading the selection strategy: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("vendors: parsing the selection strategy: %w", err)
	}
	if v := m[string(service)]; v != "" {
		return v, nil
	}
	return defaultStrategy(service), nil
}

// SetStrategy stores the selection strategy for service.
func (s *Store) SetStrategy(ctx context.Context, service Service, strategy string) error {
	switch {
	case strategy == Priority:
	case service == Conversion && strategy == BestRate:
	case service == Energy && strategy == Cheapest:
	default:
		return fmt.Errorf("vendors: %q is not a strategy for %s vendors", strategy, service)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ('vendor_selection', jsonb_build_object($1::text, $2::text))
		ON CONFLICT (key) DO UPDATE SET value = settings.value || jsonb_build_object($1::text, $2::text), updated_at = now()
	`, string(service), strategy)
	return err
}

func defaultStrategy(service Service) string {
	if service == Energy {
		return Cheapest
	}
	return BestRate
}

// usable filters the registry down to the vendors that are configured
// (credentials present -- a key of configured) and available now, in
// priority order.
func (s *Store) usable(ctx context.Context, service Service, configured map[string]bool) ([]Vendor, error) {
	all, err := s.List(ctx, service)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []Vendor
	for _, v := range all {
		if configured[v.Name] && v.Available(now) {
			out = append(out, v)
		}
	}
	return out, nil
}
