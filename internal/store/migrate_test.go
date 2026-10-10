package store

import (
	"strings"
	"testing"
)

func TestLoadMigrations(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations were embedded; check the go:embed directive")
	}

	for i, m := range migrations {
		if m.version <= 0 {
			t.Errorf("migration %d has non-positive version %d", i, m.version)
		}
		if m.name == "" {
			t.Errorf("migration %d has an empty name", m.version)
		}
		if strings.TrimSpace(m.body) == "" {
			t.Errorf("migration %d (%s) has an empty body", m.version, m.name)
		}
		if i > 0 && migrations[i-1].version >= m.version {
			t.Errorf("migrations out of order: %d follows %d", m.version, migrations[i-1].version)
		}
	}
}

// SchemaVersion is derived from the embedded filenames by a lenient scan, while
// Migrate and CheckSchemaVersion use the strict loader. This pins the two
// together so the exported number can never describe a different set than the
// one the runner applies.
func TestSchemaVersionIsHighestEmbeddedMigration(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	highest := migrations[len(migrations)-1].version
	if highest != SchemaVersion {
		t.Errorf("highest migration is %d but SchemaVersion is %d; SchemaVersion must be derived from the embedded files, not edited by hand",
			highest, SchemaVersion)
	}
}

func TestCheckSchema(t *testing.T) {
	set := func(versions ...int) []migration {
		out := make([]migration, len(versions))
		for i, v := range versions {
			out[i] = migration{version: v, name: "m"}
		}
		return out
	}

	tests := []struct {
		name       string
		applied    []int
		embedded   []migration
		wantErr    string // substring; empty means no error
		notContain string
	}{
		{name: "exact match", applied: []int{1, 2, 3}, embedded: set(1, 2, 3)},
		{name: "empty database", applied: nil, embedded: set(1, 2), wantErr: "run `kiln admin migrate`"},
		{name: "behind at the top", applied: []int{1, 2}, embedded: set(1, 2, 3), wantErr: "missing migration(s) [3]"},
		{name: "gap below the maximum", applied: []int{1, 3}, embedded: set(1, 2, 3), wantErr: "missing migration(s) [2]"},
		{name: "ahead of this build", applied: []int{1, 2, 3, 4}, embedded: set(1, 2, 3), wantErr: "newer than this build"},
		{name: "unknown version below the embedded maximum", applied: []int{1, 2, 3}, embedded: set(1, 3, 4), wantErr: "migration(s) [2] this build does not know"},
		{
			// Running `kiln admin migrate` cannot make this build acceptable,
			// so the "deploy a matching version" diagnosis must win.
			name: "unknown and missing", applied: []int{1, 5}, embedded: set(1, 2),
			wantErr: "newer than this build", notContain: "run `kiln admin migrate`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSchema(tt.applied, tt.embedded)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkSchema = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkSchema = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
			if tt.notContain != "" && strings.Contains(err.Error(), tt.notContain) {
				t.Errorf("error %q should not contain %q", err, tt.notContain)
			}
		})
	}
}

func TestMigrationsHaveNoExplicitTransaction(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	// The runner wraps each migration in a transaction; a nested BEGIN/COMMIT
	// in the file would commit early and break rollback on failure.
	for _, m := range migrations {
		for _, line := range strings.Split(m.body, "\n") {
			trimmed := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(trimmed, "--") {
				continue
			}
			if trimmed == "BEGIN;" || trimmed == "COMMIT;" {
				t.Errorf("migration %03d_%s contains an explicit %s; the runner manages the transaction",
					m.version, m.name, trimmed)
			}
		}
	}
}

func TestMigrationNamePattern(t *testing.T) {
	tests := []struct {
		filename string
		ok       bool
	}{
		{"001_initial.sql", true},
		{"012_add_tokens.sql", true},
		{"1_x.sql", true},
		{"initial.sql", false},
		{"001-initial.sql", false},
		{"001_Initial.sql", false},
		{"001_initial.txt", false},
		{"001_initial.sql.bak", false},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := migrationName.MatchString(tt.filename)
			if got != tt.ok {
				t.Errorf("MatchString(%q) = %v, want %v", tt.filename, got, tt.ok)
			}
		})
	}
}
