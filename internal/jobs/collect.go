package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// collectPages reads whatever the agent wrote into the scratch directory.
//
// Paths are made relative to the scratch root and then validated; a file that
// escaped via symlink is reported rather than silently read, since the scratch
// directory is the only place the agent is permitted to write.
//
// A file that fails to parse is returned as a violation carrying the real
// parse error -- "unterminated frontmatter" reaches the corrective prompt as
// exactly that, not as a misleading "body is empty".
func collectPages(scratchDir string) ([]*wiki.Page, []wiki.Violation, error) {
	root, err := filepath.EvalSymlinks(scratchDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("resolve scratch dir: %w", err)
	}

	var (
		out        []*wiki.Page
		violations []wiki.Violation
	)

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
			// A malformed page is a validation problem, not a read failure:
			// carry the actual parse error so the retry fixes the real defect.
			violations = append(violations, wiki.Violation{
				Path:   filepath.ToSlash(rel),
				Reason: fmt.Sprintf("page failed to parse: %v", err),
			})
			return nil
		}
		out = append(out, page)
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, violations, nil
}

// pagesFrom takes whatever the runner produced and normalizes it to pages.
//
// The API runner returns page content as structured data and touches no
// filesystem; the CLI runner writes files into the scratch directory. Reading
// structured output when it is present is what lets the API path skip the
// scratch directory, and with it the path-escape checks that only exist because
// an agent with write access might stray outside it.
func pagesFrom(res *agent.Result, scratchDir string) ([]*wiki.Page, []wiki.Violation, error) {
	if res == nil || res.Generation == nil {
		return collectPages(scratchDir)
	}

	out := make([]*wiki.Page, 0, len(res.Generation.Pages))
	for _, gp := range res.Generation.Pages {
		out = append(out, &wiki.Page{
			Path: gp.Path,
			Slug: wiki.SlugFromPath(gp.Path),
			Meta: wiki.Frontmatter{
				Type:    wiki.PageType(gp.Type),
				Title:   gp.Title,
				Tags:    gp.Tags,
				Related: gp.Related,
				Sources: gp.Sources,
			},
			Body: gp.Body,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil, nil
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

// truncatePreservingArch caps the work list while keeping the architecture
// synthesis.
//
// Architecture sorts last so it regenerates after the modules it summarizes,
// which means a plain slice would drop it first every time. On a repository with
// more modules than the cap it would then never run, and the wiki would keep
// per-module pages with no page tying them together.
func truncatePreservingArch(dirty []diff.Key, limit int) []diff.Key {
	if limit <= 0 || len(dirty) <= limit {
		return dirty
	}

	var hasArch bool
	for _, k := range dirty {
		if k == diff.ArchOverview {
			hasArch = true
			break
		}
	}
	if !hasArch {
		return dirty[:limit]
	}

	// Reserve the final slot for architecture and fill the rest in order.
	out := make([]diff.Key, 0, limit)
	for _, k := range dirty {
		if len(out) == limit-1 {
			break
		}
		if k != diff.ArchOverview {
			out = append(out, k)
		}
	}
	return append(out, diff.ArchOverview)
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

// regenKeysFor maps pages the cascade marked for regeneration back to the
// surviving source units that own them, so their prose stops describing
// deleted material. The keys are appended after the hash gate on purpose: a
// regeneration forced by a departed sibling has, by definition, an unchanged
// input hash.
func regenKeysFor(cascade diff.Cascade, sources []diff.SourceRecord, already []diff.Key) []diff.Key {
	if len(cascade.RegeneratePages) == 0 {
		return nil
	}
	regen := make(map[string]bool, len(cascade.RegeneratePages))
	for _, p := range cascade.RegeneratePages {
		regen[p] = true
	}
	dropping := make(map[diff.Key]bool, len(cascade.DropSources))
	for _, k := range cascade.DropSources {
		dropping[k] = true
	}
	have := make(map[diff.Key]bool, len(already))
	for _, k := range already {
		have[k] = true
	}

	var out []diff.Key
	for _, s := range sources {
		if dropping[s.Key] || have[s.Key] {
			continue
		}
		for _, p := range s.FilesWritten {
			if regen[p] {
				out = append(out, s.Key)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sanitize makes a cache key safe as a directory name. A short content hash is
// appended because the character replacement is lossy: module:a/b and
// module:a_b would otherwise share a scratch directory and a session ID.
func sanitize(key string) string {
	base := strings.NewReplacer("/", "_", ":", "_", "\\", "_", ".", "_").Replace(key)
	sum := sha256.Sum256([]byte(key))
	return base + "-" + hex.EncodeToString(sum[:4])
}

// mergeKeys unions two key lists, preserving first-seen order.
func mergeKeys(a, b []diff.Key) []diff.Key {
	seen := make(map[diff.Key]bool, len(a)+len(b))
	out := make([]diff.Key, 0, len(a)+len(b))
	for _, k := range append(append([]diff.Key{}, a...), b...) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// maxCandidatePages bounds the page list quoted in a deletion review's detail.
const maxCandidatePages = 10

// deletionCandidates finds sources on record that the current map no longer
// contains -- each one a question for the review queue, never an automatic
// deletion.
//
// A key is only flagged when its producing namespace was synced this run
// (per synced, when provided): a `kiln build` run without --docs must not
// flag every uploaded document as deleted merely because uploads were not
// mapped this time. Explicit synced namespaces close the old blind spot
// where removing the *last* source of a kind raised no flag -- the caller
// knows it synced uploads and found nothing, which is exactly a deletion.
// A nil synced falls back to inferring from surviving unit prefixes.
func deletionCandidates(sources []diff.SourceRecord, m *mapper.WorkspaceMap, approved []diff.Key, synced map[string]bool) []DeletionCandidate {
	units := unitsByKey(m)

	prefixes := map[string]bool{}
	for k := range units {
		prefixes[diff.Key(k).Prefix()] = true
	}
	skip := make(map[diff.Key]bool, len(approved))
	for _, k := range approved {
		skip[k] = true
	}

	var out []DeletionCandidate
	for _, s := range sources {
		if s.Key == diff.ArchOverview || skip[s.Key] {
			continue
		}
		if _, live := units[string(s.Key)]; live {
			continue
		}
		if synced != nil {
			if !synced[diff.Namespace(s.Key)] {
				continue
			}
		} else if !prefixes[s.Key.Prefix()] {
			continue
		}
		out = append(out, DeletionCandidate{
			Key:    s.Key,
			Detail: describeDeletion(sources, s.Key),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// describeDeletion states the consequence a reviewer is approving: how many
// pages go, which ones, and that shared pages are regenerated rather than
// removed. This is the number a deletion review shows before anyone confirms.
func describeDeletion(sources []diff.SourceRecord, key diff.Key) string {
	c := diff.PlanCascade(sources, []diff.Key{key})

	var b strings.Builder
	fmt.Fprintf(&b, "Source %s is no longer present in the map.", key)
	if n := c.PageCount(); n > 0 {
		fmt.Fprintf(&b, " Approving removes %d page(s):", n)
		for i, p := range c.DeletePages {
			if i == maxCandidatePages {
				fmt.Fprintf(&b, " … and %d more", n-maxCandidatePages)
				break
			}
			b.WriteString(" ")
			b.WriteString(p)
		}
		b.WriteString(".")
	} else {
		b.WriteString(" No pages are exclusively owned by it.")
	}
	if len(c.RegeneratePages) > 0 {
		fmt.Fprintf(&b, " %d shared page(s) would be regenerated, not removed.", len(c.RegeneratePages))
	}
	return b.String()
}
