package api

import (
	"regexp"
	"strings"
	"testing"
)

// The UI has no JS test harness, so these are source-level guards for the
// rules that keep it safe: it routes by workspace id (slugs repeat across
// orgs, and a bare slug can resolve to someone else's bench), builds every
// workspace URL through encodeURIComponent, and stays inside the CSP, which
// forbids inline styles and inline handlers.

func uiSource(t *testing.T, name string) string {
	t.Helper()
	b, err := uiFS.ReadFile("ui/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUIWorkspaceURLsAreEncoded(t *testing.T) {
	interp := regexp.MustCompile(`/workspaces/\$\{([^}]*(?:\([^)]*\))?[^}]*)\}`)
	for _, file := range []string{"app.js", "sources.js"} {
		src := uiSource(t, file)
		matches := interp.FindAllStringSubmatch(src, -1)
		if len(matches) == 0 {
			t.Fatalf("%s: found no workspace URLs; the guard is out of date", file)
		}
		for _, m := range matches {
			expr := strings.TrimSpace(m[1])
			// ws / ctx.ws are values already passed through
			// encodeURIComponent at their definition (state.workspace -> ws).
			if expr == "ws" || expr == "ctx.ws" || strings.HasPrefix(expr, "encodeURIComponent(") {
				continue
			}
			t.Errorf("%s: workspace interpolated into a URL without encodeURIComponent: ${%s}", file, expr)
		}
	}

	// The aliases above are only safe if they are encoded where defined.
	for _, file := range []string{"app.js", "sources.js"} {
		for _, m := range regexp.MustCompile(`const ws = ([^;\n]*)`).FindAllStringSubmatch(uiSource(t, file), -1) {
			rhs := m[1]
			if strings.HasPrefix(rhs, "encodeURIComponent(") {
				continue
			}
			// Raw copies are only compared against state.workspace for
			// "raced a switch" checks or encoded at the call site.
			if rhs == "state.workspace" || strings.HasPrefix(rhs, "state.workspace ||") {
				continue
			}
			t.Errorf("%s: unexpected `const ws = %s`", file, rhs)
		}
	}
}

func TestUIRoutesByWorkspaceID(t *testing.T) {
	app := uiSource(t, "app.js")

	// state.workspace holds an id, so it must never be compared with a slug.
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`\.slug\s*===?\s*state\.workspace`),
		regexp.MustCompile(`state\.workspace\s*===?\s*[\w.]*\.slug`),
		regexp.MustCompile(`\.slug\s*===?\s*(slug|id)\b.*loadWorkspace`),
		regexp.MustCompile(`data-slug=`),
		regexp.MustCompile(`dataset\.slug`),
	} {
		if loc := bad.FindStringIndex(app); loc != nil {
			t.Errorf("app.js compares or routes by slug: %q", app[loc[0]:min(loc[1]+40, len(app))])
		}
	}

	// What is remembered across reloads is an id. The only writers of the key
	// are loadWorkspace(id) and bench creation (the id the API returned).
	writes := regexp.MustCompile(`setItem\(WORKSPACE_KEY,\s*([^)]*)\)`).FindAllStringSubmatch(app, -1)
	if len(writes) != 2 {
		t.Fatalf("expected two writers of WORKSPACE_KEY, found %d", len(writes))
	}
	for _, w := range writes {
		if v := strings.TrimSpace(w[1]); v != "id" && v != "created.id" {
			t.Errorf("WORKSPACE_KEY written with %q; it must hold a workspace id", v)
		}
	}

	// A stored non-id (a legacy slug) migrates only through the unambiguous path.
	for _, want := range []string{"pickInitialBench", "hits.length === 1", "UUID_RE.test(stored)"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js lost its legacy-slug migration (%q missing)", want)
		}
	}
}

func TestUIStaysInsideTheCSP(t *testing.T) {
	inlineStyle := regexp.MustCompile(`\sstyle\s*=\s*["'{]`)
	inlineHandler := regexp.MustCompile(`\son[a-z]+\s*=\s*["']`)
	for _, file := range []string{"app.js", "sources.js", "index.html"} {
		src := uiSource(t, file)
		if loc := inlineStyle.FindStringIndex(src); loc != nil {
			t.Errorf("%s: inline style attribute: %q", file, src[loc[0]:min(loc[1]+30, len(src))])
		}
		if loc := inlineHandler.FindStringIndex(src); loc != nil {
			t.Errorf("%s: inline event handler: %q", file, src[loc[0]:min(loc[1]+30, len(src))])
		}
	}
}
