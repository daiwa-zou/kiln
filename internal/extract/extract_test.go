package extract

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// exec.CommandContext kills only the direct child. A converter that forks a
// helper which inherits stdout leaves Wait blocked on the pipe until the
// helper exits, so the timeout bounded nothing. WaitDelay is what makes it a
// wall-clock bound.
func TestToolTimeoutBoundsWallClockWhenAGrandchildHoldsStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	old := toolWaitDelay
	toolWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { toolWaitDelay = old })

	// The backgrounded sleeper inherits stdout and outlives the converter.
	path := filepath.Join(t.TempDir(), "forking-converter")
	script := "#!/bin/sh\nsleep 20 &\nexec sleep 20\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := runTool(context.Background(), path, 200*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("runTool took %s; the timeout plus WaitDelay should have bounded it well under that", elapsed)
	}
}

// A converter that hangs is reported as ErrTimedOut, through the dispatch's
// wrapping too: the upload connector classifies the skip with errors.Is, and
// matching the message instead would turn every timeout into a plain
// extractor error the day the message is reworded.
func TestToolTimeoutIsTheSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	hanging := filepath.Join(t.TempDir(), "hanging-converter")
	if err := os.WriteFile(hanging, []byte("#!/bin/sh\nexec sleep 20\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := runTool(context.Background(), hanging, 100*time.Millisecond)
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("runTool err = %v, want ErrTimedOut", err)
	}
	if !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Errorf("the message lost the tool and the timeout: %v", err)
	}

	es := Extractors{&PDFExtractor{Binary: hanging, Timeout: 100 * time.Millisecond}}
	if _, err := es.Extract(context.Background(), writeFile(t, "doc.pdf", "%PDF")); !errors.Is(err, ErrTimedOut) {
		t.Errorf("Extractors.Extract err = %v, want it to wrap ErrTimedOut", err)
	}

	// A converter that fails for its own reasons is not a timeout.
	failing := filepath.Join(t.TempDir(), "failing-converter")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(context.Background(), failing, time.Minute); err == nil || errors.Is(err, ErrTimedOut) {
		t.Errorf("a failing converter reads as a timeout: %v", err)
	}
}

// A small compressed PDF can expand to gigabytes of text. The converter must
// be stopped at the cap, not allowed to fill memory and then checked.
func TestToolOutputOverTheCapIsRefusedAndTheToolStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}

	// yes never exits on its own, so returning at all proves it was stopped.
	path := filepath.Join(t.TempDir(), "endless-converter")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec yes 'a line of extracted text'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := runTool(context.Background(), path, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "above the") || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want an over-the-limit error", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("the cap should be reported as such, not as a timeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %s; the process was not stopped at the cap", elapsed)
	}
}

func TestCappedWriterKeepsExactlyTheCap(t *testing.T) {
	stopped := 0
	w := &cappedWriter{max: 5, stop: func() { stopped++ }}
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil || w.exceeded {
		t.Fatalf("first write: n=%d err=%v exceeded=%v", n, err, w.exceeded)
	}
	// Exactly the cap is allowed; only a byte beyond it is too much.
	if n, err := w.Write([]byte("de")); n != 2 || err != nil || w.exceeded {
		t.Fatalf("write to the cap: n=%d err=%v exceeded=%v", n, err, w.exceeded)
	}
	if n, err := w.Write([]byte("f")); n != 1 || err != nil || !w.exceeded {
		t.Fatalf("write past the cap: n=%d err=%v exceeded=%v", n, err, w.exceeded)
	}
	_, _ = w.Write([]byte("more"))
	if got := w.buf.String(); got != "abcde" {
		t.Errorf("buffer = %q, want %q", got, "abcde")
	}
	if stopped != 1 {
		t.Errorf("stop called %d times, want 1", stopped)
	}
}

// The converter parses an untrusted document, so it must not inherit the
// parent's KILN_* secrets.
func TestToolRunsWithoutTheParentsSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	t.Setenv("KILN_MASTER_KEY", "master-secret")
	t.Setenv("KILN_SESSION_SECRET", "session-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-parent")

	path := filepath.Join(t.TempDir(), "env-dumper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runTool(context.Background(), path, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"KILN_", "ANTHROPIC_"} {
		if strings.Contains(out, key) {
			t.Errorf("child environment contains %s*:\n%s", key, out)
		}
	}
	if !strings.Contains(out, "PATH=") {
		t.Errorf("PATH did not reach the child:\n%s", out)
	}
}
