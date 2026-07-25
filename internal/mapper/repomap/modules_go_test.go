package repomap

import (
	"reflect"
	"testing"
)

func TestParseGoMod(t *testing.T) {
	tests := []struct {
		name         string
		dir          string
		body         string
		wantPath     string
		wantRequires []string
		wantReplaces []string
	}{
		{
			name: "simple module with direct and indirect requires",
			dir:  ".",
			body: `module github.com/watchtower/watchtower

go 1.24

require (
	github.com/go-chi/chi/v5 v5.2.1
	go.uber.org/zap v1.27.0
)

require (
	github.com/davecgh/go-spew v1.1.2 // indirect
	github.com/fsnotify/fsnotify v1.8.0 // indirect
)
`,
			wantPath:     "github.com/watchtower/watchtower",
			wantRequires: []string{"github.com/go-chi/chi/v5", "go.uber.org/zap"},
		},
		{
			name:     "nested module",
			dir:      "apps/ripple",
			body:     "module github.com/daiwa-zou/ripple\n\ngo 1.24\n",
			wantPath: "github.com/daiwa-zou/ripple",
		},
		{
			// InfraFlux wires shared/ into pilot/ and node/ this way, so these
			// are real intra-repo edges rather than external dependencies.
			name: "local replace becomes an intra-repo edge",
			dir:  "pilot",
			body: `module github.com/InfraFlux/pilot

go 1.24

require github.com/InfraFlux/shared v0.0.0

replace github.com/InfraFlux/shared => ../shared
`,
			wantPath:     "github.com/InfraFlux/pilot",
			wantRequires: []string{"github.com/InfraFlux/shared"},
			wantReplaces: []string{"../shared"},
		},
		{
			name: "remote replace is not a local edge",
			dir:  ".",
			body: `module example.com/x

go 1.24

replace example.com/old => example.com/new v1.2.3
`,
			wantPath: "example.com/x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGoMod(tt.dir, []byte(tt.body))
			if err != nil {
				t.Fatalf("ParseGoMod: %v", err)
			}
			if got.Path != tt.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tt.wantPath)
			}
			if got.Dir != tt.dir {
				t.Errorf("Dir = %q, want %q", got.Dir, tt.dir)
			}
			if !reflect.DeepEqual(got.Requires, tt.wantRequires) {
				t.Errorf("Requires = %v, want %v", got.Requires, tt.wantRequires)
			}
			if !reflect.DeepEqual(got.LocalReplaces, tt.wantReplaces) {
				t.Errorf("LocalReplaces = %v, want %v", got.LocalReplaces, tt.wantReplaces)
			}
		})
	}
}

func TestParseGoModInvalid(t *testing.T) {
	if _, err := ParseGoMod(".", []byte("this is not a go.mod\n{{{")); err == nil {
		t.Error("ParseGoMod accepted invalid input")
	}
}

func TestHasGoMain(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"plain main", "package main\n\nfunc main() {}\n", true},
		{"non-main", "package auth\n", false},
		{"leading comments and blank lines", "// Command foo does things.\n//\n// More.\n\npackage main\n", true},
		{"trailing line comment", "package main // the entry point\n", true},
		{"build tag before package", "//go:build linux\n\npackage main\n", true},
		{"main-ish name is not main", "package mainly\n", false},
		{"empty file", "", false},
		{"only comments", "// nothing here\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasGoMain(tt.content); got != tt.want {
				t.Errorf("hasGoMain(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestGoPackageDirs(t *testing.T) {
	// The watchtower shape: one go.mod, but really five services plus cmd and
	// pkg trees. Without this, a 15k-LOC repo collapses into one useless page.
	files := []string{
		"cmd/auth/main.go",
		"cmd/gateway/main.go",
		"internal/auth/server.go",
		"internal/auth/oidc.go",
		"internal/cluster/client.go",
		"pkg/config/config.go",
		"README.md",
		"ui/src/app.tsx",
	}

	got := goPackageDirs(".", files)
	want := []string{"cmd/auth", "cmd/gateway", "internal/auth", "internal/cluster", "pkg/config"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("goPackageDirs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestGoPackageDirsScopedToModule(t *testing.T) {
	// Files outside the module directory must not be attributed to it.
	files := []string{
		"apps/ripple/main.go",
		"apps/ripple/internal/task/task.go",
		"apps/beacon/main.go",
	}

	got := goPackageDirs("apps/ripple", files)
	want := []string{".", "internal/task"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("goPackageDirs = %v, want %v", got, want)
	}
}
