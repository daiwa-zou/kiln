package publish

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/wiki"
)

func testPage(path, slug, title, body string) wiki.Page {
	return wiki.Page{
		Path: path, Slug: slug,
		Meta: wiki.Frontmatter{
			Type:  wiki.PageType(strings.TrimSuffix(filepath.Dir(path), "s")),
			Title: title, Created: "2026-08-14", Updated: "2026-08-14",
		},
		Body: body,
	}
}

func TestTreeRendersPagesAndArtifacts(t *testing.T) {
	tree := Tree(Input{
		Bench: "Demo Bench",
		Pages: []wiki.Page{
			testPage("concepts/dispatch.md", "dispatch", "Dispatch", "# Dispatch\n\nHow work moves.\n"),
		},
		Index:    "# Index\n\n- Dispatch\n",
		Overview: "# Overview\n\nA bench.\n",
		Log:      "# Log\n\n2026-08-14 build\n",
	})

	for _, want := range []string{"concepts/dispatch.md", "index.md", "overview.md", "log.md", "README.md"} {
		if _, ok := tree[want]; !ok {
			t.Errorf("tree is missing %s: %v", want, SortedPaths(tree))
		}
	}
	// Frontmatter survives, so a published page is still a kiln page.
	page := string(tree["concepts/dispatch.md"])
	if !strings.HasPrefix(page, "---\n") || !strings.Contains(page, "title: Dispatch") {
		t.Errorf("page lost its frontmatter:\n%s", page)
	}
	if !strings.Contains(page, "# Dispatch") {
		t.Errorf("page lost its body:\n%s", page)
	}
	// The README warns that edits here are pointless, which is the one thing a
	// visitor most needs to know.
	readme := string(tree["README.md"])
	if !strings.Contains(readme, "written by a machine") {
		t.Errorf("README does not say the files are generated:\n%s", readme)
	}
	if !strings.Contains(readme, "Demo Bench") {
		t.Errorf("README does not name the bench:\n%s", readme)
	}
}

// TestTreeRewritesFigureReferences is the difference between a wiki that reads
// on GitHub and one full of literal `figure:` text.
func TestTreeRewritesFigureReferences(t *testing.T) {
	tree := Tree(Input{
		Pages: []wiki.Page{
			testPage("concepts/revenue.md", "revenue", "Revenue",
				"# Revenue\n\n![Q4 by region](figure:abc123)\n"),
			// A reserved artifact sits at the root, so its relative path to the
			// same figure differs.
			testPage("entities/madrigal.md", "madrigal", "Madrigal",
				"# Madrigal\n\n![Unknown](figure:missing)\n"),
		},
		Figures: []Figure{
			{ID: "abc123", ContentType: "image/png", Data: []byte("png-bytes")},
			{ID: "unused", ContentType: "image/png", Data: []byte("never-referenced")},
		},
	})

	page := string(tree["concepts/revenue.md"])
	if !strings.Contains(page, "![Q4 by region](../figures/abc123.png)") {
		t.Errorf("figure reference was not rewritten to a checked-in path:\n%s", page)
	}
	if _, ok := tree["figures/abc123.png"]; !ok {
		t.Errorf("the referenced figure was not written: %v", SortedPaths(tree))
	}
	// Only referenced figures ship: a repository is for browsing, and every
	// picture the bench ever held would bury the handful that are used.
	if _, ok := tree["figures/unused.png"]; ok {
		t.Error("an unreferenced figure was published")
	}
	// An id with no figure keeps its reference form rather than becoming a
	// path to a file that does not exist.
	if !strings.Contains(string(tree["entities/madrigal.md"]), "(figure:missing)") {
		t.Error("an unknown figure reference was rewritten to a broken path")
	}
}

func TestTreeSkipsEmptyArtifacts(t *testing.T) {
	tree := Tree(Input{Pages: []wiki.Page{
		testPage("concepts/a.md", "a", "A", "# A\n"),
	}})

	for _, name := range []string{"index.md", "overview.md", "log.md"} {
		if _, ok := tree[name]; ok {
			t.Errorf("%s was published blank", name)
		}
	}
}

func TestCleanPrefix(t *testing.T) {
	// A leading slash is noise in a repository-relative prefix, not an
	// absolute path: someone typing "/wiki" means the wiki directory at the
	// repository root, and refusing that would be pedantry.
	ok := map[string]string{
		"":            "",
		"wiki":        "wiki",
		"/wiki/":      "wiki",
		"/docs":       "docs",
		"docs/wiki":   "docs/wiki",
		"./docs/wiki": "docs/wiki",
	}
	for in, want := range ok {
		got, err := cleanPrefix(in)
		if err != nil || got != want {
			t.Errorf("cleanPrefix(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../escape", "/../escape", ".git", ".git/hooks", `back\slash`} {
		if _, err := cleanPrefix(bad); err == nil {
			t.Errorf("cleanPrefix(%q) was accepted", bad)
		}
	}
}

func TestSafeRelPathRefusesEscapes(t *testing.T) {
	for _, bad := range []string{"../outside.md", "/etc/passwd", ".git/config", "", "."} {
		if _, err := safeRelPath(bad); err == nil {
			t.Errorf("safeRelPath(%q) was accepted", bad)
		}
	}
	if got, err := safeRelPath("concepts/a.md"); err != nil || got != "concepts/a.md" {
		t.Errorf("safeRelPath rejected an ordinary page: %q %v", got, err)
	}
}

// --- push, against a real local repository -----------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=test", "-c", "user.email=test@example.com",
	}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// bareRepo creates an empty upstream to push into, so the push path is
// exercised end to end without touching the network.
func bareRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := filepath.Join(t.TempDir(), "upstream.git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(dir), "init", "--bare", "--quiet", "-b", "main", dir)
	return dir
}

// checkout clones the upstream so a test can read what was actually pushed.
func checkout(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "clone", "--quiet", remote, "work")
	return filepath.Join(dir, "work")
}

func pushTree(t *testing.T, remote, prefix, message string, files map[string][]byte) (Result, error) {
	t.Helper()
	return Push(context.Background(), PushOptions{
		Remote: remote, Branch: "main", Prefix: prefix,
		Files: files, Message: message, allowLocalRemote: true,
	})
}

func TestPushCreatesTheBranchOnAnEmptyRepository(t *testing.T) {
	remote := bareRepo(t)

	res, err := pushTree(t, remote, "wiki", "Publish wiki", map[string][]byte{
		"index.md":             []byte("# Index\n"),
		"concepts/dispatch.md": []byte("# Dispatch\n"),
	})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Commit == "" || res.Files != 2 {
		t.Errorf("result = %+v", res)
	}

	work := checkout(t, remote)
	for _, want := range []string{"wiki/index.md", "wiki/concepts/dispatch.md"} {
		if _, err := os.Stat(filepath.Join(work, want)); err != nil {
			t.Errorf("%s is not in the pushed tree: %v", want, err)
		}
	}
}

// TestPushRemovesPagesThatAreGone is why the subtree is replaced rather than
// merged: a page deleted in kiln has to leave the repository too.
func TestPushRemovesPagesThatAreGone(t *testing.T) {
	remote := bareRepo(t)

	if _, err := pushTree(t, remote, "wiki", "first", map[string][]byte{
		"concepts/keep.md": []byte("# Keep\n"),
		"concepts/gone.md": []byte("# Gone\n"),
	}); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if _, err := pushTree(t, remote, "wiki", "second", map[string][]byte{
		"concepts/keep.md": []byte("# Keep\n"),
	}); err != nil {
		t.Fatalf("second push: %v", err)
	}

	work := checkout(t, remote)
	if _, err := os.Stat(filepath.Join(work, "wiki/concepts/keep.md")); err != nil {
		t.Errorf("the surviving page is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "wiki/concepts/gone.md")); !os.IsNotExist(err) {
		t.Error("a page removed from the wiki is still in the repository")
	}
}

// TestPushLeavesTheRestOfTheRepositoryAlone: a prefix means the wiki owns one
// subtree, not the repository.
func TestPushLeavesTheRestOfTheRepositoryAlone(t *testing.T) {
	remote := bareRepo(t)

	// Seed the upstream with a file the wiki does not own.
	seed := t.TempDir()
	git(t, seed, "clone", "--quiet", remote, "work")
	work := filepath.Join(seed, "work")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hand written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "--quiet", "-m", "seed")
	git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	if _, err := pushTree(t, remote, "wiki", "publish", map[string][]byte{
		"index.md": []byte("# Index\n"),
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	after := checkout(t, remote)
	body, err := os.ReadFile(filepath.Join(after, "README.md"))
	if err != nil || string(body) != "hand written\n" {
		t.Errorf("the repository's own README was disturbed: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(after, "wiki/index.md")); err != nil {
		t.Errorf("the wiki was not published: %v", err)
	}
}

// TestPushWithNoChangesMakesNoCommit: an unchanged bench republishing is the
// common case, and a commit per build with no diff would make the history
// useless.
func TestPushWithNoChangesMakesNoCommit(t *testing.T) {
	remote := bareRepo(t)
	files := map[string][]byte{"index.md": []byte("# Index\n")}

	if _, err := pushTree(t, remote, "wiki", "first", files); err != nil {
		t.Fatalf("first push: %v", err)
	}
	before := git(t, checkout(t, remote), "rev-list", "--count", "HEAD")

	if _, err := pushTree(t, remote, "wiki", "second", files); !errors.Is(err, ErrNothingToPush) {
		t.Fatalf("second push = %v, want ErrNothingToPush", err)
	}
	after := git(t, checkout(t, remote), "rev-list", "--count", "HEAD")
	if before != after {
		t.Errorf("commit count moved %s -> %s for an unchanged wiki", strings.TrimSpace(before), strings.TrimSpace(after))
	}
}

// TestPushAtTheRepositoryRootKeepsGit: with no prefix the managed subtree is
// the root, and clearing it must not take .git with it.
func TestPushAtTheRepositoryRootKeepsGit(t *testing.T) {
	remote := bareRepo(t)

	if _, err := pushTree(t, remote, "", "publish at root", map[string][]byte{
		"index.md": []byte("# Index\n"),
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	work := checkout(t, remote)
	if _, err := os.Stat(filepath.Join(work, "index.md")); err != nil {
		t.Errorf("root publish did not land: %v", err)
	}
}

func TestPushRefusesAnUnvalidatedRemote(t *testing.T) {
	// Without the test hook the connector's clone policy applies, so a
	// filesystem path is refused the same way it is for a clone.
	_, err := Push(context.Background(), PushOptions{
		Remote: "/tmp/somewhere.git", Branch: "main",
		Files: map[string][]byte{"index.md": []byte("x")},
	})
	if err == nil {
		t.Fatal("a filesystem remote was accepted")
	}
	if _, err := Push(context.Background(), PushOptions{
		Remote: "https://example.com/x.git",
	}); err == nil {
		t.Fatal("a push with no branch was accepted")
	}
}
