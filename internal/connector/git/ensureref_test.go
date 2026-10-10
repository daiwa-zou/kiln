package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// runGit runs git in dir for fixture setup, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// shallowOrigin publishes the two-commit fixture to a bare repository and makes
// a depth-1 clone of it, the shape a worker's checkout has. It returns the
// clone and the fixture's two full SHAs.
func shallowOrigin(t *testing.T) (clone, bare, first, second string) {
	t.Helper()
	src, first, second := fixtureRepo(t)

	bare = filepath.Join(t.TempDir(), "origin.git")
	runGit(t, src, "clone", "-q", "--bare", src, bare)
	// GitHub serves any reachable commit by id; a plain local server does so
	// only when told to.
	runGit(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")

	clone = filepath.Join(t.TempDir(), "checkout")
	cmd := exec.Command("git", "clone", "-q", "--depth", "1", "--single-branch", "--no-tags", "file://"+bare, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shallow clone: %v\n%s", err, out)
	}
	return clone, bare, first, second
}

func TestEnsureRefDeepensAShallowCloneToAFullSHABase(t *testing.T) {
	clone, _, first, second := shallowOrigin(t)
	ctx := context.Background()

	if hasCommit(ctx, clone, first) {
		t.Fatal("fixture is not shallow: the base is already present")
	}
	if !EnsureRef(ctx, clone, "", first) {
		t.Fatal("EnsureRef could not resolve a full-SHA base after a depth-1 clone")
	}

	// The whole point: the range is usable, so the caller routes incrementally
	// instead of rebuilding everything.
	changes, ok := DiffRange(ctx, clone, "", first, second)
	if !ok || len(changes) != 3 {
		t.Errorf("DiffRange after deepening = %v ok=%v, want the 3 changed paths", changes, ok)
	}
}

func TestEnsureRefFetchesACommitDeepeningCannotReach(t *testing.T) {
	clone, bare, _, _ := shallowOrigin(t)
	ctx := context.Background()

	// A commit on a branch the single-branch clone never tracked: deepening
	// main's history cannot produce it, only a direct fetch by id can.
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, filepath.Dir(work), "clone", "-q", bare, work)
	runGit(t, work, "checkout", "-q", "-b", "side")
	if err := os.WriteFile(filepath.Join(work, "side.go"), []byte("package s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "side")
	runGit(t, work, "push", "-q", "origin", "side")
	side := runGit(t, work, "rev-parse", "HEAD")

	if !EnsureRef(ctx, clone, "", side) {
		t.Fatal("EnsureRef did not fetch an unreachable commit by its full SHA")
	}

	// A 7-character abbreviation is exactly what runs used to store: no server
	// resolves one, so it cannot be fetched.
	if EnsureRef(ctx, clone, "", side[:7]+"0") {
		t.Error("an abbreviated, unknown SHA was reported resolvable")
	}
}

func TestEnsureRefRefusesOptionShapedRefs(t *testing.T) {
	clone, _, _, _ := shallowOrigin(t)
	if EnsureRef(context.Background(), clone, "", "--upload-pack=touch /tmp/pwned") {
		t.Error("an option-shaped ref was accepted")
	}
}

func TestEnsureRefCapsHowMuchAFetchMayGrowTheCheckout(t *testing.T) {
	clone, _, first, _ := shallowOrigin(t)

	// One byte of allowance: the deepen succeeds on the wire but the checkout
	// grew past the cap, so the range is reported unusable rather than kept.
	ok := ensureRef(context.Background(), clone, "", first, fetchLimits{Timeout: time.Minute, MaxBytes: 1})
	if ok {
		t.Error("a fetch that overran the byte cap was accepted")
	}
}

// fakeGitRecording puts a "git" first on PATH that appends each invocation's
// argv and environment to a log, then fails. Fetching is the interesting part
// of EnsureRef, and a failing git lets it walk every attempt it makes.
func fakeGitRecording(t *testing.T) (logFile string) {
	t.Helper()
	bin := t.TempDir()
	logFile = filepath.Join(bin, "calls.txt")
	script := "#!/bin/sh\n{\n  echo \"CALL $*\"\n  env | sed 's/^/ENV /'\n} >> '" + logFile + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KILN_MASTER_KEY", "master-secret")
	return logFile
}

// fetchCalls splits the recording into the invocations that are fetches,
// returning each one's argv and environment.
func fetchCalls(t *testing.T, logFile string) (argvs []string, envs [][]string) {
	t.Helper()
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("the fake git never ran: %v", err)
	}
	for block := range strings.SplitSeq("\n"+string(raw), "\nCALL ") {
		if block == "" {
			continue
		}
		argv, rest, _ := strings.Cut(block, "\n")
		if !strings.Contains(argv, " fetch ") {
			continue
		}
		var env []string
		for line := range strings.SplitSeq(rest, "\n") {
			if kv, ok := strings.CutPrefix(line, "ENV "); ok {
				env = append(env, kv)
			}
		}
		argvs = append(argvs, argv)
		envs = append(envs, env)
	}
	return argvs, envs
}

func TestEnsureRefFetchesAreHardenedAndAuthenticated(t *testing.T) {
	logFile := fakeGitRecording(t)
	const token = "super-secret-installation-token"
	const sha = "0123456789abcdef0123456789abcdef01234567"

	if EnsureRef(context.Background(), t.TempDir(), token, sha) {
		t.Fatal("the fake git cannot have produced a commit")
	}

	argvs, envs := fetchCalls(t, logFile)
	if len(argvs) < 2 {
		t.Fatalf("saw %d fetches (%q); want the deepen and the direct fetch", len(argvs), argvs)
	}
	var sawDeepen, sawDirect bool
	for i, argv := range argvs {
		for _, flag := range []string{
			"http.followRedirects=false", "credential.helper=", "core.symlinks=false",
		} {
			if !strings.Contains(argv, "-c "+flag) {
				t.Errorf("fetch %d lacks hardening %q: %s", i, flag, argv)
			}
		}
		if strings.Contains(argv, token) {
			t.Errorf("fetch %d put the token on the command line: %s", i, argv)
		}
		sawDeepen = sawDeepen || strings.Contains(argv, "--deepen")
		sawDirect = sawDirect || strings.HasSuffix(argv, "origin "+sha)

		env := envs[i]
		if !slices.Contains(env, "KILN_GIT_TOKEN="+token) {
			t.Errorf("fetch %d did not get the token through its environment", i)
		}
		if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "GIT_ASKPASS=") }) {
			t.Errorf("fetch %d has no askpass helper", i)
		}
		for _, want := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1"} {
			if !slices.Contains(env, want) {
				t.Errorf("fetch %d environment is missing %s", i, want)
			}
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "KILN_") && !strings.HasPrefix(kv, "KILN_GIT_TOKEN=") {
				t.Errorf("fetch %d leaked a parent secret: %s", i, strings.SplitN(kv, "=", 2)[0])
			}
		}
	}
	if !sawDeepen || !sawDirect {
		t.Errorf("fetches = %q; want both a deepen and a direct fetch of the full SHA", argvs)
	}
}

func TestValidateBranch(t *testing.T) {
	for _, ok := range []string{"main", "release/1.x", "feature_x-2", "v1.2.3"} {
		if err := ValidateBranch(ok); err != nil {
			t.Errorf("ValidateBranch(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{
		"", "-main", "--upload-pack=x", "a b", "a..b", "a//b", "a/", "a.", "x.lock",
		"a\nb", "a:b", "a~1", "a^", "a?", "a*", "a[b", "@{u}", "a\\b", strings.Repeat("a", 256),
	} {
		if err := ValidateBranch(bad); err == nil {
			t.Errorf("ValidateBranch(%q) accepted", bad)
		}
	}
}

func TestCloneShallowClonesTheConfiguredBranch(t *testing.T) {
	logFile := fakeGitRecording(t)

	_ = CloneShallow(context.Background(), CloneOptions{
		URL: "https://8.8.8.8/repo.git", Dir: t.TempDir(), Branch: "release/1.x",
	})
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("the fake git never ran: %v", err)
	}
	if !strings.Contains(string(raw), "--branch release/1.x") {
		t.Errorf("clone did not name the configured branch:\n%s", strings.SplitN(string(raw), "\n", 2)[0])
	}

	// An option-shaped branch is refused before any process starts.
	err = CloneShallow(context.Background(), CloneOptions{
		URL: "https://8.8.8.8/repo.git", Dir: t.TempDir(), Branch: "--upload-pack=x",
	})
	if err == nil || !strings.Contains(err.Error(), "branch") {
		t.Errorf("option-shaped branch = %v, want refusal", err)
	}
}

func TestEnsureRefWithoutATokenSetsNoCredentials(t *testing.T) {
	logFile := fakeGitRecording(t)

	EnsureRef(context.Background(), t.TempDir(), "", "0123456789abcdef0123456789abcdef01234567")

	_, envs := fetchCalls(t, logFile)
	if len(envs) == 0 {
		t.Fatal("no fetch was attempted")
	}
	for _, env := range envs {
		for _, kv := range env {
			if strings.HasPrefix(kv, "KILN_GIT_TOKEN=") || strings.HasPrefix(kv, "GIT_ASKPASS=") {
				t.Errorf("an anonymous fetch carried a credential variable: %s", strings.SplitN(kv, "=", 2)[0])
			}
		}
	}
}
