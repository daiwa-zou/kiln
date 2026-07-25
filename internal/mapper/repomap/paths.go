package repomap

import (
	"path"
	"strings"
)

// relativeTo returns file relative to dir, or "" when file is not under dir.
// Both are repo-relative, slash-separated paths; dir may be "." for the root.
func relativeTo(dir, file string) string {
	dir = path.Clean(dir)
	file = path.Clean(file)

	if dir == "." || dir == "" {
		return file
	}
	if file == dir {
		return "."
	}
	prefix := dir + "/"
	if !strings.HasPrefix(file, prefix) {
		return ""
	}
	return strings.TrimPrefix(file, prefix)
}

// owningDir picks the deepest directory in dirs that contains file. This is how
// a changed path is attributed to a module: longest manifest-dir prefix wins,
// so apps/ripple/main.go belongs to apps/ripple and not to the root module.
func owningDir(dirs []string, file string) string {
	best := ""
	bestLen := -1

	for _, d := range dirs {
		if relativeTo(d, file) == "" {
			continue
		}
		clean := path.Clean(d)
		n := len(clean)
		if clean == "." {
			n = 0
		}
		if n > bestLen {
			best, bestLen = clean, n
		}
	}
	return best
}
