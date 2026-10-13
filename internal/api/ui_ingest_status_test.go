package api

import (
	"regexp"
	"strings"
	"testing"
)

// Source-level pins for the per-document ingest status on the Ingest view.
// What the helpers do is proven by web/test/ingest-status.test.ts, which runs
// sources.js; these hold only what that cannot see: that the helpers are
// called from the view, that the connector bar can no longer say "Read
// cleanly" unconditionally, that the skip reason (text about a member's
// file) never reaches the DOM except through esc(), and the help copy, which
// lives in app.js where the vitest loader does not reach.

// TestUIIngestStatusHelpersUsed pins the call sites: each helper has to
// appear on a live line that is not a function declaration, so `function
// ingestChip(f, …) {` does not stand in for the row that renders the chip, and
// a call that was commented out, with `//` or inside `/* … */`, does not stand
// in for one that runs. The `${` prefixes pin the template positions; the
// summary feeds the bar from both render paths; patchIngestStatus is what a
// settled run reaches the page through.
func TestUIIngestStatusHelpersUsed(t *testing.T) {
	calls := withoutDeclarations(stripComments(uiSource(t, "sources.js")))
	for _, want := range []string{
		"ingestSummary(files)",
		"${ingestChip(f",
		"${ingestReason(f)",
		"${attentionLine(files)",
		"connectorHealth(c,",
		"connectorHealth(upload, summary)",
		"patchIngestStatus(document,",
		"onSettle: refreshDocuments",
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
// cleanly" only when no document is unread, and names which kind of unread
// it found: a skip is "not read", a document the next ingest will pick up is
// "waiting". The literal may appear once, and the return that carries it must
// be preceded by both guards as live statements, the skip guard first (a
// bench with one of each is told about the skip): comments are stripped
// first, and each guard has to start its own line, so neither `// if
// (summary?.notRead > 0) …` nor a `/* … */` block around it, on one line or
// several, counts. The bar's call site has to hand the summary over, or the
// guards are never reached.
func TestUIConnectorBarGuardsReadCleanly(t *testing.T) {
	src := stripComments(uiSource(t, "sources.js"))
	const literal = `"Read cleanly"`
	if n := strings.Count(src, literal); n != 1 {
		t.Fatalf("sources.js: %q appears %d times, want exactly 1", literal, n)
	}
	at := strings.Index(src, literal)
	before := src[max(0, at-600):at]
	guards := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*if \(summary\?\.notRead > 0\) return .*"Some documents were not read; see below\."`),
		regexp.MustCompile(`(?m)^\s*if \(summary\?\.waiting > 0\) return .*"Some documents are waiting for the next ingest; see below\."`),
	}
	last := -1
	for _, guard := range guards {
		loc := guard.FindStringIndex(before)
		if loc == nil {
			t.Errorf("sources.js: %q is not guarded by a live %s within the preceding 600 chars:\n%s", literal, guard, before)
			continue
		}
		if loc[0] < last {
			t.Errorf("sources.js: the waiting guard runs before the not-read guard; a skip beside a waiting file would be told \"waiting\"")
		}
		last = loc[0]
	}
	for _, want := range []string{
		`connectorHealth(c, c.kind === "upload" ? summary : null)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("sources.js: missing %q", want)
		}
	}
}

// TestUIPatchIngestStatusIdsExist ties the settle refresh to the page it
// patches: every `#id` patchIngestStatus looks up has to be an id the view
// renders, as `id="…"` in its markup or `id: "…"` handed to the stat helper.
// The vitest cases build their own skeleton with these ids, so renaming one in
// the view would pass them while that part of the page silently stopped
// updating after a run settles. The function body is the text between its
// declaration and the first line that is a lone `}`; the ids are looked for
// everywhere else in the file.
func TestUIPatchIngestStatusIdsExist(t *testing.T) {
	src := stripComments(uiSource(t, "sources.js"))
	start := strings.Index(src, "\nfunction patchIngestStatus(")
	if start < 0 {
		t.Fatal("sources.js: patchIngestStatus is not declared")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("sources.js: patchIngestStatus has no closing brace on its own line")
	}
	body := src[start : start+end]
	rest := src[:start] + src[start+end:]
	ids := regexp.MustCompile(`querySelector\("#([\w-]+)"\)`).FindAllStringSubmatch(body, -1)
	if len(ids) == 0 {
		t.Fatal("sources.js: patchIngestStatus queries no #id; the pin is out of date")
	}
	for _, m := range ids {
		id := m[1]
		if !strings.Contains(rest, `id="`+id+`"`) && !strings.Contains(rest, `id: "`+id+`"`) {
			t.Errorf("sources.js: patchIngestStatus patches #%s, which the view never renders", id)
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
}

// TestUIIngestHelpExplainsStates pins the one sentence a member reads about
// the three states, in the Ingest help popover (app.js) and the Getting
// started panel (sources.js). Markup and line wrapping are taken out before
// the match, so the pin is the words, not the layout.
func TestUIIngestHelpExplainsStates(t *testing.T) {
	const want = "Each uploaded document shows one of three states: read (and how many pages it produced), " +
		"waiting for the next ingest, or not read, with the reason on its row. Paused documents are left out."
	for _, name := range []string{"app.js", "sources.js"} {
		if !strings.Contains(prose(uiSource(t, name)), want) {
			t.Errorf("%s: the help copy does not say %q", name, want)
		}
	}
}

// prose flattens a UI source for a copy match: tags gone, whitespace runs
// collapsed to one space.
func prose(src string) string {
	src = regexp.MustCompile(`</?[a-z]+>`).ReplaceAllString(src, "")
	return strings.Join(strings.Fields(src), " ")
}
