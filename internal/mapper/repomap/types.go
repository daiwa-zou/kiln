package repomap

import "time"

// Module is one manifest-rooted unit of a repository: a Go module, a Cargo
// package, an npm package, or a synthetic grouping where no manifest exists.
type Module struct {
	Slug         string   `json:"slug"`     // "apps-ripple"
	Name         string   `json:"name"`     // module path / package name from the manifest
	Dir          string   `json:"dir"`      // repo-relative; "." for the root
	Kind         string   `json:"kind"`     // go | rust | node | python | proto | generic
	Manifest     string   `json:"manifest"` // "apps/ripple/go.mod"
	Languages    []string `json:"languages,omitempty"`
	Files        []string `json:"files,omitempty"`
	FileCount    int      `json:"fileCount"`
	LOC          int      `json:"loc"`
	EntryPoints  []string `json:"entryPoints,omitempty"`
	Packages     []string `json:"packages,omitempty"`
	DependsOn    []string `json:"dependsOn,omitempty"` // other module slugs
	ExternalDeps []string `json:"externalDeps,omitempty"`
	Docs         []string `json:"docs,omitempty"`
	Hash         string   `json:"hash"`
	// Empty marks a directory with no source files. Recorded so the map stays
	// faithful, but never sent to the LLM -- moneypal/butler is the real case.
	Empty bool `json:"empty,omitempty"`
	// Parent is set on sub-partitions of a large single-manifest module.
	Parent string `json:"parent,omitempty"`
}

// EntryPoint is a way the repository is executed.
type EntryPoint struct {
	Path   string `json:"path"`
	Module string `json:"module"`
	Kind   string `json:"kind"` // go-main | cargo-bin | npm-bin | npm-script | dockerfile | procfile
	Name   string `json:"name"`
}

// DocFile is human-written material found in the repository.
type DocFile struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Kind  string `json:"kind"` // readme | adr | claude-md | changelog | doc
	Hash  string `json:"hash"`
	Bytes int64  `json:"bytes"`
}

// ProtoFile records a protobuf definition without invoking protoc.
type ProtoFile struct {
	Path     string   `json:"path"`
	Package  string   `json:"package,omitempty"`
	Services []string `json:"services,omitempty"`
	Messages []string `json:"messages,omitempty"`
}

// BuildTarget is an entry in a Makefile, Taskfile, Procfile, or Tiltfile.
type BuildTarget struct {
	File string `json:"file"`
	Name string `json:"name"`
}

// ComposeService is a docker-compose service. Environment values are never
// captured -- only keys -- so secrets in a compose file cannot reach the wiki.
type ComposeService struct {
	Name      string   `json:"name"`
	Image     string   `json:"image,omitempty"`
	Build     string   `json:"build,omitempty"`
	Ports     []string `json:"ports,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
	EnvKeys   []string `json:"envKeys,omitempty"`
}

// CommitRef is a single commit in the recent history.
type CommitRef struct {
	SHA     string    `json:"sha"`
	When    time.Time `json:"when"`
	Author  string    `json:"author"`
	Subject string    `json:"subject"`
}

// GitMeta is nil for directories that are not repositories -- a real case, not
// an edge case, so everything downstream branches on this being absent.
type GitMeta struct {
	HeadSHA     string      `json:"headSha"`
	Branch      string      `json:"branch"`
	Remote      string      `json:"remote,omitempty"`
	CommitCount int         `json:"commitCount"`
	Dirty       bool        `json:"dirty"`
	Recent      []CommitRef `json:"recent,omitempty"`
	Churn       []ChurnRec  `json:"churn,omitempty"`
}

// ChurnRec counts how many recent commits touched a file.
type ChurnRec struct {
	Path    string `json:"path"`
	Commits int    `json:"commits"`
}

// LangStat is per-language file and line counts.
type LangStat struct {
	Files int `json:"files"`
	LOC   int `json:"loc"`
}

// RepoMap is the deterministic description of a repository, produced before any
// LLM call and persisted so the next run can diff against it.
type RepoMap struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Root          string              `json:"root"`
	Slug          string              `json:"slug"`
	GeneratedAt   time.Time           `json:"generatedAt"`
	Git           *GitMeta            `json:"git,omitempty"`
	Modules       []Module            `json:"modules"`
	EntryPoints   []EntryPoint        `json:"entryPoints,omitempty"`
	Docs          []DocFile           `json:"docs,omitempty"`
	Protos        []ProtoFile         `json:"protos,omitempty"`
	BuildTargets  []BuildTarget       `json:"buildTargets,omitempty"`
	Services      []ComposeService    `json:"services,omitempty"`
	Languages     map[string]LangStat `json:"languages,omitempty"`
	Hash          string              `json:"hash"`
}

// SchemaVersion is bumped when RepoMap's shape changes in a way that should
// invalidate persisted maps.
const SchemaVersion = 1
