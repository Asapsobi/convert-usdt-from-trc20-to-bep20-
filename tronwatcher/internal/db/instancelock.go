package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAnotherInstanceRunning means some other process already holds this
// service's instance lock on the same database.
var ErrAnotherInstanceRunning = errors.New("db: another instance of this service is already running against this database")

// InstanceLock guarantees at most one process drives this database at a
// time. Two watchers would each report the same deposits and race on the
// scan cursor -- so the lock is a Postgres session-level advisory
// lock held on one dedicated connection for the life of the process.
// The server releases it the moment that session ends, including on a
// crash, so a dead holder never blocks a restart.
type InstanceLock struct {
	conn *pgxpool.Conn
	name string

	lost     chan struct{}
	lostOnce sync.Once
	stop     chan struct{}
	stopOnce sync.Once
}

// instanceLockCheckInterval is how often the holder confirms its locking
// session is still alive.
const instanceLockCheckInterval = 5 * time.Second

// AcquireInstanceLock takes the lock named name, retrying for up to wait
// so a restart can overlap the previous process's shutdown. It returns
// ErrAnotherInstanceRunning if the lock is still held after wait.
func AcquireInstanceLock(ctx context.Context, p *Pool, name string, wait time.Duration) (*InstanceLock, error) {
	conn, err := p.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: acquiring a connection for the instance lock: %w", err)
	}

	deadline := time.Now().Add(wait)
	for {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, name).Scan(&got); err != nil {
			conn.Release()
			return nil, fmt.Errorf("db: taking instance lock %q: %w", name, err)
		}
		if got {
			break
		}
		if time.Now().After(deadline) {
			conn.Release()
			return nil, fmt.Errorf("%w (lock %q)", ErrAnotherInstanceRunning, name)
		}
		slog.Warn("db: another instance holds the instance lock, waiting for it to exit", "lock", name)
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	l := &InstanceLock{conn: conn, name: name, lost: make(chan struct{}), stop: make(chan struct{})}
	go l.watch()
	slog.Info("db: instance lock acquired -- this is the only instance driving this database", "lock", name)
	return l, nil
}

// Lost is closed if the locking session dies. The server has released
// the lock by then, so the caller must stop all work immediately.
func (l *InstanceLock) Lost() <-chan struct{} { return l.lost }

func (l *InstanceLock) watch() {
	ticker := time.NewTicker(instanceLockCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), instanceLockCheckInterval)
			_, err := l.conn.Exec(ctx, `SELECT 1`)
			cancel()
			if err != nil {
				slog.Error("db: instance lock session lost -- another instance may now take over", "lock", l.name, "error", err)
				l.lostOnce.Do(func() { close(l.lost) })
				return
			}
		}
	}
}

// Release gives the lock back and returns its connection to the pool.
func (l *InstanceLock) Release() {
	l.stopOnce.Do(func() {
		close(l.stop)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, l.name)
		l.conn.Release()
	})
}
