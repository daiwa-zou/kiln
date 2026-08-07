package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

func TestSteeringWritePathReachesThePrompt(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	// Upsert twice: the second write must replace, not duplicate, because the
	// table is keyed on (workspace, kind).
	if err := js.UpsertSteeringDoc(ctx, ws, "purpose", "first draft", ""); err != nil {
		t.Fatalf("UpsertSteeringDoc: %v", err)
	}
	if err := js.UpsertSteeringDoc(ctx, ws, "purpose", "final: document the dispatch path", ""); err != nil {
		t.Fatalf("UpsertSteeringDoc update: %v", err)
	}

	docs, err := js.LoadSteeringDocs(ctx, ws)
	if err != nil {
		t.Fatalf("LoadSteeringDocs: %v", err)
	}
	if docs["purpose"] != "final: document the dispatch path" {
		t.Errorf("purpose = %q, want the updated body", docs["purpose"])
	}
	if _, ok := docs["schema"]; !ok {
		t.Error("schema kind missing from LoadSteeringDocs; the UI form needs a stable shape")
	}

	// The write must reach the same loader the pipeline injects into prompts.
	steering, err := js.LoadSteering(ctx, ws)
	if err != nil {
		t.Fatalf("LoadSteering: %v", err)
	}
	if steering.Purpose != "final: document the dispatch path" {
		t.Errorf("prompt-side purpose = %q; the write API and the prompt loader disagree", steering.Purpose)
	}
}

func TestCorrectionLifecycle(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	p := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{p}}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Address by slug and by path: a wikilink carries one, the tree the other.
	bySlug, err := js.CreateCorrection(ctx, ws, "ripple", "Ripple does not own retries.", "")
	if err != nil {
		t.Fatalf("CreateCorrection by slug: %v", err)
	}
	if _, err := js.CreateCorrection(ctx, ws, "entities/ripple.md", "Beacon owns retries.", ""); err != nil {
		t.Fatalf("CreateCorrection by path: %v", err)
	}
	if _, err := js.CreateCorrection(ctx, ws, "no-such-page", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("correction on a missing page = %v, want ErrNotFound", err)
	}

	steering, err := js.LoadSteering(ctx, ws)
	if err != nil {
		t.Fatalf("LoadSteering: %v", err)
	}
	if got := len(steering.Corrections["ripple"]); got != 2 {
		t.Fatalf("active corrections = %d, want 2", got)
	}

	// Deactivation removes a correction from prompts without erasing the
	// record of what a human once said.
	if err := js.SetCorrectionActive(ctx, ws, bySlug, false); err != nil {
		t.Fatalf("SetCorrectionActive: %v", err)
	}
	steering, err = js.LoadSteering(ctx, ws)
	if err != nil {
		t.Fatalf("LoadSteering after deactivate: %v", err)
	}
	if got := len(steering.Corrections["ripple"]); got != 1 {
		t.Errorf("active corrections after deactivate = %d, want 1", got)
	}
	all, err := js.ListCorrections(ctx, ws, "ripple")
	if err != nil {
		t.Fatalf("ListCorrections: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("listed corrections = %d, want both including the inactive one", len(all))
	}

	if err := js.SetCorrectionActive(ctx, ws, "00000000-0000-0000-0000-000000000000", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("toggling an unknown correction = %v, want ErrNotFound", err)
	}
}

func TestCorrectionScopedToWorkspace(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	p := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{p}}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	id, err := js.CreateCorrection(ctx, ws, "ripple", "a note", "")
	if err != nil {
		t.Fatalf("CreateCorrection: %v", err)
	}

	other, err := js.EnsureWorkspace(ctx, "other-org", "other-ws", "Other")
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	// A caller scoped to another workspace must not be able to toggle it.
	if err := js.SetCorrectionActive(ctx, other, id, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-workspace toggle = %v, want ErrNotFound", err)
	}
}

func TestReviewQueueFromRunSummary(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	run := jobs.RunSummary{
		WorkspaceID: ws, Trigger: "manual", Status: jobs.StatusSucceeded,
		Reviews: []jobs.ReviewNote{
			{Kind: "contradiction", Title: "Two retry owners", Detail: "ripple and beacon both claim it", Unit: diff.Key("module:ripple")},
		},
	}
	if err := js.RecordRun(ctx, run); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	// The same flag on the next run must not duplicate the open question.
	if err := js.RecordRun(ctx, run); err != nil {
		t.Fatalf("RecordRun again: %v", err)
	}

	open, err := js.ListReviews(ctx, ws, "open", 100, 0)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open reviews = %d, want 1 (deduplicated)", len(open))
	}
	if !strings.Contains(open[0].Detail, "module:ripple") {
		t.Errorf("detail %q should name the unit that raised it", open[0].Detail)
	}

	if err := js.ResolveReview(ctx, ws, open[0].ID, "dismiss", ""); err != nil {
		t.Fatalf("ResolveReview: %v", err)
	}
	if err := js.ResolveReview(ctx, ws, open[0].ID, "dismiss", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("second resolve = %v, want ErrNotFound (already resolved)", err)
	}

	// Once resolved, the flag may be raised again by a future run: the
	// dedup window is open items only.
	if err := js.RecordRun(ctx, run); err != nil {
		t.Fatalf("RecordRun after resolve: %v", err)
	}
	open, err = js.ListReviews(ctx, ws, "open", 100, 0)
	if err != nil {
		t.Fatalf("ListReviews after re-raise: %v", err)
	}
	if len(open) != 1 {
		t.Errorf("re-raised reviews = %d, want 1", len(open))
	}
}

func TestDeletionApprovalRoundTrip(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	cands := []jobs.DeletionCandidate{{Key: diff.Key("module:legacy"), Detail: "Approving removes 2 page(s)."}}
	if err := js.EnsureDeletionReviews(ctx, ws, cands); err != nil {
		t.Fatalf("EnsureDeletionReviews: %v", err)
	}
	// A source that stays missing across runs asks its question exactly once.
	if err := js.EnsureDeletionReviews(ctx, ws, cands); err != nil {
		t.Fatalf("EnsureDeletionReviews repeat: %v", err)
	}
	open, err := js.ListReviews(ctx, ws, "open", 100, 0)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(open) != 1 || open[0].Kind != "deletion" {
		t.Fatalf("open deletion reviews = %+v, want exactly one", open)
	}

	// Nothing is approved until a human says so.
	keys, err := js.LoadApprovedDeletions(ctx, ws)
	if err != nil {
		t.Fatalf("LoadApprovedDeletions: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("approved before approval = %v, want none", keys)
	}

	if err := js.ResolveReview(ctx, ws, open[0].ID, "approve", ""); err != nil {
		t.Fatalf("ResolveReview approve: %v", err)
	}
	keys, err = js.LoadApprovedDeletions(ctx, ws)
	if err != nil {
		t.Fatalf("LoadApprovedDeletions after approve: %v", err)
	}
	if len(keys) != 1 || keys[0] != diff.Key("module:legacy") {
		t.Errorf("approved = %v, want [module:legacy]", keys)
	}

	// "keep" must not authorize anything.
	if err := js.EnsureDeletionReviews(ctx, ws, []jobs.DeletionCandidate{{Key: diff.Key("module:kept")}}); err != nil {
		t.Fatal(err)
	}
	open, _ = js.ListReviews(ctx, ws, "open", 100, 0)
	if err := js.ResolveReview(ctx, ws, open[0].ID, "keep", ""); err != nil {
		t.Fatalf("ResolveReview keep: %v", err)
	}
	keys, _ = js.LoadApprovedDeletions(ctx, ws)
	if len(keys) != 1 {
		t.Errorf("approved after keep = %v, want still only module:legacy", keys)
	}
}

func TestBacklinks(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	pages := []wiki.Page{
		page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n"),
		page("synthesis/arch.md", "arch", "synthesis", "Architecture", "# Arch\n\nSee [[ripple]].\n"),
		page("concepts/dispatch.md", "dispatch", "concept", "Dispatch", "# Dispatch\n\nVia [[ripple]].\n"),
	}
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: pages}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	back, err := js.Backlinks(ctx, ws, "ripple")
	if err != nil {
		t.Fatalf("Backlinks: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("backlinks = %d, want 2", len(back))
	}

	// A deleted page stops being a backlink; a dangling row here would send
	// readers to a 404.
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, SoftDeletePages: []string{"concepts/dispatch.md"},
	}); err != nil {
		t.Fatalf("Import delete: %v", err)
	}
	back, err = js.Backlinks(ctx, ws, "ripple")
	if err != nil {
		t.Fatalf("Backlinks after delete: %v", err)
	}
	if len(back) != 1 || back[0].Slug != "arch" {
		t.Errorf("backlinks after delete = %+v, want only arch", back)
	}
}

func TestWorkspaceRole(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	var userID string
	if err := js.pool.QueryRow(ctx,
		`INSERT INTO users (login) VALUES ('reader') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	// Not a member yet: no role at all.
	role, err := js.WorkspaceRole(ctx, ws, userID)
	if err != nil {
		t.Fatalf("WorkspaceRole: %v", err)
	}
	if role != "" {
		t.Errorf("role for non-member = %q, want empty", role)
	}

	if _, err := js.pool.Exec(ctx, `
		INSERT INTO org_members (org_id, user_id, role)
		SELECT ws.org_id, $2, 'viewer' FROM workspaces ws WHERE ws.id = $1`,
		ws, userID); err != nil {
		t.Fatal(err)
	}
	role, err = js.WorkspaceRole(ctx, ws, userID)
	if err != nil {
		t.Fatalf("WorkspaceRole member: %v", err)
	}
	if role != "viewer" {
		t.Errorf("role = %q, want viewer", role)
	}
}

func TestSearchReturnsSnippets(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	p := page("entities/ripple.md", "ripple", "entity", "Ripple",
		"# Ripple\n\nRipple dispatches tasks to workers and owns the retry queue.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{p}}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	hits, err := js.Search(ctx, ws, "retry queue", 10, 0, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	// The bracket markers are the contract with the UI, which escapes content
	// before swapping them for highlight spans.
	if !strings.Contains(hits[0].Snippet, "[[[") {
		t.Errorf("snippet %q carries no [[[match]]] marker", hits[0].Snippet)
	}
}

// A question is not a keyword list. plainto_tsquery ANDs every lexeme, so one
// incidental word from a sentence -- present in the question, absent from the
// page -- used to sink an otherwise perfect match and return nothing at all.
func TestSearchAnswersQuestionsNotJustKeywords(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	pages := []wiki.Page{
		page("entities/worker.md", "worker", "entity", "Worker",
			"# Worker\n\nA worker dies mid-build and its run returns to the queue after the stale deadline.\n"),
		page("concepts/budget.md", "budget", "concept", "Budget",
			"# Budget\n\nThe ledger reserves before each call so the run ceiling holds at any concurrency.\n"),
	}
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: pages}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// "happens" and "when" appear nowhere in the worker page; under AND
	// semantics this returned zero rows.
	hits, err := js.Search(ctx, ws, "what happens when a worker dies", 10, 0, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("a question matching a page on every meaningful term returned nothing")
	}
	if hits[0].Slug != "worker" {
		t.Errorf("top hit = %q, want worker", hits[0].Slug)
	}

	// Ordering is the other half of the change: a page matching every term
	// must still outrank one matching only some, or keyword search regresses
	// to keep question search working.
	hits, err = js.Search(ctx, ws, "ledger reserves", 10, 0, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Slug != "budget" {
		t.Errorf("hits = %+v, want budget ranked first on an all-terms match", hits)
	}

	// A query sharing no vocabulary with the corpus still finds nothing --
	// widening membership must not turn search into "everything, ranked".
	hits, err = js.Search(ctx, ws, "kubernetes ingress certificates", 10, 0, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("unrelated query returned %d hit(s): %+v", len(hits), hits)
	}
}

// A search box being typed into asks a question a keystroke at a time, and
// every intermediate keystroke is a word no page contains. Without prefix
// matching the results panel is empty for all but the last character of each
// word, which reads as "there is nothing here" rather than "keep typing".
func TestSearchMatchesTheWordStillBeingTyped(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	pages := []wiki.Page{
		page("entities/dispatcher.md", "dispatcher", "entity", "Dispatcher",
			"# Dispatcher\n\nThe dispatcher hands each run to a worker.\n"),
		page("concepts/budget.md", "budget", "concept", "Budget",
			"# Budget\n\nThe ledger reserves before each call.\n"),
	}
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: pages}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	hits, err := js.Search(ctx, ws, "dispat", 10, 0, true)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Slug != "dispatcher" {
		t.Errorf("hits = %+v, want dispatcher for a half-typed word", hits)
	}

	// The same half-word without the flag still finds nothing: the endpoint an
	// agent submits a finished query to keeps meaning exact terms.
	hits, err = js.Search(ctx, ws, "dispat", 10, 0, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("prefix matching leaked into the default: %+v", hits)
	}

	// Only the trailing word is widened. "ledger" is finished and absent from
	// the dispatcher page, so it must still exclude it rather than degrade to
	// an OR over prefixes of everything.
	hits, err = js.Search(ctx, ws, "ledger reser", 10, 0, true)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Slug != "budget" {
		t.Errorf("hits = %+v, want budget ranked first while its last word is typed", hits)
	}

	// A query of nothing but stopwords reduces to an empty tsquery; appending a
	// prefix marker to that is a syntax error, not a search.
	hits, err = js.Search(ctx, ws, "the", 10, 0, true)
	if err != nil {
		t.Fatalf("Search on a stopword-only query: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("stopword-only query returned %+v", hits)
	}
}
