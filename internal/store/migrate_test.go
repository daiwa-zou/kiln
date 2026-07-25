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

func TestLoadMigrationsMatchesSchemaVersion(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	highest := migrations[len(migrations)-1].version
	// Forgetting to bump SchemaVersion after adding a migration would let a
	// server boot against a schema it does not expect.
	if highest != SchemaVersion {
		t.Errorf("highest migration is %d but SchemaVersion is %d; bump SchemaVersion when adding a migration",
			highest, SchemaVersion)
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
