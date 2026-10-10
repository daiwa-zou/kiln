package git

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
)

// Commit-range change detection: the derivation diff.ChangeSet's doc comment
// always promised. Failure at any step is reported as (nil, false) rather
// than an error, because the caller's fallback — FullRebuild plus the hash
// gate — is always correct, just less surgical. Ranges are an optimization;
// they must never be a way to fail a build.

// localCmdTimeout bounds commands that only read the checkout.
const localCmdTimeout = 30 * time.Second

// fetchLimits caps the network fetches EnsureRef makes, with the same defaults
// the clone that created the checkout ran under: a hostile or enormous remote
// must not get a second, unbounded download just because the first was capped.
type fetchLimits struct {
	// Timeout bounds each fetch.
	Timeout time.Duration
	// MaxBytes caps how much the checkout may grow across the fetches of one
	// EnsureRef call. It is a growth cap, not a total, so a large local
	// repository that legitimately needs a fetch is not refused for the size it
	// already had.
	MaxBytes int64
}

var defaultFetchLimits = fetchLimits{Timeout: defaultCloneTimeout, MaxBytes: defaultCloneMaxBytes}

// fullSHA matches the only form of commit id a remote will serve by name: git
// servers resolve a "want" by full object id, never by abbreviation.
var fullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// DiffRange derives the changes between two commits in a checkout. ok is
// false when the range is unusable (unknown refs, shallow history that
// deepening could not fix, not a git directory).
//
// token, when set, authenticates the fetches EnsureRef may need against a
// private remote; a checkout made with a token needs one to be deepened.
func DiffRange(ctx context.Context, dir, token, fromRef, toRef string) (changes []diff.Change, ok bool) {
	if fromRef == "" || toRef == "" {
		return nil, false
	}
	if !EnsureRef(ctx, dir, token, fromRef) || !EnsureRef(ctx, dir, token, toRef) {
		return nil, false
	}

	out, err := gitOutput(ctx, dir, "", localCmdTimeout, "diff", "--name-status", "--no-renames", fromRef, toRef)
	if err != nil {
		return nil, false
	}

	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\n")
		status, path, found := strings.Cut(line, "\t")
		if !found || path == "" {
			continue
		}
		var kind diff.ChangeKind
		switch {
		case strings.HasPrefix(status, "A"):
			kind = diff.Added
		case strings.HasPrefix(status, "D"):
			kind = diff.Deleted
		default: // M, T (type change), and anything exotic reads as modified
			kind = diff.Modified
		}
		changes = append(changes, diff.Change{Path: path, Kind: kind})
	}
	return changes, true
}

// HeadRef returns the checkout's current commit as a full SHA, or "" when
// there is none. Callers that store the result as a later range base need the
// full form: an abbreviation cannot be fetched from a remote.
func HeadRef(ctx context.Context, dir string) string {
	out, err := gitOutput(ctx, dir, "", localCmdTimeout, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// EnsureRef makes a commit resolvable in the checkout, deepening a shallow
// clone in bounded steps and finally attempting a direct fetch of the commit
// (which GitHub's allow-any-SHA upload-pack supports; that needs the full SHA).
// False means the caller should fall back to a full rebuild.
//
// The fetches run exactly as the clone did: hardened git configuration, the
// minimal environment, token via askpass, and the clone's time and size caps.
func EnsureRef(ctx context.Context, dir, token, ref string) bool {
	return ensureRef(ctx, dir, token, ref, defaultFetchLimits)
}

func ensureRef(ctx context.Context, dir, token, ref string, lim fetchLimits) bool {
	// A ref is passed to git as an argument; one that looks like an option is
	// an argument injection, not a commit.
	if ref == "" || strings.HasPrefix(ref, "-") {
		return false
	}
	if hasCommit(ctx, dir, ref) {
		return true
	}

	base, baseErr := dirSize(dir)
	fetch := func(args ...string) bool {
		if baseErr != nil {
			return false
		}
		full := append([]string{"fetch", "--no-tags", "--no-recurse-submodules"}, args...)
		if _, err := gitOutput(ctx, dir, token, lim.Timeout, full...); err != nil {
			return false
		}
		if size, err := dirSize(dir); err != nil || size-base > lim.MaxBytes {
			return false
		}
		return true
	}

	// Deepen in two bounded steps: most webhook ranges are a handful of
	// commits, and an unbounded deepen would quietly re-download the history
	// the shallow clone existed to avoid.
	for _, deepen := range []string{"100", "1000"} {
		if !fetch("--deepen", deepen, "origin") {
			break // not shallow, no remote, or over the cap: nothing more to deepen
		}
		if hasCommit(ctx, dir, ref) {
			return true
		}
	}
	// Depth 1: a diff compares trees, so the commit needs no history of its
	// own, and a plain fetch of a SHA in a shallow repository would otherwise
	// pull its ancestry down to the shallow boundary.
	if fullSHA.MatchString(ref) && fetch("--depth", "1", "origin", ref) && hasCommit(ctx, dir, ref) {
		return true
	}
	return false
}

func hasCommit(ctx context.Context, dir, ref string) bool {
	_, err := gitOutput(ctx, dir, "", localCmdTimeout, "cat-file", "-e", ref+"^{commit}")
	return err == nil
}
