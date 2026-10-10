package childproc

import (
	"os"
	"strings"
)

// Env builds the environment for a child that parses content kiln does not
// control: enough for the binary to run, and nothing else. The parent holds
// the master key, session secret and database password in KILN_* variables,
// and a child inheriting them hands those to whatever a hostile document or
// repository manages to execute inside the tool.
//
// HOME is kept so a logged-in CLI session's credentials still resolve. USER
// and LOGNAME go with it, and are not optional decoration: on macOS the agent
// CLI keeps its session in the Keychain under an account named for the current
// user, and resolves that name from the environment (see agent.MinimalChildEnv
// for what stripping them does). The locale (LANG and LC_*) keeps converters
// from falling back to a C locale that mangles non-ASCII text.
//
// extra entries are appended as given, for the variables a caller passes on
// purpose (an API key, an askpass helper and its token).
func Env(extra ...string) []string {
	env := make([]string, 0, 10+len(extra))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if inheritedKey(k) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

func inheritedKey(k string) bool {
	switch k {
	case "PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TERM", "LANG":
		return true
	}
	return strings.HasPrefix(k, "LC_")
}

// GitEnv is Env for a git child, which reads configuration and talks to a
// remote someone else chose.
//
//   - GIT_TERMINAL_PROMPT=0: a missing credential is an error, not a hang on a
//     prompt no one will answer.
//   - GIT_CONFIG_NOSYSTEM=1 and GIT_CONFIG_GLOBAL=/dev/null: nothing the host
//     configured (a credential helper, a signing key, an include of another
//     file) changes what the command does; every setting that matters is
//     passed with -c.
//   - GIT_LFS_SKIP_SMUDGE=1: a repository cannot make the clone fetch large
//     files through an LFS filter, which is a second download, outside the
//     size cap, from a host the URL check never saw.
//
// Proxy and certificate settings are carried through because they describe the
// deployment's network, not a secret, and a clone behind an egress proxy or a
// private CA fails without them.
func GitEnv(extra ...string) []string {
	env := Env()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
			"SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH":
			env = append(env, kv)
		}
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_LFS_SKIP_SMUDGE=1",
	)
	return append(env, extra...)
}
