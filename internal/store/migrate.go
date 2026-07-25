package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed all:migrations
var migrationsFS embed.FS

// migrateLockKey is an arbitrary but fixed application ID for the Postgres
// advisory lock that serializes migration runs. Concurrent `kiln admin migrate`
// invocations block on it rather than racing, which is why migrations are never
// run implicitly on server boot.
const migrateLockKey int64 = 0x6b696c6e0001 // "kiln" + 1

// SchemaVersion is the migration version this binary expects. A server whose
// database is at a different version refuses to start rather than operating on
// an unexpected shape.
const SchemaVersion = 1

var migrationName = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.sql$`)

type migration struct {
	version int
	name    string
	body    string
}

// Migrate applies every unapplied migration in order, inside an advisory lock.
// It is idempotent and safe to run concurrently from multiple processes.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (applied []int, err error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquire connection: %w", err)
	}
	defer conn.Release()

	// Hold the lock for the whole run; released implicitly when the session ends.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return nil, fmt.Errorf("store: acquire migration lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, migrateLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("store: release migration lock: %w", unlockErr)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("store: create schema_migrations: %w", err)
	}

	current, err := currentVersion(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return applied, err
		}
		applied = append(applied, m.version)
	}
	return applied, nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin migration %d: %w", m.version, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	if _, err := tx.Exec(ctx, m.body); err != nil {
		return fmt.Errorf("store: apply migration %03d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.version, m.name); err != nil {
		return fmt.Errorf("store: record migration %d: %w", m.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit migration %d: %w", m.version, err)
	}
	return nil
}

// CurrentVersion reports the highest applied migration version, or 0 when the
// database has never been migrated.
func CurrentVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: acquire connection: %w", err)
	}
	defer conn.Release()
	return currentVersion(ctx, conn.Conn())
}

func currentVersion(ctx context.Context, conn *pgx.Conn) (int, error) {
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: check schema_migrations: %w", err)
	}
	if !exists {
		return 0, nil
	}

	var version *int
	if err := conn.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}

// CheckSchemaVersion verifies the database matches what this binary expects.
// Callers should fail startup on error rather than proceeding: running against
// an unexpected schema produces confusing failures much later.
func CheckSchemaVersion(ctx context.Context, pool *pgxpool.Pool) error {
	got, err := CurrentVersion(ctx, pool)
	if err != nil {
		return err
	}
	switch {
	case got == SchemaVersion:
		return nil
	case got < SchemaVersion:
		return fmt.Errorf("store: database schema is at version %d, this build expects %d; run `kiln admin migrate`", got, SchemaVersion)
	default:
		return fmt.Errorf("store: database schema is at version %d, newer than this build expects (%d); deploy a matching kiln version", got, SchemaVersion)
	}
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read embedded migrations: %w", err)
	}

	var out []migration
	seen := map[int]string{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("store: migration %q does not match NNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration %q has an unparseable version: %w", e.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: duplicate migration version %d (%s and %s)", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: m[2], body: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}
