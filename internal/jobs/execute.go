package jobs

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/connector"
	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	"github.com/daiwa-zou/kiln/internal/connector/upload"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
	"github.com/daiwa-zou/kiln/internal/mapper/repomap"
)

// SourceSpec names the material one build consumes.
type SourceSpec struct {
	// Path is the repository directory the git connector scans.
	Path string
	// DocsDir optionally merges a documents directory into the same wiki
	// through the upload connector.
	DocsDir string
	// Slug names the bench, for the git connector's cache keys.
	Slug string
}

// ExecuteRequest is one full build: sync, map, route, then the pipeline.
type ExecuteRequest struct {
	RunID       string
	WorkspaceID string
	Trigger     string
	Source      SourceSpec

	// Force skips the content-hash gate so every routed unit regenerates.
	Force bool
	// DryRun plans and estimates without invoking the agent.
	DryRun bool

	// Progress receives human-readable sync narration; nil discards it.
	Progress io.Writer
}

// Execute syncs sources through the connector registry, maps and routes them,
// and runs the build pipeline. It is the single entry point shared by the CLI
// and the worker, so server-side builds exercise exactly the code path local
// builds do.
func (p *Pipeline) Execute(ctx context.Context, req ExecuteRequest) (*BuildResult, error) {
	out := req.Progress
	if out == nil {
		out = io.Discard
	}

	// Sync goes through the connector registry rather than calling the scanner
	// directly, so the abstraction is exercised by the path that uses it rather
	// than assumed to work.
	conn, err := connector.Get("git")
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "syncing via %s connector: %s\n", conn.Kind(), req.Source.Path)

	set, err := conn.Sync(ctx, connector.Config{"path": req.Source.Path, "slug": req.Source.Slug}, "")
	if err != nil {
		return nil, err
	}

	// Routing and prompt grounding need the module graph, which a flat item
	// list cannot express. The git connector carried it on the same sync.
	rm := gitconn.MapOf(set)
	if rm == nil {
		return nil, fmt.Errorf("git connector returned no repository map")
	}
	wm := rm.ToWorkspaceMap()
	fmt.Fprintf(out, "  %d modules, %d units, %d edges\n", len(rm.Modules), len(wm.Units), len(wm.Edges))
	if rm.Git != nil && rm.Git.HeadSHA != "" {
		fmt.Fprintf(out, "  at %s on %s\n", shortRef(rm.Git.HeadSHA), rm.Git.Branch)
	} else {
		fmt.Fprintln(out, "  not a git repository; change detection uses content hashes")
	}

	router := routerFor(rm)

	// A second connector's material merges into the same map, so code and
	// documents produce one wiki whose pages can link across the boundary
	// rather than two wikis that cannot see each other.
	if req.Source.DocsDir != "" {
		docMap, docRouter, staging, err := syncDocs(ctx, out, req.Source.DocsDir)
		if staging != "" {
			defer os.RemoveAll(staging)
		}
		if err != nil {
			return nil, err
		}
		merged, err := mapper.Merge(req.Source.Path, wm, docMap)
		if err != nil {
			return nil, err
		}
		wm = merged
		maps.Copy(router.DocPaths, docRouter)
		// Section units have no path of their own; register them under their
		// parent so routing a document also routes its chapters.
		router.DocSections = sectionsByParent(docMap)
	}

	breq := BuildRequest{
		RunID:       req.RunID,
		WorkspaceID: req.WorkspaceID,
		Trigger:     req.Trigger,
		Ref:         gitRef(rm),
		SourceDir:   req.Source.Path,
		Map:         wm,
		Router:      router,
		// Every unit is a candidate; the pipeline's hash gate is what actually
		// decides. A local path has no commit range to diff against, and the
		// gate is both cheaper and more precise than a filesystem comparison --
		// it compares each unit's content hash (and the map hash, for the
		// architecture synthesis) to what the last run recorded, so a repeat
		// build still costs nothing.
		Changes: diff.ChangeSet{FullRebuild: true},
		Force:   req.Force,
		DryRun:  req.DryRun,
	}
	// The API runner returns pages as data, so a scratch directory is only
	// created for the CLI runner that writes files.
	if agent.WritesFiles(p.Runner) {
		scratch, err := os.MkdirTemp("", "kiln-scratch-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(scratch)
		breq.ScratchDir = scratch
	}

	return p.Build(ctx, breq)
}

// syncDocs ingests a documents directory through the upload connector and maps
// it with docmap, returning the map and the doc paths to add to routing.
//
// The returned staging directory holds the extracted text the unit inputs point
// at. It must outlive the build -- prompts are assembled from it -- so the
// caller owns removing it rather than a defer here.
func syncDocs(ctx context.Context, out io.Writer, dir string) (_ *mapper.WorkspaceMap, _ map[string]diff.Key, staging string, err error) {
	absDocs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, "", err
	}

	conn, err := connector.Get("upload")
	if err != nil {
		return nil, nil, "", err
	}
	fmt.Fprintf(out, "syncing via %s connector: %s\n", conn.Kind(), absDocs)

	staging, err = os.MkdirTemp("", "kiln-docs-")
	if err != nil {
		return nil, nil, "", err
	}

	set, err := conn.Sync(ctx, connector.Config{"path": absDocs}, staging)
	if err != nil {
		return nil, nil, staging, err
	}

	payload := upload.PayloadOf(set)
	if payload == nil {
		return nil, nil, staging, fmt.Errorf("upload connector returned no documents payload")
	}

	dm := &docmap.Mapper{}
	wm, err := dm.MapDocs(ctx, staging, payload.Docs)
	if err != nil {
		return nil, nil, staging, err
	}

	fmt.Fprintf(out, "  %d document(s), %d units\n", len(payload.Docs), len(wm.Units))
	// Skips are reported rather than swallowed: a folder that silently ingested
	// half its files would look like a working build.
	for _, s := range payload.Skipped {
		fmt.Fprintf(out, "  skipped %s: %s\n", s.Path, s.Reason)
	}

	// Keyed by the docs-directory-relative path. These never collide with the
	// repo's own change paths today because docs are routed only under
	// FullRebuild; when incremental doc changes land, the change source must
	// produce paths relative to the same docs root.
	routes := map[string]diff.Key{}
	for _, d := range payload.Docs {
		routes[d.Origin] = diff.DocKey(d.Origin)
	}
	return wm, routes, staging, nil
}

// routerFor maps module directories to their cache keys so a changed path is
// attributed to the module that owns it.
//
// Sub-partitions are registered alongside their parents. They are the reason a
// 15k-LOC repository becomes readable pages rather than one useless page, so
// leaving them out would mean a full rebuild covered only the top-level module.
// Attribution takes the longest matching prefix, so a change under
// internal/auth lands on that service rather than on the repository root.
func routerFor(rm *repomap.RepoMap) diff.Router {
	dirs := map[string]diff.Key{}
	for _, m := range rm.Modules {
		if !m.Empty {
			dirs[m.Dir] = diff.ModuleKey(m.Slug)
		}
	}

	docs := map[string]diff.Key{}
	for _, d := range rm.Docs {
		docs[d.Path] = diff.DocKey(d.Path)
	}

	return diff.Router{ModuleDirs: dirs, DocPaths: docs, Cosmetic: cosmeticPath}
}

// cosmeticPath marks files that never justify an LLM call on their own:
// images, archives, lockfiles, editor and CI chrome. Manifest and
// architectural files are exempted by the router itself.
func cosmeticPath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp",
		".woff", ".woff2", ".ttf", ".eot",
		".zip", ".gz", ".tar", ".tgz",
		".lock", ".sum":
		return true
	}
	base := filepath.Base(p)
	switch base {
	case ".gitignore", ".gitattributes", ".editorconfig", ".prettierrc",
		"package-lock.json", "yarn.lock", "pnpm-lock.yaml":
		return true
	}
	return false
}

// sectionsByParent indexes section units under their parent document key, for
// the router.
func sectionsByParent(wm *mapper.WorkspaceMap) map[diff.Key][]diff.Key {
	out := map[diff.Key][]diff.Key{}
	for _, u := range wm.Units {
		if u.Kind != "doc-section" {
			continue
		}
		parent, _ := u.Meta["parent"].(string)
		if parent == "" {
			continue
		}
		out[diff.Key(parent)] = append(out[diff.Key(parent)], diff.Key(u.Key))
	}
	return out
}

func gitRef(rm *repomap.RepoMap) string {
	if rm.Git == nil {
		return ""
	}
	return shortRef(rm.Git.HeadSHA)
}

func shortRef(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
