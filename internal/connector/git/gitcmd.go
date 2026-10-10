package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/childproc"
)

// Every git process kiln starts against a checkout of someone else's
// repository is built here, so the clone and the later fetches that deepen it
// cannot drift apart: a hardening flag added for one is on for all, and a token
// reaches git one way only.

// hardening is passed to git as top-level -c options on every invocation,
// rather than relying on what an earlier command persisted into the checkout's
// config.
//
//   - http.followRedirects=false: the URL was validated before the process
//     started, and a redirect after that is a second, unvalidated destination.
//   - core.symlinks=false: a symlink in a hostile working tree must not become a
//     portal to the worker's filesystem when the scanner walks it.
//   - credential.helper=: no helper (the keychain, a cache daemon) may answer in
//     place of, or alongside, the askpass token.
var hardening = []string{
	"-c", "http.followRedirects=false",
	"-c", "core.symlinks=false",
	"-c", "credential.helper=",
}

// gitCmd builds a hardened git command. dir, when non-empty, is the checkout it
// runs in (git -C dir); clone passes "" because its destination is an argument.
//
// When token is set it is handed over through an askpass helper and the
// KILN_GIT_TOKEN environment variable, never the command line or URL, where it
// would show in process listings. The returned cleanup removes the helper and
// must be called once the command has finished; it is always non-nil.
//
// The environment is childproc.GitEnv: the parent's KILN_* secrets stay out of
// reach and the token is the only one the child sees.
func gitCmd(ctx context.Context, dir, token string, args ...string) (cmd *exec.Cmd, cleanup func(), err error) {
	cleanup = func() {}

	full := append([]string(nil), hardening...)
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)

	cmd = exec.CommandContext(ctx, "git", full...)
	// A grandchild (git-remote-https) holding the output pipes must not pin the
	// caller past the timeout it asked for.
	cmd.WaitDelay = childproc.WaitDelay
	cmd.Env = childproc.GitEnv()

	if token != "" {
		near := dir
		if near == "" && len(args) > 0 {
			// clone: the destination is the last argument.
			near = args[len(args)-1]
		}
		askpass, clean, aerr := WriteAskpass(near)
		if aerr != nil {
			return nil, cleanup, aerr
		}
		cleanup = clean
		cmd.Env = append(cmd.Env,
			"GIT_ASKPASS="+askpass,
			"KILN_GIT_TOKEN="+token,
		)
	}
	return cmd, cleanup, nil
}

// gitOutput runs a hardened git command in dir and returns its stdout, bounded
// by timeout. Errors name the subcommand but never carry the token: it is only
// ever in the child's environment.
func gitOutput(ctx context.Context, dir, token string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, cleanup, err := gitCmd(ctx, dir, token, args...)
	if err != nil {
		return "", err
	}
	defer cleanup()

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
