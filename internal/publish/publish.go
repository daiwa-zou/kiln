// Package publish writes a bench's wiki into a git repository.
//
// The wiki already lives in Postgres, so this is not storage in the sense of
// durability -- it is storage in the sense of *somewhere else*. A wiki in a
// repository can be read on GitHub without kiln running, reviewed in a pull
// request, diffed between builds, checked out by anything that reads files,
// and kept after a bench is gone. That is a different set of properties from
// the database, not a better one, which is why this is a mirror rather than a
// move: the database stays authoritative and every push overwrites the
// repository from it.
//
// Overwrite is what makes it correct. A wiki is a set of pages, not a stream of
// edits: a page deleted in kiln has to disappear from the repository, and
// reconciling that by computing per-file changes would mean maintaining a
// second model of what the repository holds. Instead the managed subtree is
// emptied and rewritten each time, and git works out the diff -- which is the
// one thing git is unambiguously better at than any code here.
package publish

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/daiwa-zou/kiln/internal/wiki"
)

// Figure is one picture the wiki references, to be written alongside the pages.
type Figure struct {
	// ID is what a page's `figure:ID` reference names.
	ID          string
	ContentType string
	Data        []byte
}

// Input is everything a published wiki contains.
type Input struct {
	// Bench names the workspace, for the generated README.
	Bench string
	Pages []wiki.Page
	// Index, Overview and Log are the derived artifacts. Empty ones are
	// skipped rather than written blank.
	Index    string
	Overview string
	Log      string
	// Figures are the images pages reference. Only those actually referenced
	// are written: a repository is a place people browse, and shipping every
	// picture the bench ever extracted alongside the handful a page uses makes
	// it worse.
	Figures []Figure
	// ReadmeSuffix is appended to the generated README, for a deployment that
	// wants to say where the bench lives.
	ReadmeSuffix string
}

// Where things land in the published tree. The artifact names match
// wiki.ReservedPages, which is the contract the reader and the MCP server
// already use; naming them here rather than indexing that slice keeps the
// association readable at each use.
const (
	figuresDir   = "figures"
	indexFile    = "index.md"
	overviewFile = "overview.md"
	logFile      = "log.md"
)

// Tree renders the wiki as the files to commit, keyed by repository-relative
// path.
//
// Figure references are rewritten here, and that rewrite is most of the point.
// A page stores `![caption](figure:ID)` because the API resolves it at read
// time, which is right for kiln and useless on GitHub -- the reference is not a
// URL and renders as literal text. Published pages point at a checked-in file
// instead, so the wiki reads correctly in the place it was published to.
func Tree(in Input) map[string][]byte {
	out := map[string][]byte{}

	// Only referenced figures are written, so the file set is decided by the
	// pages rather than by what the bench happens to hold.
	byID := make(map[string]Figure, len(in.Figures))
	for _, f := range in.Figures {
		byID[f.ID] = f
	}
	used := map[string]bool{}

	for _, p := range in.Pages {
		if p.Path == "" {
			continue
		}
		body := wiki.ResolveFigures(p.Body, func(id string) string {
			f, ok := byID[id]
			if !ok {
				// Unknown ids keep their reference form rather than becoming a
				// path to a file that will not exist.
				return ""
			}
			used[id] = true
			return relativeFigurePath(p.Path, id, f.ContentType)
		})

		// Frontmatter is kept rather than stripped: it is what makes a
		// published page round-trippable, and it is the only record on disk of
		// when the page was written and from what.
		out[p.Path] = []byte(p.Meta.Render() + "\n" + strings.TrimLeft(body, "\n"))
	}

	for _, artifact := range []struct{ name, body string }{
		{indexFile, in.Index},
		{overviewFile, in.Overview},
		{logFile, in.Log},
	} {
		if strings.TrimSpace(artifact.body) != "" {
			out[artifact.name] = []byte(artifact.body)
		}
	}

	for id := range used {
		f := byID[id]
		out[path.Join(figuresDir, id+extensionFor(f.ContentType))] = f.Data
	}

	out["README.md"] = []byte(readme(in, len(out)))
	return out
}

// relativeFigurePath links a page to a figure file from wherever that page
// sits. Pages live one directory deep and the reserved artifacts sit at the
// root, so the prefix differs; computing it beats assuming it.
func relativeFigurePath(pagePath, id, contentType string) string {
	target := path.Join(figuresDir, id+extensionFor(contentType))
	if dir := path.Dir(pagePath); dir != "." && dir != "" {
		return "../" + target
	}
	return target
}

func extensionFor(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

// readme explains what the repository is to whoever opens it first.
//
// Worth generating rather than leaving the root bare: someone landing on this
// repository has no way to know the files are machine-written and that editing
// them accomplishes nothing, and finding that out by having an edit vanish is
// the worst way to learn it.
func readme(in Input, fileCount int) string {
	var b strings.Builder
	name := in.Bench
	if name == "" {
		name = "this bench"
	}
	fmt.Fprintf(&b, "# %s\n\n", name)
	b.WriteString("A wiki compiled by [kiln](https://github.com/daiwa-zou/kiln) and mirrored here.\n\n")
	b.WriteString("**This repository is written by a machine.** Every push replaces the\n")
	b.WriteString("published files from kiln's own copy, so edits made here are overwritten\n")
	b.WriteString("by the next build rather than merged. To change a page, correct it in\n")
	b.WriteString("kiln — corrections are re-injected into every future rebuild — or change\n")
	b.WriteString("the sources it was written from.\n\n")

	if strings.TrimSpace(in.Overview) != "" {
		fmt.Fprintf(&b, "Start at [%s](%s).\n\n", overviewFile, overviewFile)
	} else if strings.TrimSpace(in.Index) != "" {
		fmt.Fprintf(&b, "Start at [%s](%s).\n\n", indexFile, indexFile)
	}
	fmt.Fprintf(&b, "%d page(s), %d file(s) in total.\n", len(in.Pages), fileCount+1)

	if suffix := strings.TrimSpace(in.ReadmeSuffix); suffix != "" {
		b.WriteString("\n")
		b.WriteString(suffix)
		b.WriteString("\n")
	}
	return b.String()
}

// SortedPaths returns a tree's paths in a stable order, for logging and tests.
func SortedPaths(tree map[string][]byte) []string {
	out := make([]string, 0, len(tree))
	for p := range tree {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
