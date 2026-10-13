package api

import (
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	uploadconn "github.com/daiwa-zou/kiln/internal/connector/upload"
	"github.com/daiwa-zou/kiln/internal/store"
)

// The derivation is the one place the three status words are decided, so
// every rule and every ordering question gets a row here. No database: the
// store's facts are built by hand.
func TestDeriveIngest(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) // the bytes changed
	before := t0.Add(-time.Hour)
	after := t0.Add(time.Hour)
	str := func(s string) *string { return &s }
	tm := func(v time.Time) *time.Time { return &v }
	pages := 6

	// row builds a FileIngestRow around the content timestamp; cases tweak it.
	row := func(mods ...func(*store.FileIngestRow)) store.FileIngestRow {
		r := store.FileIngestRow{ContentAt: t0}
		r.Enabled = true
		for _, m := range mods {
			m(&r)
		}
		return r
	}
	source := func(readAt time.Time) func(*store.FileIngestRow) {
		return func(r *store.FileIngestRow) {
			r.HasSource, r.Pages, r.ReadAt = true, &pages, tm(readAt)
		}
	}
	skip := func(class string, live bool) func(*store.FileIngestRow) {
		return func(r *store.FileIngestRow) { r.SkipClass, r.SkipLive = str(class), live }
	}
	staged := func(at time.Time) func(*store.FileIngestRow) {
		return func(r *store.FileIngestRow) { r.StagedAt = tm(at) }
	}
	paused := func(r *store.FileIngestRow) { r.Enabled = false }

	idle := store.FileIngestBench{}
	pending := store.FileIngestBench{PendingRun: true}
	budgetPaused := store.FileIngestBench{BudgetPaused: true}
	pendingBudgetPaused := store.FileIngestBench{PendingRun: true, BudgetPaused: true}

	read := IngestJSON{Status: ingestRead, Pages: &pages, ReadAt: str("2026-10-01T13:00:00Z")}
	waiting := IngestJSON{Status: ingestWaiting}
	unrecorded := IngestJSON{Status: ingestSkipped, Reason: str(skipSentenceUnrecorded)}
	noText := IngestJSON{Status: ingestSkipped, Reason: str(skipSentences["no_text"]), Class: str("no_text")}
	pausedJSON := IngestJSON{Status: ingestSkipped, Reason: str(skipSentences[skipClassPaused]), Class: str(skipClassPaused)}

	cases := []struct {
		name  string
		row   store.FileIngestRow
		bench store.FileIngestBench
		want  IngestJSON
	}{
		// Rule 5: nothing has looked at these bytes yet. The bench's runs are
		// not consulted, so a run that never staged the file (whatever its
		// trigger or connector) leaves it here.
		{name: "no build has staged it", row: row(), bench: idle, want: waiting},
		{name: "staged before the bytes changed (replaced since)", row: row(staged(before)), bench: idle, want: waiting},
		{name: "staged, nothing read, but a build is pending", row: row(staged(after)), bench: pending, want: waiting},

		// Rule 4: a build staged these bytes and left no source and no reason.
		{name: "staged after the change, no source, no class", row: row(staged(after)), bench: idle, want: unrecorded},
		{name: "staged at the moment of the change counts as staged", row: row(staged(t0)), bench: idle, want: unrecorded},
		// A continuation refused over budget: the file was passed over, and
		// "press Ingest now" would be refused too, so the sentence says why.
		// The wording is pinned: nothing re-enqueues after a refusal, so the
		// sentence must ask for the press, not promise an automatic read.
		{name: "budget-paused bench names the budget, not another ingest", row: row(staged(after)), bench: budgetPaused,
			want: IngestJSON{Status: ingestSkipped,
				Reason: str("Not read yet. This bench has used its budget for now. Press Ingest now once the budget window has reset.")}},
		// A refusal stages nothing, so a fresh upload has no stamp; with
		// nothing queued it would wait forever, and the budget sentence is
		// the truth about it.
		{name: "budget-paused bench, never staged: the budget sentence", row: row(), bench: budgetPaused,
			want: IngestJSON{Status: ingestSkipped,
				Reason: str("Not read yet. This bench has used its budget for now. Press Ingest now once the budget window has reset.")}},
		{name: "budget-paused bench, never staged, a run queued: waiting", row: row(), bench: pendingBudgetPaused, want: waiting},
		{name: "budget-paused bench with a queued run is waiting", row: row(staged(after)), bench: pendingBudgetPaused, want: waiting},
		{name: "budget-paused bench leaves a paused file paused", row: row(paused), bench: budgetPaused, want: pausedJSON},
		{name: "budget-paused bench leaves a paused file's stale source waiting", row: row(paused, source(before)), bench: budgetPaused, want: waiting},
		{name: "budget-paused bench does not touch a read file", row: row(source(after), staged(after)), bench: budgetPaused, want: read},
		{name: "budget-paused bench does not touch a recorded skip", row: row(skip("no_text", true), staged(after)), bench: budgetPaused, want: noText},

		// Rule 3: a source read since the bytes changed.
		{name: "read: source after the bytes changed", row: row(source(after), staged(after)), bench: idle, want: read},
		{name: "read: equal timestamps count as read",
			row: row(source(t0)), bench: idle,
			want: IngestJSON{Status: ingestRead, Pages: &pages, ReadAt: str("2026-10-01T12:00:00Z")}},
		{name: "replaced since read, not staged since: waiting", row: row(source(before), staged(before)), bench: idle, want: waiting},
		// Replaced while the build that read it ran: the stamp is for the old
		// bytes (the sink guards it by sha), the source was written after the
		// change, and it describes bytes no longer on the row.
		{name: "replaced during the build that read it: waiting", row: row(source(after), staged(before)), bench: idle, want: waiting},
		{name: "replaced during the build, a run queued: waiting", row: row(source(after), staged(before)), bench: pending, want: waiting},
		{name: "replaced since read, staged since, passed over", row: row(source(before), staged(after)), bench: idle, want: unrecorded},

		// Rule 2: a live skip, and what outranks it.
		{name: "live skip beats a source", row: row(source(after), skip("no_text", true)), bench: idle, want: noText},
		{name: "sha-stale skip is history, source wins", row: row(source(after), skip("no_text", false)), bench: idle, want: read},
		{name: "sha-stale skip, nothing read, not staged since: waiting", row: row(skip("no_text", false)), bench: idle, want: waiting},
		{name: "live flag with a null class falls through", row: row(func(r *store.FileIngestRow) { r.SkipLive = true }, staged(after)),
			bench: idle, want: unrecorded},
		{name: "unknown class reads as extractor_error, token echoed", row: row(skip("mystery", true)), bench: idle,
			want: IngestJSON{Status: ingestSkipped, Reason: str(skipSentences["extractor_error"]), Class: str("mystery")}},

		// Rule 1: paused.
		{name: "paused, never read", row: row(paused, staged(after)), bench: idle, want: pausedJSON},
		{name: "paused, never read, nothing staged", row: row(paused), bench: idle, want: pausedJSON},
		{name: "paused with a recorded skip: paused wins", row: row(paused, skip("no_text", true)), bench: idle, want: pausedJSON},
		{name: "paused with a live source stays read", row: row(paused, source(after)), bench: idle, want: read},
		// A paused file is never read, yet its stale source falls through
		// rather than reading "paused": the pages it describes are on the
		// bench and stale, and "paused" would hide that. Pinned so a change is
		// deliberate.
		{name: "paused with a stale source falls through to waiting", row: row(paused, source(before)), bench: idle, want: waiting},
		{name: "paused with a stale source, staged since, passed over", row: row(paused, source(before), staged(after)), bench: idle, want: unrecorded},
		{name: "paused with a stale source and a live skip: the skip", row: row(paused, source(before), skip("no_text", true)),
			bench: idle, want: noText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveIngest(tc.row, tc.bench); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deriveIngest = %s, want %s", describe(got), describe(tc.want))
			}
		})
	}
}

// describe renders an IngestJSON with its pointers followed, for a readable
// failure.
func describe(j IngestJSON) string {
	out := "{" + j.Status
	if j.Pages != nil {
		out += " pages=" + strconv.Itoa(*j.Pages)
	}
	if j.ReadAt != nil {
		out += " readAt=" + *j.ReadAt
	}
	if j.Reason != nil {
		out += " reason=" + *j.Reason
	}
	if j.Class != nil {
		out += " class=" + *j.Class
	}
	return out + "}"
}

// Every class the connector can produce has a sentence of its own, so a class
// added to the connector without one here fails rather than falling back to
// the extractor_error copy; the paused class is the derivation's own. Each
// sentence is pinned verbatim because the UI renders it as is and the wording
// was settled against what members reading the page need to do next.
func TestSkipSentencesCoverEveryClass(t *testing.T) {
	classes := append(slices.Clone(uploadconn.SkipClasses), skipClassPaused)
	for _, class := range classes {
		if _, ok := skipSentences[class]; !ok {
			t.Errorf("no sentence for class %q", class)
		}
	}
	for class := range skipSentences {
		if !slices.Contains(classes, class) {
			t.Errorf("sentence for %q, which no connector produces", class)
		}
	}
	want := map[string]string{
		"no_text":           "No text found, probably a scan. Upload a text version in its place, or delete this one.",
		"extractor_missing": "This file type can't be read on this server. An admin needs to enable it.",
		"unsupported_type":  "This file type is not supported.",
		"timed_out":         "Reading this file took too long and was stopped. Try a smaller file, or run another ingest.",
		"extractor_error":   "This file could not be read. Check the Log for the error.",
		"hidden_path":       "Hidden files and folders such as .git are not read. Delete this one and upload it under a plain name.",
		"paused":            "Paused: skipped on ingest",
	}
	for class, sentence := range want {
		if got := *skippedJSON(class).Reason; got != sentence {
			t.Errorf("%s: %q, want %q", class, got, sentence)
		}
	}
	if got := skipSentenceUnrecorded; got != "Not read by the last ingest. Press Ingest now to try again, or check the Log." {
		t.Errorf("unrecorded sentence = %q", got)
	}
}
