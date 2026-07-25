package repomap

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileRec is one indexed file.
type FileRec struct {
	Path  string // repo-relative, slash-separated
	Size  int64
	LOC   int
	Lang  string
	Hash  string
	IsDoc bool
}

// WalkOptions controls which files are indexed.
type WalkOptions struct {
	ExcludeDirs  map[string]bool
	ExcludeGlobs []string
	MaxFileBytes int64
	// FollowSymlinks is off by default: a symlink pointing outside the tree
	// would let a repository pull arbitrary host files into a wiki.
	FollowSymlinks bool
}

// DefaultExcludeDirs are never descended into. Build output and vendored
// dependencies are not what a wiki should describe, and skipping them is the
// difference between indexing hundreds of files and hundreds of thousands.
func DefaultExcludeDirs() map[string]bool {
	return map[string]bool{
		".git": true, ".hg": true, ".svn": true,
		"node_modules": true, "vendor": true, "target": true,
		"dist": true, "build": true, "out": true,
		".venv": true, "venv": true, "__pycache__": true,
		".next": true, ".nuxt": true, ".turbo": true, ".cache": true,
		".idea": true, ".vscode": true, ".gradle": true,
		"coverage": true, ".pytest_cache": true, ".mypy_cache": true,
		".terraform": true, "Pods": true, ".obsidian": true,
	}
}

// DefaultExcludeGlobs skip files that are noise in a wiki: lockfiles, minified
// bundles, generated protobuf code, and binaries.
func DefaultExcludeGlobs() []string {
	return []string{
		"*.lock", "go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock",
		"Cargo.lock", "poetry.lock", "*.min.js", "*.min.css", "*.map",
		"*.pb.go", "*_pb2.py", "*_pb.js", "*.generated.*",
		"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp", "*.ico", "*.svg",
		"*.pdf", "*.zip", "*.tar", "*.gz", "*.bin", "*.exe", "*.dll", "*.so", "*.dylib",
		"*.woff", "*.woff2", "*.ttf", "*.eot", "*.mp4", "*.mp3", "*.wav",
		".DS_Store", "*.swp", "*~",
	}
}

// secretPatterns are excluded at scan time and blocked again at prompt
// assembly. A credential that reaches the wiki has been published to everyone
// who can read it, so this is checked in both places rather than one.
var secretPatterns = []string{
	".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ed25519*",
	"*credential*", "*secret*", "*.keystore", ".netrc", ".npmrc", ".pypirc",
}

// DefaultMaxFileBytes caps what is read. Larger files are recorded in the map
// but never opened, so a stray database dump cannot blow up memory.
const DefaultMaxFileBytes int64 = 1 << 20 // 1 MiB

// Walk indexes a directory tree.
func Walk(root string, opts WalkOptions) ([]FileRec, error) {
	if opts.ExcludeDirs == nil {
		opts.ExcludeDirs = DefaultExcludeDirs()
	}
	if opts.ExcludeGlobs == nil {
		opts.ExcludeGlobs = DefaultExcludeGlobs()
	}
	if opts.MaxFileBytes == 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	var out []FileRec

	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree should not abort the scan; the rest of the
			// repository is still worth mapping.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			if opts.ExcludeDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}

		if d.Type()&os.ModeSymlink != 0 && !opts.FollowSymlinks {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if matchesAny(d.Name(), opts.ExcludeGlobs) || matchesAny(d.Name(), secretPatterns) {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}

		rec := FileRec{
			Path:  rel,
			Size:  info.Size(),
			Lang:  DetectLanguage(rel),
			IsDoc: isDocPath(rel),
		}

		if info.Size() <= opts.MaxFileBytes {
			if h, loc, err := hashAndCount(path); err == nil {
				rec.Hash, rec.LOC = h, loc
			}
		}

		out = append(out, rec)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func matchesAny(name string, globs []string) bool {
	lower := strings.ToLower(name)
	for _, g := range globs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
		if ok, _ := filepath.Match(strings.ToLower(g), lower); ok {
			return true
		}
	}
	return false
}

// hashAndCount reads a file once, producing both its digest and line count.
func hashAndCount(path string) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, 32*1024)
	var lines int
	var sawContent bool

	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			chunk := buf[:n]
			sawContent = true
			for _, b := range chunk {
				if b == '\n' {
					lines++
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}

	// A final line without a trailing newline still counts.
	if sawContent {
		if info, err := f.Stat(); err == nil && info.Size() > 0 {
			if last, err := lastByte(path); err == nil && last != '\n' {
				lines++
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), lines, nil
}

func lastByte(path string) (byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return 0, err
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, info.Size()-1); err != nil {
		return 0, err
	}
	return buf[0], nil
}

// DetectLanguage maps a path to a language name by extension and filename.
func DetectLanguage(path string) string {
	base := filepath.Base(path)
	switch base {
	case "Makefile", "makefile", "GNUmakefile":
		return "Makefile"
	case "Dockerfile":
		return "Dockerfile"
	case "Tiltfile", "Procfile":
		return base
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "Go"
	case ".rs":
		return "Rust"
	case ".ts":
		return "TypeScript"
	case ".tsx":
		return "TSX"
	case ".js", ".mjs", ".cjs":
		return "JavaScript"
	case ".jsx":
		return "JSX"
	case ".py":
		return "Python"
	case ".rb":
		return "Ruby"
	case ".java":
		return "Java"
	case ".kt":
		return "Kotlin"
	case ".swift":
		return "Swift"
	case ".c", ".h":
		return "C"
	case ".cc", ".cpp", ".hpp":
		return "C++"
	case ".cs":
		return "C#"
	case ".sh", ".bash", ".zsh":
		return "Shell"
	case ".proto":
		return "Protobuf"
	case ".sql":
		return "SQL"
	case ".md", ".mdx":
		return "Markdown"
	case ".yml", ".yaml":
		return "YAML"
	case ".toml":
		return "TOML"
	case ".json":
		return "JSON"
	case ".html":
		return "HTML"
	case ".css", ".scss":
		return "CSS"
	default:
		return ""
	}
}

// isDocPath reports whether a file is human-written prose rather than code.
func isDocPath(path string) bool {
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))

	if !strings.HasSuffix(lower, ".md") && !strings.HasSuffix(lower, ".mdx") &&
		!strings.HasSuffix(lower, ".rst") && !strings.HasSuffix(lower, ".txt") {
		return false
	}
	if strings.HasPrefix(base, "readme") || strings.HasPrefix(base, "changelog") ||
		strings.HasPrefix(base, "contributing") || base == "claude.md" || base == "agents.md" {
		return true
	}
	return strings.Contains(lower, "docs/") || strings.Contains(lower, "adr")
}
