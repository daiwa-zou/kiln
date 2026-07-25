package jobs

import (
	"context"
	"sync"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// memStore is an in-memory Store for testing the pipeline without Postgres.
// It mirrors the real semantics that matter: soft deletion, source records
// gating regeneration, and imports being all-or-nothing.
type memStore struct {
	mu sync.Mutex

	sources  map[diff.Key]diff.SourceRecord
	pages    map[string]wiki.Page
	steering Steering

	imports []ImportRequest
	runs    []RunSummary

	// failImport makes Import return an error, to prove nothing is recorded
	// when the commit fails.
	failImport error
}

func newMemStore() *memStore {
	return &memStore{
		sources: map[diff.Key]diff.SourceRecord{},
		pages:   map[string]wiki.Page{},
	}
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

func (m *memStore) Import(_ context.Context, in ImportRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failImport != nil {
		return m.failImport
	}

	for _, p := range in.UpsertPages {
		m.pages[p.Path] = p
	}
	for _, path := range in.SoftDeletePages {
		delete(m.pages, path)
	}
	for _, s := range in.UpsertSources {
		m.sources[s.Key] = s
	}
	for _, k := range in.DropSources {
		delete(m.sources, k)
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
