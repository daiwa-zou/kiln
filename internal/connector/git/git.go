// Package git materializes a local repository checkout as a SourceSet.
//
// For M0 the repository is a path already on disk, so "materializing" is
// validating it and describing what is there. A remote clone lands behind the
// same interface later: the pipeline sees a SourceSet either way.
package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/mapper/repomap"
)

// Connector reads a git repository or plain directory from local disk.
type Connector struct{}

func init() { connector.Register(&Connector{}) }

// New returns a git connector.
func New() *Connector { return &Connector{} }

// Kind implements connector.Connector.
func (c *Connector) Kind() string { return "git" }

// Trigger implements connector.Connector.
//
// A local path has nothing to push an event, so it is manual. A remote git
// connector would report webhook instead.
func (c *Connector) Trigger() connector.TriggerMode { return connector.TriggerManual }

// Sync validates the path and describes the repository.
//
// dst is ignored: the material is already on disk and copying a repository to
// scan it would waste time and space for nothing.
func (c *Connector) Sync(ctx context.Context, cfg connector.Config, _ string) (*connector.SourceSet, error) {
	path, err := cfg.RequireString("path")
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		return nil, fmt.Errorf("connector/git: %s is not a directory", path)
	}

	slug, _ := cfg.String("slug")
	scanner := &repomap.Scanner{Slug: slug}
	rm, err := scanner.Scan(ctx, abs)
	if err != nil {
		return nil, fmt.Errorf("connector/git: scan: %w", err)
	}

	// The map rides along so a consumer that knows this is a git source gets
	// the module graph from the same sync, rather than scanning a second time.
	set := &connector.SourceSet{Root: abs, Kind: c.Kind(), Native: rm}

	for _, m := range rm.Modules {
		// Empty directories are recorded in the map so it stays faithful, but
		// they are not material and must not become units.
		if m.Empty {
			continue
		}
		set.Items = append(set.Items, connector.Item{
			Key:   "module:" + m.Slug,
			Kind:  "module",
			Title: m.Name,
			Hash:  m.Hash,
			Path:  m.Dir,
			Meta: map[string]any{
				"kind":   m.Kind,
				"loc":    m.LOC,
				"parent": m.Parent,
			},
		})
	}

	for _, d := range rm.Docs {
		set.Items = append(set.Items, connector.Item{
			Key:   "doc:" + d.Path,
			Kind:  "doc",
			Title: d.Title,
			Hash:  d.Hash,
			Path:  d.Path,
			Meta:  map[string]any{"kind": d.Kind},
		})
	}

	return set, nil
}

// MapOf extracts the repository map a git sync carried in SourceSet.Native.
//
// Returns nil for a set produced by any other connector, so a caller written
// against git sources degrades rather than panicking on a document workspace.
func MapOf(set *connector.SourceSet) *repomap.RepoMap {
	if set == nil {
		return nil
	}
	rm, _ := set.Native.(*repomap.RepoMap)
	return rm
}
