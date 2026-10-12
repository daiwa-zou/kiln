package api

import (
	"testing"
	"time"

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
	row := func(mod func(*store.FileIngestRow)) store.FileIngestRow {
		r := store.FileIngestRow{ContentAt: t0}
		r.Enabled = true
		if mod != nil {
			mod(&r)
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
	both := func(mods ...func(*store.FileIngestRow)) func(*store.FileIngestRow) {
		return func(r *store.FileIngestRow) {
			for _, m := range mods {
				m(r)
			}
		}
	}
	paused := func(r *store.FileIngestRow) { r.Enabled = false }

	noRuns := store.FileIngestBench{}
	ranAfter := store.FileIngestBench{LastRun: tm(after)}
	ranBefore := store.FileIngestBench{LastRun: tm(before)}
	ranAfterPending := store.FileIngestBench{LastRun: tm(after), PendingRun: true}
	ranAfterPaused := store.FileIngestBench{LastRun: tm(after), BudgetPaused: true}
	ranBeforePaused := store.FileIngestBench{LastRun: tm(before), BudgetPaused: true}
	ranAfterPendingPaused := store.FileIngestBench{LastRun: tm(after), PendingRun: true, BudgetPaused: true}

	cases := []struct {
		name   string
		row    store.FileIngestRow
		bench  store.FileIngestBench
		status string
		pages  *int
		readAt *string
		reason *string
		class  *string
	}{
		{"no runs yet", row(nil), noRuns, ingestWaiting, nil, nil, nil, nil},
		{"uploaded after the last build", row(nil), ranBefore, ingestWaiting, nil, nil, nil, nil},
		{"uploaded before the last build, no source, no class", row(nil), ranAfter, ingestSkipped,
			nil, nil, str(skipSentenceUnrecorded), nil},
		{"pending continuation keeps an unread file waiting", row(nil), ranAfterPending, ingestWaiting, nil, nil, nil, nil},
		// A continuation refused over budget: the file was passed over, and
		// "press Ingest now" would be refused too, so the sentence says why.
		// The wording is pinned: nothing re-enqueues after a refusal, so the
		// sentence must ask for the press, not promise an automatic read.
		{"budget-paused bench names the budget, not another ingest", row(nil), ranAfterPaused, ingestSkipped,
			nil, nil, str("Not read yet. This bench has used its budget for now. Press Ingest now once the budget window has reset."), nil},
		{"budget-paused bench, uploaded after the last build, still waiting", row(nil), ranBeforePaused,
			ingestWaiting, nil, nil, nil, nil},
		{"budget-paused bench with a queued run is waiting", row(nil), ranAfterPendingPaused, ingestWaiting, nil, nil, nil, nil},
		{"budget-paused bench does not touch a read file", row(source(after)), ranAfterPaused, ingestRead,
			&pages, str("2026-10-01T13:00:00Z"), nil, nil},
		{"budget-paused bench does not touch a recorded skip", row(skip("no_text", true)), ranAfterPaused, ingestSkipped,
			nil, nil, str(skipSentences["no_text"]), str("no_text")},
		{"read: source after the bytes changed", row(source(after)), ranAfter, ingestRead,
			&pages, str("2026-10-01T13:00:00Z"), nil, nil},
		{"read: equal timestamps count as read", row(source(t0)), noRuns, ingestRead,
			&pages, str("2026-10-01T12:00:00Z"), nil, nil},
		{"replaced since read: stale source is waiting", row(source(before)), ranBefore, ingestWaiting, nil, nil, nil, nil},
		{"replaced since read, last build passed it over", row(source(before)), ranAfter, ingestSkipped,
			nil, nil, str(skipSentenceUnrecorded), nil},
		{"live skip beats a source", row(both(source(after), skip("no_text", true))), ranAfter, ingestSkipped,
			nil, nil, str(skipSentences["no_text"]), str("no_text")},
		{"sha-stale skip is history, source wins", row(both(source(after), skip("no_text", false))), ranAfter, ingestRead,
			&pages, str("2026-10-01T13:00:00Z"), nil, nil},
		{"sha-stale skip, nothing read, waiting", row(skip("no_text", false)), ranBefore, ingestWaiting, nil, nil, nil, nil},
		{"legacy live flag with a null class falls through", row(func(r *store.FileIngestRow) { r.SkipLive = true }),
			ranAfter, ingestSkipped, nil, nil, str(skipSentenceUnrecorded), nil},
		{"unknown class reads as extractor_error, token echoed", row(skip("mystery", true)), noRuns, ingestSkipped,
			nil, nil, str(skipSentences["extractor_error"]), str("mystery")},
		{"paused, never read", row(paused), ranAfter, ingestSkipped,
			nil, nil, str(skipSentences[skipClassPaused]), str(skipClassPaused)},
		{"paused, never read, no runs", row(paused), noRuns, ingestSkipped,
			nil, nil, str(skipSentences[skipClassPaused]), str(skipClassPaused)},
		{"paused with a recorded skip: paused wins", row(both(paused, skip("no_text", true))), ranAfter, ingestSkipped,
			nil, nil, str(skipSentences[skipClassPaused]), str(skipClassPaused)},
		{"paused with a live source stays read", row(both(paused, source(after))), ranAfter, ingestRead,
			&pages, str("2026-10-01T13:00:00Z"), nil, nil},
		// Accepted for this run: a paused file is not read, yet its stale
		// source falls through to waiting. Pinned so a change is deliberate.
		{"paused with a stale source falls through to waiting", row(both(paused, source(before))), ranBefore,
			ingestWaiting, nil, nil, nil, nil},
		{"paused with a stale source, last build passed it over", row(both(paused, source(before))), ranAfter,
			ingestSkipped, nil, nil, str(skipSentenceUnrecorded), nil},
		{"paused with a stale source and a live skip: the skip", row(both(paused, source(before), skip("no_text", true))),
			ranAfter, ingestSkipped, nil, nil, str(skipSentences["no_text"]), str("no_text")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveIngest(tc.row, tc.bench)
			if got.Status != tc.status {
				t.Errorf("status = %q, want %q", got.Status, tc.status)
			}
			if !equalInt(got.Pages, tc.pages) {
				t.Errorf("pages = %v, want %v", deref(got.Pages), deref(tc.pages))
			}
			if !equalStr(got.ReadAt, tc.readAt) {
				t.Errorf("readAt = %v, want %v", deref(got.ReadAt), deref(tc.readAt))
			}
			if !equalStr(got.Reason, tc.reason) {
				t.Errorf("reason = %v, want %v", deref(got.Reason), deref(tc.reason))
			}
			if !equalStr(got.Class, tc.class) {
				t.Errorf("class = %v, want %v", deref(got.Class), deref(tc.class))
			}
		})
	}
}

// Every class the sink may write has its own sentence, and each sentence
// names what to do next or where to look; the copy was settled with the UI
// lens and the UI renders it verbatim.
func TestSkipSentencesCoverEveryClass(t *testing.T) {
	for _, class := range []string{"no_text", "extractor_missing", "unsupported_type", "timed_out", "extractor_error", "paused"} {
		if _, ok := skipSentences[class]; !ok {
			t.Errorf("no sentence for class %q", class)
		}
	}
	want := map[string]string{
		"no_text":           "No text found, probably a scan. Upload a text version.",
		"extractor_missing": "This file type can't be read on this server. An admin needs to enable it.",
		"unsupported_type":  "This file type is not supported.",
		"timed_out":         "Reading this file took too long and was stopped. Try a smaller file, or run another ingest.",
		"extractor_error":   "This file could not be read. Check the Log for the error.",
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

func equalInt(a, b *int) bool    { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func equalStr(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
