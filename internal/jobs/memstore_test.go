package jobs

import (
	"context"
	"fmt"
	"sync"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// memStore is an in-memory Store for testing the pipeline without Postgres.
// It mirrors the real semantics that matter, because the pipeline suite runs
// only against this fake: slug is the page identity (Postgres keys pages by
// (wiki, slug), so a same-slug write replaces the row whatever its path),
// deletion matches slug as well as path, artifacts are stored with
// replace/append semantics, and imports are all-or-nothing.
type memStore struct {
	mu sync.Mutex

	sources  map[diff.Key]diff.SourceRecord
	pages    map[string]wiki.Page
	steering Steering

	index    string
	overview string
	log      string

	imports []ImportRequest
	runs    []RunSummary
	// renamed is every document name the pipeline applied, latest wins.
	renamed map[string]string

	// approvedDeletions and deletionReviews mirror the review-queue half of the
	// human loop: the pipeline consumes the former and files the latter.
	approvedDeletions []diff.Key
	deletionReviews   []DeletionCandidate

	// failImport makes Import return an error, to prove the run is still
	// ledgered as failed when the commit does not land.
	failImport error

	// figures mirrors the figure store, keyed by source key. Replace
	// semantics, like the real one: what a document currently contains is
	// what is recorded for it.
	figures map[string][]FigureRecord

	// trailingUnitCost is what TrailingUnitCost reports; zero means no history.
	trailingUnitCost float64

	// seeded and marks record the progress calls, in order. The pipeline makes
	// them for the benefit of anything watching a build, so the tests assert on
	// the sequence rather than on a final state.
	seeded []diff.Key
	marks  []ItemSummary
	// failProgress makes both progress calls fail, to prove a build still
	// succeeds when it cannot report on itself.
	failProgress error
}

func newMemStore() *memStore {
	return &memStore{
		sources: map[diff.Key]diff.SourceRecord{},
		pages:   map[string]wiki.Page{},
		figures: map[string][]FigureRecord{},
	}
}

// ReplaceFigures mirrors the real store: the given set becomes exactly what
// the source has, and blob keys nothing references any more come back.
func (m *memStore) ReplaceFigures(_ context.Context, _, sourceKey string, figs []FigureRecord) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	keep := map[string]bool{}
	for _, f := range figs {
		keep[f.SHA256] = true
	}
	var orphaned []string
	for _, prior := range m.figures[sourceKey] {
		if !keep[prior.SHA256] {
			orphaned = append(orphaned, prior.BlobKey)
		}
	}

	// Ids are assigned here because the real store assigns them, and the
	// prompt and validation both key on the id rather than the digest.
	stored := make([]FigureRecord, 0, len(figs))
	for i, f := range figs {
		if f.ID == "" {
			f.ID = fmt.Sprintf("fig-%s-%d", f.SHA256, i)
		}
		stored = append(stored, f)
	}
	m.figures[sourceKey] = stored
	return orphaned, nil
}

func (m *memStore) FiguresForSources(_ context.Context, _ string, keys []string) ([]FigureRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []FigureRecord
	for _, k := range keys {
		out = append(out, m.figures[k]...)
	}
	return out, nil
}

func (m *memStore) LoadSources(context.Context, string) ([]diff.SourceRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]diff.SourceRecord, 0, len(m.sources))
	for _, s := range m.sources {
		out = append(out, s)
	}
	return out, nil
}

func (m *memStore) LoadPages(context.Context, string) ([]wiki.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]wiki.Page, 0, len(m.pages))
	for _, p := range m.pages {
		out = append(out, p)
	}
	return out, nil
}

func (m *memStore) LoadSteering(context.Context, string) (Steering, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.steering, nil
}

// renamed records what each build decided its documents are called, so a test
// can assert the pipeline applied names without a database.
func (m *memStore) RenameDocuments(_ context.Context, _ string, names map[string]string) error {
	if m.renamed == nil {
		m.renamed = map[string]string{}
	}
	for path, name := range names {
		m.renamed[path] = name
	}
	return nil
}

func (m *memStore) Import(_ context.Context, in ImportRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failImport != nil {
		return m.failImport
	}

	for _, p := range in.UpsertPages {
		// Postgres upserts ON CONFLICT (wiki_id, slug): a page reusing an
		// existing slug replaces that row even when its path moved.
		for path, existing := range m.pages {
			if existing.Slug == p.Slug && path != p.Path {
				delete(m.pages, path)
			}
		}
		m.pages[p.Path] = p
	}
	for _, path := range in.SoftDeletePages {
		// The real store matches slug as well as path, so a page whose type
		// change moved it still deletes.
		slug := wiki.SlugFromPath(path)
		for existingPath, existing := range m.pages {
			if existingPath == path || existing.Slug == slug {
				delete(m.pages, existingPath)
			}
		}
	}
	for _, s := range in.UpsertSources {
		m.sources[s.Key] = s
	}
	for _, k := range in.DropSources {
		delete(m.sources, k)
	}

	// Artifacts mirror writeArtifacts: index and overview replaced wholesale,
	// the log appended.
	if in.Index != "" {
		m.index = in.Index
	}
	if in.Overview != "" {
		m.overview = in.Overview
	}
	if in.LogEntry != "" {
		if m.log == "" {
			m.log = wiki.LogHeader + "\n\n" + in.LogEntry
		} else {
			m.log += "\n" + in.LogEntry
		}
	}

	m.imports = append(m.imports, in)
	return nil
}

func (m *memStore) RecordRun(_ context.Context, run RunSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, run)
	return nil
}

func (m *memStore) SeedRunItems(_ context.Context, _ string, keys []diff.Key, _ float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failProgress != nil {
		return m.failProgress
	}
	m.seeded = append(m.seeded, keys...)
	return nil
}

func (m *memStore) MarkRunItem(_ context.Context, _ string, item ItemSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failProgress != nil {
		return m.failProgress
	}
	m.marks = append(m.marks, item)
	return nil
}

// marksOf returns one unit's progress records in order, for asserting on what
// they carried rather than only on the status they announced.
func (m *memStore) marksOf(key diff.Key) []ItemSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ItemSummary
	for _, it := range m.marks {
		if it.Key == key {
			out = append(out, it)
		}
	}
	return out
}

// marksFor returns one unit's status transitions in order, which is the
// property the progress feature turns on: pending -> running -> settled.
func (m *memStore) marksFor(key diff.Key) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, it := range m.marks {
		if it.Key == key {
			out = append(out, it.Status)
		}
	}
	return out
}

func (m *memStore) LoadApprovedDeletions(context.Context, string) ([]diff.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.approvedDeletions, nil
}

func (m *memStore) EnsureDeletionReviews(_ context.Context, _ string, cands []DeletionCandidate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Dedup by key, matching the real store's one-open-question-per-source rule.
	seen := map[diff.Key]bool{}
	for _, existing := range m.deletionReviews {
		seen[existing.Key] = true
	}
	for _, c := range cands {
		if !seen[c.Key] {
			m.deletionReviews = append(m.deletionReviews, c)
			seen[c.Key] = true
		}
	}
	return nil
}

func (m *memStore) TrailingUnitCost(context.Context, string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.trailingUnitCost, nil
}

func (m *memStore) lastImport() *ImportRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.imports) == 0 {
		return nil
	}
	return &m.imports[len(m.imports)-1]
}

func (m *memStore) importCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.imports)
}
