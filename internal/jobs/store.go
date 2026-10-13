// Package jobs runs the build pipeline: sync, map, plan, generate, validate,
// import, post-pass.
//
// The pipeline is idempotent at every step so a crashed worker can re-run a job
// from the start without corrupting state. Nothing is committed until agent
// output has passed validation.
package jobs

import (
	"context"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// Store is what the pipeline needs from persistence.
//
// Declared here rather than in the store package so the pipeline can be tested
// against an in-memory fake, and so the SQL stays isolated behind an interface
// the caller shapes. Import is deliberately one method: pages, links, sources,
// and run items must land in a single transaction or a crash mid-write leaves a
// wiki whose index disagrees with its pages.
type Store interface {
	// LoadSources returns every live source record for a workspace, which is
	// the baseline the incremental skip and cascade deletion are computed from.
	LoadSources(ctx context.Context, workspaceID string) ([]diff.SourceRecord, error)

	// TouchSources records that the build read the named sources at `at` and
	// found their text unchanged, so the hash gate dropped them. A source's
	// fetched_at otherwise moves only when its unit regenerates, and the
	// per-document ingest status compares it against when the file's bytes
	// last changed: a replacement that re-saves the same text would read as
	// never read again without this. Best-effort, like the progress calls.
	TouchSources(ctx context.Context, workspaceID string, keys []string, at time.Time) error

	// CountUploads is the bench's uploaded documents as its file rows hold
	// them: staged counts the enabled rows, skipped those among them with a
	// skip recorded against their current bytes. The overview reads these
	// when a build synced no uploads, so a run pinned to another connector
	// does not rewrite the page as if every document had been read.
	CountUploads(ctx context.Context, workspaceID string) (staged, skipped int, err error)

	// LoadPageMeta returns live pages without their bodies -- path, slug, type,
	// title and frontmatter -- which is all that resolving wikilinks, keeping
	// created dates and rebuilding the index and overview need. Page.Body is
	// empty on everything it returns, and callers must not rely on it.
	//
	// Prefer this to LoadPages: a bench's prose can run to hundreds of
	// megabytes, and shipping it to read a slug is the cost this exists to avoid.
	LoadPageMeta(ctx context.Context, workspaceID string) ([]wiki.Page, error)

	// LoadPages returns live pages with their bodies. For a caller that will
	// actually read the prose of most of them; one that has chosen a few pages
	// should ask for just those with LoadPagesBySlug.
	LoadPages(ctx context.Context, workspaceID string) ([]wiki.Page, error)

	// LoadPagesBySlug returns the named live pages with their bodies.
	LoadPagesBySlug(ctx context.Context, workspaceID string, slugs []string) ([]wiki.Page, error)

	// LinkingSlugs returns the slugs of live pages that link to the given slug,
	// from the materialized link table rather than from scanning bodies.
	LinkingSlugs(ctx context.Context, workspaceID, slug string) ([]string, error)

	// LoadSteering returns the purpose and schema documents injected into every
	// prompt, plus any pinned corrections for the pages being regenerated. This
	// is how human knowledge survives regeneration.
	LoadSteering(ctx context.Context, workspaceID string) (Steering, error)

	// Import commits one run's output atomically.
	Import(ctx context.Context, in ImportRequest) error

	// RenameDocuments records what ingest worked out each uploaded document is
	// called, keyed by the path its row is stored under.
	//
	// Deliberately not part of Import. A name is a fact about the document,
	// read out of its own text at sync; it does not depend on the agent then
	// writing pages, and the run that most needs it is the one that changes no
	// pages at all -- an established bench whose documents are all named after
	// their filenames and whose content has not moved since.
	RenameDocuments(ctx context.Context, workspaceID string, names map[string]string) error

	// RecordRun persists the run summary, including any review flags the agent
	// raised -- questions it wants a human to judge rather than guess at.
	RecordRun(ctx context.Context, run RunSummary) error

	// SeedRunItems records the plan as pending items before any unit runs, and
	// MarkRunItem settles one as the run reaches it. Together they are what
	// makes a build in flight watchable: without them the only observable
	// states are "running" and "finished", and a bench ingesting a large
	// repository looks the same five seconds and five minutes in.
	//
	// Both are best-effort by contract. Progress reporting must never fail a
	// build that is otherwise fine, so the pipeline logs their errors and
	// carries on; RecordRun remains the authority on what actually happened.
	// Implementations may no-op for runs that have no row yet -- a CLI build
	// creates its run only at the end, so there is nothing to attach to.
	SeedRunItems(ctx context.Context, runID string, keys []diff.Key, estCostUSD float64) error
	MarkRunItem(ctx context.Context, runID string, item ItemSummary) error

	// LoadApprovedDeletions returns source keys whose removal a human approved
	// through the review queue. Deletion is never automatic: a suspended token
	// and a genuine deletion look identical at the sync layer, so the cascade
	// only ever acts on keys returned here or passed explicitly by the caller.
	LoadApprovedDeletions(ctx context.Context, workspaceID string) ([]diff.Key, error)

	// EnsureDeletionReviews files one open review item per disappeared source,
	// deduplicated so a source that stays missing raises exactly one question.
	EnsureDeletionReviews(ctx context.Context, workspaceID string, cands []DeletionCandidate) error

	// TrailingUnitCost returns the average actual cost of recently succeeded
	// units for this workspace, or 0 when there is no history. It feeds the
	// pre-spend estimate, so previews reflect what this workspace's pages
	// really cost rather than a global constant.
	TrailingUnitCost(ctx context.Context, workspaceID string) (float64, error)

	// ReplaceFigures makes the recorded figures for one source exactly those
	// given, returning the blob keys of any that are no longer referenced.
	//
	// Deliberately not part of Import. Figures are a fact about the document's
	// bytes, established at sync, and they have to be readable by the
	// generation step that decides which ones to put on a page -- which
	// happens long before there is anything to import.
	ReplaceFigures(ctx context.Context, workspaceID, sourceKey string, figs []FigureRecord) ([]string, error)

	// FiguresForSources returns the figures belonging to the given source
	// keys, for telling one unit's generation which pictures it may cite.
	FiguresForSources(ctx context.Context, workspaceID string, keys []string) ([]FigureRecord, error)
}

// FigureRecord is one stored picture, as the pipeline sees it. The pipeline
// deals in this rather than store.FigureRow for the same reason Store is
// declared here: the SQL stays behind the interface, and the fake can be a map.
type FigureRecord struct {
	// ID is empty on the way in and set by the store on the way out; it is
	// what a page's figure reference resolves to.
	ID string
	// SourceKey is the unit key of the document the figure came from.
	SourceKey string
	// Ref is the extractor's within-document identifier.
	Ref         string
	BlobKey     string
	ContentType string
	Width       int
	Height      int
	SizeBytes   int64
	Page        int
	// Caption is the document's own words for the figure, empty when it had
	// none. This is the single most useful thing the model is given about a
	// picture it cannot see.
	Caption string
	Ordinal int
	SHA256  string
}

// DeletionCandidate is a source that vanished from the map and needs a human
// to decide between deletion and retention.
type DeletionCandidate struct {
	Key diff.Key
	// Detail is the human-facing consequence summary: how many pages an
	// approval would remove, and which.
	Detail string
}

// Steering is the human-authored context injected into prompts.
type Steering struct {
	Purpose string
	Schema  string
	// Corrections maps a page slug to pinned corrections for it. Pages are
	// never hand-edited -- an edit would be clobbered on regeneration -- so
	// corrections live outside the page and are re-injected every time.
	Corrections map[string][]string
	// PromotedCorrections are corrections that began as an agent's proposal
	// and were pinned by a member who read and confirmed the text. Kept apart
	// from Corrections so the prompt can say so: the text was not written by
	// the member whose confirmation put it there.
	PromotedCorrections map[string][]string
	// Suppressed maps the slug of a page a human deleted to their reason for
	// deleting it, empty when they gave none.
	//
	// The same mechanism as Corrections and the opposite instruction: a
	// correction says write this differently, a suppression says do not write
	// it at all. Carried here because a deleted page whose source still exists
	// would otherwise be rewritten by the very next run.
	Suppressed map[string]string
}

// ImportRequest is everything one run changed.
type ImportRequest struct {
	WorkspaceID string
	RunID       string

	// UpsertPages are validated pages to write.
	UpsertPages []wiki.Page
	// SoftDeletePages are wiki-relative paths to mark deleted. Deletion is
	// never hard here: kiln requires approval to remove anything, and a
	// retention window makes a mistake recoverable.
	SoftDeletePages []string
	// UpsertSources records what each unit hashed to and which pages it wrote,
	// which is what gates the next run's regeneration.
	UpsertSources []diff.SourceRecord
	// DropSources are source keys removed by an approved cascade.
	DropSources []diff.Key
	// DeleteBlobs are the blobs no remaining source references after
	// DropSources. Import records them for deletion in its own transaction;
	// the caller still tries the deletes after the commit, and the worker's
	// sweep finishes whatever that attempt did not.
	DeleteBlobs []string

	// Index, Overview, and Log are the deterministic artifacts. They are
	// derived from page frontmatter, never agent-written.
	Index    string
	Overview string
	LogEntry string
}

// RunSummary is the record of one pipeline execution.
type RunSummary struct {
	RunID       string
	WorkspaceID string
	Trigger     string
	Ref         string
	Status      string
	CostUSD     float64
	Tokens      int
	Created     int
	Updated     int
	Deleted     int
	Err         string
	Items       []ItemSummary
	// Reviews are flags the agent raised for human judgment. They were emitted
	// on every run since the schema first asked for them; recording them is
	// what turns the review queue from a table into a feature.
	Reviews []ReviewNote
}

// ReviewNote is one agent-raised flag, tagged with the unit that raised it.
type ReviewNote struct {
	Kind   string
	Title  string
	Detail string
	Unit   diff.Key
}

// ItemSummary is one work item's outcome.
type ItemSummary struct {
	Key     diff.Key
	Status  string
	CostUSD float64
	// EstCostUSD is what the plan projected for this unit before it ran.
	// Recorded so estimates can be audited against actuals, which is also
	// where the trailing average that produces future estimates comes from.
	EstCostUSD float64
	Turns      int
	// Tokens is what this unit consumed across its agent calls. Carried per
	// unit rather than only summed into the run so a build in flight can be
	// totalled from the units that have settled, which is what makes the
	// figure visible while it is still moving.
	Tokens int
	Err    string
}

// Run trigger values: why a run exists, as stored in runs.trigger and carried
// on RunSummary.Trigger. They are plain strings because the column is text and
// the queue methods take the trigger as a string. SQL that has to name one
// (the research-run insert, the queue's merge guard) keeps the literal and
// points back here.
const (
	// TriggerManual is a person asking for a build (the CLI, or the API).
	TriggerManual = "manual"
	// TriggerWebhook is a push event from a watched origin.
	TriggerWebhook = "webhook"
	// TriggerPoll is the worker's poll scheduler finding a connector due.
	TriggerPoll = "poll"
	// TriggerUpload is a browser upload landing on an upload connector that
	// syncs on events.
	TriggerUpload = "upload"
	// TriggerContinuation resumes a build that stopped partway, so the units it
	// did not reach are picked up without waiting for the next event.
	TriggerContinuation = "continuation"
	// TriggerResearch answers one review item's question. It is not a build:
	// the queue never merges events into it.
	TriggerResearch = "research"
)

// Run status values.
const (
	StatusPending    = "pending"
	StatusRunning    = "running"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusNoChanges  = "no_changes"
	StatusPartial    = "partial"
	StatusOverBudget = "over_budget"
	StatusCanceled   = "canceled"
	// StatusDeferred is a unit that was planned but never reached: the page cap
	// held it back, or the run stopped early. Only ever a run *item* status. It
	// exists so a finished run has no items still claiming to be pending, which
	// would read as work in progress on a run that ended.
	StatusDeferred = "deferred"
)
