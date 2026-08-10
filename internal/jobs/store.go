// Package jobs runs the build pipeline: sync, map, plan, generate, validate,
// import, post-pass.
//
// The pipeline is idempotent at every step so a crashed worker can re-run a job
// from the start without corrupting state. Nothing is committed until agent
// output has passed validation.
package jobs

import (
	"context"

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

	// LoadPages returns live pages, used to resolve wikilinks and rebuild the
	// index.
	LoadPages(ctx context.Context, workspaceID string) ([]wiki.Page, error)

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
