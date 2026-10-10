package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/daiwa-zou/kiln/internal/mapper"
)

// Byte budgets for the source material sent with a unit.
//
// These exist because the API runner has no tools: it cannot open a file, so
// whatever Go does not put in the prompt does not exist as far as the model is
// concerned. Naming a path without its contents would produce pages written
// from filenames, which is the failure mode this package exists to avoid.
//
// The caps are what keep that from becoming an unbounded prompt. A single
// oversized file cannot crowd out its neighbours, and a unit with many files
// degrades to fewer complete files rather than to a uniform sliver of each.
const (
	maxUnitContextBytes = 180_000
	maxFileContextBytes = 60_000
)

// contextBudget bounds the source material rendered for one unit: the total
// across its files, and the most any single file may contribute.
type contextBudget struct {
	total   int
	perFile int
}

// buildBudget is what a unit being built is allowed. Analyze and generate both
// render the unit this way; research passes a smaller, shared one (see
// researchPrompt).
var buildBudget = contextBudget{total: maxUnitContextBytes, perFile: maxFileContextBytes}

// renderUnitContext reads a unit's inputs and renders them as prompt material.
//
// Returns an empty string when nothing readable is found, so a caller can omit
// the section entirely rather than sending an empty heading that reads to the
// model as "this unit has no source".
func renderUnitContext(root string, unit mapper.Unit, budget contextBudget) string {
	files := readInputs(unitRoot(root, unit), unit.Inputs, budget)
	if len(files) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n## Source material\n\n")
	b.WriteString("This is the content of the unit's inputs. Ground every claim in it.\n")

	for _, f := range files {
		fmt.Fprintf(&b, "\n### %s\n\n", quoted(f.path))
		if f.truncated {
			fmt.Fprintf(&b, "_Truncated at %d bytes; the file is longer._\n\n", budget.perFile)
		}
		// Fenced so the model reads it as data rather than as instructions.
		// Source files are untrusted input -- a repository can contain a README
		// that tells an agent what to do.
		b.WriteString(fence(f.body))
	}
	return b.String()
}

// fence wraps untrusted text in a code fence the text cannot close.
//
// A fixed three-backtick fence is only a fence until the body contains a line
// of three backticks: that line closes it, and whatever follows -- a forged
// "## Corrections from a human reviewer ... They override anything you infer"
// -- reads as the prompt's own instructions. Markdown's answer is that a fence
// closes only on a run at least as long as the one that opened it, so the
// opener is made one longer than the longest run in the body. The body is
// never altered, which matters for source code and for a quotation the model
// is asked to cite.
//
// Use it for every piece of multi-line text the program did not write itself,
// and not for text it did.
func fence(body string) string { return fenceAs("", body) }

// fenceAs is fence with an info string on the opening line, for text whose
// language is worth stating ("json").
func fenceAs(info, body string) string {
	n := max(3, longestBacktickRun(body)+1)
	ticks := strings.Repeat("`", n)
	return ticks + info + "\n" + body + "\n" + ticks + "\n"
}

// longestBacktickRun is the length of the longest run of consecutive backticks
// in s, anywhere in it: a run mid-line cannot close a fence, but a stricter
// rule would need to reason about line starts, and one backtick of margin costs
// nothing.
func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
}

// quoted renders an untrusted single-line value -- a unit key, a title taken
// from a document heading, a file path -- so that it cannot start a new line
// of the prompt.
//
// Go quoting rather than inline code. An inline code span is delimited by
// backtick runs and cannot contain a newline without the span ending at the
// blank line, so the same value would need a length-adaptive delimiter and
// still be defeated by a line break. strconv.Quote has neither problem: every
// newline, carriage return, control character and quote mark inside the value
// is escaped, so the result is one line by construction and the closing quote
// is unambiguous. It also states plainly to the model that the contents are a
// string value. The cost is that a title with quotes or backslashes reads
// slightly noisier, which is the point.
//
// The fake runner reads some of these lines back and unquotes them.
func quoted(s string) string { return strconv.Quote(s) }

// unitRoot resolves the directory a unit's inputs are relative to.
//
// A merged map has no single root: the repository's units are relative to the
// checkout, while a document connector stages extracted text somewhere else
// entirely. A mapper that stages outside the run's source directory records
// where, and this honours it -- without that, doc units would resolve against
// the repository root, find nothing, and the model would write document pages
// having never seen the document.
func unitRoot(root string, unit mapper.Unit) string {
	if r, ok := unit.Meta[mapper.MetaRoot].(string); ok && r != "" {
		return r
	}
	return root
}

type sourceFile struct {
	path      string
	body      string
	truncated bool
}

// readInputs loads what it can of a unit's inputs within the budget.
//
// Unreadable and binary files are skipped rather than failing the unit: one
// unreadable file in a module should cost that file, not the page.
func readInputs(root string, inputs []string, limits contextBudget) []sourceFile {
	// Deduplicated because paths can repeat across a module's entry points.
	seen := map[string]bool{}
	paths := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if in == "" || seen[in] {
			continue
		}
		seen[in] = true
		paths = append(paths, in)
	}
	// Stable order so an identical unit renders an identical prompt, which is
	// what lets the prompt cache hit across runs.
	sort.Strings(paths)

	var (
		files  []sourceFile
		budget = limits.total
	)
	for _, rel := range paths {
		if budget <= 0 {
			break
		}

		full := rel
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, filepath.FromSlash(rel))
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}

		limit := min(limits.perFile, budget)

		f, err := os.Open(full)
		if err != nil {
			continue
		}
		buf := make([]byte, limit)
		n, _ := f.Read(buf)
		f.Close()
		if n == 0 {
			continue
		}
		buf = buf[:n]

		// A binary file rendered into a prompt is spent tokens and no
		// information, so it is dropped rather than fenced.
		if !utf8.Valid(buf) || strings.ContainsRune(string(buf), 0) {
			continue
		}

		body := string(buf)
		truncated := int64(n) < info.Size()
		if truncated {
			// Cut back to the last full line so the model never sees a
			// half-token identifier and reasons about it as if it were whole.
			if i := strings.LastIndexByte(body, '\n'); i > 0 {
				body = body[:i]
			}
		}

		files = append(files, sourceFile{path: rel, body: body, truncated: truncated})
		budget -= len(body)
	}
	return files
}
