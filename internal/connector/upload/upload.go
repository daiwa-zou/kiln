// Package upload materializes a folder of documents as a SourceSet.
//
// This is the second Connector alongside git, and the pair is what proves the
// abstraction: the two are about as unlike as source kinds get. git describes a
// module graph and never copies anything; upload runs every file through an
// external converter and is the only durable holder of what it ingests. Both
// produce a SourceSet, and nothing downstream can tell them apart.
package upload

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/extract"
	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
)

// Connector reads a directory of documents.
type Connector struct {
	// Extractors converts documents to text. Defaults to the standard set.
	Extractors extract.Extractors
}

func init() { connector.Register(&Connector{}) }

// New returns an upload connector.
func New() *Connector { return &Connector{} }

// Kind implements connector.Connector.
func (c *Connector) Kind() string { return "upload" }

// Trigger implements connector.Connector. Uploads arrive when a person sends
// them, so there is nothing to poll and nothing to receive a webhook from.
func (c *Connector) Trigger() connector.TriggerMode { return connector.TriggerManual }

// SkipReason records why a file was not ingested, so a folder with one
// unreadable PDF still produces a wiki and says what it left out.
type SkipReason struct {
	Path   string
	Reason string
}

// Sync extracts every supported document under the configured path.
//
// A file that cannot be extracted is skipped rather than failing the sync. One
// corrupt PDF in a folder of fifty should not cost the other forty-nine, and
// the skip is reported so it is visible rather than silent.
func (c *Connector) Sync(ctx context.Context, cfg connector.Config, dst string) (*connector.SourceSet, error) {
	// SECURITY: "path" may name any directory the process can read. That is
	// fine while only the CLI supplies it; the moment connector configs become
	// API- or database-driven, this is a local-file-inclusion primitive and
	// needs an allowlist of permitted roots before that ships.
	root, err := cfg.RequireString("path")
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		return nil, fmt.Errorf("connector/upload: %s is not a directory", root)
	}

	extractors := c.Extractors
	if extractors == nil {
		extractors = extract.DefaultExtractors()
	}

	files, err := listDocuments(abs)
	if err != nil {
		return nil, err
	}

	set := &connector.SourceSet{Root: abs, Kind: c.Kind()}
	var (
		docs  []docmap.Doc
		skips []SkipReason
	)

	for _, rel := range files {
		full := filepath.Join(abs, filepath.FromSlash(rel))

		res, err := extractors.Extract(ctx, full)
		if err != nil {
			skips = append(skips, SkipReason{Path: rel, Reason: skipReason(err)})
			continue
		}
		if strings.TrimSpace(res.Text) == "" {
			// An empty extraction is not material. Sending it on would spend a
			// model call to write a page about nothing.
			skips = append(skips, SkipReason{Path: rel, Reason: "extracted no text"})
			continue
		}

		// Staging the extracted text lets a mapper and the agent read it
		// without re-running the converter.
		staged := rel
		if dst != "" {
			staged, err = stage(dst, rel, res.Text)
			if err != nil {
				return nil, err
			}
		}

		key := "doc:" + rel
		set.Items = append(set.Items, connector.Item{
			Key:    key,
			Kind:   "doc",
			Title:  titleOf(rel, res.Text),
			Hash:   res.Hash,
			Path:   staged,
			Origin: rel,
			Meta:   map[string]any{"format": string(res.Format), "tool": res.Tool},
		})
		docs = append(docs, docmap.Doc{
			Key: key, Path: staged, Title: titleOf(rel, res.Text),
			Text: res.Text, Hash: res.Hash, Origin: rel,
		})
	}

	// The extracted text rides along so a mapper does not have to re-read and
	// re-convert every file, the same way git carries its repository map.
	set.Native = &Payload{Docs: docs, Skipped: skips}
	return set, nil
}

// Payload is the upload connector's richer description, carried on
// SourceSet.Native.
type Payload struct {
	Docs    []docmap.Doc
	Skipped []SkipReason
}

// PayloadOf extracts an upload payload, returning nil for any other connector's
// set so a caller written against uploads degrades rather than panicking.
func PayloadOf(set *connector.SourceSet) *Payload {
	if set == nil {
		return nil
	}
	p, _ := set.Native.(*Payload)
	return p
}

// skipReason turns an extraction error into something short enough to report in
// a list, while keeping the actionable distinction between a missing tool and a
// bad document.
func skipReason(err error) string {
	switch {
	case errors.Is(err, extract.ErrToolMissing):
		return "required extraction tool is not installed"
	case errors.Is(err, extract.ErrNoExtractor):
		return "unsupported file type"
	default:
		return err.Error()
	}
}

// excludedDirs are never descended into.
var excludedDirs = map[string]bool{
	".git": true, "node_modules": true, ".cache": true,
	"__MACOSX": true, ".obsidian": true,
}

// listDocuments walks a folder for candidate files, in sorted order so a sync
// is reproducible.
func listDocuments(root string) ([]string, error) {
	var out []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree should not abort the walk; the rest of the
			// folder is still worth ingesting.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if excludedDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if extract.DetectFormat(path) == extract.FormatUnknown {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("connector/upload: walk: %w", err)
	}

	sort.Strings(out)
	return out, nil
}

// stage writes extracted text beside the staging root as markdown.
func stage(dst, rel, text string) (string, error) {
	staged := strings.TrimSuffix(rel, filepath.Ext(rel)) + ".md"
	full := filepath.Join(dst, filepath.FromSlash(staged))

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("connector/upload: stage %s: %w", rel, err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		return "", fmt.Errorf("connector/upload: stage %s: %w", rel, err)
	}
	return staged, nil
}

// titleOf prefers the document's first heading and falls back to its filename.
func titleOf(rel, text string) string {
	for _, line := range strings.Split(text, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			if title := strings.TrimSpace(after); title != "" {
				return title
			}
		}
	}
	base := filepath.Base(rel)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
