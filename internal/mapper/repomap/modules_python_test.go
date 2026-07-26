package repomap

import (
	"reflect"
	"testing"
)

func TestParsePyprojectPEP621(t *testing.T) {
	body := []byte(`
[project]
name = "sawmill"
dependencies = [
  "requests[socks]>=2.28,<3",
  "pydantic ~= 2.0",
  "click",
]

[project.scripts]
sawmill = "sawmill.cli:main"
feller = "sawmill.feller:main"
`)
	m, err := ParsePyproject(body)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "sawmill" {
		t.Errorf("name = %q", m.Name)
	}
	// Specifiers, extras, and spaces are stripped to bare package names.
	if want := []string{"click", "pydantic", "requests"}; !reflect.DeepEqual(m.Deps, want) {
		t.Errorf("deps = %v, want %v", m.Deps, want)
	}
	if want := []string{"feller", "sawmill"}; !reflect.DeepEqual(m.Scripts, want) {
		t.Errorf("scripts = %v, want %v", m.Scripts, want)
	}
}

func TestParsePyprojectPoetry(t *testing.T) {
	body := []byte(`
[tool.poetry]
name = "old-style"

[tool.poetry.dependencies]
python = "^3.11"
httpx = "^0.27"
numpy = { version = ">=1.26", optional = true }

[tool.poetry.scripts]
old = "old_style.main:run"
`)
	m, err := ParsePyproject(body)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "old-style" {
		t.Errorf("name = %q", m.Name)
	}
	// The interpreter requirement is not a library dependency.
	if want := []string{"httpx", "numpy"}; !reflect.DeepEqual(m.Deps, want) {
		t.Errorf("deps = %v, want %v", m.Deps, want)
	}
	if want := []string{"old"}; !reflect.DeepEqual(m.Scripts, want) {
		t.Errorf("scripts = %v, want %v", m.Scripts, want)
	}
}

func TestParsePyprojectMalformed(t *testing.T) {
	if _, err := ParsePyproject([]byte("not = [valid")); err == nil {
		t.Error("malformed toml parsed without error")
	}
	// A tool-config-only pyproject (no [project], no poetry) is legal and
	// yields an empty manifest, not an error.
	m, err := ParsePyproject([]byte("[tool.ruff]\nline-length = 100\n"))
	if err != nil || m.Name != "" || len(m.Deps) != 0 {
		t.Errorf("tool-only pyproject: %+v, %v", m, err)
	}
}

func TestParseTaskfile(t *testing.T) {
	body := []byte(`version: '3'

vars:
  BIN: kiln

tasks:
  build:
    cmds:
      - go build ./...
  test:
    desc: run the suite
    cmds:
      - go test ./...
  db:up:
    cmds:
      - docker start pg
`)
	got := ParseTaskfile("Taskfile.yml", body)
	names := namesOf(got)
	want := []string{"build", "test", "db:up"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tasks = %v, want %v", names, want)
	}
	// vars keys must not be misread as tasks; nested keys (cmds, desc) must
	// not either.
	for _, n := range names {
		if n == "BIN" || n == "cmds" || n == "desc" {
			t.Errorf("non-task %q captured", n)
		}
	}
}

func TestParseTiltfile(t *testing.T) {
	body := []byte(`
docker_build('kiln-image', '.')
k8s_resource('api', port_forwards=8080)
local_resource(
    'worker',
    serve_cmd='./bin/kiln worker')
k8s_resource('api', labels=['dup'])
`)
	got := ParseTiltfile("Tiltfile", body)
	names := namesOf(got)
	want := []string{"kiln-image", "api", "worker"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("resources = %v, want %v (deduped, in order)", names, want)
	}
}

func namesOf(ts []BuildTarget) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}
