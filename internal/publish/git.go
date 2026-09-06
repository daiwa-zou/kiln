package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
)

// Pushing reuses the clone path's policy rather than inventing a second one.
// The remote is validated by connector/git (https only, no credentials in the
// URL, no private address space) and the token reaches git through the same
// askpass helper, so it never appears in a command line or in the repository
// config where a later `git remote -v` would print it.
//
// The push itself is deliberately unremarkable: shallow-fetch the branch, empty
// the managed subtree, write the tree, commit, push. No force, so a repository
// that moved on rejects the push rather than losing whatever moved it -- and
// the next build tries again from the new tip.

const (
	defaultPushTimeout = 3 * time.Minute
	// defaultAuthor is what appears in the log when a deployment has not said
	// who is publishing. A name and address that obviously belong to a machine
	// beats borrowing a person's.
	defaultAuthorName  = "kiln"
	defaultAuthorEmail = "kiln@users.noreply.github.com"
)

// ErrNothingToPush means the repository already matched the wiki, so no commit
// was made. Not a failure: an unchanged bench republishing is the common case,
// and a commit per build with no diff would make the history useless.
var ErrNothingToPush = errors.New("publish: repository already matches the wiki")

// PushOptions describes one publish.
type PushOptions struct {
	// Remote is the https URL of the repository to publish into.
	Remote string
	// Branch is the branch to publish onto; it is created if absent.
	Branch string
	// Prefix is the subdirectory the wiki owns, empty for the repository root.
	// Only this subtree is replaced, so a repository can hold a README, CI
	// workflows and a published wiki side by side.
	Prefix string
	// Token authenticates the push.
	Token string
	// Files is the tree to write, from Tree.
	Files map[string][]byte
	// Message is the commit subject.
	Message string

	AuthorName  string
	AuthorEmail string
	Timeout     time.Duration

	// allowLocalRemote permits a filesystem remote, for tests that push to a
	// bare repository instead of the network. Unexported: a publish target is
	// API-writable configuration and must never be able to reach this.
	allowLocalRemote bool
}

// Result reports what a push did.
type Result struct {
	// Commit is the sha that was pushed.
	Commit string
	// Files is how many files the published tree contains.
	Files int
}

// Push mirrors the wiki into the repository, returning ErrNothingToPush when
// the repository already matched.
func Push(ctx context.Context, opts PushOptions) (Result, error) {
	if opts.Branch == "" {
		return Result{}, fmt.Errorf("publish: no branch configured")
	}
	if !opts.allowLocalRemote {
		if _, err := gitconn.ValidateRemoteURL(opts.Remote); err != nil {
			return Result{}, err
		}
	}
	prefix, err := cleanPrefix(opts.Prefix)
	if err != nil {
		return Result{}, err
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultPushTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "kiln-publish-")
	if err != nil {
		return Result{}, fmt.Errorf("publish: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	env, cleanupAskpass, err := gitEnv(dir, opts.Token)
	if err != nil {
		return Result{}, err
	}
	defer cleanupAskpass()

	author := gitAuthor{name: opts.AuthorName, email: opts.AuthorEmail}
	run := func(args ...string) (string, error) { return runGit(ctx, dir, env, author, args...) }

	if _, err := run("init", "--quiet", "-b", opts.Branch); err != nil {
		return Result{}, err
	}
	if _, err := run("remote", "add", "origin", opts.Remote); err != nil {
		return Result{}, err
	}

	// A branch that does not exist yet is the first publish, not an error: the
	// commit below becomes the branch's root.
	existing := true
	if _, err := run("fetch", "--depth", "1", "origin", opts.Branch); err != nil {
		existing = false
	}
	if existing {
		if _, err := run("checkout", "--quiet", "-B", opts.Branch, "FETCH_HEAD"); err != nil {
			return Result{}, err
		}
	}

	if err := replaceSubtree(dir, prefix, opts.Files); err != nil {
		return Result{}, err
	}

	if _, err := run("add", "--all", "."); err != nil {
		return Result{}, err
	}
	// --quiet still exits non-zero when there is nothing staged, which is the
	// signal that the repository already matched.
	if _, err := run("diff", "--cached", "--quiet"); err == nil {
		return Result{}, ErrNothingToPush
	}

	message := opts.Message
	if message == "" {
		message = "Update wiki"
	}
	if _, err := run("commit", "--quiet", "-m", message); err != nil {
		return Result{}, err
	}
	if _, err := run("push", "origin", "HEAD:refs/heads/"+opts.Branch); err != nil {
		return Result{}, err
	}

	sha, err := run("rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	return Result{Commit: strings.TrimSpace(sha), Files: len(opts.Files)}, nil
}

// cleanPrefix normalizes the configured subdirectory and refuses anything that
// would escape the checkout. The prefix is API-writable configuration, so this
// is a boundary rather than a formatting nicety.
func cleanPrefix(raw string) (string, error) {
	// Leading and trailing slashes are trimmed rather than refused: someone
	// typing "/wiki" means the wiki directory at the repository root, and the
	// prefix is repository-relative by construction.
	p := strings.Trim(strings.TrimSpace(raw), "/")
	if p == "" {
		return "", nil
	}
	if filepath.IsAbs(p) || strings.Contains(p, `\`) {
		return "", fmt.Errorf("publish: prefix %q must be a relative path", raw)
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", fmt.Errorf("publish: prefix %q escapes the repository", raw)
	}
	if strings.HasPrefix(clean, ".git/") || clean == ".git" {
		return "", fmt.Errorf("publish: prefix %q would overwrite git's own directory", raw)
	}
	return clean, nil
}

// replaceSubtree empties the wiki's subtree and writes the new one.
//
// Emptying first is what makes a deleted page disappear from the repository.
// The one thing that must survive is .git itself: with no prefix the managed
// subtree *is* the repository root, and removing that directory would destroy
// the checkout rather than update it.
func replaceSubtree(root, prefix string, files map[string][]byte) error {
	target := root
	if prefix != "" {
		target = filepath.Join(root, filepath.FromSlash(prefix))
	}

	entries, err := os.ReadDir(target)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("publish: read %s: %w", target, err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(target, e.Name())); err != nil {
			return fmt.Errorf("publish: clear %s: %w", e.Name(), err)
		}
	}

	for rel, body := range files {
		clean, err := safeRelPath(rel)
		if err != nil {
			return err
		}
		full := filepath.Join(target, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("publish: mkdir for %s: %w", clean, err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			return fmt.Errorf("publish: write %s: %w", clean, err)
		}
	}
	return nil
}

// safeRelPath refuses a tree entry that would land outside the subtree. The
// tree is built from page paths, which validation already constrains, so this
// is defense in depth rather than the boundary.
func safeRelPath(rel string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(rel)))
	if clean == "" || clean == "." || filepath.IsAbs(clean) ||
		clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, ".git/") {
		return "", fmt.Errorf("publish: refusing to write %q", rel)
	}
	return clean, nil
}

// gitEnv builds the environment every git call runs with: no terminal prompt,
// no inherited credential helper, and the askpass helper when a token is set.
func gitEnv(near, token string) (env []string, cleanup func(), err error) {
	env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		// A published wiki's commits are machine-made; an inherited signing
		// config would make every push fail on a worker with no key.
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if token == "" {
		return env, func() {}, nil
	}
	script, cleanup, err := gitconn.WriteAskpass(near)
	if err != nil {
		return nil, nil, err
	}
	return append(env, "GIT_ASKPASS="+script, "KILN_GIT_TOKEN="+token), cleanup, nil
}

// gitAuthor is who the commit is attributed to, falling back to a name that
// obviously belongs to a machine rather than borrowing a person's.
type gitAuthor struct{ name, email string }

func (a gitAuthor) or(defName, defEmail string) (string, string) {
	name, email := strings.TrimSpace(a.name), strings.TrimSpace(a.email)
	if name == "" {
		name = defName
	}
	if email == "" {
		email = defEmail
	}
	return name, email
}

func runGit(ctx context.Context, dir string, env []string, author gitAuthor, args ...string) (string, error) {
	name, email := author.or(defaultAuthorName, defaultAuthorEmail)
	full := append([]string{
		"-c", "credential.helper=",
		"-c", "http.followRedirects=false",
		"-c", "commit.gpgsign=false",
		"-c", "user.name=" + name,
		"-c", "user.email=" + email,
	}, args...)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("publish: git %s timed out", args[0])
		}
		// The token never appears in args, so the message is safe to surface.
		return "", fmt.Errorf("publish: git %s: %s", args[0], firstLine(stderr.String()+stdout.String()))
	}
	return stdout.String(), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "failed with no output"
	}
	return s
}
