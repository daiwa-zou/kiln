package store

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/config"
)

// Opening the pool is the first thing every process does and the last thing
// anyone tests. It has one piece of real behavior -- retrying the first
// connection, because compose starts Postgres and kiln together and a
// container that gives up two seconds in is a bad first impression -- and that
// retry loop had never run under test.

func TestOpenRejectsADSNItCannotParse(t *testing.T) {
	// Hermetic: the failure is in parsing, before any dialing.
	_, err := Open(context.Background(), &config.Config{
		Database: config.Database{URL: "postgres://user:pass@host:notaport/db"},
	})
	if err == nil {
		t.Fatal("Open on an unparseable DSN returned no error")
	}
	// The message has to name the DSN to be actionable, and must not carry the
	// password to be safe to log.
	if !strings.Contains(err.Error(), "parse dsn") {
		t.Errorf("error = %v, want it to say which stage failed", err)
	}
	if strings.Contains(err.Error(), "pass") {
		t.Errorf("error leaked the password: %v", err)
	}
}

// Behind a transaction-mode pooler the exec mode is what decides whether a
// query that lands on a different server connection still works, so the knob
// has to reach pgx -- and an empty one has to leave pgx alone.
func TestOpenAppliesQueryExecMode(t *testing.T) {
	const dsn = "postgres://kiln@localhost:5432/kiln?sslmode=disable"
	tests := []struct {
		mode    string
		dsn     string
		want    pgx.QueryExecMode
		wantErr bool
	}{
		{mode: "", want: pgx.QueryExecModeCacheStatement},
		{mode: "cache_statement", want: pgx.QueryExecModeCacheStatement},
		{mode: "cache_describe", want: pgx.QueryExecModeCacheDescribe},
		{mode: "exec", want: pgx.QueryExecModeExec},
		// An empty knob defers to the DSN, which is how the pooled CI legs
		// select a mode for tests that build their pool from the URL alone.
		{mode: "", dsn: dsn + "&default_query_exec_mode=exec", want: pgx.QueryExecModeExec},
		// The knob wins over the DSN when both are set.
		{mode: "cache_describe", dsn: dsn + "&default_query_exec_mode=exec", want: pgx.QueryExecModeCacheDescribe},
		// Dropped: Validate refuses these, and a caller that skipped Validate
		// must still not get them silently.
		{mode: "simple_protocol", wantErr: true},
		{mode: "describe_exec", wantErr: true},
		// Nor from the DSN, which Validate never sees.
		{mode: "", dsn: dsn + "&default_query_exec_mode=simple_protocol", wantErr: true},
		{mode: "", dsn: dsn + "&default_query_exec_mode=describe_exec", wantErr: true},
	}
	for _, tt := range tests {
		name := tt.mode
		if tt.dsn != "" {
			name += "+dsn"
		}
		t.Run(name, func(t *testing.T) {
			url := dsn
			if tt.dsn != "" {
				url = tt.dsn
			}
			cfg := &config.Config{Database: config.Database{URL: url, QueryExecMode: tt.mode}}
			pc, err := poolConfig(cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("poolConfig accepted query_exec_mode %q", tt.mode)
				}
				// Open must refuse it too, before it dials anything.
				if _, err := Open(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "query_exec_mode") {
					t.Errorf("Open with query_exec_mode %q = %v, want a query_exec_mode error", tt.mode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("poolConfig: %v", err)
			}
			if got := pc.ConnConfig.DefaultQueryExecMode; got != tt.want {
				t.Errorf("DefaultQueryExecMode = %v, want %v", got, tt.want)
			}
			// Spread reconnects so a pod's connections do not all expire, and
			// re-authenticate through the pooler, in the same instant.
			if pc.MaxConnLifetimeJitter != maxConnLifetimeJitter {
				t.Errorf("MaxConnLifetimeJitter = %v, want %v", pc.MaxConnLifetimeJitter, maxConnLifetimeJitter)
			}
		})
	}
}

// Behind a pooler with Postgres down, a ping is accepted and then queues for a
// server connection until the pooler gives up. One attempt must not inherit
// that wait, or Open takes minutes to report an outage. A listener that accepts
// and never answers stands in for the pooler.
func TestPingAttemptIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	pc, err := pgxpool.ParseConfig("postgres://kiln@" + ln.Addr().String() + "/kiln?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	prev := pingAttemptTimeout
	pingAttemptTimeout = 200 * time.Millisecond
	t.Cleanup(func() { pingAttemptTimeout = prev })

	done := make(chan error, 1)
	go func() { done <- pingOnce(ctx, pool) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ping against a silent server succeeded")
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("one ping attempt against a silent server did not give up on its own")
	}
}

func TestOpenGivesUpWhenTheCallerDoes(t *testing.T) {
	// A pool pointed at nothing: the retry loop would spend the better part of
	// twenty seconds before reporting. Cancellation has to cut it short, or a
	// process asked to stop during startup hangs past its shutdown deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	start := time.Now()
	_, err := Open(ctx, &config.Config{
		Database: config.Database{
			// Port 1 refuses immediately rather than hanging on a filtered
			// port, so the test measures the loop and not the dial.
			URL: "postgres://kiln:kiln@127.0.0.1:1/kiln?sslmode=disable&connect_timeout=1",
		},
	})
	if err == nil {
		t.Fatal("Open against nothing returned no error")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error = %v, want it to report the cancellation rather than exhausting the retries", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Open took %v after its context was cancelled, want it to return promptly", elapsed)
	}
	// On-call needs to know which address failed, and how, at every attempt:
	// behind a pooler that is the difference between pooler down and
	// Postgres down.
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error = %v, want it to name the target address", err)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "target=127.0.0.1:1") ||
		!strings.Contains(got, "attempt=1") || !strings.Contains(got, "err=") {
		t.Errorf("failed attempt not logged at Warn with target, attempt and error; log = %q", got)
	}
}

func TestOpenPingAndCloseAgainstARealDatabase(t *testing.T) {
	dsn := os.Getenv("KILN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KILN_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}

	db, err := Open(context.Background(), &config.Config{
		Database: config.Database{URL: dsn, MaxConns: 4, MinConns: 1},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Ping(context.Background()); err != nil {
		t.Errorf("Ping on a freshly opened pool: %v", err)
	}
	// The pool config the caller asked for has to reach pgx, or the connection
	// limits in config.toml are decoration.
	if got := db.Pool.Config().MaxConns; got != 4 {
		t.Errorf("MaxConns = %d, want the configured 4", got)
	}

	db.Close()
	// Closing twice, and closing a nil DB, are both things shutdown paths do
	// when an earlier step already failed.
	db.Close()
	var nilDB *DB
	nilDB.Close()
	(&DB{}).Close()
}

func TestPingWithBackoffSucceedsOnTheFirstTry(t *testing.T) {
	dsn := os.Getenv("KILN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KILN_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	start := time.Now()
	if err := pingWithBackoff(context.Background(), pool); err != nil {
		t.Fatalf("pingWithBackoff against a live database: %v", err)
	}
	// No backoff is paid when the first ping lands: a healthy start must not
	// cost a quarter second per process.
	if elapsed := time.Since(start); elapsed > pingBaseBackoff {
		t.Errorf("pingWithBackoff took %v on a reachable database, want no backoff at all", elapsed)
	}
}
