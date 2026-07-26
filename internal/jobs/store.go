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

	// RecordRun persists the run summary, including any review flags the agent
	// raised -- questions it wants a human to judge rather than guess at.
	RecordRun(ctx context.Context, run RunSummary) error

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
	Err        string
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
)
