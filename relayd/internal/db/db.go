// Package db owns the Postgres connection pool and the transaction
// helper every write path in relayd runs through. Mirrors every sibling
// service's own identical db package (separate Go modules, no shared
// internal package) -- this is its own database, separate from C1's and
// from every other service's own.
package db

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Queryer is satisfied by both *Pool and pgx.Tx.
type Queryer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Config is the connection configuration, read from the environment.
type Config struct {
	DatabaseURL string
}

// ConfigFromEnv reads Config from RELAYD_DATABASE_URL.
func ConfigFromEnv() (Config, error) {
	url := os.Getenv("RELAYD_DATABASE_URL")
	if url == "" {
		return Config{}, errors.New("db: RELAYD_DATABASE_URL is not set")
	}
	return Config{DatabaseURL: url}, nil
}

// Pool wraps a pgxpool.Pool.
type Pool struct {
	*pgxpool.Pool
}

// Open creates and validates a connection pool against cfg.DatabaseURL.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// Tx runs fn inside a transaction on p, committing if fn returns nil and
// rolling back if fn returns an error or panics.
func Tx(ctx context.Context, p *Pool, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}

	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(ctx)
			panic(r)
		}
	}()

	if err := fn(ctx, tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("db: rollback after %w: %v", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
