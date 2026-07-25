package repomap

import (
	"reflect"
	"testing"
)

func TestParseCargoToml(t *testing.T) {
	// The sawmill shape: two [[bin]] targets, so entry points cannot be
	// assumed to be one per package.
	body := `[package]
name = "sawmill"
version = "0.1.0"
edition = "2021"

[[bin]]
name = "sawmill"
path = "src/main.rs"

[[bin]]
name = "feller"
path = "src/bin/feller.rs"

[dependencies]
rdkafka = { version = "0.37", features = ["tokio"] }
axum    = { version = "0.7", features = ["macros"] }
serde   = "1"
shared  = { path = "../shared" }
`

	got, err := ParseCargoToml([]byte(body))
	if err != nil {
		t.Fatalf("ParseCargoToml: %v", err)
	}

	if got.Name != "sawmill" {
		t.Errorf("Name = %q, want sawmill", got.Name)
	}
	if got.IsWorkspaceOnly {
		t.Error("IsWorkspaceOnly = true, want false for a manifest with [package]")
	}

	wantBins := []CargoBin{
		{Name: "sawmill", Path: "src/main.rs"},
		{Name: "feller", Path: "src/bin/feller.rs"},
	}
	if !reflect.DeepEqual(got.Bins, wantBins) {
		t.Errorf("Bins = %v, want %v", got.Bins, wantBins)
	}

	wantDeps := []string{"axum", "rdkafka", "serde", "shared"}
	if !reflect.DeepEqual(got.Deps, wantDeps) {
		t.Errorf("Deps = %v, want %v", got.Deps, wantDeps)
	}

	// A path dependency is an intra-repo edge, not an external crate.
	wantLocal := []string{"../shared"}
	if !reflect.DeepEqual(got.LocalDeps, wantLocal) {
		t.Errorf("LocalDeps = %v, want %v", got.LocalDeps, wantLocal)
	}
}

func TestParseCargoTomlVirtualWorkspace(t *testing.T) {
	body := `[workspace]
members = ["crates/*", "tools/cli"]
`

	got, err := ParseCargoToml([]byte(body))
	if err != nil {
		t.Fatalf("ParseCargoToml: %v", err)
	}
	if !got.IsWorkspaceOnly {
		t.Error("IsWorkspaceOnly = false, want true for a manifest with no [package]")
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want empty", got.Name)
	}
	want := []string{"crates/*", "tools/cli"}
	if !reflect.DeepEqual(got.Members, want) {
		t.Errorf("Members = %v, want %v", got.Members, want)
	}
}

func TestParseCargoTomlLib(t *testing.T) {
	body := "[package]\nname = \"shared\"\n\n[lib]\nname = \"shared\"\npath = \"src/lib.rs\"\n"

	got, err := ParseCargoToml([]byte(body))
	if err != nil {
		t.Fatalf("ParseCargoToml: %v", err)
	}
	if !got.HasLib {
		t.Error("HasLib = false, want true")
	}
	if len(got.Bins) != 0 {
		t.Errorf("Bins = %v, want none", got.Bins)
	}
}

func TestParseCargoTomlInvalid(t *testing.T) {
	if _, err := ParseCargoToml([]byte("[package\nname = broken")); err == nil {
		t.Error("ParseCargoToml accepted invalid TOML")
	}
}

func TestRustModPaths(t *testing.T) {
	files := []string{
		"src/main.rs",
		"src/bin/feller.rs",
		"src/db/mod.rs",
		"src/db/query.rs",
		"src/pipeline/stage.rs",
		"src/http/router.rs",
		"web/src/app.tsx",
	}

	got := rustModPaths(".", files)
	want := []string{"src/bin", "src/db", "src/pipeline", "src/http"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("rustModPaths = %v, want %v", got, want)
	}
}

func TestRustModPathsIgnoresTopLevelSrcFiles(t *testing.T) {
	// A file directly in src/ is part of the crate root, not its own module dir.
	got := rustModPaths(".", []string{"src/main.rs", "src/lib.rs"})
	if len(got) != 0 {
		t.Errorf("rustModPaths = %v, want none", got)
	}
}
