package transfers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// A Rental is one request to a resource vendor (TRON energy) for one
// transfer, recorded before the vendor is asked -- so a crash mid-request
// is visible on restart instead of silently buying twice -- and closed
// with its outcome and cost.
type Rental struct {
	ID             int64
	Job            string
	Purpose        Purpose
	Address        string
	Resource       string // ENERGY or BANDWIDTH
	Units          int64
	IdempotencyKey string
	Provider       *string
	Status         string // PENDING, CONFIRMED, FAILED
	CostSun        *int64
	Error          *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const rentalSelect = `
	SELECT id, job, purpose, address, resource, units, idempotency_key, provider, status, cost_sun, error, created_at, updated_at
	FROM resource_rentals`

func scanRental(row scanRow) (Rental, error) {
	var r Rental
	var purpose string
	err := row.Scan(&r.ID, &r.Job, &purpose, &r.Address, &r.Resource, &r.Units, &r.IdempotencyKey,
		&r.Provider, &r.Status, &r.CostSun, &r.Error, &r.CreatedAt, &r.UpdatedAt)
	r.Purpose = Purpose(purpose)
	return r, err
}

// LatestRental returns the most recent rental of resource for job and
// purpose, and how many there have been.
func (s *Store) LatestRental(ctx context.Context, job string, purpose Purpose, resource string) (Rental, int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM resource_rentals WHERE job = $1 AND purpose = $2 AND resource = $3`,
		job, string(purpose), resource).Scan(&n); err != nil {
		return Rental{}, 0, fmt.Errorf("transfers: counting rentals for %s: %w", job, err)
	}
	r, err := scanRental(s.pool.QueryRow(ctx, rentalSelect+`
		WHERE job = $1 AND purpose = $2 AND resource = $3 ORDER BY id DESC LIMIT 1`, job, string(purpose), resource))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rental{}, 0, nil
	}
	if err != nil {
		return Rental{}, 0, fmt.Errorf("transfers: reading the latest rental for %s: %w", job, err)
	}
	return r, n, nil
}

// CreateRental records a rental as PENDING, before the vendor is asked.
func (s *Store) CreateRental(ctx context.Context, r Rental) (Rental, error) {
	out, err := scanRental(s.pool.QueryRow(ctx, `
		INSERT INTO resource_rentals (job, purpose, address, resource, units, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO UPDATE SET updated_at = resource_rentals.updated_at
		RETURNING id, job, purpose, address, resource, units, idempotency_key, provider, status, cost_sun, error, created_at, updated_at
	`, r.Job, string(r.Purpose), r.Address, r.Resource, r.Units, r.IdempotencyKey))
	if err != nil {
		return Rental{}, fmt.Errorf("transfers: recording a rental for %s: %w", r.Job, err)
	}
	return out, nil
}

// FinishRental records a rental's outcome.
func (s *Store) FinishRental(ctx context.Context, id int64, confirmed bool, provider *string, costSun *int64, cause error) error {
	status := "FAILED"
	if confirmed {
		status = "CONFIRMED"
	}
	var errText *string
	if cause != nil {
		t := cause.Error()
		errText = &t
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE resource_rentals SET status = $2, provider = $3, cost_sun = $4, error = $5, updated_at = now()
		WHERE id = $1 AND status = 'PENDING'
	`, id, status, provider, costSun, errText); err != nil {
		return fmt.Errorf("transfers: finishing rental %d: %w", id, err)
	}
	return nil
}

// RentalsForJob returns every rental made for job (a leg's external id),
// oldest first -- for tracking what a transaction cost.
func (s *Store) RentalsForJob(ctx context.Context, job string) ([]Rental, error) {
	rows, err := s.pool.Query(ctx, rentalSelect+` WHERE job = $1 ORDER BY id`, job)
	if err != nil {
		return nil, fmt.Errorf("transfers: listing rentals for %s: %w", job, err)
	}
	defer rows.Close()
	var out []Rental
	for rows.Next() {
		r, err := scanRental(rows)
		if err != nil {
			return nil, fmt.Errorf("transfers: listing rentals for %s: %w", job, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
