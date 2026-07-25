package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/config"
)

// DB wraps the connection pool.
type DB struct {
	Pool *pgxpool.Pool
}

// Open connects to Postgres and verifies reachability.
//
// The initial connection is retried with backoff because compose brings
// Postgres and kiln up concurrently; a container that exits because the
// database was not ready two seconds in is a bad operator experience.
func Open(ctx context.Context, cfg *config.Config) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.DSN())
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn (%s): %w", cfg.Database.Redacted(), err)
	}

	poolCfg.MaxConns = cfg.Database.MaxConns
	poolCfg.MinConns = cfg.Database.MinConns
	poolCfg.MaxConnLifetime = cfg.Database.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.Database.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: create pool: %w", err)
	}

	if err := pingWithBackoff(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

// Close releases the pool.
func (db *DB) Close() {
	if db != nil && db.Pool != nil {
		db.Pool.Close()
	}
}

// Ping reports whether the database is reachable. Used by /readyz.
func (db *DB) Ping(ctx context.Context) error {
	return db.Pool.Ping(ctx)
}

const (
	pingAttempts    = 8
	pingBaseBackoff = 250 * time.Millisecond
	pingMaxBackoff  = 5 * time.Second
)

func pingWithBackoff(ctx context.Context, pool *pgxpool.Pool) error {
	backoff := pingBaseBackoff
	var lastErr error

	for attempt := 1; attempt <= pingAttempts; attempt++ {
		if err := pool.Ping(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if attempt == pingAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("store: connect cancelled: %w", ctx.Err())
		case <-time.After(backoff):
		}
		if backoff = backoff * 2; backoff > pingMaxBackoff {
			backoff = pingMaxBackoff
		}
	}
	return fmt.Errorf("store: database unreachable after %d attempts: %w", pingAttempts, lastErr)
}
