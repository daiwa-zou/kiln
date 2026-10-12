package api

import (
	"regexp"
	"strings"
	"testing"
)

// Source-level pins for the per-document ingest status on the Ingest view
// (ST-U-2). Behaviour is a browser check; these catch the rules a reviewer
// would otherwise have to remember: the helpers exist, the connector bar can
// no longer say "Read cleanly" unconditionally, and the skip reason (text
// about a member's file) never reaches the DOM except through esc().

// TestUIIngestStatusHelpersExist pins the pure helpers the row, the stat and
// the Documents header share, so ROADMAP item 11 can build on the same names.
func TestUIIngestStatusHelpersExist(t *testing.T) {
	src := uiSource(t, "sources.js")
	for _, want := range []string{
		"function ingestSummary(",
		"function unreadCount(",
		"function ingestChip(",
		"function ingestReason(",
		"function attentionLine(",
		"function connectorHealth(",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("sources.js: missing %q", want)
		}
	}
	// The helpers are used, not merely defined: each call site has to appear
	// on a line that is not a function declaration, so `function ingestChip(f,
	// …) {` does not stand in for the row that renders the chip. The `${`
	// prefixes pin the template positions; unreadCount feeds the bar and the
	// Ingest now link through unreadDocs.
	calls := withoutDeclarations(src)
	for _, want := range []string{
		"ingestSummary(files)",
		"${ingestChip(f",
		"${ingestReason(f)",
		"${attentionLine(files)",
		"connectorHealth(c,",
		"unreadCount(summary)",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("sources.js: %q is never called", want)
		}
	}
}

// withoutDeclarations drops every line that declares a function, leaving the
// lines that can only be call sites.
func withoutDeclarations(src string) string {
	decl := regexp.MustCompile(`^\s*(async\s+)?function\s`)
	lines := strings.Split(src, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if decl.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// stripComments drops every `/* … */` block, across lines, and then every
// line that is only a `//` comment, so a guard that was commented out either
// way no longer satisfies a substring match. Trailing `//` comments are left
// alone: a URL in a string carries `//` too. sources.js has no `/*` inside a
// string or regex literal; if one arrives, the non-greedy match ends at the
// first `*/` and the pin fails loudly rather than passing by accident.
func stripComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	lines := strings.Split(src, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// TestUIConnectorBarGuardsReadCleanly: the upload source's bar says "Read
// cleanly" only when no document is unread. The literal may appear once, and
// the return that carries it must be preceded by the unread guard as a live
// statement: comments are stripped first, and the guard has to start its own
// line, so neither `// if (unread > 0) …` nor a `/* … */` block around it,
// on one line or several, counts.
// The behaviour itself is proven by web/test/ingest-status.test.ts.
func TestUIConnectorBarGuardsReadCleanly(t *testing.T) {
	src := stripComments(uiSource(t, "sources.js"))
	const literal = `"Read cleanly"`
	if n := strings.Count(src, literal); n != 1 {
		t.Fatalf("sources.js: %q appears %d times, want exactly 1", literal, n)
	}
	at := strings.Index(src, literal)
	before := src[max(0, at-400):at]
	guard := regexp.MustCompile(`(?m)^\s*if \(unread > 0\) return`)
	if !guard.MatchString(before) {
		t.Errorf("sources.js: %q is not guarded by a live unread check within the preceding 400 chars:\n%s", literal, before)
	}
	for _, want := range []string{
		`"Some documents are unread; see below."`,
		`connectorHealth(c, c.kind === "upload" ? unreadDocs : 0)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("sources.js: missing %q", want)
		}
	}
}

// TestUIIngestReasonIsEscaped: every read of ingest.reason / ingest.class in
// the UI is the argument of esc(). The reason is a fixed sentence today, but
// the field is text about a member's file and the rule has to hold whatever
// the API sends.
func TestUIIngestReasonIsEscaped(t *testing.T) {
	src := uiSource(t, "sources.js")
	re := regexp.MustCompile(`ingest\??\.(reason|class)\b`)
	locs := re.FindAllStringIndex(src, -1)
	if len(locs) == 0 {
		t.Fatal("sources.js: ingest.reason is never rendered; the guard is out of date")
	}
	var reason, class int
	for _, loc := range locs {
		// Allow "esc(f.ingest?.reason" and "esc(ingest.reason": the esc( must
		// be the nearest open call before the field.
		head := src[max(0, loc[0]-12):loc[0]]
		if !strings.HasSuffix(head, "esc(f.") && !strings.HasSuffix(head, "esc(") {
			t.Errorf("sources.js: %q rendered without esc(): …%s%s…", src[loc[0]:loc[1]], head, src[loc[1]:min(len(src), loc[1]+20)])
		}
		if strings.HasSuffix(src[loc[0]:loc[1]], "reason") {
			reason++
		} else {
			class++
		}
	}
	if reason == 0 || class == 0 {
		t.Errorf("sources.js: want both the reason and the class rendered; got reason=%d class=%d", reason, class)
	}
	// The reason is a visible line, not a hover: it sits in its own element
	// with the class token as a data attribute, never only in a title.
	if !strings.Contains(src, `data-class="${esc(f.ingest?.class)}"`) {
		t.Error("sources.js: the reason line does not carry data-class")
	}
	if strings.Contains(src, `title="${esc(f.ingest?.reason)}"`) {
		t.Error("sources.js: the reason is hover-only")
	}
}

// TestUIIngestCopy pins the words a member reads, so a rewording is a
// deliberate change to the story's done-when rather than drift.
func TestUIIngestCopy(t *testing.T) {
	src := uiSource(t, "sources.js")
	for _, want := range []string{
		"Waiting for the next ingest",
		`>Not read</span>`,
		`plural(ing.pages, "page")`,
		"ingest in progress",
		`data-ingest-now>Ingest now</button>`,
		`${n.notRead} not read`,
		`${n.paused} paused`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("sources.js: missing %q", want)
		}
	}
	// The attention line's two forms.
	if !strings.Contains(src, `${n === 1 ? "was" : "were"} not read`) {
		t.Error("sources.js: the attention line does not say 'was not read' / 'were not read'")
	}
	// The Documents note's Ingest now presses the existing button rather than
	// enqueueing on its own.
	if !strings.Contains(src, `[data-ingest-now]`) || !strings.Contains(src, "buildBtn?.click()") {
		t.Error("sources.js: the note's Ingest now is not wired to the header button")
	}
	// The help popover says what the three words mean.
	app := uiSource(t, "app.js")
	for _, want := range []string{"<em>read</em>", "<em>waiting</em>", "<em>not read</em>"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js: the Ingest help does not explain %q", want)
		}
	}
}
