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
	webconn "github.com/daiwa-zou/kiln/internal/connector/web"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
	"github.com/daiwa-zou/kiln/internal/mapper/repomap"
)

// SourceConnectors maps each sync source to the connector row that supplied
// it, for per-source attribution on import.
type SourceConnectors struct {
	Git    string
	Upload string
	Web    string
}

// For returns the connector id owning a unit key's namespace.
func (c SourceConnectors) For(key diff.Key) string {
	switch diff.Namespace(key) {
	case "doc:upload":
		return c.Upload
	case "doc:web":
		return c.Web
	}
	return c.Git
}

// SourceSpec names the material one build consumes.
type SourceSpec struct {
	// Path is the repository directory the git connector scans.
	Path string
	// DocsDir optionally merges a documents directory into the same wiki
	// through the upload connector.
	DocsDir string
	// WebURLs optionally merges fetched pages into the same wiki through
	// the web connector.
	WebURLs []string
	// Slug names the bench, for the git connector's cache keys.
	Slug string
	// BlobKeys names the stored blobs behind each staged document's unit key,
	// set when DocsDir was materialized from the blob store rather than a
	// local folder. It rides to the source records so an approved deletion
	// can cascade to storage. Nil for local --docs directories.
	BlobKeys map[string][]string
	// SkippedKeys are unit keys deliberately left out of this sync (paused
	// documents). Their absence from the map is a choice, not a
	// disappearance, so no deletion review may be raised for them.
	SkippedKeys []diff.Key
}

// ExecuteRequest is one full build: sync, map, route, then the pipeline.
type ExecuteRequest struct {
	RunID       string
	WorkspaceID string
	Trigger     string
	Source      SourceSpec

	// Connectors attributes each synced namespace's source records to the
	// connector that produced them; zero for CLI builds.
	Connectors SourceConnectors

	// BaseRef, when set, requests incremental routing: changes are derived
	// from `git diff BaseRef..HEAD` and only the affected units are routed.
	// Any failure to use the range falls back to a full (hash-gated) rebuild
	// — ranges are an optimization, never a correctness dependency.
	BaseRef string

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
	if req.Source.Path == "" && req.Source.DocsDir == "" && len(req.Source.WebURLs) == 0 {
		return nil, fmt.Errorf("nothing to build: no repository, documents, or web pages")
	}

	// The repository is optional: a bench fed only by documents or web pages
	// builds from those alone. When present, sync goes through the connector
	// registry rather than calling the scanner directly, so the abstraction is
	// exercised by the path that uses it rather than assumed to work.
	var rm *repomap.RepoMap
	wm := &mapper.WorkspaceMap{SchemaVersion: 1}
	router := diff.Router{ModuleDirs: map[string]diff.Key{}, DocPaths: map[string]diff.Key{}, Cosmetic: cosmeticPath}
	if req.Source.Path != "" {
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
		rm = gitconn.MapOf(set)
		if rm == nil {
			return nil, fmt.Errorf("git connector returned no repository map")
		}
		wm = rm.ToWorkspaceMap()
		fmt.Fprintf(out, "  %d modules, %d units, %d edges\n", len(rm.Modules), len(wm.Units), len(wm.Edges))
		if rm.Git != nil && rm.Git.HeadSHA != "" {
			fmt.Fprintf(out, "  at %s on %s\n", shortRef(rm.Git.HeadSHA), rm.Git.Branch)
		} else {
			fmt.Fprintln(out, "  not a git repository; change detection uses content hashes")
		}

		router = routerFor(rm)
	}

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

	// Web pages merge the same way: a third source kind, one wiki.
	if len(req.Source.WebURLs) > 0 {
		webMap, webRouter, staging, err := syncWeb(ctx, out, req.Source.WebURLs)
		if staging != "" {
			defer os.RemoveAll(staging)
		}
		if err != nil {
			return nil, err
		}
		merged, err := mapper.Merge(req.Source.Path, wm, webMap)
		if err != nil {
			return nil, err
		}
		wm = merged
		maps.Copy(router.DocPaths, webRouter)
		for parent, sections := range sectionsByParent(webMap) {
			if router.DocSections == nil {
				router.DocSections = map[diff.Key][]diff.Key{}
			}
			router.DocSections[parent] = sections
		}
	}

	// Without a base ref, every unit is a candidate and the pipeline's hash
	// gate decides — a repeat build still costs nothing. With one (webhook
	// runs, or build --since), the router narrows the plan to the units the
	// commit range actually touched, which is what suppresses cosmetic churn
	// and keeps plan previews honest on big repositories.
	changes := diff.ChangeSet{FullRebuild: true}
	if req.BaseRef != "" && req.Source.Path != "" {
		head := gitconn.HeadRef(ctx, req.Source.Path)
		if list, ok := gitconn.DiffRange(ctx, req.Source.Path, req.BaseRef, head); ok && head != "" {
			changes = diff.ChangeSet{FromRef: req.BaseRef, ToRef: head, Changes: list}
			fmt.Fprintf(out, "  incremental: %d changed path(s) since %s\n", len(list), req.BaseRef)
		} else {
			fmt.Fprintf(out, "  range %s..HEAD unusable; falling back to a full hash-gated rebuild\n", req.BaseRef)
		}
	}

	// Which namespaces this run synced decides which disappearances are
	// deletions: modules, entries, arch, and repo docs only when a repository
	// was actually scanned; uploads and web pages only when their sources were
	// consulted. A docs-only run that claimed the git namespaces would file a
	// deletion review for every repo-derived source it never looked at.
	var syncedNS []string
	if req.Source.Path != "" {
		syncedNS = append(syncedNS, "module", "entry", "arch", "doc")
	}
	if req.Source.DocsDir != "" {
		syncedNS = append(syncedNS, "doc:upload")
	}
	if len(req.Source.WebURLs) > 0 {
		syncedNS = append(syncedNS, "doc:web")
	}

	breq := BuildRequest{
		RunID:            req.RunID,
		WorkspaceID:      req.WorkspaceID,
		WorkspaceSlug:    req.Source.Slug,
		Trigger:          req.Trigger,
		Connectors:       req.Connectors,
		SyncedNamespaces: syncedNS,
		Ref:              gitRef(rm),
		SourceDir:        req.Source.Path,
		BlobKeys:         req.Source.BlobKeys,
		SkippedKeys:      req.Source.SkippedKeys,
		Map:              wm,
		Router:           router,
		Changes:          changes,
		Force:            req.Force,
		DryRun:           req.DryRun,
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

	// Routes are keyed by the namespaced upload path (diff.UploadOrigin), the
	// same namespace the connector keys the source cache with, so an uploaded
	// document can never collide with a repo file of the same name. A future
	// incremental change source for uploads must emit paths through
	// diff.UploadOrigin to be routable.
	routes := map[string]diff.Key{}
	for _, d := range payload.Docs {
		routes[diff.UploadOrigin(d.Origin)] = diff.Key(d.Key)
	}
	return wm, routes, staging, nil
}

// syncWeb ingests fetched pages through the web connector, mirroring
// syncDocs: the staging directory holds the extracted text the unit inputs
// point at and must outlive the build.
func syncWeb(ctx context.Context, out io.Writer, urls []string) (_ *mapper.WorkspaceMap, _ map[string]diff.Key, staging string, err error) {
	conn, err := connector.Get("web")
	if err != nil {
		return nil, nil, "", err
	}
	fmt.Fprintf(out, "syncing via %s connector: %d url(s)\n", conn.Kind(), len(urls))

	staging, err = os.MkdirTemp("", "kiln-web-")
	if err != nil {
		return nil, nil, "", err
	}

	set, err := conn.Sync(ctx, connector.Config{"urls": urls}, staging)
	if err != nil {
		return nil, nil, staging, err
	}
	payload := webconn.PayloadOf(set)
	if payload == nil {
		return nil, nil, staging, fmt.Errorf("web connector returned no documents payload")
	}

	dm := &docmap.Mapper{}
	wm, err := dm.MapDocs(ctx, staging, payload.Docs)
	if err != nil {
		return nil, nil, staging, err
	}

	fmt.Fprintf(out, "  %d page(s), %d units\n", len(payload.Docs), len(wm.Units))
	for _, s := range payload.Skipped {
		fmt.Fprintf(out, "  skipped %s: %s\n", s.URL, s.Reason)
	}

	routes := map[string]diff.Key{}
	for _, d := range payload.Docs {
		routes[diff.WebOrigin(d.Origin)] = diff.Key(d.Key)
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
	if rm == nil || rm.Git == nil {
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
