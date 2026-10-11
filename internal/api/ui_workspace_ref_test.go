package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Source-level guards for the rules that keep the UI safe: it routes by
// workspace id (slugs repeat across orgs, and a bare slug can resolve to
// someone else's bench), builds every workspace URL through
// encodeURIComponent, and stays inside the CSP, which forbids inline styles
// and inline handlers. They read the legacy files from the embed and the
// typed UI's sources (web/) from disk, so they run in the Go suite without
// Node. Behaviour is tested in web/test under vitest; these catch the rules a
// reviewer has to otherwise remember.

func uiSource(t *testing.T, name string) string {
	t.Helper()
	b, err := uiFS.ReadFile("ui/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// webSources reads every .ts/.tsx file under web/src, keyed by its path
// relative to the repository root. Missing web/src is a failure, not an empty
// result: the guards below would otherwise pass by scanning nothing.
func webSources(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..", "web", "src")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(p); ext != ".ts" && ext != ".tsx" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(strings.TrimPrefix(p, filepath.Join("..", "..")+string(filepath.Separator)))] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("reading web/src: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("web/src has no .ts/.tsx files; the guard is out of date")
	}
	return out
}

func TestUIWorkspaceURLsAreEncoded(t *testing.T) {
	interp := regexp.MustCompile(`/workspaces/\$\{([^}]*(?:\([^)]*\))?[^}]*)\}`)
	sources := map[string]string{"app.js": uiSource(t, "app.js"), "sources.js": uiSource(t, "sources.js")}
	for name, src := range webSources(t) {
		sources[name] = src
	}
	for file, src := range sources {
		matches := interp.FindAllStringSubmatch(src, -1)
		// The legacy files build dozens of workspace URLs; finding none means
		// the pattern drifted. A web/src file may simply not build any yet.
		if len(matches) == 0 && !strings.HasPrefix(file, "web/") {
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
	sources := map[string]string{}
	// The served shell is the build output; web/index.html is its source.
	for _, file := range []string{"app.js", "sources.js", "dist/index.html"} {
		sources["internal/api/ui/"+file] = uiSource(t, file)
	}
	shell, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	sources["web/index.html"] = string(shell)
	// In JSX, style={...} is the same inline style attribute once rendered,
	// and a string-valued on*= is an inline handler; onClick={fn} is neither.
	for name, src := range webSources(t) {
		sources[name] = src
	}
	for file, src := range sources {
		if loc := inlineStyle.FindStringIndex(src); loc != nil {
			t.Errorf("%s: inline style attribute: %q", file, src[loc[0]:min(loc[1]+30, len(src))])
		}
		if loc := inlineHandler.FindStringIndex(src); loc != nil {
			t.Errorf("%s: inline event handler: %q", file, src[loc[0]:min(loc[1]+30, len(src))])
		}
	}
}
