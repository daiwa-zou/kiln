package repomap

import (
	"reflect"
	"testing"
)

func TestParsePackageJSON(t *testing.T) {
	body := `{
	  "name": "watchtower-ui",
	  "private": true,
	  "scripts": { "dev": "vite", "build": "tsc && vite build" },
	  "dependencies": { "react": "^19.0.0", "sigma": "^3.0.0" },
	  "devDependencies": { "vite": "^8.0.0", "typescript": "^5.7.0" }
	}`

	got, err := ParsePackageJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}

	if got.Name != "watchtower-ui" {
		t.Errorf("Name = %q, want watchtower-ui", got.Name)
	}
	if !got.Private {
		t.Error("Private = false, want true")
	}

	wantScripts := []string{"build", "dev"}
	if !reflect.DeepEqual(got.Scripts, wantScripts) {
		t.Errorf("Scripts = %v, want %v", got.Scripts, wantScripts)
	}

	// Dev dependencies count as dependencies: a reader cares that a module uses
	// vitest, not which stanza declares it.
	wantDeps := []string{"react", "sigma", "typescript", "vite"}
	if !reflect.DeepEqual(got.Deps, wantDeps) {
		t.Errorf("Deps = %v, want %v", got.Deps, wantDeps)
	}
}

func TestParsePackageJSONBinForms(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "string bin takes the package name",
			body: `{"name":"kiln-mcp","bin":"./dist/index.js"}`,
			want: []string{"kiln-mcp"},
		},
		{
			name: "object bin uses its keys",
			body: `{"name":"tools","bin":{"alpha":"./a.js","beta":"./b.js"}}`,
			want: []string{"alpha", "beta"},
		},
		{
			name: "absent bin yields none",
			body: `{"name":"lib"}`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePackageJSON([]byte(tt.body))
			if err != nil {
				t.Fatalf("ParsePackageJSON: %v", err)
			}
			if !reflect.DeepEqual(got.Bins, tt.want) {
				t.Errorf("Bins = %v, want %v", got.Bins, tt.want)
			}
		})
	}
}

func TestParsePackageJSONWorkspaceForms(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "array form",
			body: `{"name":"root","workspaces":["apps/*","packages/*"]}`,
			want: []string{"apps/*", "packages/*"},
		},
		{
			name: "object form",
			body: `{"name":"root","workspaces":{"packages":["apps/*"]}}`,
			want: []string{"apps/*"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePackageJSON([]byte(tt.body))
			if err != nil {
				t.Fatalf("ParsePackageJSON: %v", err)
			}
			if !reflect.DeepEqual(got.Workspaces, tt.want) {
				t.Errorf("Workspaces = %v, want %v", got.Workspaces, tt.want)
			}
		})
	}
}

func TestParsePackageJSONLocalDeps(t *testing.T) {
	body := `{
	  "name": "app",
	  "dependencies": {
	    "shared": "workspace:*",
	    "vendored": "file:../vendored",
	    "linked": "link:../linked",
	    "react": "^19.0.0"
	  }
	}`

	got, err := ParsePackageJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}

	want := []string{"linked", "shared", "vendored"}
	if !reflect.DeepEqual(got.LocalDeps, want) {
		t.Errorf("LocalDeps = %v, want %v", got.LocalDeps, want)
	}
}

func TestParsePackageJSONDeduplicatesAcrossStanzas(t *testing.T) {
	body := `{"name":"x","dependencies":{"typescript":"^5"},"devDependencies":{"typescript":"^5"}}`

	got, err := ParsePackageJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}
	if want := []string{"typescript"}; !reflect.DeepEqual(got.Deps, want) {
		t.Errorf("Deps = %v, want %v", got.Deps, want)
	}
}

func TestParsePackageJSONInvalid(t *testing.T) {
	if _, err := ParsePackageJSON([]byte(`{"name": broken`)); err == nil {
		t.Error("ParsePackageJSON accepted invalid JSON")
	}
}
