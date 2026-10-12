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
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/extract"
	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
	"github.com/daiwa-zou/kiln/internal/naming"
)

// Connector reads a directory of documents.
type Connector struct {
	// Extractors converts documents to text. Defaults to the standard set.
	Extractors extract.Extractors
	// Figures bounds recovery of the pictures and graphs a document carries.
	// Defaults to extract.DefaultFigureOptions; a caller that wants text only
	// sets Max to a negative number.
	Figures extract.FigureOptions
}

func init() { connector.Register(&Connector{}) }

// Kind implements connector.Connector.
func (c *Connector) Kind() string { return connector.KindUpload }

// SkipReason records why a file was not ingested, so a folder with one
// unreadable PDF still produces a wiki and says what it left out.
//
// Reason is the text the CLI prints; Class is the same fact as a fixed token,
// for the server, which persists a token and never extractor text about a
// user's file. Partial marks a document that was ingested with something
// missing (its figures), as opposed to one that produced nothing.
type SkipReason struct {
	Path    string
	Reason  string
	Class   string
	Partial bool
}

// The skip classes. A sink keys its persistence, its log and its metric labels
// on these and nothing else, so an unknown class is folded into
// SkipExtractorError rather than given a label of its own.
const (
	SkipNoText           = "no_text"
	SkipExtractorMissing = "extractor_missing"
	SkipUnsupportedType  = "unsupported_type"
	SkipTimedOut         = "timed_out"
	SkipExtractorError   = "extractor_error"
)

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
	// Figures are on by default. A bench ingesting a folder of reports wants
	// their charts; making it opt-in would mean every deployment silently
	// dropped them until someone found the setting.
	figures := c.Figures
	if figures.Max == 0 {
		figures = extract.DefaultFigureOptions()
	} else if figures.Max < 0 {
		figures = extract.FigureOptions{}
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

		res, err := extractors.ExtractWithFigures(ctx, full, figures)
		if err != nil {
			skips = append(skips, SkipReason{Path: rel, Reason: skipReason(err), Class: skipClass(err)})
			continue
		}
		// A document whose text read fine but whose pictures did not is
		// reported, not skipped: the prose is still worth a page, and a silent
		// loss of figures is exactly the failure nobody would notice.
		if res.FigureErr != nil {
			skips = append(skips, SkipReason{
				Path: rel, Reason: "figures could not be read: " + res.FigureErr.Error(),
				Class: SkipExtractorError, Partial: true})
		}
		if strings.TrimSpace(res.Text) == "" {
			// An empty extraction is not material. Sending it on would spend a
			// model call to write a page about nothing.
			skips = append(skips, SkipReason{Path: rel, Reason: "extracted no text", Class: SkipNoText})
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

		// The key namespaces the docs-directory-relative path through
		// diff.UploadOrigin, so an uploaded README.md can never collide with
		// the repo's own README.md in the source cache or the router.
		key := string(diff.DocKey(diff.UploadOrigin(rel)))
		set.Items = append(set.Items, connector.Item{
			Key:    key,
			Kind:   "doc",
			Title:  naming.FromDocument(rel, res.Text),
			Hash:   res.Hash,
			Path:   staged,
			Origin: rel,
			Meta:   map[string]any{"format": string(res.Format), "tool": res.Tool},
		})
		docs = append(docs, docmap.Doc{
			Key: key, Path: staged, Title: naming.FromDocument(rel, res.Text),
			Text: res.Text, Hash: res.Hash, Origin: rel, Figures: res.Figures,
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

// skipClass is skipReason's token form, for a sink that persists facts about a
// file rather than extractor text.
func skipClass(err error) string {
	switch {
	case errors.Is(err, extract.ErrToolMissing):
		return SkipExtractorMissing
	case errors.Is(err, extract.ErrNoExtractor):
		return SkipUnsupportedType
	// A converter that hung is runTool's sentinel; the caller's own deadline
	// arriving mid-extraction is the context's.
	case errors.Is(err, extract.ErrTimedOut), errors.Is(err, context.DeadlineExceeded):
		return SkipTimedOut
	default:
		return SkipExtractorError
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
