package agent

import "encoding/json"

// GeneratedPage is one page returned by the model.
//
// The model returns page *content*, not files on disk. Go writes them after
// validation, so the model never touches a filesystem — which removes the whole
// class of path-escape and stray-write problems rather than defending against it.
type GeneratedPage struct {
	Path    string   `json:"path"`
	Type    string   `json:"type"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Tags    []string `json:"tags,omitempty"`
	Related []string `json:"related,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// ReviewFlag is something the model wants a human to judge rather than guess at.
type ReviewFlag struct {
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// GenerationResult is the structured payload of a generate step.
type GenerationResult struct {
	Pages    []GeneratedPage `json:"pages"`
	Findings []string        `json:"findings,omitempty"`
	Reviews  []ReviewFlag    `json:"reviews,omitempty"`
}

// AnalysisPage is one page the analyze step proposes.
type AnalysisPage struct {
	Path     string   `json:"path"`
	Type     string   `json:"type"`
	Title    string   `json:"title"`
	Summary  string   `json:"summary"`
	Tags     []string `json:"tags,omitempty"`
	Related  []string `json:"related,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// AnalysisResult is the structured payload of an analyze step. Validating this
// before the generate call is what keeps a bad plan from costing generation money.
type AnalysisResult struct {
	Pages    []AnalysisPage `json:"pages"`
	Findings []string       `json:"findings,omitempty"`
	Reviews  []ReviewFlag   `json:"reviews,omitempty"`
}

// pageTypes are the six agent-writable page types. Overview is excluded: it is
// derived from frontmatter, never authored.
var pageTypes = []any{"entity", "concept", "source", "query", "comparison", "synthesis"}

// pagePathPattern constrains a path to one level inside a known type directory.
// Enforced in the schema so a malformed path costs nothing to reject, and again
// in Go because a schema is not a security boundary.
const pagePathPattern = `^(entities|concepts|sources|queries|comparisons|synthesis)/[a-z0-9]+(-[a-z0-9]+)*\.md$`

func stringArray() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}

func reviewsSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"kind", "title", "detail"},
			"properties": map[string]any{
				"kind":   map[string]any{"enum": []any{"contradiction", "uncertain", "gap"}},
				"title":  map[string]any{"type": "string"},
				"detail": map[string]any{"type": "string"},
			},
		},
	}
}

// AnalysisSchemaJSON constrains the analyze step's output.
func AnalysisSchemaJSON() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"pages", "findings"},
		"properties": map[string]any{
			"pages": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"path", "type", "title", "summary"},
					"properties": map[string]any{
						"path":     map[string]any{"type": "string", "pattern": pagePathPattern},
						"type":     map[string]any{"enum": pageTypes},
						"title":    map[string]any{"type": "string"},
						"summary":  map[string]any{"type": "string"},
						"tags":     stringArray(),
						"related":  stringArray(),
						"evidence": stringArray(),
					},
				},
			},
			"findings": stringArray(),
			"reviews":  reviewsSchema(),
		},
	}
}

// GenerationSchemaJSON constrains the generate step's output.
//
// Because the model returns page bodies as data rather than writing files, the
// schema is the first validation gate: a wrong page type or an out-of-bounds
// path is rejected by the API before it costs anything downstream.
func GenerationSchemaJSON() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"pages"},
		"properties": map[string]any{
			"pages": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"path", "type", "title", "body"},
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "pattern": pagePathPattern},
						"type":    map[string]any{"enum": pageTypes},
						"title":   map[string]any{"type": "string"},
						"body":    map[string]any{"type": "string"},
						"tags":    stringArray(),
						"related": stringArray(),
						"sources": stringArray(),
					},
				},
			},
			"findings": stringArray(),
			"reviews":  reviewsSchema(),
		},
	}
}

// ParseGeneration decodes a structured generate response.
func ParseGeneration(raw string) (*GenerationResult, error) {
	var out GenerationResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ParseAnalysis decodes a structured analyze response.
func ParseAnalysis(raw string) (*AnalysisResult, error) {
	var out AnalysisResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return &out, nil
}
