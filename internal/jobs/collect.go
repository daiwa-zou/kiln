package jobs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// collectPages reads whatever the agent wrote into the scratch directory.
//
// Paths are made relative to the scratch root and then validated; a file that
// escaped via symlink is reported rather than silently read, since the scratch
// directory is the only place the agent is permitted to write.
func collectPages(scratchDir string) ([]*wiki.Page, error) {
	root, err := filepath.EvalSymlinks(scratchDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve scratch dir: %w", err)
	}

	var out []*wiki.Page

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("agent wrote outside the scratch directory: %s", path)
		}

		raw, err := os.ReadFile(resolved)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}

		page, err := wiki.ParsePage(filepath.ToSlash(rel), raw)
		if err != nil {
			// A malformed page is a validation problem, not a read failure, so
			// it is carried forward as an unparseable page rather than aborting
			// the whole batch.
			out = append(out, &wiki.Page{Path: filepath.ToSlash(rel), Slug: wiki.SlugFromPath(rel)})
			return nil
		}
		out = append(out, page)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func derefPages(in []*wiki.Page) []wiki.Page {
	out := make([]wiki.Page, 0, len(in))
	for _, p := range in {
		out = append(out, *p)
	}
	return out
}

func pagePaths(pages []wiki.Page) []string {
	out := make([]string, 0, len(pages))
	for _, p := range pages {
		out = append(out, p.Path)
	}
	sort.Strings(out)
	return out
}

func knownSlugs(pages []wiki.Page) map[string]bool {
	out := make(map[string]bool, len(pages))
	for _, p := range pages {
		out[p.Slug] = true
	}
	return out
}

func unitsByKey(m *mapper.WorkspaceMap) map[string]mapper.Unit {
	out := map[string]mapper.Unit{}
	if m == nil {
		return out
	}
	for _, u := range m.Units {
		out[u.Key] = u
	}
	return out
}

// mergePages combines existing pages with newly written ones and removes those
// the cascade deleted, producing the set the index is rebuilt from.
func mergePages(existing, written []wiki.Page, deleted []string) []wiki.Page {
	gone := make(map[string]bool, len(deleted))
	for _, d := range deleted {
		gone[d] = true
	}
	replaced := make(map[string]bool, len(written))
	for _, w := range written {
		replaced[w.Path] = true
	}

	out := make([]wiki.Page, 0, len(existing)+len(written))
	for _, p := range existing {
		if gone[p.Path] || replaced[p.Path] {
			continue
		}
		out = append(out, p)
	}
	for _, w := range written {
		if !gone[w.Path] {
			out = append(out, w)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// countChanges splits written pages into those that are new and those that
// replaced an existing page.
func countChanges(existing, written []wiki.Page) (created, updated int) {
	known := make(map[string]bool, len(existing))
	for _, p := range existing {
		known[p.Path] = true
	}
	for _, w := range written {
		if known[w.Path] {
			updated++
		} else {
			created++
		}
	}
	return created, updated
}

func buildLogLines(s RunSummary, spent float64) []string {
	lines := wiki.SummarizeChanges(s.Created, s.Updated, s.Deleted)
	if s.Trigger != "" {
		lines = append(lines, "Trigger: "+s.Trigger)
	}
	if spent > 0 {
		lines = append(lines, fmt.Sprintf("Cost: $%.4f across %d units", spent, len(s.Items)))
	}
	return lines
}

// runStatus summarizes item outcomes into a single run status.
func runStatus(items []ItemSummary) string {
	if len(items) == 0 {
		return StatusNoChanges
	}
	var ok, failed int
	for _, it := range items {
		if it.Status == StatusSucceeded {
			ok++
		} else {
			failed++
		}
	}
	switch {
	case failed == 0:
		return StatusSucceeded
	case ok == 0:
		return StatusFailed
	default:
		// Valid pages are kept; failed units retain their stale hash and retry.
		return StatusPartial
	}
}

// sanitize makes a cache key safe as a directory name.
func sanitize(key string) string {
	return strings.NewReplacer("/", "_", ":", "_", "\\", "_", "..", "_").Replace(key)
}

var _ = diff.Key("")
