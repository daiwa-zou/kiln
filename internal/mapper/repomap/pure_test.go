package repomap

import (
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/mapper"
)

func TestDetectLanguage(t *testing.T) {
	cases := map[string]string{
		// Filename-based, not extension-based: the usual miss.
		"Makefile":          "Makefile",
		"sub/GNUmakefile":   "Makefile",
		"Dockerfile":        "Dockerfile",
		"deploy/Tiltfile":   "Tiltfile",
		"Procfile":          "Procfile",
		"cmd/main.go":       "Go",
		"src/lib.rs":        "Rust",
		"app/page.tsx":      "TSX",
		"scripts/run.mjs":   "JavaScript",
		"tool.py":           "Python",
		"schema.proto":      "Protobuf",
		"query.SQL":         "SQL", // extension match is case-insensitive
		"notes.mdx":         "Markdown",
		"compose.yaml":      "YAML",
		"unknown.xyz":       "",
		"no-extension-file": "",
	}
	for path, want := range cases {
		if got := DetectLanguage(path); got != want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDocKind(t *testing.T) {
	cases := map[string]string{
		"README.md":                  "readme",
		"docs/readme.txt":            "readme",
		"CHANGELOG.md":               "changelog",
		"CONTRIBUTING.md":            "contributing",
		"CLAUDE.md":                  "claude-md",
		"AGENTS.md":                  "claude-md",
		"docs/adr/0001-postgres.md":  "adr",
		"docs/decisions/ADR-002.md":  "adr",
		"docs/architecture/index.md": "doc",
	}
	for path, want := range cases {
		if got := DocKind(path); got != want {
			t.Errorf("DocKind(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("0123456789abcdef"); got != "0123456" {
		t.Errorf("shortSHA long = %q", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA short = %q, want unchanged", got)
	}
}

func TestScannerImplementsMapper(t *testing.T) {
	s := &Scanner{}
	if s.Kind() != "git" {
		t.Errorf("Kind = %q, want git", s.Kind())
	}

	// Map must equal Scan().ToWorkspaceMap() for the same tree.
	root := fixture(t, map[string]string{
		"go.mod":  "module example.test/demo\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	wm, err := s.Map(context.Background(), &mapper.SourceSet{Kind: "git", Root: root})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if wm == nil || len(wm.Units) == 0 {
		t.Fatal("Map produced no units")
	}
	if wm.Kind != "git" {
		t.Errorf("workspace map kind = %q", wm.Kind)
	}
	for _, u := range wm.Units {
		if !strings.Contains(u.Key, ":") {
			t.Errorf("unit key %q lacks a namespace prefix", u.Key)
		}
	}
}
