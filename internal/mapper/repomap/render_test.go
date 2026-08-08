package repomap

import (
	"strings"
	"testing"
)

func TestRenderProtos(t *testing.T) {
	// The summary is what the agent cites, so each proto must surface its
	// package and services, and a bare file must not drag empty parens along.
	rm := &RepoMap{
		Modules: []Module{{Slug: "root", Dir: "."}},
		Protos: []ProtoFile{
			{Path: "api/v1/wiki.proto", Package: "kiln.v1", Services: []string{"Wiki", "Search"}},
			{Path: "api/internal/raw.proto"},
		},
	}

	out := rm.Render()
	if !strings.Contains(out, "## Protobuf definitions") {
		t.Fatalf("no protos section:\n%s", out)
	}
	if !strings.Contains(out, "`api/v1/wiki.proto` (package `kiln.v1`) — services: Wiki, Search") {
		t.Errorf("annotated proto line missing:\n%s", out)
	}
	if !strings.Contains(out, "- `api/internal/raw.proto`\n") {
		t.Errorf("bare proto rendered with empty annotations:\n%s", out)
	}
}

func TestRenderOmitsEmptySections(t *testing.T) {
	rm := &RepoMap{Modules: []Module{{Slug: "root", Dir: "."}}}
	out := rm.Render()
	for _, heading := range []string{"## Protobuf definitions", "## Entry points", "## Compose services"} {
		if strings.Contains(out, heading) {
			t.Errorf("empty section %q rendered", heading)
		}
	}
}
