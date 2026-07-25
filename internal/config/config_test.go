package config

import (
	"strings"
	"testing"
)

func TestDatabaseDSN(t *testing.T) {
	tests := []struct {
		name string
		db   Database
		want string
	}{
		{
			name: "explicit url wins over discrete fields",
			db: Database{
				URL:  "postgres://someone:pw@elsewhere:5433/other?sslmode=disable",
				Host: "ignored", Port: 1, Name: "ignored", User: "ignored",
			},
			want: "postgres://someone:pw@elsewhere:5433/other?sslmode=disable",
		},
		{
			name: "assembled with password",
			db:   Database{Host: "postgres", Port: 5432, Name: "kiln", User: "kiln", Password: "s3cret", SSLMode: "require"},
			want: "postgres://kiln:s3cret@postgres:5432/kiln?sslmode=require",
		},
		{
			name: "assembled without password",
			db:   Database{Host: "postgres", Port: 5432, Name: "kiln", User: "kiln", SSLMode: "require"},
			want: "postgres://kiln@postgres:5432/kiln?sslmode=require",
		},
		{
			name: "root cert included",
			db: Database{
				Host: "db.example.com", Port: 5432, Name: "kiln", User: "kiln",
				SSLMode: "verify-full", SSLRootCert: "/etc/ssl/rds.pem",
			},
			want: "postgres://kiln@db.example.com:5432/kiln?sslmode=verify-full&sslrootcert=%2Fetc%2Fssl%2Frds.pem",
		},
		{
			name: "password with url-unsafe characters is escaped",
			db:   Database{Host: "postgres", Port: 5432, Name: "kiln", User: "kiln", Password: "p@ss/w:rd?", SSLMode: "disable"},
			want: "postgres://kiln:p%40ss%2Fw%3Ard%3F@postgres:5432/kiln?sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.db.DSN(); got != tt.want {
				t.Errorf("DSN() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestDatabaseRedacted(t *testing.T) {
	db := Database{Host: "postgres", Port: 5432, Name: "kiln", User: "kiln", Password: "hunter2", SSLMode: "require"}

	got := db.Redacted()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("Redacted() leaked the password: %q", got)
	}
	if !strings.Contains(got, "kiln:xxxxx@") {
		t.Errorf("Redacted() = %q, want the password replaced", got)
	}
}

func TestDatabaseRedactedNoPassword(t *testing.T) {
	db := Database{Host: "postgres", Port: 5432, Name: "kiln", User: "kiln", SSLMode: "require"}

	// A DSN with no password should be unchanged, not gain a placeholder.
	if got, want := db.Redacted(), db.DSN(); got != want {
		t.Errorf("Redacted() = %q, want %q", got, want)
	}
}

func TestDefaultMaxConnsByRole(t *testing.T) {
	// The worker holds long-lived import transactions; sizing it like the
	// server would exhaust Postgres connections as replicas scale.
	if got := defaultMaxConns(RoleWorker); got != defaultWorkerMaxConns {
		t.Errorf("worker max_conns = %d, want %d", got, defaultWorkerMaxConns)
	}
	if got := defaultMaxConns(RoleServer); got != defaultServerMaxConns {
		t.Errorf("server max_conns = %d, want %d", got, defaultServerMaxConns)
	}
	if defaultWorkerMaxConns >= defaultServerMaxConns {
		t.Errorf("worker pool (%d) should be smaller than server pool (%d)",
			defaultWorkerMaxConns, defaultServerMaxConns)
	}
}
