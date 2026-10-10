package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/jobs"
)

// requestBench is a workspace with two connectors, which is the smallest bench
// on which "the build is pinned to a different connector" can happen.
type requestBench struct {
	pool       *pgxpool.Pool
	s          *WikiStore
	ws         string
	connA      string
	connB      string
	reviewItem string
}

func newRequestBench(t *testing.T) requestBench {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) string {
		id, err := s.CreateConnector(ctx, ConnectorRow{
			WorkspaceID: ws, Kind: "git", Name: name,
			Config: map[string]any{"path": "/srv/" + name}, Enabled: true, TriggerMode: "webhook",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if err := s.FileReview(ctx, ws, "gap", "retry-policy is missing", "two pages link to it"); err != nil {
		t.Fatal(err)
	}
	var item string
	if err := pool.QueryRow(ctx, `SELECT id FROM review_items WHERE workspace_id = $1`, ws).Scan(&item); err != nil {
		t.Fatal(err)
	}
	return requestBench{pool: pool, s: s, ws: ws, connA: mk("repo-a"), connB: mk("repo-b"), reviewItem: item}
}

// finish records a claimed run as succeeded, which frees the workspace's slot.
func (b requestBench) finish(t *testing.T, runID, trigger string) {
	t.Helper()
	if err := b.s.RecordRun(context.Background(), jobs.RunSummary{
		RunID: runID, WorkspaceID: b.ws, Trigger: trigger, Status: jobs.StatusSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
}

func (b requestBench) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := b.pool.QueryRow(context.Background(), query, b.ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWebhookDuringQueuedResearchStillBecomesABuild(t *testing.T) {
	b := newRequestBench(t)
	ctx := context.Background()

	researchID, err := b.s.RequestResearch(ctx, b.ws, b.reviewItem)
	if err != nil {
		t.Fatal(err)
	}

	// Before: this was reported as created:false against the research run, which
	// then built nothing. The caller must now be told the truth about it.
	res, err := b.s.EnqueueBuild(ctx, b.ws, "webhook", b.connA, EnqueueOptions{RefTo: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != EnqueueDeferred || res.Created() || res.RunID != researchID || res.ActiveTrigger != "research" {
		t.Fatalf("enqueue behind research = %+v, want deferred behind %s", res, researchID)
	}
	// The research run was not touched: it is not a build and took no ref.
	var refTo *string
	if err := b.pool.QueryRow(ctx, `SELECT ref_to FROM runs WHERE id = $1`, researchID).Scan(&refTo); err != nil {
		t.Fatal(err)
	}
	if refTo != nil {
		t.Errorf("the push's head landed on the research run: %q", *refTo)
	}

	// The worker takes the research run and nothing else: the build is not
	// claimable while the bench is busy.
	claimed, err := b.s.ClaimNextRun(ctx, "w1")
	if err != nil || claimed == nil || claimed.ID != researchID || claimed.Trigger != "research" {
		t.Fatalf("first claim = %+v err=%v, want the research run", claimed, err)
	}
	if again, err := b.s.ClaimNextRun(ctx, "w2"); err != nil || again != nil {
		t.Fatalf("a second claim during research = %+v err=%v, want nothing", again, err)
	}
	if n := b.count(t, `SELECT count(*) FROM build_requests WHERE workspace_id = $1`); n != 1 {
		t.Fatalf("pending requests during research = %d, want 1", n)
	}

	// Research done: the request becomes a build, with what the push carried.
	b.finish(t, researchID, "research")
	build, err := b.s.ClaimNextRun(ctx, "w2")
	if err != nil || build == nil {
		t.Fatalf("no build followed the research run: %+v err=%v", build, err)
	}
	if build.Trigger != "webhook" || build.ConnectorID != b.connA || build.RefTo != "abc123" {
		t.Errorf("follow-up build = %+v, want the webhook push for connector A at abc123", build)
	}
	if n := b.count(t, `SELECT count(*) FROM build_requests WHERE workspace_id = $1`); n != 0 {
		t.Errorf("request still pending after it became a run: %d", n)
	}
}

func TestBuildRequestWhileABuildIsRunningIsNotLost(t *testing.T) {
	b := newRequestBench(t)
	ctx := context.Background()

	first, err := b.s.EnqueueBuild(ctx, b.ws, "manual", "", EnqueueOptions{})
	if err != nil || !first.Created() {
		t.Fatalf("first enqueue = %+v err=%v", first, err)
	}
	running, err := b.s.ClaimNextRun(ctx, "w1")
	if err != nil || running == nil {
		t.Fatal(err)
	}

	// The running build planned its work from what it read at the start, so a
	// push that lands now is not in it.
	res, err := b.s.EnqueueBuild(ctx, b.ws, "webhook", b.connA, EnqueueOptions{RefTo: "def456"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != EnqueueDeferred || res.RunID != running.ID || res.ActiveTrigger != "manual" {
		t.Fatalf("enqueue behind a running build = %+v", res)
	}
	var refTo *string
	if err := b.pool.QueryRow(ctx, `SELECT ref_to FROM runs WHERE id = $1`, running.ID).Scan(&refTo); err != nil {
		t.Fatal(err)
	}
	if refTo != nil {
		t.Errorf("a running build was handed a new target: %q", *refTo)
	}
	if next, err := b.s.ClaimNextRun(ctx, "w2"); err != nil || next != nil {
		t.Fatalf("claimed while the first build was running: %+v err=%v", next, err)
	}

	b.finish(t, running.ID, "manual")
	next, err := b.s.ClaimNextRun(ctx, "w2")
	if err != nil || next == nil || next.Trigger != "webhook" || next.RefTo != "def456" {
		t.Fatalf("follow-up after the running build = %+v err=%v", next, err)
	}
}

func TestTwoWebhooksMergeIntoOneQueuedBuild(t *testing.T) {
	b := newRequestBench(t)
	ctx := context.Background()

	first, err := b.s.EnqueueBuild(ctx, b.ws, "webhook", b.connA, EnqueueOptions{RefTo: "aaa"})
	if err != nil || !first.Created() {
		t.Fatalf("first webhook = %+v err=%v", first, err)
	}
	second, err := b.s.EnqueueBuild(ctx, b.ws, "webhook", b.connA, EnqueueOptions{RefTo: "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	// The response names what absorbed the request.
	if second.Outcome != EnqueueMerged || second.RunID != first.RunID || second.ActiveTrigger != "webhook" || second.Created() {
		t.Fatalf("second webhook = %+v, want merged into %s", second, first.RunID)
	}

	if n := b.count(t, `SELECT count(*) FROM runs WHERE workspace_id = $1`); n != 1 {
		t.Errorf("runs = %d, want the two webhooks collapsed into 1", n)
	}
	if n := b.count(t, `SELECT count(*) FROM build_requests WHERE workspace_id = $1`); n != 0 {
		t.Errorf("a merge left a pending request behind: %d", n)
	}
	run, err := b.s.ClaimNextRun(ctx, "w1")
	if err != nil || run == nil {
		t.Fatal(err)
	}
	if run.RefTo != "bbb" || run.ConnectorID != b.connA {
		t.Errorf("merged build = %+v, want the newest head, still pinned to A", run)
	}
}

func TestPinnedBuildWidensWhenADifferentConnectorsRequestMerges(t *testing.T) {
	b := newRequestBench(t)
	ctx := context.Background()

	first, err := b.s.EnqueueBuild(ctx, b.ws, "webhook", b.connA, EnqueueOptions{RefTo: "aaa"})
	if err != nil || !first.Created() {
		t.Fatal(err)
	}
	pinned := func() any {
		var c *string
		if err := b.pool.QueryRow(ctx, `SELECT connector_id FROM runs WHERE id = $1`, first.RunID).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c == nil {
			return nil
		}
		return *c
	}
	if pinned() != b.connA {
		t.Fatalf("setup: build pinned to %v, want connector A", pinned())
	}

	// Pinned to A, then B pushes: a build for A alone would leave B's change
	// out of the one build that was supposed to include it.
	res, err := b.s.EnqueueBuild(ctx, b.ws, "webhook", b.connB, EnqueueOptions{RefTo: "bbb"})
	if err != nil || res.Outcome != EnqueueMerged || res.RunID != first.RunID {
		t.Fatalf("merge from B = %+v err=%v", res, err)
	}
	if got := pinned(); got != nil {
		t.Errorf("build still pinned to %v after B merged; want every connector", got)
	}

	// Widening is one-way: A pushing again does not re-pin it.
	if _, err := b.s.EnqueueBuild(ctx, b.ws, "webhook", b.connA, EnqueueOptions{RefTo: "ccc"}); err != nil {
		t.Fatal(err)
	}
	if got := pinned(); got != nil {
		t.Errorf("a same-connector request re-pinned a widened build to %v", got)
	}

	// An unpinned request (a manual Build click) widens a pinned build too.
	other := t.Name() // second workspace so the pinned build above is untouched
	ws2, err := b.s.EnsureWorkspace(ctx, "local", "other", other)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := b.s.CreateConnector(ctx, ConnectorRow{WorkspaceID: ws2, Kind: "git", Name: "x",
		Config: map[string]any{"path": "/srv/x"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	run2, err := b.s.EnqueueBuild(ctx, ws2, "poll", c2, EnqueueOptions{})
	if err != nil || !run2.Created() {
		t.Fatal(err)
	}
	if _, err := b.s.EnqueueBuild(ctx, ws2, "manual", "", EnqueueOptions{}); err != nil {
		t.Fatal(err)
	}
	var c *string
	if err := b.pool.QueryRow(ctx, `SELECT connector_id FROM runs WHERE id = $1`, run2.RunID).Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Errorf("manual build click left the poll build pinned to %q", *c)
	}
}

func TestDeferredRequestsMergeAndWidenLikeQueuedBuilds(t *testing.T) {
	b := newRequestBench(t)
	ctx := context.Background()

	researchID, err := b.s.RequestResearch(ctx, b.ws, b.reviewItem)
	if err != nil {
		t.Fatal(err)
	}
	soon, later := time.Now().Add(time.Minute), time.Now().Add(time.Hour)
	if _, err := b.s.EnqueueBuild(ctx, b.ws, "webhook", b.connA,
		EnqueueOptions{RefTo: "aaa", NotBefore: later}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.EnqueueBuild(ctx, b.ws, "upload", b.connB,
		EnqueueOptions{RefTo: "bbb", NotBefore: soon}); err != nil {
		t.Fatal(err)
	}
	// A request carrying no head must not erase the one already recorded.
	if _, err := b.s.EnqueueBuild(ctx, b.ws, "poll", b.connB, EnqueueOptions{}); err != nil {
		t.Fatal(err)
	}

	if n := b.count(t, `SELECT count(*) FROM build_requests WHERE workspace_id = $1`); n != 1 {
		t.Fatalf("pending requests = %d, want the three merged into 1", n)
	}
	claimed, err := b.s.ClaimNextRun(ctx, "w1")
	if err != nil || claimed == nil || claimed.ID != researchID {
		t.Fatalf("claim = %+v err=%v", claimed, err)
	}
	b.finish(t, researchID, "research")

	// Make the earliest start time claimable without waiting for it.
	if _, err := b.pool.Exec(ctx, `UPDATE build_requests SET not_before = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	build, err := b.s.ClaimNextRun(ctx, "w1")
	if err != nil || build == nil {
		t.Fatalf("no follow-up build: %+v err=%v", build, err)
	}
	if build.Trigger != "webhook" {
		t.Errorf("trigger = %q, want the first request's", build.Trigger)
	}
	if build.ConnectorID != "" {
		t.Errorf("build pinned to %q; requests for two connectors must widen to all", build.ConnectorID)
	}
	if build.RefTo != "bbb" {
		t.Errorf("ref_to = %q, want the newest head that carried one", build.RefTo)
	}
}

func TestDeferredRequestKeepsTheEarliestStartTime(t *testing.T) {
	b := newRequestBench(t)
	ctx := context.Background()
	if _, err := b.s.RequestResearch(ctx, b.ws, b.reviewItem); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if _, err := b.s.EnqueueBuild(ctx, b.ws, "upload", b.connA, EnqueueOptions{NotBefore: later}); err != nil {
		t.Fatal(err)
	}
	// An immediate request (no debounce) must not wait out the other's.
	if _, err := b.s.EnqueueBuild(ctx, b.ws, "manual", "", EnqueueOptions{}); err != nil {
		t.Fatal(err)
	}
	var nb *time.Time
	if err := b.pool.QueryRow(ctx, `SELECT not_before FROM build_requests WHERE workspace_id = $1`, b.ws).Scan(&nb); err != nil {
		t.Fatal(err)
	}
	if nb != nil {
		t.Errorf("not_before = %v; an immediate request must clear the debounce", nb)
	}
}
