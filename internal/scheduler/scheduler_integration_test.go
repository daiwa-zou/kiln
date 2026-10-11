package scheduler

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

// testStore connects to KILN_TEST_DATABASE_URL and migrates a fresh schema.
// Duplicated from the store package's helpers, which do not cross package
// boundaries.
func testStore(t *testing.T) *store.WikiStore {
	t.Helper()
	dsn := os.Getenv("KILN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KILN_TEST_DATABASE_URL not set; skipping scheduler integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.NewWikiStore(pool)
}

// C9: the package's concurrency contract, against the real queries. Two
// schedulers ticking at once -- a rollout's overlap, or workers left running
// duties beside a scheduler -- requeue a stale run once, enqueue a due poll
// connector once, and sweep without error.
func TestTickIsSafeConcurrently(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	// A run claimed by a worker that died two hours ago.
	staleWS, err := js.EnsureWorkspace(ctx, "acme", "stale", "Stale")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := js.EnqueueRun(ctx, staleWS, "manual", ""); err != nil {
		t.Fatal(err)
	}
	if run, err := js.ClaimNextRun(ctx, "dead-pod-1"); err != nil || run == nil {
		t.Fatalf("claim: %v %v", run, err)
	}
	if _, err := js.Pool().Exec(ctx,
		`UPDATE runs SET claimed_at = now() - interval '2 hours' WHERE workspace_id = $1`, staleWS); err != nil {
		t.Fatal(err)
	}

	// A poll connector that has never synced, so it is due.
	pollWS, err := js.EnsureWorkspace(ctx, "acme", "polled", "Polled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateConnector(ctx, store.ConnectorRow{
		WorkspaceID: pollWS, Kind: "git", Name: "repo", TriggerMode: "poll", Enabled: true,
		Config: map[string]any{"url": "https://example.com/repo.git"},
	}); err != nil {
		t.Fatal(err)
	}

	log, buf := bufferLogger()
	var metrics []*observability.Metrics
	var wg sync.WaitGroup
	for range 2 {
		m := observability.NewMetrics()
		metrics = append(metrics, m)
		s := New(Config{Store: js, Log: log, Metrics: m, StaleAfter: time.Hour,
			SourcePollInterval: 10 * time.Minute, SoftDeleteRetention: time.Hour, RunRetention: time.Hour})
		wg.Add(1)
		go func() { defer wg.Done(); s.Tick(ctx) }()
	}
	wg.Wait()

	if out := buf.String(); strings.Contains(out, "level=ERROR") || strings.Contains(out, "level=WARN msg=\"scheduler duty") {
		t.Errorf("a concurrent tick failed:\n%s", out)
	}
	requeued := 0
	for _, m := range metrics {
		body := scrape(t, m)
		if strings.Contains(body, "kiln_runs_stale_requeued_total 1") {
			requeued++
		}
	}
	if requeued != 1 {
		t.Errorf("%d schedulers report requeueing the stale run, want exactly 1", requeued)
	}
	var status string
	if err := js.Pool().QueryRow(ctx, `SELECT status FROM runs WHERE workspace_id = $1`, staleWS).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Errorf("stale run status = %q, want queued", status)
	}
	var polled int
	if err := js.Pool().QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE workspace_id = $1 AND trigger = 'poll'`, pollWS).Scan(&polled); err != nil {
		t.Fatal(err)
	}
	if polled != 1 {
		t.Errorf("poll runs = %d, want exactly 1 from two concurrent schedulers", polled)
	}
}
