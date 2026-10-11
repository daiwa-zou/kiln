package store

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	poolCfg, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}

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

// maxConnLifetimeJitter spreads connection expiry. Without it every connection
// a pod opened at startup expires in the same instant an hour later, and the
// pod reconnects -- and, behind a pooler, re-authenticates -- all at once.
const maxConnLifetimeJitter = 10 * time.Minute

// queryExecModes maps database.query_exec_mode to pgx. It mirrors
// config.ValidQueryExecModes; a value outside it is refused here too, so a
// caller that built its config without Validate cannot get a mode a pooler
// breaks by accident.
var queryExecModes = map[string]pgx.QueryExecMode{
	"cache_statement": pgx.QueryExecModeCacheStatement,
	"cache_describe":  pgx.QueryExecModeCacheDescribe,
	"exec":            pgx.QueryExecModeExec,
}

// poolConfig turns the configuration into a pgx pool config without dialing.
func poolConfig(cfg *config.Config) (*pgxpool.Config, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.DSN())
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn (%s): %w", cfg.Database.Redacted(), err)
	}

	// Empty leaves whatever the DSN chose (default_query_exec_mode), or pgx's
	// own default when it chose nothing.
	if m := cfg.Database.QueryExecMode; m != "" {
		mode, ok := queryExecModes[m]
		if !ok {
			return nil, fmt.Errorf("store: unknown database query_exec_mode %q", m)
		}
		poolCfg.ConnConfig.DefaultQueryExecMode = mode
	}
	// The DSN can choose a mode too (default_query_exec_mode), and the same
	// modes are refused from there: Validate only sees the knob.
	if !slices.Contains(slices.Collect(maps.Values(queryExecModes)), poolCfg.ConnConfig.DefaultQueryExecMode) {
		return nil, fmt.Errorf("store: query_exec_mode %q from the database URL is not allowed (want one of %s)",
			poolCfg.ConnConfig.DefaultQueryExecMode, strings.Join(config.ValidQueryExecModes, ", "))
	}
	if poolCfg.MaxConnLifetimeJitter == 0 {
		poolCfg.MaxConnLifetimeJitter = maxConnLifetimeJitter
	}

	// Only values the caller actually set override pgx's own defaults. Every
	// one of these has a default in config/defaults.go, so a configured
	// process always sets them -- but a zero here is not "use the default", it
	// is a value pgx takes literally: MaxConns of 0 is rejected outright, and
	// a MaxConnLifetime of 0 expires every connection the moment it is made.
	// Copying unconditionally meant any caller holding a config it built
	// itself got a pool that could not serve a single query.
	if cfg.Database.MaxConns > 0 {
		poolCfg.MaxConns = cfg.Database.MaxConns
	}
	if cfg.Database.MinConns > 0 {
		poolCfg.MinConns = cfg.Database.MinConns
	}
	if cfg.Database.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.Database.MaxConnLifetime
	}
	if cfg.Database.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.Database.MaxConnIdleTime
	}
	return poolCfg, nil
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

// pingAttemptTimeout caps one attempt. Behind a pooler with Postgres down, a
// ping is accepted by the pooler and then queues for a server connection for
// the pooler's whole query_wait_timeout; without a cap, eight attempts took
// minutes to report an outage. A variable only so tests can shorten it.
var pingAttemptTimeout = 5 * time.Second

func pingOnce(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, pingAttemptTimeout)
	defer cancel()
	return pool.Ping(ctx)
}

func pingWithBackoff(ctx context.Context, pool *pgxpool.Pool) error {
	backoff := pingBaseBackoff
	var lastErr error
	// Named in every log line and error: behind a pooler, "refused" from the
	// pooler's address and "timed out" after reaching it are different
	// outages (pooler down versus Postgres down), and a bare "context
	// deadline exceeded" says neither.
	cc := pool.Config().ConnConfig
	target := net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port)))

	for attempt := 1; attempt <= pingAttempts; attempt++ {
		err := pingOnce(ctx, pool)
		if err == nil {
			return nil
		}
		lastErr = err
		slog.Default().Warn("database ping failed",
			"target", target, "attempt", attempt, "of", pingAttempts, "err", err)

		if attempt == pingAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("store: connect to %s cancelled: %w", target, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff = backoff * 2; backoff > pingMaxBackoff {
			backoff = pingMaxBackoff
		}
	}
	return fmt.Errorf("store: database at %s unreachable after %d attempts: %w", target, pingAttempts, lastErr)
}
