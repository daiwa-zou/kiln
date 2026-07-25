package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/mapper"
)

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnitContextCarriesFileContents(t *testing.T) {
	// The API runner has no tools. If Go does not put the source in the prompt,
	// the model writes the page from the filename -- so this is the test that
	// stands between kiln and confidently fabricated documentation.
	root := t.TempDir()
	write(t, root, "internal/auth/token.go", "package auth\n\nfunc Mint() string { return \"\" }\n")

	got := renderUnitContext(root, mapper.Unit{
		Key:    "module:auth",
		Inputs: []string{"internal/auth/token.go"},
	})

	if !strings.Contains(got, "func Mint()") {
		t.Errorf("prompt context omitted the file body:\n%s", got)
	}
	if !strings.Contains(got, "internal/auth/token.go") {
		t.Errorf("prompt context omitted the path:\n%s", got)
	}
}

func TestUnitContextResolvesStagedInputsAgainstTheirOwnRoot(t *testing.T) {
	// A merged map holds units from two connectors with different roots. A doc
	// unit resolved against the repository root reads nothing, and the failure
	// is silent: the run succeeds and the page is invented.
	repo := t.TempDir()
	staging := t.TempDir()
	write(t, staging, "handbook.md", "# Handbook\n\nOn-call rotates weekly.\n")

	unit := mapper.Unit{
		Key:    "doc:handbook.pdf",
		Inputs: []string{"handbook.md"},
		Meta:   map[string]any{mapper.MetaRoot: staging},
	}

	got := renderUnitContext(repo, unit)
	if !strings.Contains(got, "On-call rotates weekly") {
		t.Errorf("staged document was not read:\n%s", got)
	}
}

func TestUnitContextIsEmptyWhenNothingIsReadable(t *testing.T) {
	// An empty string lets the caller omit the section. An empty heading reads
	// to the model as "this unit has no source", which is a different claim.
	got := renderUnitContext(t.TempDir(), mapper.Unit{
		Key:    "module:ghost",
		Inputs: []string{"does/not/exist.go"},
	})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestUnitContextSkipsBinaryFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "logo.png", "\x89PNG\x00\x00binary\x00garbage")
	write(t, root, "main.go", "package main\n")

	got := renderUnitContext(root, mapper.Unit{
		Inputs: []string{"logo.png", "main.go"},
	})

	if strings.Contains(got, "logo.png") {
		t.Errorf("binary file was sent to the model:\n%s", got)
	}
	if !strings.Contains(got, "package main") {
		t.Errorf("readable neighbour was dropped along with it:\n%s", got)
	}
}

func TestUnitContextCapsAFileAndSaysSo(t *testing.T) {
	root := t.TempDir()
	huge := strings.Repeat("// a line of filler\n", maxFileContextBytes/20+500)
	write(t, root, "generated.go", huge)

	got := renderUnitContext(root, mapper.Unit{Inputs: []string{"generated.go"}})

	if len(got) > maxFileContextBytes+2000 {
		t.Errorf("context is %d bytes, want it capped near %d", len(got), maxFileContextBytes)
	}
	// Silent truncation would let the model describe a file's second half as
	// absent rather than as unseen.
	if !strings.Contains(got, "Truncated") {
		t.Errorf("truncation was not disclosed:\n%s", got[:200])
	}
}

func TestUnitContextBudgetsAcrossFiles(t *testing.T) {
	root := t.TempDir()
	body := strings.Repeat("x", maxFileContextBytes)
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		write(t, root, name, body)
	}

	got := renderUnitContext(root, mapper.Unit{
		Inputs: []string{"a.go", "b.go", "c.go", "d.go", "e.go"},
	})

	if len(got) > maxUnitContextBytes+5000 {
		t.Errorf("unit context is %d bytes, want it capped near %d", len(got), maxUnitContextBytes)
	}
}

func TestUnitContextDeduplicatesInputs(t *testing.T) {
	// Every section of a split document points at the same parent file, so a
	// naive render would send a book once per chapter.
	root := t.TempDir()
	write(t, root, "book.md", "# Book\n\nchapter text\n")

	got := renderUnitContext(root, mapper.Unit{
		Inputs: []string{"book.md", "book.md", "book.md"},
	})

	if n := strings.Count(got, "chapter text"); n != 1 {
		t.Errorf("file body appears %d times, want 1", n)
	}
}
