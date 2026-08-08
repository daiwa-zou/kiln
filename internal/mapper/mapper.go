// Package mapper turns a materialized set of sources into the partitioning that
// drives page generation.
//
// The mappers (repomap for code, docmap for documents) are the only
// source-kind-specific pieces of the pipeline besides the prompts. Everything
// downstream of WorkspaceMap -- diff routing, cascade deletion, validation,
// the deterministic index, storage, and the UI -- is shared across every
// connector.
package mapper

import "time"

// Unit is one primary page plus its satellites: a code module, a document, a
// web page. It is the granularity at which regeneration is decided, so its
// Hash is what gates whether an LLM call happens at all.
type Unit struct {
	Key    string         `json:"key"`  // "module:apps-ripple", "doc:reports/q3.pdf"
	Kind   string         `json:"kind"` // module | doc | page | url | artifact
	Slug   string         `json:"slug"` // "module-apps-ripple"
	Title  string         `json:"title"`
	Dir    string         `json:"dir,omitempty"`
	Inputs []string       `json:"inputs"` // paths the agent should start from
	Hash   string         `json:"hash"`   // sha256 over the unit's content
	LOC    int            `json:"loc,omitempty"`
	Empty  bool           `json:"empty,omitempty"`
	Meta   map[string]any `json:"meta,omitempty"`
}

// MetaRoot is the Unit.Meta key naming the directory a unit's Inputs are
// relative to.
//
// Set it when a mapper's inputs do not live under the run's source directory --
// staged extractions, for instance. Omitting it means "relative to the source
// directory", which is what every in-tree mapper wants.
const MetaRoot = "root"

// Edge is a dependency or reference between units, extracted deterministically
// so dependency pages are grounded rather than inferred by the model.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"` // requires | replaces | imports | references
}

// WorkspaceMap is a Mapper's output.
type WorkspaceMap struct {
	SchemaVersion int       `json:"schemaVersion"`
	Kind          string    `json:"kind"`
	Root          string    `json:"root"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Units         []Unit    `json:"units"`
	Edges         []Edge    `json:"edges"`
	Summary       string    `json:"summary"` // rendered, citable by the agent
	Hash          string    `json:"hash"`    // changes when the shape changes
}
