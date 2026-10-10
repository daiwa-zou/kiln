package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

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

func TestOpenGivesUpWhenTheCallerDoes(t *testing.T) {
	// A pool pointed at nothing: the retry loop would spend the better part of
	// twenty seconds before reporting. Cancellation has to cut it short, or a
	// process asked to stop during startup hangs past its shutdown deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

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
