package agent

import (
	"reflect"
	"testing"
)

// The page taxonomy in the schema is derived from the wiki package. These
// literals are what it was when it was hand-copied; the schema the API sees
// must stay byte-identical, so the derivation is checked against them rather
// than against itself.
func TestPageSchemaDerivedFromWikiTaxonomy(t *testing.T) {
	wantTypes := []any{"entity", "concept", "source", "query", "comparison", "synthesis"}
	if !reflect.DeepEqual(pageTypes, wantTypes) {
		t.Errorf("pageTypes = %v, want %v", pageTypes, wantTypes)
	}

	const wantPattern = `^(entities|concepts|sources|queries|comparisons|synthesis)/[a-z0-9]+(-[a-z0-9]+)*\.md$`
	if pagePathPattern != wantPattern {
		t.Errorf("pagePathPattern = %q, want %q", pagePathPattern, wantPattern)
	}
}
