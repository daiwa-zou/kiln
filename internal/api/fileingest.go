package api

import (
	"time"

	uploadconn "github.com/daiwa-zou/kiln/internal/connector/upload"
	"github.com/daiwa-zou/kiln/internal/store"
)

// The per-document ingest status: what a member reads on Ingest to learn
// whether the wiki has read an upload, is waiting to, or passed it over and
// why. The store hands over facts (a live skip, a source row, when a build
// last staged the bytes); this file turns them into one of three words plus a
// reason sentence. Kept a pure function so every branch is table-tested
// without a database.

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
// and can carry an action. The keys are uploadconn.SkipClasses plus the paused
// class, which fileingest_test.go pins; an unknown token reads as
// extractor_error.
var skipSentences = map[string]string{
	uploadconn.SkipNoText:           "No text found, probably a scan. Upload a text version in its place, or delete this one.",
	uploadconn.SkipExtractorMissing: "This file type can't be read on this server. An admin needs to enable it.",
	uploadconn.SkipUnsupportedType:  "This file type is not supported.",
	uploadconn.SkipTimedOut:         "Reading this file took too long and was stopped. Try a smaller file, or run another ingest.",
	uploadconn.SkipExtractorError:   "This file could not be read. Check the Log for the error.",
	uploadconn.SkipHiddenPath:       "Hidden files and folders such as .git are not read. Rename or move it.",
	skipClassPaused:                 "Paused: skipped on ingest",
}

// skipSentenceUnrecorded is the reason when a build staged the file and
// finished without reading it and no class was recorded: a unit that failed,
// or a build that was interrupted after the sync. Never "re-upload": identical
// bytes change nothing.
const skipSentenceUnrecorded = "Not read by the last ingest. Press Ingest now to try again, or check the Log."

// skipSentenceBudgetPaused replaces skipSentenceUnrecorded while the bench's
// latest run was refused over budget: the continuation that would have read
// the file was refused, and so would an "Ingest now" pressed before the window
// resets. It asks for the press rather than promising a read, because a
// refusal enqueues nothing (worker.refuseOverBudget): only a poll connector
// comes back on its own, and a manual or webhook bench waits for a person.
const skipSentenceBudgetPaused = "Not read yet. This bench has used its budget for now. Press Ingest now once the budget window has reset."

// IngestJSON is the ingest member of a file row. Pointers are deliberate: a
// waiting file has no pages, readAt or reason, and the UI reads an explicit
// null rather than a missing key.
type IngestJSON struct {
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
//  4. a build staged the current bytes and no build is queued or running ->
//     skipped, class null (the build saw the file and neither read it nor
//     said why; when the bench's latest run was refused over budget, the
//     sentence says so instead of asking for another ingest that would be
//     refused too);
//  5. otherwise waiting: no build has staged these bytes yet, or one is about
//     to.
//
// A skip outranks a source row because a replacement that is now image-only
// still has the old source until the cascade removes it. Rule 1 asks for no
// source row at all (HasSource is false for a soft-deleted one), so a paused
// file whose source is current stays read, and a paused file whose source is
// stale (replaced after its read) falls through to rules 4 and 5 although
// paused files are never read. That fall-through is kept: the row still
// describes pages the wiki has, and "paused" would hide that they are stale.
// The tests pin it so a change is deliberate.
func deriveIngest(row store.FileIngestRow, bench store.FileIngestBench) IngestJSON {
	switch {
	case !row.Enabled && !row.HasSource:
		return skippedJSON(skipClassPaused)
	case row.SkipLive && row.SkipClass != nil:
		return skippedJSON(*row.SkipClass)
	case row.HasSource && row.ReadAt != nil && !row.ReadAt.Before(row.ContentAt):
		readAt := row.ReadAt.UTC().Format(time.RFC3339)
		return IngestJSON{Status: ingestRead, Pages: row.Pages, ReadAt: &readAt}
	case row.StagedAt != nil && !row.StagedAt.Before(row.ContentAt) && !bench.PendingRun:
		reason := skipSentenceUnrecorded
		if bench.BudgetPaused {
			reason = skipSentenceBudgetPaused
		}
		return IngestJSON{Status: ingestSkipped, Reason: &reason}
	default:
		return IngestJSON{Status: ingestWaiting}
	}
}

// skippedJSON is the skipped status for a class token. The token is echoed as
// recorded even when the sentence falls back, so the log can still be searched
// by it. It is safe to echo because the set is closed by construction
// (upload.skipClass and Sync are its only producers) and the store caps its
// length; nothing here validates it.
func skippedJSON(class string) IngestJSON {
	reason, ok := skipSentences[class]
	if !ok {
		reason = skipSentences[uploadconn.SkipExtractorError]
	}
	return IngestJSON{Status: ingestSkipped, Reason: &reason, Class: &class}
}
