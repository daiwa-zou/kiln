package extract

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		path string
		want Format
	}{
		{"notes.md", FormatMarkdown},
		{"NOTES.MD", FormatMarkdown},
		{"readme.txt", FormatText},
		{"report.pdf", FormatPDF},
		{"deck.pptx", FormatOffice},
		{"sheet.xlsx", FormatOffice},
		{"book.epub", FormatOffice},
		{"page.html", FormatHTML},
		{"binary.zip", FormatUnknown},
		{"noextension", FormatUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := DetectFormat(tt.path); got != tt.want {
				t.Errorf("DetectFormat(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestPassthroughWorksWithNoToolsInstalled(t *testing.T) {
	// Text and markdown are pure Go, so they extract in any deployment. This is
	// what lets a dev machine with no poppler still exercise the pipeline.
	path := writeFile(t, "notes.md", "# Title\r\n\r\nBody text.   \r\n")

	res, err := DefaultExtractors().Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Tool != "passthrough" {
		t.Errorf("Tool = %q", res.Tool)
	}
	// Normalization means a hash depends on content, not on the line endings or
	// trailing whitespace a particular editor happened to leave.
	if strings.Contains(res.Text, "\r") {
		t.Error("CRLF survived normalization")
	}
	if strings.Contains(res.Text, "   \n") {
		t.Error("trailing whitespace survived normalization")
	}
	if res.Hash == "" {
		t.Error("no hash was computed")
	}
}

func TestMissingToolIsDistinctFromExtractionFailure(t *testing.T) {
	// An operator seeing this needs "install poppler", not "this PDF is broken".
	es := Extractors{&PDFExtractor{Binary: "kiln-nonexistent-pdftotext"}}
	path := writeFile(t, "report.pdf", "%PDF-1.4\n")

	_, err := es.Extract(context.Background(), path)
	if err == nil {
		t.Fatal("Extract succeeded with a missing binary")
	}
	if !errors.Is(err, ErrToolMissing) {
		t.Errorf("err = %v, want ErrToolMissing so the cause is actionable", err)
	}
}

func TestUnhandledFormatIsDistinctFromMissingTool(t *testing.T) {
	path := writeFile(t, "archive.zip", "PK\x03\x04")

	_, err := DefaultExtractors().Extract(context.Background(), path)
	if !errors.Is(err, ErrNoExtractor) {
		t.Errorf("err = %v, want ErrNoExtractor", err)
	}
	// Nothing to install would fix an unsupported format, so it must not be
	// reported as a missing tool.
	if errors.Is(err, ErrToolMissing) {
		t.Error("an unsupported format was reported as a missing tool")
	}
}

func TestSupportedReflectsWhatIsInstalled(t *testing.T) {
	supported := DefaultExtractors().Supported()

	// Passthrough is always available, so these two are always supported.
	for _, want := range []Format{FormatText, FormatMarkdown} {
		if !containsFormat(supported, want) {
			t.Errorf("%s should always be supported: %v", want, supported)
		}
	}

	// PDF support depends on the deployment, so this asserts the report agrees
	// with reality rather than asserting a particular answer.
	pdf := &PDFExtractor{}
	if got := containsFormat(supported, FormatPDF); got != pdf.Available() {
		t.Errorf("Supported() reports PDF=%v but pdftotext available=%v", got, pdf.Available())
	}
}

func TestFirstAvailableExtractorWins(t *testing.T) {
	// An unavailable extractor listed first must not shadow a working one.
	es := Extractors{
		&PDFExtractor{Binary: "kiln-nonexistent"},
		&fakeExtractor{format: FormatPDF, text: "from the fallback"},
	}

	res, err := es.Extract(context.Background(), writeFile(t, "x.pdf", "%PDF"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Text != "from the fallback" {
		t.Errorf("Text = %q, want the fallback's output", res.Text)
	}
}

// fakeExtractor stands in for a tool without needing one installed.
type fakeExtractor struct {
	format Format
	text   string
	err    error
}

func (f *fakeExtractor) Handles(x Format) bool { return x == f.format }
func (f *fakeExtractor) Available() bool       { return true }
func (f *fakeExtractor) Tool() string          { return "fake" }
func (f *fakeExtractor) Extract(context.Context, string) (string, error) {
	return f.text, f.err
}

func TestExtractionFailureNamesTheTool(t *testing.T) {
	es := Extractors{&fakeExtractor{format: FormatPDF, err: errors.New("malformed xref")}}

	_, err := es.Extract(context.Background(), writeFile(t, "x.pdf", "%PDF"))
	if err == nil {
		t.Fatal("Extract succeeded despite an extractor error")
	}
	// Troubleshooting a bad extraction starts with knowing what produced it.
	if !strings.Contains(err.Error(), "fake") {
		t.Errorf("err = %v, want it to name the tool", err)
	}
}

// fakeTool writes an executable that ignores its arguments and prints body,
// standing in for a converter so the real exec path is exercised without
// depending on poppler or pandoc being installed.
func fakeTool(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}

	path := filepath.Join(t.TempDir(), "fake-converter")
	script := "#!/bin/sh\ncat <<'KILN_EOF'\n" + body + "\nKILN_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubprocessExtractorAgainstRealBinary(t *testing.T) {
	tool := fakeTool(t, "extracted body\r\ntrailing   ")
	es := Extractors{&PDFExtractor{Binary: tool}}

	res, err := es.Extract(context.Background(), writeFile(t, "doc.pdf", "%PDF-1.4\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(res.Text, "extracted body") {
		t.Errorf("Text = %q", res.Text)
	}
	// Subprocess output goes through the same normalization as passthrough, so
	// a hash never depends on which tool produced the text.
	if strings.Contains(res.Text, "\r") || strings.Contains(res.Text, "   \n") {
		t.Errorf("subprocess output was not normalized: %q", res.Text)
	}
}

func TestSubprocessFailureIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}

	// A converter that exits non-zero must surface as an error, not as empty
	// output that would quietly produce a blank page.
	path := filepath.Join(t.TempDir(), "failing-converter")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'malformed xref' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	es := Extractors{&PDFExtractor{Binary: path}}
	_, err := es.Extract(context.Background(), writeFile(t, "doc.pdf", "%PDF"))
	if err == nil {
		t.Fatal("a failing converter was treated as success")
	}
	if !strings.Contains(err.Error(), "malformed xref") {
		t.Errorf("stderr should reach the error, got: %v", err)
	}
}

func TestHashTextIsContentOnly(t *testing.T) {
	// The same document staged in two different temp directories must hash
	// identically, or every re-extraction would look like a change and nothing
	// would ever be skipped.
	const body = "# Title\n\nSame content.\n"

	first, err := DefaultExtractors().Extract(context.Background(), writeFile(t, "a.md", body))
	if err != nil {
		t.Fatal(err)
	}
	second, err := DefaultExtractors().Extract(context.Background(), writeFile(t, "b.md", body))
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash {
		t.Error("hash depends on where the document was staged, not on its content")
	}

	if HashText("a") == HashText("b") {
		t.Error("different content produced the same hash")
	}
}

func containsFormat(haystack []Format, needle Format) bool {
	for _, f := range haystack {
		if f == needle {
			return true
		}
	}
	return false
}
