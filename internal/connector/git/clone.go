package git

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Remote-clone policy. Connector configs are API-writable data, so the URL a
// clone follows is attacker-shaped until proven otherwise: https only, no
// credentials smuggled into the URL, no hosts that resolve into the private
// address space, no redirects after the URL was checked, bounded depth, time,
// and size.

// Clone limits, overridable per call.
const (
	defaultCloneTimeout  = 5 * time.Minute
	defaultCloneMaxBytes = 1 << 30 // 1 GiB working tree
)

// CloneOptions describes one shallow clone.
type CloneOptions struct {
	// URL is the https remote. It must already have passed ValidateRemoteURL;
	// CloneShallow re-checks rather than trusting the caller.
	URL string
	// Token, when set, authenticates the clone (a git_pat credential). It is
	// handed to git through an askpass helper and environment variable, never
	// through the URL or command line, where it would be visible in ps and
	// process listings.
	Token string
	// Dir is the destination directory; it must exist and be empty.
	Dir string

	Timeout  time.Duration
	MaxBytes int64
}

// ValidateRemoteURL enforces the remote-clone policy on a configured URL.
func ValidateRemoteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("connector/git: remote url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("connector/git: remote url must be https, got %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("connector/git: remote url must not embed credentials; store them as a credential instead")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("connector/git: remote url has no host")
	}

	// Resolve now and refuse private space. This is a pre-flight check, not
	// pinning -- git resolves again when it connects -- but combined with
	// redirects being disabled it closes the casual SSRF paths: a config
	// cannot name localhost, a link-local metadata endpoint, or an internal
	// RFC1918 service outright.
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("connector/git: resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isForbiddenAddress(ip) {
			return nil, fmt.Errorf("connector/git: %s resolves to %s, which is not a public address", host, ip)
		}
	}
	return u, nil
}

func isForbiddenAddress(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || // includes 169.254.169.254 cloud metadata
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// CloneShallow materializes a depth-1 clone of a validated https remote.
func CloneShallow(ctx context.Context, opts CloneOptions) error {
	if _, err := ValidateRemoteURL(opts.URL); err != nil {
		return err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultCloneTimeout
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultCloneMaxBytes
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"clone",
		"--depth", "1",
		"--single-branch",
		"--no-tags",
		// The URL was validated before this process started; a redirect after
		// that would be a second, unvalidated destination.
		"-c", "http.followRedirects=false",
		// A symlink in a hostile working tree must not become a portal to the
		// worker's filesystem when the scanner walks it.
		"-c", "core.symlinks=false",
		"-c", "credential.helper=",
		opts.URL, opts.Dir,
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	if opts.Token != "" {
		askpass, cleanup, err := writeAskpass(opts.Dir)
		if err != nil {
			return err
		}
		defer cleanup()
		cmd.Env = append(cmd.Env,
			"GIT_ASKPASS="+askpass,
			"KILN_GIT_TOKEN="+opts.Token,
		)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("connector/git: clone timed out after %s", timeout)
		}
		return fmt.Errorf("connector/git: clone failed: %s", firstLine(string(out)))
	}

	if size, err := dirSize(opts.Dir); err != nil {
		return err
	} else if size > maxBytes {
		// Remove what was fetched: an oversized clone left on disk is exactly
		// the resource exhaustion the cap exists to prevent.
		_ = os.RemoveAll(opts.Dir)
		return fmt.Errorf("connector/git: clone is %d bytes, over the %d byte cap", size, maxBytes)
	}
	return nil
}

// writeAskpass materializes the credential helper git calls for a username
// and password. The token travels via environment, so neither the command
// line nor the script contains it.
func writeAskpass(near string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(filepath.Dir(near), "kiln-askpass-")
	if err != nil {
		return "", nil, fmt.Errorf("connector/git: askpass dir: %w", err)
	}
	script := filepath.Join(dir, "askpass.sh")
	body := "#!/bin/sh\ncase \"$1\" in\n  Username*) echo x-access-token ;;\n  *) echo \"$KILN_GIT_TOKEN\" ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("connector/git: write askpass: %w", err)
	}
	return script, func() { os.RemoveAll(dir) }, nil
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("connector/git: size clone: %w", err)
	}
	return total, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
