package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"time"

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

// SchemaVersion is the highest migration version this binary embeds, derived
// from the files in migrations/ so that adding a migration is one new file and
// no edit to a shared constant (which conflicted in every migration PR).
//
// It is informational: it is what /version reports and what `admin doctor`
// prints. Whether a database is acceptable is decided by comparing the full set
// of applied versions to the embedded set (see CheckSchemaVersion), not by this
// number alone, because a database can sit at the right maximum and still be
// missing a lower migration.
//
// It is a variable only because Go cannot evaluate an embed.FS in a constant;
// treat it as read-only. The scan here is deliberately lenient (it skips names
// it cannot parse rather than panicking at init): a malformed name is reported
// by loadMigrations, which Migrate and CheckSchemaVersion both run.
var SchemaVersion = highestEmbeddedVersion()

func highestEmbeddedVersion() int {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0
	}
	highest := 0
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, err := strconv.Atoi(m[1]); err == nil && v > highest {
			highest = v
		}
	}
	return highest
}

var migrationName = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.sql$`)

type migration struct {
	version int
	name    string
	body    string
}

// Migrate applies every embedded migration that is not recorded in
// schema_migrations, in ascending version order, inside an advisory lock. It is
// idempotent and safe to run concurrently from multiple processes.
//
// "Not recorded" is decided per version, not against the highest applied
// version. Two branches can add migrations and land in either order, or a dev
// server can migrate between merges; a migration numbered below one already
// applied is still owed, and is applied (and reported) here.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (applied []int, err error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	return migrateWith(ctx, pool, migrations)
}

// migrateWith is Migrate over an explicit, version-sorted migration set, so
// tests can exercise ordering without editing the embedded files.
func migrateWith(ctx context.Context, pool *pgxpool.Pool, migrations []migration) (applied []int, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquire connection: %w", err)
	}
	defer conn.Release()

	// Hold the lock for the whole run; released explicitly below, or with the
	// session. Bounded acquisition so an operator waiting behind a hung
	// migration gets a diagnosis instead of an indefinite silent block.
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := conn.Exec(lockCtx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		if lockCtx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("store: migration lock not acquired within 5m; another migration may be hung: %w", err)
		}
		return nil, fmt.Errorf("store: acquire migration lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, migrateLockKey); unlockErr != nil {
			// The session still holds the lock. Close the underlying
			// connection so Release destroys it instead of returning it to
			// the pool, where the lock would silently outlive this call and
			// wedge every later migration.
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
			if err == nil {
				err = fmt.Errorf("store: release migration lock: %w", unlockErr)
			}
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

	have, err := appliedVersions(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}
	current := 0
	for v := range have {
		current = max(current, v)
	}

	for _, m := range migrations {
		if have[m.version] {
			continue
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return applied, err
		}
		if m.version < current {
			// Visible in the log as well as the returned list: a back-filled
			// migration means two branches landed out of order, and the operator
			// should know the schema gained something below its old maximum.
			slog.Default().Warn("applied migration below the database's highest applied version",
				"version", m.version, "name", m.name, "highest_applied", current)
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
// database has never been migrated. It says nothing about gaps below that
// version; use CheckSchemaVersion to judge whether the schema is complete.
func CurrentVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	versions, err := AppliedVersions(ctx, pool)
	if err != nil {
		return 0, err
	}
	if len(versions) == 0 {
		return 0, nil
	}
	return versions[len(versions)-1], nil
}

// AppliedVersions returns every version recorded in schema_migrations in
// ascending order, or nil when the database has never been migrated.
func AppliedVersions(ctx context.Context, pool *pgxpool.Pool) ([]int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquire connection: %w", err)
	}
	defer conn.Release()
	set, err := appliedVersions(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}
	return sortedVersions(set), nil
}

func sortedVersions(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// appliedVersions reads the set of recorded versions. A database that has no
// schema_migrations table yet has applied nothing.
func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]bool, error) {
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("store: check schema_migrations: %w", err)
	}
	set := map[int]bool{}
	if !exists {
		return set, nil
	}

	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read applied versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: read applied versions: %w", err)
		}
		set[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read applied versions: %w", err)
	}
	return set, nil
}

// CheckSchemaVersion verifies the database holds exactly the migrations this
// binary embeds. Callers should fail startup on error rather than proceeding:
// running against an unexpected schema produces confusing failures much later.
func CheckSchemaVersion(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := AppliedVersions(ctx, pool)
	if err != nil {
		return err
	}
	return checkSchema(applied, migrations)
}

// checkSchema compares the applied versions with the embedded set.
//
// Unknown versions (applied but not embedded) are checked rather than only the
// maximum. Every version above the embedded maximum is unknown, so this covers
// the old "newer than this build" case; it additionally catches a database that
// holds a lower version this build lacks, which a max comparison would wave
// through even though that migration changed the schema under this binary. It
// is checked before missing versions: if the database is from a different
// build, `kiln admin migrate` would not make this build acceptable.
func checkSchema(applied []int, migrations []migration) error {
	embedded := make(map[int]bool, len(migrations))
	for _, m := range migrations {
		embedded[m.version] = true
	}
	have := make(map[int]bool, len(applied))
	var unknown []int
	for _, v := range applied {
		have[v] = true
		if !embedded[v] {
			unknown = append(unknown, v)
		}
	}
	var missing []int
	for _, m := range migrations {
		if !have[m.version] {
			missing = append(missing, m.version)
		}
	}

	highest := 0
	if len(applied) > 0 {
		highest = applied[len(applied)-1]
	}
	expected := 0
	if len(migrations) > 0 {
		expected = migrations[len(migrations)-1].version
	}

	switch {
	case len(unknown) > 0:
		return fmt.Errorf("store: database schema is at version %d and has migration(s) %v this build does not know (it expects up to %d); it is newer than this build, deploy a matching kiln version",
			highest, unknown, expected)
	case len(missing) > 0:
		return fmt.Errorf("store: database schema is at version %d but is missing migration(s) %v (this build expects %d); run `kiln admin migrate`",
			highest, missing, expected)
	}
	return nil
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
