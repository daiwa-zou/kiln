package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
)

// fixtureRepo builds a real two-commit repository and returns its dir and
// both commit SHAs.
func fixtureRepo(t *testing.T) (dir, first, second string) {
	t.Helper()
	dir = t.TempDir()

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "main")
	write("keep.go", "package a\n")
	write("edit.go", "package a\n")
	write("gone.go", "package a\n")
	run("add", "-A")
	run("commit", "-q", "-m", "first")
	first = HeadRef(context.Background(), dir)

	write("edit.go", "package a // changed\n")
	write("sub/new.go", "package b\n")
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "second")
	second = HeadRef(context.Background(), dir)

	if first == "" || second == "" {
		t.Fatal("fixture SHAs empty")
	}
	return dir, first, second
}

func TestDiffRange(t *testing.T) {
	dir, first, second := fixtureRepo(t)

	changes, ok := DiffRange(context.Background(), dir, first, second)
	if !ok {
		t.Fatal("DiffRange not ok on a healthy range")
	}
	got := map[string]diff.ChangeKind{}
	for _, c := range changes {
		got[c.Path] = c.Kind
	}
	want := map[string]diff.ChangeKind{
		"edit.go":    diff.Modified,
		"sub/new.go": diff.Added,
		"gone.go":    diff.Deleted,
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("%s = %s, want %s", p, got[p], k)
		}
	}

	// Short SHAs resolve too — runs.ref stores the 7-char form.
	if _, ok := DiffRange(context.Background(), dir, first[:7], second[:7]); !ok {
		t.Error("short-sha range rejected")
	}
	// An identical range is empty but ok: the run becomes no_changes without
	// hashing anything.
	if changes, ok := DiffRange(context.Background(), dir, second, second); !ok || len(changes) != 0 {
		t.Errorf("self-range = %v ok=%v, want empty ok", changes, ok)
	}
}

func TestDiffRangeFallsBackCleanly(t *testing.T) {
	dir, _, second := fixtureRepo(t)
	ctx := context.Background()

	// Unknown base (simulates a force-push): not ok, caller full-rebuilds.
	if _, ok := DiffRange(ctx, dir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", second); ok {
		t.Error("unknown base accepted")
	}
	// Empty refs and non-repos are unusable, not errors.
	if _, ok := DiffRange(ctx, dir, "", second); ok {
		t.Error("empty base accepted")
	}
	if _, ok := DiffRange(ctx, t.TempDir(), "a", "b"); ok {
		t.Error("non-repo accepted")
	}
}

func TestHeadRef(t *testing.T) {
	dir, _, second := fixtureRepo(t)
	if got := HeadRef(context.Background(), dir); got != second {
		t.Errorf("HeadRef = %q, want %q", got, second)
	}
	if got := HeadRef(context.Background(), t.TempDir()); got != "" {
		t.Errorf("HeadRef on non-repo = %q, want empty", got)
	}
}
