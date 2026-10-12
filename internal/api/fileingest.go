package api

import (
	"time"

	"github.com/daiwa-zou/kiln/internal/store"
)

// The per-document ingest status: what a member reads on Ingest to learn
// whether the wiki has read an upload, is waiting to, or passed it over and
// why. The store hands over facts (a live skip, a source row, the last run);
// this file turns them into one of three words plus a reason sentence. Kept a
// pure function so every branch is table-tested without a database.

// Ingest status words. The UI keys its chips on these; the set is closed.
const (
	ingestRead    = "read"
	ingestWaiting = "waiting"
	ingestSkipped = "skipped"
)

// skipClassPaused is the class the derivation itself assigns to a paused file
// that nothing has read; the sink never writes it.
const skipClassPaused = "paused"

// skipSentences maps a persisted skip class to the sentence a member sees.
// Only the token is stored (never extractor text), so the copy is fixed here
// and can carry an action. An unknown token reads as extractor_error.
var skipSentences = map[string]string{
	"no_text":           "No text found, probably a scan. Upload a text version.",
	"extractor_missing": "This file type can't be read on this server. An admin needs to enable it.",
	"unsupported_type":  "This file type is not supported.",
	"timed_out":         "Reading this file took too long and was stopped. Try a smaller file, or run another ingest.",
	"extractor_error":   "This file could not be read. Check the Log for the error.",
	skipClassPaused:     "Paused: skipped on ingest",
}

// skipSentenceUnrecorded is the reason when the last ingest finished without
// reading the file and no class was recorded: a build by an older binary, or a
// unit that failed. Never "re-upload": identical bytes change nothing.
const skipSentenceUnrecorded = "Not read by the last ingest. Press Ingest now to try again, or check the Log."

// skipSentenceBudgetPaused replaces skipSentenceUnrecorded while the bench's
// latest run was refused over budget: the continuation that would have read
// the file was refused, and so would an "Ingest now" pressed before the window
// resets. It asks for the press rather than promising a read, because a
// refusal enqueues nothing (worker.refuseOverBudget): only a poll connector
// comes back on its own, and a manual or webhook bench waits for a person.
const skipSentenceBudgetPaused = "Not read yet. This bench has used its budget for now. Press Ingest now once the budget window has reset."

// ingestJSON is the ingest member of a file row. Pointers are deliberate: a
// waiting file has no pages, readAt or reason, and the UI reads an explicit
// null rather than a missing key.
type ingestJSON struct {
	Status string  `json:"status"`
	Pages  *int    `json:"pages"`
	ReadAt *string `json:"readAt"`
	Reason *string `json:"reason"`
	// Class is the skip class token behind Reason (null when the status is
	// not skipped, or when no class was recorded), so an operator can search
	// the log by path and class.
	Class *string `json:"class"`
}

// deriveIngest is the status derivation, first match wins:
//
//  1. paused and nothing live was read -> skipped, "paused";
//  2. a skip recorded against the current bytes -> skipped, its class;
//  3. a source row read at or after the bytes changed -> read;
//  4. the last ingest began after the bytes changed and no ingest is queued
//     or running -> skipped, class null (the cause was not recorded; when
//     the bench's latest run was refused over budget, the sentence says so
//     instead of asking for another ingest that would be refused too);
//  5. otherwise waiting: no qualifying run yet, uploaded since the last one,
//     or a continuation is still pending.
//
// A skip outranks a source row because a replacement that is now image-only
// still has the old source until the cascade removes it. Rule 1 asks for no
// source row at all (HasSource is false for a soft-deleted one), so a paused
// file whose source is current stays read, and a paused file whose source is
// stale (replaced after its read) falls through to rules 4 and 5 although
// paused files are not read. That is accepted for this run and pinned by the
// tests so a later change is deliberate.
func deriveIngest(row store.FileIngestRow, bench store.FileIngestBench) ingestJSON {
	switch {
	case !row.Enabled && !row.HasSource:
		return skippedJSON(skipClassPaused)
	case row.SkipLive && row.SkipClass != nil:
		return skippedJSON(*row.SkipClass)
	case row.HasSource && row.ReadAt != nil && !row.ReadAt.Before(row.ContentAt):
		readAt := row.ReadAt.UTC().Format(time.RFC3339)
		return ingestJSON{Status: ingestRead, Pages: row.Pages, ReadAt: &readAt}
	case bench.LastRun != nil && !bench.LastRun.Before(row.ContentAt) && !bench.PendingRun:
		reason := skipSentenceUnrecorded
		if bench.BudgetPaused {
			reason = skipSentenceBudgetPaused
		}
		return ingestJSON{Status: ingestSkipped, Reason: &reason}
	default:
		return ingestJSON{Status: ingestWaiting}
	}
}

// skippedJSON is the skipped status for a class token. The token is echoed
// as recorded (it is an allow-listed word, capped by the store) even when the
// sentence falls back, so the log can still be searched by it.
func skippedJSON(class string) ingestJSON {
	reason, ok := skipSentences[class]
	if !ok {
		reason = skipSentences["extractor_error"]
	}
	return ingestJSON{Status: ingestSkipped, Reason: &reason, Class: &class}
}
