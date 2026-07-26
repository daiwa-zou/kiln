package worker

import (
	"fmt"
	"path/filepath"
	"strings"
)

// AllowedPath resolves a connector-supplied local path against the permitted
// roots and returns the resolved form to use for reading.
//
// SECURITY: connector configs are API-writable data. Without this gate, a
// database-configured path is a local-file-inclusion primitive — anyone who
// can write a connector row can point kiln at any directory the worker
// process can read. The check runs on the symlink-resolved path, so a
// permitted directory containing a symlink out to /etc does not become an
// escape hatch.
func AllowedPath(path string, roots []string) (string, error) {
	if len(roots) == 0 {
		return "", fmt.Errorf(
			"local source paths are denied: worker.permitted_source_roots is empty")
	}
	if !filepath.IsAbs(path) {
		// A relative path would resolve against the worker's cwd, which the
		// config author does not control and should not be probing.
		return "", fmt.Errorf("source path %q must be absolute", path)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("source path %q cannot be resolved: %w", path, err)
	}

	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			// A configured root that does not exist permits nothing.
			continue
		}
		if resolved == resolvedRoot ||
			strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("source path %q is outside every permitted root", path)
}
