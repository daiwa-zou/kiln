package jobs

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/connector"
	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	"github.com/daiwa-zou/kiln/internal/connector/upload"
	webconn "github.com/daiwa-zou/kiln/internal/connector/web"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/extract"
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
	case diff.NamespaceUpload:
		return c.Upload
	case diff.NamespaceWeb:
		return c.Web
	}
	return c.Git
}

// SourceSpec names the material one build consumes.
type SourceSpec struct {
	// Path is the repository directory the git connector scans.
	Path string
	// GitToken is the credential Path was cloned with, when it was cloned from
	// a private remote. Incremental builds need it to deepen that clone or
	// fetch the base commit; without it the fetch is anonymous and fails, and
	// every push falls back to a full rebuild. It is never logged or persisted.
	GitToken string `json:"-"`
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

	// OnDocSkips, when set, receives the documents the sync could not read,
	// once, after every source has synced and before anything is built. It is
	// the typed counterpart of the "skipped ..." lines Progress narrates: the
	// CLI reads those, a server build has no Progress, and without this hook a
	// skipped upload was discarded there. It is called even when nothing was
	// skipped, so a sink can clear what an earlier build recorded.
	OnDocSkips func(ctx context.Context, skips []DocSkip)
}

// DocSkip is one document a sync left out, typed for a sink rather than
// narrated for a person.
//
// Raw is the extractor's own text and is about a user's file: it is for an
// operator log after sanitising, never for persistence or a prompt. Class is
// the fact to persist. Subject for a fetched page is reduced to scheme and
// host, so a query string or userinfo in a configured URL never leaves the
// connector.
type DocSkip struct {
	// Kind is the connector that skipped it: connector.KindUpload or
	// connector.KindWeb.
	Kind string
	// Subject is the stored path of an upload, or the scheme and host of a
	// fetched page.
	Subject string
	// Class is one of the upload package's Skip* tokens.
	Class string
	// Raw is the first line of the reason, with URLs reduced to scheme and
	// host.
	Raw string
	// Partial marks a document that was ingested with something missing
	// (its figures); it still produced a page.
	Partial bool
}

// docSkipped is one skipped item in the two forms it is reported in: the
// (subject, reason) pair the CLI prints verbatim, and the typed skip the
// sink receives, whose subject and text are already reduced for a log.
type docSkipped struct {
	subject, reason string
	skip            DocSkip
}

// deliverableSkips is what a sink should hear about from one sync: nothing
// about a document that produced a page (a partial skip, or a skip followed
// by a successful read of the same path), and one entry per subject, the
// last one. A sink that persisted a partial skip would mark a document with
// a page on the bench as unread.
func deliverableSkips(skipped []docSkipped, docs []docmap.Doc) []DocSkip {
	produced := make(map[string]bool, len(docs))
	for _, d := range docs {
		produced[d.Origin] = true
	}
	var out []DocSkip
	index := map[string]int{}
	for _, s := range skipped {
		if s.skip.Partial || produced[s.subject] {
			continue
		}
		if i, ok := index[s.skip.Subject]; ok {
			out[i] = s.skip
			continue
		}
		index[s.skip.Subject] = len(out)
		out = append(out, s.skip)
	}
	return out
}

// urlPattern finds the URLs in free text, for RedactURLs.
var urlPattern = regexp.MustCompile(`https?://[^\s"'<>()\[\]]+`)

// RedactURLs reduces every URL in s to its scheme and host, dropping the
// path, query and userinfo. A configured web URL can carry a token in its
// query, and a connector's error text quotes the URL it failed on; both
// would otherwise reach a log line.
func RedactURLs(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "<url>"
		}
		return u.Scheme + "://" + u.Host
	})
}

// material is one call's synced sources: the map every later step reads, the
// router builds attribute changed paths with, the repository map when there
// was a repository, and the staging directories that must outlive the sync.
//
// Close is separate from the sync because unit prompts are assembled from the
// staged text: releasing it inside materialize would leave the map pointing at
// files that no longer exist.
type material struct {
	Map    *mapper.WorkspaceMap
	Router diff.Router
	Repo   *repomap.RepoMap

	// DocNames maps an uploaded document's stored path to the name ingest
	// derived for it, having read the text. Only uploads appear here: a
	// repository file or a fetched page has no row to rename.
	DocNames map[string]string

	// Figures are the pictures recovered from each document, keyed by unit
	// key. Empty for a repository-only build: source code has no figures.
	Figures map[string][]extract.Figure

	// Skips are the documents the document sources could not read, for
	// ExecuteRequest.OnDocSkips. Already reduced to what a sink should hear
	// (see deliverableSkips).
	Skips []DocSkip

	staging []string
}

// Close removes whatever the sync staged. Safe on a partially built material,
// which is what materialize closes on its own failure paths: a connector that
// staged text and then failed to map it has still made a directory that needs
// removing.
func (m *material) Close() {
	if m == nil {
		return
	}
	for _, dir := range m.staging {
		os.RemoveAll(dir)
	}
	m.staging = nil
}

// materialize syncs every configured source through the connector registry and
// merges them into one map. Shared by builds and by research so a question is
// answered against exactly the material a build would have read -- a second
// acquisition path would eventually answer questions about a different corpus
// than the one the pages were written from.
//
// A failure releases its own staging and answers nil, so the caller's Close is
// only ever reached with material worth closing. A connector that staged text
// and then failed to map it has still made a directory that needs removing,
// which is why cleanup cannot simply be left to a caller that got no material.
func materialize(ctx context.Context, out io.Writer, src SourceSpec) (*material, error) {
	m := &material{
		Map:    &mapper.WorkspaceMap{SchemaVersion: 1},
		Router: diff.Router{ModuleDirs: map[string]diff.Key{}, DocPaths: map[string]diff.Key{}, Cosmetic: cosmeticPath},
	}
	fail := func(err error) (*material, error) {
		m.Close()
		return nil, err
	}

	// The repository is optional: a bench fed only by documents or web pages
	// builds from those alone. When present, sync goes through the connector
	// registry rather than calling the scanner directly, so the abstraction is
	// exercised by the path that uses it rather than assumed to work.
	if src.Path != "" {
		conn, err := connector.Get(connector.KindGit)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "syncing via %s connector: %s\n", conn.Kind(), src.Path)

		set, err := conn.Sync(ctx, connector.Config{"path": src.Path, "slug": src.Slug}, "")
		if err != nil {
			return fail(err)
		}

		// Routing and prompt grounding need the module graph, which a flat item
		// list cannot express. The git connector carried it on the same sync.
		rm := gitconn.MapOf(set)
		if rm == nil {
			return fail(fmt.Errorf("git connector returned no repository map"))
		}
		m.Repo = rm
		m.Map = rm.ToWorkspaceMap()
		fmt.Fprintf(out, "  %d modules, %d units, %d edges\n", len(rm.Modules), len(m.Map.Units), len(m.Map.Edges))
		if rm.Git != nil && rm.Git.HeadSHA != "" {
			fmt.Fprintf(out, "  at %s on %s\n", shortRef(rm.Git.HeadSHA), rm.Git.Branch)
		} else {
			fmt.Fprintln(out, "  not a git repository; change detection uses content hashes")
		}

		m.Router = routerFor(rm)
	}

	// A second connector's material merges into the same map, so code and
	// documents produce one wiki whose pages can link across the boundary
	// rather than two wikis that cannot see each other.
	if src.DocsDir != "" {
		doc, err := syncDocs(ctx, out, src.DocsDir)
		if doc.Staging != "" {
			m.staging = append(m.staging, doc.Staging)
		}
		if err != nil {
			return fail(err)
		}
		if err := m.absorb(src.Path, doc); err != nil {
			return fail(err)
		}
	}

	// Web pages merge the same way: a third source kind, one wiki.
	if len(src.WebURLs) > 0 {
		web, err := syncWeb(ctx, out, src.WebURLs)
		if web.Staging != "" {
			m.staging = append(m.staging, web.Staging)
		}
		if err != nil {
			return fail(err)
		}
		if err := m.absorb(src.Path, web); err != nil {
			return fail(err)
		}
	}

	return m, nil
}

// absorb merges one document source's sync into the material: its units into
// the map, its documents and their sections into the router, and its figures
// and document names into the lists the pipeline reads. Every document source
// goes through here so none can drift from the others -- they used to be two
// copies, one of which replaced the router's sections and the figures where
// the other merged into them, and that only worked because the documents block
// happened to run first.
//
// root is the repository directory the map is merged against, empty when
// there is none.
func (m *material) absorb(root string, s docSync) error {
	merged, err := mapper.Merge(root, m.Map, s.Map)
	if err != nil {
		return err
	}
	m.Map = merged
	maps.Copy(m.Router.DocPaths, s.Routes)

	// Section units have no path of their own; register them under their
	// parent so routing a document also routes its chapters.
	if m.Router.DocSections == nil {
		m.Router.DocSections = map[diff.Key][]diff.Key{}
	}
	maps.Copy(m.Router.DocSections, sectionsByParent(s.Map))

	if len(s.Names) > 0 {
		if m.DocNames == nil {
			m.DocNames = map[string]string{}
		}
		maps.Copy(m.DocNames, s.Names)
	}

	// Figures are carried generically even for sources that recover none. A
	// fetched page's images are remote URLs rather than embedded bytes, and
	// retrieving them would have to go back through the connector's address
	// guard as a second class of request; until that exists the web connector
	// recovers no figures, and merging here keeps the seam ready rather than
	// pretending the case is handled.
	if len(s.Figures) > 0 {
		if m.Figures == nil {
			m.Figures = map[string][]extract.Figure{}
		}
		maps.Copy(m.Figures, s.Figures)
	}
	m.Skips = append(m.Skips, s.Skips...)
	return nil
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

	mat, err := materialize(ctx, out, req.Source)
	if err != nil {
		return nil, err
	}
	defer mat.Close()
	wm, router, rm := mat.Map, mat.Router, mat.Repo

	// Before the build rather than after: what was skipped is a fact about
	// extraction, true whatever the build goes on to do, and a build that
	// fails or is interrupted should not take the record of it down too.
	if req.OnDocSkips != nil {
		req.OnDocSkips(ctx, mat.Skips)
	}

	// Without a base ref, every unit is a candidate and the pipeline's hash
	// gate decides — a repeat build still costs nothing. With one (webhook
	// runs, or build --since), the router narrows the plan to the units the
	// commit range actually touched, which is what suppresses cosmetic churn
	// and keeps plan previews honest on big repositories.
	changes := diff.ChangeSet{FullRebuild: true}
	if req.BaseRef != "" && req.Source.Path != "" {
		head := gitconn.HeadRef(ctx, req.Source.Path)
		if list, ok := gitconn.DiffRange(ctx, req.Source.Path, req.Source.GitToken, req.BaseRef, head); ok && head != "" {
			changes = diff.ChangeSet{FromRef: req.BaseRef, ToRef: head, Changes: list}
			fmt.Fprintf(out, "  incremental: %d changed path(s) since %s\n", len(list), shortRef(req.BaseRef))
		} else {
			fmt.Fprintf(out, "  range %s..HEAD unusable; falling back to a full hash-gated rebuild\n", shortRef(req.BaseRef))
		}
	}

	// Which namespaces this run synced decides which disappearances are
	// deletions: modules, entries, arch, and repo docs only when a repository
	// was actually scanned; uploads and web pages only when their sources were
	// consulted. A docs-only run that claimed the git namespaces would file a
	// deletion review for every repo-derived source it never looked at.
	var syncedNS []string
	if req.Source.Path != "" {
		syncedNS = append(syncedNS, diff.PrefixModule, diff.PrefixEntry, diff.PrefixArch, diff.PrefixDoc)
	}
	if req.Source.DocsDir != "" {
		syncedNS = append(syncedNS, diff.NamespaceUpload)
	}
	if len(req.Source.WebURLs) > 0 {
		syncedNS = append(syncedNS, diff.NamespaceWeb)
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
		DocumentNames:    mat.DocNames,
		Figures:          mat.Figures,
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

// docSource names the facts on which the upload and web ingests differ. Both
// are the same procedure -- fetch through a connector, map with docmap, report
// skips, build routes -- and syncDocSource is that procedure written once.
type docSource struct {
	kind    string           // connector to fetch through
	subject string           // what is being synced, for the log line
	prefix  string           // staging directory prefix
	noun    string           // what one unit of material is called in output
	config  connector.Config // connector-specific location of the material
	// renames says a doc's Origin is the path a stored row is keyed on, so the
	// title ingest derived is worth writing back to that row. True for uploads
	// only: a repository file or a fetched page has no row to rename.
	renames bool
	// origin namespaces a doc's origin into the change-routing key space
	// (diff.UploadOrigin / diff.WebOrigin).
	origin func(string) string
	// payload reads the connector's native result: the docs to map, and the
	// skipped items, each as the line to print and the skip to deliver.
	payload func(*connector.SourceSet) ([]docmap.Doc, []docSkipped, bool)
}

// docSync is what one document ingest produced. A struct rather than a fifth
// and sixth return value: the procedure already handed back four things, and
// figures made the positional form unreadable at the call site.
type docSync struct {
	Map    *mapper.WorkspaceMap
	Routes map[string]diff.Key
	// Names is what ingest worked out each document is called, keyed by the
	// path its row is stored under.
	Names map[string]string
	// Figures are the pictures recovered from each document, keyed by unit
	// key. Empty when the connector recovered none.
	Figures map[string][]extract.Figure
	// Staging holds the extracted text the unit inputs point at. It outlives
	// the sync -- prompts are assembled from it -- so the caller owns removing
	// it.
	Staging string
	// Skips are the documents this sync could not read, reduced to what a
	// sink should hear (deliverableSkips). The same skips were narrated to the
	// progress writer in full.
	Skips []DocSkip
}

// syncDocSource ingests documents through one connector and maps them with
// docmap, returning the map and the routes to add to change routing.
//
// The returned staging directory holds the extracted text the unit inputs point
// at. It must outlive the build -- prompts are assembled from it -- so the
// caller owns removing it rather than a defer here.
func syncDocSource(ctx context.Context, out io.Writer, src docSource) (res docSync, err error) {
	conn, err := connector.Get(src.kind)
	if err != nil {
		return res, err
	}
	fmt.Fprintf(out, "syncing via %s connector: %s\n", conn.Kind(), src.subject)

	res.Staging, err = os.MkdirTemp("", src.prefix)
	if err != nil {
		return res, err
	}

	set, err := conn.Sync(ctx, src.config, res.Staging)
	if err != nil {
		return res, err
	}
	docs, skipped, ok := src.payload(set)
	if !ok {
		return res, fmt.Errorf("%s connector returned no documents payload", src.kind)
	}

	// Sections are staged beside the documents, so Close removes them with the
	// rest of res.Staging; their unit inputs resolve against that directory.
	dm := &docmap.Mapper{StageSections: true}
	wm, err := dm.MapDocs(ctx, res.Staging, docs)
	if err != nil {
		return res, err
	}

	fmt.Fprintf(out, "  %d %s, %d units\n", len(docs), src.noun, len(wm.Units))
	// Skips are reported rather than swallowed: a folder that silently ingested
	// half its files would look like a working build.
	for _, s := range skipped {
		fmt.Fprintf(out, "  skipped %s: %s\n", s.subject, s.reason)
	}
	res.Skips = deliverableSkips(skipped, docs)

	// Routes are keyed by the namespaced origin, the same namespace the
	// connector keys the source cache with, so an ingested document can never
	// collide with a repo file of the same name. A future incremental change
	// source for either connector must emit paths through the same origin
	// function to be routable.
	routes := map[string]diff.Key{}
	names := map[string]string{}
	figures := map[string][]extract.Figure{}
	for _, d := range docs {
		routes[src.origin(d.Origin)] = diff.Key(d.Key)
		// Keyed on Origin, the path the file is stored under, because that is
		// what the row is keyed on. Path here is the staged copy, which exists
		// only for the length of this run.
		if src.renames && d.Origin != "" && d.Title != "" {
			names[d.Origin] = d.Title
		}
		if len(d.Figures) > 0 {
			figures[d.Key] = d.Figures
		}
	}

	res.Map, res.Routes, res.Names, res.Figures = wm, routes, names, figures
	return res, nil
}

// syncDocs ingests a documents directory through the upload connector.
func syncDocs(ctx context.Context, out io.Writer, dir string) (docSync, error) {
	absDocs, err := filepath.Abs(dir)
	if err != nil {
		return docSync{}, err
	}
	return syncDocSource(ctx, out, docSource{
		kind:    connector.KindUpload,
		subject: absDocs,
		prefix:  "kiln-docs-",
		noun:    "document(s)",
		config:  connector.Config{"path": absDocs},
		renames: true,
		origin:  diff.UploadOrigin,
		payload: uploadPayload,
	})
}

// uploadPayload reads the upload connector's result. The skip's Raw is the
// whole reason: an extractor names the file it failed on, and that path is
// the staging directory's, which is the sink's to strip.
func uploadPayload(set *connector.SourceSet) ([]docmap.Doc, []docSkipped, bool) {
	p := upload.PayloadOf(set)
	if p == nil {
		return nil, nil, false
	}
	skipped := make([]docSkipped, 0, len(p.Skipped))
	for _, s := range p.Skipped {
		skipped = append(skipped, docSkipped{
			subject: s.Path, reason: s.Reason,
			skip: DocSkip{
				Kind: connector.KindUpload, Subject: s.Path, Class: s.Class,
				Raw: RedactURLs(firstLine(s.Reason)), Partial: s.Partial,
			},
		})
	}
	return p.Docs, skipped, true
}

// syncWeb ingests fetched pages through the web connector.
func syncWeb(ctx context.Context, out io.Writer, urls []string) (docSync, error) {
	return syncDocSource(ctx, out, docSource{
		kind:    connector.KindWeb,
		subject: fmt.Sprintf("%d url(s)", len(urls)),
		prefix:  "kiln-web-",
		noun:    "page(s)",
		config:  connector.Config{"urls": urls},
		origin:  diff.WebOrigin,
		payload: webPayload,
	})
}

// webPayload reads the web connector's result. The web connector has no
// classes of its own: a page that could not be fetched or read is one failure
// kind to the bench, so every skip is an extractor error. The URL is reduced
// to scheme and host in both the subject and the text, because a configured
// URL may carry a token in its query.
func webPayload(set *connector.SourceSet) ([]docmap.Doc, []docSkipped, bool) {
	p := webconn.PayloadOf(set)
	if p == nil {
		return nil, nil, false
	}
	skipped := make([]docSkipped, 0, len(p.Skipped))
	for _, s := range p.Skipped {
		skipped = append(skipped, docSkipped{
			subject: s.URL, reason: s.Reason,
			skip: DocSkip{
				Kind: connector.KindWeb, Subject: RedactURLs(s.URL), Class: upload.SkipExtractorError,
				Raw: RedactURLs(firstLine(s.Reason)),
			},
		})
	}
	return p.Docs, skipped, true
}

// firstLine is the part of an error that belongs on one log line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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

// gitRef is the full commit id a run records. Abbreviating it here would store a
// base the next incremental build cannot fetch from a remote.
func gitRef(rm *repomap.RepoMap) string {
	if rm == nil || rm.Git == nil {
		return ""
	}
	return rm.Git.HeadSHA
}

func shortRef(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
