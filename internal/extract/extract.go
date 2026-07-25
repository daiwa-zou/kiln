// Package extract normalizes documents to markdown or plain text.
//
// Go's PDF and Office ecosystem is weak -- the mature Office library is
// commercially licensed and pure-Go PDF extractors handle real-world layouts
// poorly -- so these shell out to tools baked into the worker image. Whether a
// tool is present is a property of the deployment, not of the code, so a missing
// binary produces a clear, catchable error rather than a panic or silent empty
// output.
package extract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Format is the shape of an input document.
type Format string

const (
	FormatText     Format = "text"
	FormatMarkdown Format = "markdown"
	FormatPDF      Format = "pdf"
	FormatOffice   Format = "office"
	FormatHTML     Format = "html"
	FormatUnknown  Format = "unknown"
)

// ErrNoExtractor means nothing handles this format.
var ErrNoExtractor = errors.New("extract: no extractor for format")

// ErrToolMissing means the extractor exists but its binary is not installed.
//
// Distinguished from a genuine extraction failure so an operator sees "install
// poppler" rather than "this PDF is corrupt".
var ErrToolMissing = errors.New("extract: required tool is not installed")

// Result is extracted content plus how it was produced.
type Result struct {
	Text string
	// Format is what the input was recognized as.
	Format Format
	// Tool names what did the extraction, for troubleshooting a bad result.
	Tool string
	// Hash is a digest of the extracted text, so a re-extraction that produces
	// identical content does not look like a change.
	Hash string
}

// Extractor converts one document to text.
type Extractor interface {
	// Handles reports whether this extractor claims a format.
	Handles(Format) bool
	// Available reports whether it can actually run right now. An extractor
	// whose binary is absent is registered but unavailable, which is what makes
	// the missing-tool error specific.
	Available() bool
	// Extract reads a file and returns normalized text.
	Extract(ctx context.Context, path string) (string, error)
	// Tool names the underlying implementation.
	Tool() string
}

// DetectFormat classifies a path by extension.
func DetectFormat(path string) Format {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".mdx", ".markdown":
		return FormatMarkdown
	case ".txt", ".text", ".rst", ".org":
		return FormatText
	case ".pdf":
		return FormatPDF
	case ".docx", ".doc", ".pptx", ".ppt", ".xlsx", ".xls", ".odt", ".odp", ".ods", ".epub", ".rtf":
		return FormatOffice
	case ".html", ".htm":
		return FormatHTML
	default:
		return FormatUnknown
	}
}

// Extractors is an ordered set. The first entry that handles a format and is
// available wins, so a preferred tool can be listed ahead of a fallback.
type Extractors []Extractor

// DefaultExtractors is the standard set.
func DefaultExtractors() Extractors {
	return Extractors{
		&PassthroughExtractor{},
		&PDFExtractor{Binary: "pdftotext"},
		&PandocExtractor{Binary: "pandoc"},
	}
}

// Extract normalizes one file.
func (es Extractors) Extract(ctx context.Context, path string) (*Result, error) {
	format := DetectFormat(path)

	var claimed bool
	for _, e := range es {
		if !e.Handles(format) {
			continue
		}
		claimed = true
		if !e.Available() {
			continue
		}

		text, err := e.Extract(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("extract: %s (%s): %w", filepath.Base(path), e.Tool(), err)
		}
		return &Result{
			Text:   text,
			Format: format,
			Tool:   e.Tool(),
			Hash:   HashText(text),
		}, nil
	}

	if claimed {
		// Something handles the format but cannot run, which is a deployment
		// problem an operator can fix.
		return nil, fmt.Errorf("%w: %s needs a tool that is not installed", ErrToolMissing, format)
	}
	return nil, fmt.Errorf("%w: %s", ErrNoExtractor, format)
}

// Supported reports which formats can actually be extracted right now, so a
// caller can report capability rather than discovering it per file.
func (es Extractors) Supported() []Format {
	seen := map[Format]bool{}
	var out []Format

	for _, f := range []Format{FormatText, FormatMarkdown, FormatPDF, FormatOffice, FormatHTML} {
		for _, e := range es {
			if e.Handles(f) && e.Available() && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// HashText digests extracted content. Keyed on content alone so re-extracting
// the same document never looks like a change.
func HashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// PassthroughExtractor handles text and markdown, which need no conversion.
// Pure Go, so it works in any deployment.
type PassthroughExtractor struct{}

func (p *PassthroughExtractor) Handles(f Format) bool {
	return f == FormatText || f == FormatMarkdown
}
func (p *PassthroughExtractor) Available() bool { return true }
func (p *PassthroughExtractor) Tool() string    { return "passthrough" }

// MaxExtractBytes caps how much text one document may contribute. The full
// text is held in memory through mapping and staging, so an unbounded read is
// how a single oversized upload takes down a worker.
const MaxExtractBytes = 32 << 20 // 32 MiB

func (p *PassthroughExtractor) Extract(_ context.Context, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > MaxExtractBytes {
		return "", fmt.Errorf("extract: %s is %d bytes, above the %d-byte limit", path, info.Size(), MaxExtractBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return normalize(string(raw)), nil
}

// defaultTimeout bounds a subprocess. A malformed document can send a converter
// into a very long loop, and one bad upload should not stall a run.
const defaultTimeout = 2 * time.Minute

// PDFExtractor shells out to poppler's pdftotext.
type PDFExtractor struct {
	Binary  string
	Timeout time.Duration
}

func (p *PDFExtractor) Handles(f Format) bool { return f == FormatPDF }
func (p *PDFExtractor) Tool() string          { return p.binary() }

func (p *PDFExtractor) Available() bool {
	_, err := exec.LookPath(p.binary())
	return err == nil
}

func (p *PDFExtractor) binary() string {
	if p.Binary != "" {
		return p.Binary
	}
	return "pdftotext"
}

func (p *PDFExtractor) Extract(ctx context.Context, path string) (string, error) {
	// -layout preserves column structure, which matters for tables; "-" sends
	// output to stdout instead of writing a sibling file next to the input.
	return runTool(ctx, p.binary(), p.Timeout, "-layout", "-enc", "UTF-8", path, "-")
}

// PandocExtractor shells out to pandoc, which covers the whole Office and ebook
// set with one tool.
type PandocExtractor struct {
	Binary  string
	Timeout time.Duration
}

func (p *PandocExtractor) Handles(f Format) bool { return f == FormatOffice || f == FormatHTML }
func (p *PandocExtractor) Tool() string          { return p.binary() }

func (p *PandocExtractor) Available() bool {
	_, err := exec.LookPath(p.binary())
	return err == nil
}

func (p *PandocExtractor) binary() string {
	if p.Binary != "" {
		return p.Binary
	}
	return "pandoc"
}

func (p *PandocExtractor) Extract(ctx context.Context, path string) (string, error) {
	// GitHub-flavored markdown without wrapping: hard-wrapped output would make
	// every later diff noisy for no benefit. --sandbox because the inputs are
	// untrusted documents: it disables pandoc's file inclusion and network
	// access from within a document.
	return runTool(ctx, p.binary(), p.Timeout, "--sandbox", "-t", "gfm", "--wrap=none", path)
}

func runTool(ctx context.Context, binary string, timeout time.Duration, args ...string) (string, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%s timed out after %s", binary, timeout)
		}
		return "", fmt.Errorf("%s: %w (%s)", binary, err, truncate(stderr.String(), 200))
	}
	return normalize(stdout.String()), nil
}

// normalize makes output comparable across tools and platforms, so a hash
// depends on content rather than on line endings or trailing whitespace.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimRight(s, " \t\n")

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
