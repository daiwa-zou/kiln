package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// skipFacts is what the skip columns hold for one path, read raw so the tests
// see the row and not the store's view of it.
type skipFacts struct {
	reason    *string
	sha       *string
	skippedAt *time.Time
	updatedAt time.Time
}

func readSkip(t *testing.T, s *WikiStore, ws, path string) skipFacts {
	t.Helper()
	var f skipFacts
	if err := s.Pool().QueryRow(context.Background(), `
		SELECT skip_reason, skip_sha256, skipped_at, updated_at
		FROM workspace_files WHERE workspace_id = $1 AND path = $2`, ws, path).
		Scan(&f.reason, &f.sha, &f.skippedAt, &f.updatedAt); err != nil {
		t.Fatalf("read skip columns for %s: %v", path, err)
	}
	return f
}

func mustCreateFile(t *testing.T, s *WikiStore, ws, path, sha string) string {
	t.Helper()
	id, _, err := s.CreateFile(context.Background(), FileRow{
		WorkspaceID: ws, Path: path, BlobKey: "ws/" + ws + "/uploads/" + sha,
		SizeBytes: 10, SHA256: sha,
	})
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	return id
}

// mustImportSources records sources the way a build does; the file rows must
// exist first or the import fence drops them.
func mustImportSources(t *testing.T, s *WikiStore, ws string, recs ...diff.SourceRecord) {
	t.Helper()
	if err := s.Import(context.Background(), jobs.ImportRequest{WorkspaceID: ws, UpsertSources: recs}); err != nil {
		t.Fatalf("import sources: %v", err)
	}
}

func mustInsertRun(t *testing.T, s *WikiStore, ws, trigger, status string, startedAt any) {
	t.Helper()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO runs (workspace_id, trigger, status, started_at) VALUES ($1, $2, $3, $4)`,
		ws, trigger, status, startedAt); err != nil {
		t.Fatalf("insert %s %s run: %v", trigger, status, err)
	}
}

func ingestRow(t *testing.T, rows []FileIngestRow, path string) FileIngestRow {
	t.Helper()
	for _, r := range rows {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", path, rows)
	return FileIngestRow{}
}

func strOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestRecordFileSkipsSetsRepeatsAndGuardsSHA(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	mustCreateFile(t, s, ws, "b.pdf", "b1")
	before := readSkip(t, s, ws, "a.pdf")
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	changed, err := s.RecordFileSkips(ctx, ws, []FileSkip{{Path: "a.pdf", SHA256: "a1", Class: "no_text"}}, nil, at)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if want := []string{"a.pdf"}; !slices.Equal(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	got := readSkip(t, s, ws, "a.pdf")
	if strOf(got.reason) != "no_text" || strOf(got.sha) != "a1" || got.skippedAt == nil || !got.skippedAt.Equal(at) {
		t.Errorf("after record: reason=%s sha=%s at=%v", strOf(got.reason), strOf(got.sha), got.skippedAt)
	}
	if !got.updatedAt.Equal(before.updatedAt) {
		t.Errorf("recording a skip moved updated_at %v -> %v; the Ingest list sorts on it", before.updatedAt, got.updatedAt)
	}

	// The same skip again is a no-op: nothing reported, the stamp stays.
	changed, err = s.RecordFileSkips(ctx, ws, []FileSkip{{Path: "a.pdf", SHA256: "a1", Class: "no_text"}}, nil, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("repeat reported changed = %v, want nothing", changed)
	}
	if got := readSkip(t, s, ws, "a.pdf"); !got.skippedAt.Equal(at) {
		t.Errorf("repeat moved skipped_at to %v", got.skippedAt)
	}

	// A different class for the same bytes is a change.
	changed, err = s.RecordFileSkips(ctx, ws, []FileSkip{{Path: "a.pdf", SHA256: "a1", Class: "extractor_error"}}, nil, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("reclass: %v", err)
	}
	if want := []string{"a.pdf"}; !slices.Equal(changed, want) {
		t.Errorf("reclass changed = %v, want %v", changed, want)
	}
	if got := readSkip(t, s, ws, "a.pdf"); strOf(got.reason) != "extractor_error" || !got.skippedAt.Equal(at.Add(time.Hour)) {
		t.Errorf("after reclass: reason=%s at=%v", strOf(got.reason), got.skippedAt)
	}

	// A skip for bytes the row no longer holds (replaced mid-build) is a no-op.
	changed, err = s.RecordFileSkips(ctx, ws, []FileSkip{{Path: "b.pdf", SHA256: "stale", Class: "no_text"}}, nil, at)
	if err != nil {
		t.Fatalf("stale sha: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("stale sha reported changed = %v", changed)
	}
	if got := readSkip(t, s, ws, "b.pdf"); got.reason != nil || got.sha != nil || got.skippedAt != nil {
		t.Errorf("stale sha wrote a skip: %+v", got)
	}

	// An unknown path matches nothing and fails nothing.
	if _, err := s.RecordFileSkips(ctx, ws, []FileSkip{{Path: "gone.pdf", SHA256: "x", Class: "no_text"}}, nil, at); err != nil {
		t.Fatalf("unknown path: %v", err)
	}
	// Nothing to do is not a transaction.
	if changed, err := s.RecordFileSkips(ctx, ws, nil, nil, at); err != nil || changed != nil {
		t.Fatalf("empty call = %v, %v", changed, err)
	}
}

func TestRecordFileSkipsClearsReadPathsOnly(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	at := time.Now()
	ids := map[string]string{}
	for _, p := range []string{"a.pdf", "b.pdf", "c.pdf"} {
		ids[p] = mustCreateFile(t, s, ws, p, p+"-sha")
	}
	if _, err := s.RecordFileSkips(ctx, ws, []FileSkip{
		{Path: "a.pdf", SHA256: "a.pdf-sha", Class: "no_text"},
		{Path: "b.pdf", SHA256: "b.pdf-sha", Class: "no_text"},
		{Path: "c.pdf", SHA256: "c.pdf-sha", Class: "no_text"},
	}, nil, at); err != nil {
		t.Fatalf("seed skips: %v", err)
	}
	// b is paused: the next sync neither reads it nor clears it.
	if err := s.SetFileEnabled(ctx, ws, ids["b.pdf"], false); err != nil {
		t.Fatal(err)
	}

	// The next build read a (and d, which was never skipped): only a clears.
	changed, err := s.RecordFileSkips(ctx, ws, nil, []string{"a.pdf", "d.pdf"}, at)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("clearing reported changed = %v", changed)
	}
	if got := readSkip(t, s, ws, "a.pdf"); got.reason != nil || got.sha != nil || got.skippedAt != nil {
		t.Errorf("a.pdf was read but keeps its skip: %+v", got)
	}
	for _, p := range []string{"b.pdf", "c.pdf"} {
		if got := readSkip(t, s, ws, p); strOf(got.reason) != "no_text" {
			t.Errorf("%s lost its skip without being read: %+v", p, got)
		}
	}

	// A row with skip columns set deletes like any other and takes them along.
	if _, _, err := s.DeleteFile(ctx, ws, ids["c.pdf"]); err != nil {
		t.Fatalf("delete skipped file: %v", err)
	}
	if rows := mustListIngest(t, s, ws); len(rows) != 2 {
		t.Errorf("after delete, %d rows: %+v", len(rows), rows)
	}
}

func mustListIngest(t *testing.T, s *WikiStore, ws string) []FileIngestRow {
	t.Helper()
	rows, _, err := s.ListFilesWithIngest(context.Background(), ws)
	if err != nil {
		t.Fatalf("ListFilesWithIngest: %v", err)
	}
	return rows
}

// One class the allow-list should have caught must not fail the rows beside
// it: left() truncates to the column's cap and the batch lands.
func TestRecordFileSkipsLongClassDoesNotSinkBatch(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	mustCreateFile(t, s, ws, "ok.pdf", "ok1")
	mustCreateFile(t, s, ws, "bad.pdf", "bad1")
	mustCreateFile(t, s, ws, "huge.pdf", "huge1")

	long := strings.Repeat("ü", 41)
	huge := strings.Repeat("x", 1<<20)
	changed, err := s.RecordFileSkips(ctx, ws, []FileSkip{
		{Path: "huge.pdf", SHA256: "huge1", Class: huge},
		{Path: "bad.pdf", SHA256: "bad1", Class: long},
		{Path: "ok.pdf", SHA256: "ok1", Class: "no_text"},
	}, nil, time.Now())
	if err != nil {
		t.Fatalf("record with an over-long class: %v", err)
	}
	if want := []string{"bad.pdf", "huge.pdf", "ok.pdf"}; !slices.Equal(changed, want) {
		t.Errorf("changed = %v, want %v (sorted, every row)", changed, want)
	}
	if got := readSkip(t, s, ws, "ok.pdf"); strOf(got.reason) != "no_text" {
		t.Errorf("the good row did not land: %+v", got)
	}
	if got := readSkip(t, s, ws, "bad.pdf"); strOf(got.reason) != strings.Repeat("ü", 40) {
		t.Errorf("long class stored as %q, want 40 runes", strOf(got.reason))
	}
	if got := readSkip(t, s, ws, "huge.pdf"); len(strOf(got.reason)) != 40 {
		t.Errorf("1 MB class stored as %d bytes, want 40", len(strOf(got.reason)))
	}
	// The CHECK is still the backstop for anything that bypasses left().
	if _, err := s.Pool().Exec(ctx, `UPDATE workspace_files SET skip_reason = $2 WHERE path = 'ok.pdf' AND workspace_id = $1`,
		ws, strings.Repeat("a", 41)); err == nil {
		t.Error("a 41-character skip_reason was accepted; the CHECK is missing")
	}
}

// A batch arrives in staging order with a path repeated; the store sorts it,
// keeps the last entry for the repeat, and reports changes sorted.
func TestRecordFileSkipsSortsAndCollapsesBatch(t *testing.T) {
	s, ws := jobStore(t)
	for _, p := range []string{"a.pdf", "b.pdf", "c.pdf"} {
		mustCreateFile(t, s, ws, p, p+"-sha")
	}
	changed, err := s.RecordFileSkips(context.Background(), ws, []FileSkip{
		{Path: "c.pdf", SHA256: "c.pdf-sha", Class: "no_text"},
		{Path: "a.pdf", SHA256: "a.pdf-sha", Class: "no_text"},
		{Path: "b.pdf", SHA256: "b.pdf-sha", Class: "no_text"},
		{Path: "a.pdf", SHA256: "a.pdf-sha", Class: "extractor_error"},
	}, nil, time.Now())
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if want := []string{"a.pdf", "b.pdf", "c.pdf"}; !slices.Equal(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if got := readSkip(t, s, ws, "a.pdf"); strOf(got.reason) != "extractor_error" {
		t.Errorf("repeated path stored %q, want the last entry", strOf(got.reason))
	}
}

func TestListFilesWithIngestPagesUnionSectionsAndDedupe(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	mustCreateFile(t, s, ws, "a.pdf.bak", "a2")
	mustCreateFile(t, s, ws, "never.pdf", "n1")
	mustCreateFile(t, s, ws, "gone.pdf", "g1")
	aKey := diff.DocKey(diff.UploadOrigin("a.pdf"))
	mustImportSources(t, s, ws,
		diff.SourceRecord{Key: aKey, InputHash: "h1",
			FilesWritten: []string{"sources/a.md", "concepts/shared.md"}},
		diff.SourceRecord{Key: aKey + "#intro", InputHash: "h2",
			FilesWritten: []string{"sources/a-intro.md", "concepts/shared.md"}},
		diff.SourceRecord{Key: aKey + "#method", InputHash: "h3",
			FilesWritten: []string{"sources/a.md"}},
		// Same prefix, different document: must not be counted into a.pdf.
		diff.SourceRecord{Key: diff.DocKey(diff.UploadOrigin("a.pdf.bak")), InputHash: "h4",
			FilesWritten: []string{"sources/a-bak.md"}},
		diff.SourceRecord{Key: diff.DocKey(diff.UploadOrigin("gone.pdf")), InputHash: "h5",
			FilesWritten: []string{"sources/gone.md"}},
	)
	// A soft-deleted source is not a read.
	if _, err := s.Pool().Exec(ctx, `UPDATE sources SET deleted_at = now() WHERE workspace_id = $1 AND key = $2`,
		ws, string(diff.DocKey(diff.UploadOrigin("gone.pdf")))); err != nil {
		t.Fatal(err)
	}

	rows := mustListIngest(t, s, ws)
	if len(rows) != 4 || rows[0].Path != "a.pdf" || rows[1].Path != "a.pdf.bak" {
		t.Fatalf("rows = %+v, want 4 ordered by path", rows)
	}
	a := ingestRow(t, rows, "a.pdf")
	if !a.HasSource || a.Pages == nil || *a.Pages != 3 {
		t.Errorf("a.pdf: has_source=%v pages=%v, want true, 3 (a.md, a-intro.md, shared.md once)", a.HasSource, a.Pages)
	}
	if a.ReadAt == nil {
		t.Error("a.pdf: ReadAt nil for a read document")
	}
	if a.SkipClass != nil || a.SkipLive {
		t.Errorf("a.pdf: skip facts %v %v on a row never skipped", a.SkipClass, a.SkipLive)
	}
	if a.SHA256 != "a1" || a.BlobKey == "" || a.CreatedAt.IsZero() {
		t.Errorf("a.pdf: FileRow fields not carried: %+v", a.FileRow)
	}
	if bak := ingestRow(t, rows, "a.pdf.bak"); bak.Pages == nil || *bak.Pages != 1 {
		t.Errorf("a.pdf.bak: pages=%v, want 1", bak.Pages)
	}
	if never := ingestRow(t, rows, "never.pdf"); never.HasSource || never.Pages != nil || never.ReadAt != nil {
		t.Errorf("never.pdf: %+v, want no source facts", never)
	}
	if gone := ingestRow(t, rows, "gone.pdf"); gone.HasSource || gone.Pages != nil {
		t.Errorf("gone.pdf: soft-deleted source still reads as a source: %+v", gone)
	}

	// upsertSource now stamps fetched_at, and ReadAt is that stamp.
	var fetched *time.Time
	if err := s.Pool().QueryRow(ctx, `SELECT fetched_at FROM sources WHERE workspace_id = $1 AND key = $2`,
		ws, string(aKey)).Scan(&fetched); err != nil {
		t.Fatal(err)
	}
	if fetched == nil {
		t.Fatal("upsertSource left fetched_at NULL")
	}
	if !a.ReadAt.Equal(*fetched) {
		t.Errorf("ReadAt = %v, want fetched_at %v", a.ReadAt, fetched)
	}
	// A legacy row without fetched_at falls back to the source's updated_at.
	var srcUpdated time.Time
	if err := s.Pool().QueryRow(ctx, `UPDATE sources SET fetched_at = NULL WHERE workspace_id = $1 AND key = $2 RETURNING updated_at`,
		ws, string(aKey)).Scan(&srcUpdated); err != nil {
		t.Fatal(err)
	}
	if a := ingestRow(t, mustListIngest(t, s, ws), "a.pdf"); a.ReadAt == nil || !a.ReadAt.Equal(srcUpdated) {
		t.Errorf("legacy ReadAt = %v, want the source's updated_at %v", a.ReadAt, srcUpdated)
	}

	// Re-importing re-stamps fetched_at on the update path too.
	if _, err := s.Pool().Exec(ctx, `UPDATE sources SET fetched_at = now() - interval '1 day' WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	mustImportSources(t, s, ws, diff.SourceRecord{Key: aKey, InputHash: "h1b", FilesWritten: []string{"sources/a.md"}})
	if err := s.Pool().QueryRow(ctx, `SELECT fetched_at FROM sources WHERE workspace_id = $1 AND key = $2`, ws, string(aKey)).Scan(&fetched); err != nil {
		t.Fatal(err)
	}
	if fetched == nil || time.Since(*fetched) > time.Hour {
		t.Errorf("re-import left fetched_at at %v", fetched)
	}

	// Skip facts: live only while the sha matches.
	if _, err := s.RecordFileSkips(ctx, ws, []FileSkip{{Path: "never.pdf", SHA256: "n1", Class: "no_text"}}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if never := ingestRow(t, mustListIngest(t, s, ws), "never.pdf"); strOf(never.SkipClass) != "no_text" || !never.SkipLive {
		t.Errorf("never.pdf after skip: class=%s live=%v", strOf(never.SkipClass), never.SkipLive)
	}
	mustCreateFile(t, s, ws, "never.pdf", "n2")
	if never := ingestRow(t, mustListIngest(t, s, ws), "never.pdf"); strOf(never.SkipClass) != "no_text" || never.SkipLive {
		t.Errorf("never.pdf after replacement: class=%s live=%v, want the old class and not live", strOf(never.SkipClass), never.SkipLive)
	}
}

func TestListFilesWithIngestBenchFacts(t *testing.T) {
	s, ws := jobStore(t)
	other, err := s.EnsureWorkspace(context.Background(), "test-org", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	bench := func() FileIngestBench {
		t.Helper()
		_, b, err := s.ListFilesWithIngest(context.Background(), ws)
		if err != nil {
			t.Fatalf("ListFilesWithIngest: %v", err)
		}
		return b
	}

	if b := bench(); b.LastRun != nil || b.PendingRun {
		t.Errorf("empty bench: %+v", b)
	}

	// Runs that never read a document do not count: research, failed,
	// over-budget, never started, another bench.
	mustInsertRun(t, s, ws, "research", "succeeded", "2026-10-01T00:00:00Z")
	mustInsertRun(t, s, ws, "manual", "failed", "2026-10-02T00:00:00Z")
	mustInsertRun(t, s, ws, "manual", "over_budget", "2026-10-03T00:00:00Z")
	mustInsertRun(t, s, ws, "manual", "succeeded", nil)
	mustInsertRun(t, s, other, "manual", "succeeded", "2026-10-09T00:00:00Z")
	if b := bench(); b.LastRun != nil || b.PendingRun {
		t.Errorf("non-qualifying runs counted: %+v", b)
	}

	mustInsertRun(t, s, ws, "manual", "succeeded", "2026-10-04T00:00:00Z")
	mustInsertRun(t, s, ws, "webhook", "no_changes", "2026-10-05T00:00:00Z")
	mustInsertRun(t, s, ws, "webhook", "partial", "2026-10-06T00:00:00Z")
	want := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if b := bench(); b.LastRun == nil || !b.LastRun.Equal(want) || b.PendingRun {
		t.Errorf("last run = %+v, want %v and no pending run", b, want)
	}

	// A research run in flight is not an ingest about to read anything.
	mustInsertRun(t, s, ws, "research", "running", "2026-10-07T00:00:00Z")
	if b := bench(); b.PendingRun {
		t.Error("a running research run reads as a pending ingest")
	}
	if _, err := s.Pool().Exec(context.Background(), `DELETE FROM runs WHERE status = 'running'`); err != nil {
		t.Fatal(err)
	}
	// A queued continuation makes an unread document waiting, not passed over.
	mustInsertRun(t, s, ws, "manual", "queued", nil)
	if b := bench(); !b.PendingRun || b.LastRun == nil || !b.LastRun.Equal(want) {
		t.Errorf("queued run: %+v, want pending and the same last run", b)
	}
}

// contentChangedAt reads the raw column: nil for a legacy row.
func contentChangedAt(t *testing.T, s *WikiStore, ws, path string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT content_changed_at FROM workspace_files WHERE workspace_id = $1 AND path = $2`, ws, path).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// Pins a row's content_changed_at an hour back so a bump is unambiguous
// without sleeping between transactions.
func ageContent(t *testing.T, s *WikiStore, ws, path string) time.Time {
	t.Helper()
	var at time.Time
	if err := s.Pool().QueryRow(context.Background(), `
		UPDATE workspace_files SET content_changed_at = now() - interval '1 hour'
		WHERE workspace_id = $1 AND path = $2 RETURNING content_changed_at`, ws, path).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestCreateFileContentChangedAtMovesOnlyOnSHAChange(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	if at := contentChangedAt(t, s, ws, "a.pdf"); at == nil {
		t.Fatal("insert left content_changed_at NULL")
	}

	// Same bytes again: the row is replaced (updated_at moves) but the content
	// did not change.
	aged := ageContent(t, s, ws, "a.pdf")
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	if at := contentChangedAt(t, s, ws, "a.pdf"); at == nil || !at.Equal(aged) {
		t.Errorf("same-sha re-upload moved content_changed_at %v -> %v", aged, at)
	}
	row := ingestRow(t, mustListIngest(t, s, ws), "a.pdf")
	if !row.ContentAt.Equal(aged) {
		t.Errorf("ContentAt = %v, want content_changed_at %v", row.ContentAt, aged)
	}
	if !row.UpdatedAt.After(aged) {
		t.Errorf("re-upload did not move updated_at (%v)", row.UpdatedAt)
	}

	// New bytes: the content changed.
	mustCreateFile(t, s, ws, "a.pdf", "a2")
	if at := contentChangedAt(t, s, ws, "a.pdf"); at == nil || !at.After(aged) {
		t.Errorf("new-sha re-upload left content_changed_at at %v", at)
	}

	// A legacy row reads as changed when it was created.
	if _, err := s.Pool().Exec(ctx, `UPDATE workspace_files SET content_changed_at = NULL WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if row := ingestRow(t, mustListIngest(t, s, ws), "a.pdf"); !row.ContentAt.Equal(row.CreatedAt) {
		t.Errorf("legacy ContentAt = %v, want created_at %v", row.ContentAt, row.CreatedAt)
	}
}

func TestSetFileEnabledResumeBumpsContentOnlyWithoutSource(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	readID := mustCreateFile(t, s, ws, "read.pdf", "r1")
	neverID := mustCreateFile(t, s, ws, "never.pdf", "n1")
	droppedID := mustCreateFile(t, s, ws, "dropped.pdf", "d1")
	mustImportSources(t, s, ws,
		diff.SourceRecord{Key: diff.DocKey(diff.UploadOrigin("read.pdf")), InputHash: "h", FilesWritten: []string{"sources/read.md"}},
		diff.SourceRecord{Key: diff.DocKey(diff.UploadOrigin("dropped.pdf")), InputHash: "h", FilesWritten: []string{"sources/dropped.md"}},
	)
	if _, err := s.Pool().Exec(ctx, `UPDATE sources SET deleted_at = now() WHERE workspace_id = $1 AND key = $2`,
		ws, string(diff.DocKey(diff.UploadOrigin("dropped.pdf")))); err != nil {
		t.Fatal(err)
	}
	readAged := ageContent(t, s, ws, "read.pdf")
	neverAged := ageContent(t, s, ws, "never.pdf")
	droppedAged := ageContent(t, s, ws, "dropped.pdf")

	// Pausing never touches the content stamp.
	for _, id := range []string{readID, neverID, droppedID} {
		if err := s.SetFileEnabled(ctx, ws, id, false); err != nil {
			t.Fatal(err)
		}
	}
	if at := contentChangedAt(t, s, ws, "never.pdf"); !at.Equal(neverAged) {
		t.Errorf("pause moved content_changed_at to %v", at)
	}

	for _, id := range []string{readID, neverID, droppedID} {
		if err := s.SetFileEnabled(ctx, ws, id, true); err != nil {
			t.Fatal(err)
		}
	}
	// A read file's pages are still current: it stays read.
	if at := contentChangedAt(t, s, ws, "read.pdf"); !at.Equal(readAged) {
		t.Errorf("resuming a read file moved content_changed_at %v -> %v", readAged, at)
	}
	// A never-read file is now wanted: it becomes waiting rather than "not
	// read by the last ingest", which skipped it on purpose.
	if at := contentChangedAt(t, s, ws, "never.pdf"); !at.After(neverAged) {
		t.Errorf("resuming a never-read file left content_changed_at at %v", at)
	}
	// A soft-deleted source is no source.
	if at := contentChangedAt(t, s, ws, "dropped.pdf"); !at.After(droppedAged) {
		t.Errorf("resuming a file with a dropped source left content_changed_at at %v", at)
	}
}

func mustCreateConnector(t *testing.T, s *WikiStore, ws, kind string) string {
	t.Helper()
	id, err := s.CreateConnector(context.Background(), ConnectorRow{
		WorkspaceID: ws, Kind: kind, Name: kind, Config: map[string]any{}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create %s connector: %v", kind, err)
	}
	return id
}

// mustInsertPinnedRun is mustInsertRun for a run pinned to one connector, the
// way the poll scheduler and a connector's "sync now" enqueue them.
func mustInsertPinnedRun(t *testing.T, s *WikiStore, ws, connectorID, trigger, status string, startedAt any) {
	t.Helper()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO runs (workspace_id, connector_id, trigger, status, started_at) VALUES ($1, $2, $3, $4, $5)`,
		ws, connectorID, trigger, status, startedAt); err != nil {
		t.Fatalf("insert pinned %s %s run: %v", trigger, status, err)
	}
}

// A run pinned to the git or web connector builds from that connector alone,
// so it never staged an upload: it is not the last ingest of the documents,
// and while queued it is not the ingest about to read them.
func TestListFilesWithIngestIgnoresRunsPinnedToOtherConnectors(t *testing.T) {
	s, ws := jobStore(t)
	uploadID := mustCreateConnector(t, s, ws, "upload")
	webID := mustCreateConnector(t, s, ws, "web")
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	aged := ageContent(t, s, ws, "a.pdf")
	bench := func() FileIngestBench {
		t.Helper()
		_, b, err := s.ListFilesWithIngest(context.Background(), ws)
		if err != nil {
			t.Fatalf("ListFilesWithIngest: %v", err)
		}
		return b
	}

	// The red's reproduction: a poll of the web connector after the upload.
	mustInsertPinnedRun(t, s, ws, webID, "poll", "no_changes", aged.Add(30*time.Minute))
	if b := bench(); b.LastRun != nil {
		t.Errorf("a poll pinned to the web connector counts as the last ingest: %+v", b)
	}
	mustInsertPinnedRun(t, s, ws, webID, "poll", "queued", nil)
	if b := bench(); b.PendingRun {
		t.Error("a queued poll of the web connector reads as a pending ingest")
	}
	if _, err := s.Pool().Exec(context.Background(), `DELETE FROM runs WHERE status = 'queued'`); err != nil {
		t.Fatal(err)
	}

	// An upload connector with a path reads a worker-local folder, not the
	// workspace's files, so a poll pinned to it stages no upload either.
	folderID, err := s.CreateConnector(context.Background(), ConnectorRow{
		WorkspaceID: ws, Kind: "upload", Name: "folder", Config: map[string]any{"path": "/srv/docs"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create path-mode upload connector: %v", err)
	}
	mustInsertPinnedRun(t, s, ws, folderID, "poll", "no_changes", aged.Add(35*time.Minute))
	if b := bench(); b.LastRun != nil {
		t.Errorf("a poll pinned to a path-mode upload connector counts as the last ingest: %+v", b)
	}

	// Pinned to the upload connector, or to none: these stage uploads.
	want := aged.Add(45 * time.Minute)
	mustInsertPinnedRun(t, s, ws, uploadID, "manual", "succeeded", want)
	if b := bench(); b.LastRun == nil || !b.LastRun.Equal(want) {
		t.Errorf("a run pinned to the upload connector was not counted: %+v", b)
	}
	want = aged.Add(time.Hour)
	mustInsertRun(t, s, ws, "manual", "no_changes", want)
	if b := bench(); b.LastRun == nil || !b.LastRun.Equal(want) {
		t.Errorf("an unpinned run was not counted: %+v", b)
	}
	mustInsertPinnedRun(t, s, ws, uploadID, "manual", "queued", nil)
	if b := bench(); !b.PendingRun {
		t.Error("a queued run pinned to the upload connector is not a pending ingest")
	}
}

// runs.connector_id is ON DELETE SET NULL, so deleting the web connector
// turns its pinned poll runs into unpinned ones. They still never staged an
// upload; the scheduler pins every poll, so an unpinned poll is not counted.
// (Red QA's regression test, round 2.)
func TestListFilesWithIngestIgnoresRunsOfDeletedConnectors(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	_ = mustCreateConnector(t, s, ws, "upload")
	webID := mustCreateConnector(t, s, ws, "web")
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	aged := ageContent(t, s, ws, "a.pdf")

	mustInsertPinnedRun(t, s, ws, webID, "poll", "no_changes", aged.Add(30*time.Minute))
	_, b, err := s.ListFilesWithIngest(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if b.LastRun != nil {
		t.Fatalf("setup: pinned poll counted: %+v", b)
	}

	if _, err := s.DeleteConnector(ctx, ws, webID); err != nil {
		t.Fatalf("DeleteConnector: %v", err)
	}
	_, b, err = s.ListFilesWithIngest(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if b.LastRun != nil {
		t.Errorf("after the web connector was deleted its old poll run counts as the last ingest: %+v", b)
	}
}

// BudgetPaused is a fact about the latest run, whatever its connector: the
// budget is the bench's.
func TestListFilesWithIngestBudgetPaused(t *testing.T) {
	s, ws := jobStore(t)
	bench := func() FileIngestBench {
		t.Helper()
		_, b, err := s.ListFilesWithIngest(context.Background(), ws)
		if err != nil {
			t.Fatalf("ListFilesWithIngest: %v", err)
		}
		return b
	}
	insert := func(trigger, status string, createdAt string) {
		t.Helper()
		if _, err := s.Pool().Exec(context.Background(), `
			INSERT INTO runs (workspace_id, trigger, status, started_at, created_at) VALUES ($1, $2, $3, $4, $4)`,
			ws, trigger, status, createdAt); err != nil {
			t.Fatal(err)
		}
	}

	if bench().BudgetPaused {
		t.Error("an empty bench reads as budget-paused")
	}
	insert("manual", "succeeded", "2026-10-01T00:00:00Z")
	if bench().BudgetPaused {
		t.Error("a succeeded run reads as budget-paused")
	}
	// The continuation was refused: the deferred documents wait for the window.
	insert("continuation", "over_budget", "2026-10-02T00:00:00Z")
	b := bench()
	if !b.BudgetPaused {
		t.Error("the latest run is over_budget but the bench is not budget-paused")
	}
	if b.LastRun == nil || !b.LastRun.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("an over_budget run moved LastRun: %+v", b)
	}
	// A research run after it says nothing about the budget; a later build does.
	insert("research", "succeeded", "2026-10-03T00:00:00Z")
	if !bench().BudgetPaused {
		t.Error("a later research run cleared the pause")
	}
	insert("manual", "no_changes", "2026-10-04T00:00:00Z")
	if bench().BudgetPaused {
		t.Error("a later build left the bench budget-paused")
	}
}

// A path that was skipped and also listed as read (the store does not require
// the lists to be disjoint) keeps its skip: the clear must not undo the write
// that preceded it in the same transaction and report it changed anyway.
func TestRecordFileSkipsPathInBothListsKeepsItsSkip(t *testing.T) {
	s, ws := jobStore(t)
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	mustCreateFile(t, s, ws, "b.pdf", "b1")
	if _, err := s.RecordFileSkips(context.Background(), ws, []FileSkip{{Path: "b.pdf", SHA256: "b1", Class: "no_text"}}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	changed, err := s.RecordFileSkips(context.Background(), ws,
		[]FileSkip{{Path: "a.pdf", SHA256: "a1", Class: "no_text"}}, []string{"a.pdf", "b.pdf"}, time.Now())
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if want := []string{"a.pdf"}; !slices.Equal(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if got := readSkip(t, s, ws, "a.pdf"); strOf(got.reason) != "no_text" {
		t.Errorf("a.pdf skip after being listed as read too: %q, want no_text kept", strOf(got.reason))
	}
	// The read path that was not skipped clears as before.
	if got := readSkip(t, s, ws, "b.pdf"); got.reason != nil {
		t.Errorf("b.pdf skip not cleared: %q", strOf(got.reason))
	}
}

func TestTouchSourcesStampsLiveRowsOnly(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	other, err := s.EnsureWorkspace(ctx, "test-org", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	mustCreateFile(t, s, ws, "gone.pdf", "g1")
	mustCreateFile(t, s, other, "a.pdf", "a1")
	aKey := string(diff.DocKey(diff.UploadOrigin("a.pdf")))
	goneKey := string(diff.DocKey(diff.UploadOrigin("gone.pdf")))
	mustImportSources(t, s, ws,
		diff.SourceRecord{Key: diff.Key(aKey), InputHash: "h", FilesWritten: []string{"sources/a.md"}},
		diff.SourceRecord{Key: diff.Key(goneKey), InputHash: "h", FilesWritten: []string{"sources/gone.md"}})
	mustImportSources(t, s, other,
		diff.SourceRecord{Key: diff.Key(aKey), InputHash: "h", FilesWritten: []string{"sources/a.md"}})
	past := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.Pool().Exec(ctx, `UPDATE sources SET fetched_at = $1`, past); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE sources SET deleted_at = now() WHERE workspace_id = $1 AND key = $2`, ws, goneKey); err != nil {
		t.Fatal(err)
	}
	fetched := func(wsID, key string) time.Time {
		t.Helper()
		var at time.Time
		if err := s.Pool().QueryRow(ctx, `SELECT fetched_at FROM sources WHERE workspace_id = $1 AND key = $2`, wsID, key).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}

	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	if err := s.TouchSources(ctx, ws, []string{aKey, goneKey, "doc:upload:unknown.pdf"}, at); err != nil {
		t.Fatalf("TouchSources: %v", err)
	}
	if got := fetched(ws, aKey); !got.Equal(at) {
		t.Errorf("live source fetched_at = %v, want %v", got, at)
	}
	if got := fetched(ws, goneKey); !got.Equal(past) {
		t.Errorf("soft-deleted source was touched: %v", got)
	}
	if got := fetched(other, aKey); !got.Equal(past) {
		t.Errorf("another bench's source was touched: %v", got)
	}
	// Nothing to touch is not a statement.
	if err := s.TouchSources(ctx, ws, nil, at); err != nil {
		t.Errorf("empty TouchSources: %v", err)
	}
}

// The red's second finding: a replacement whose bytes differ but whose text
// does not never regenerates its unit, so only the gate's touch can keep it
// read. The facts here are what deriveIngest (internal/api) reads: read when
// the source was fetched at or after the bytes changed.
func TestReplacementWithUnchangedTextStaysReadOnceTouched(t *testing.T) {
	s, ws := jobStore(t)
	ctx := context.Background()
	mustCreateFile(t, s, ws, "a.pdf", "a1")
	aKey := diff.DocKey(diff.UploadOrigin("a.pdf"))
	mustImportSources(t, s, ws, diff.SourceRecord{Key: aKey, InputHash: "h", FilesWritten: []string{"sources/a.md"}})
	if _, err := s.Pool().Exec(ctx, `UPDATE sources SET fetched_at = now() - interval '1 hour' WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE workspace_files SET content_changed_at = now() - interval '2 hours' WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if row := ingestRow(t, mustListIngest(t, s, ws), "a.pdf"); row.ReadAt == nil || row.ReadAt.Before(row.ContentAt) {
		t.Fatalf("setup: the document should read as read: %+v", row)
	}

	// Re-saved: new sha, same words. The bytes changed after the read.
	mustCreateFile(t, s, ws, "a.pdf", "a2")
	row := ingestRow(t, mustListIngest(t, s, ws), "a.pdf")
	if !row.ReadAt.Before(row.ContentAt) {
		t.Fatalf("setup: a new sha should make the read stale: read %v, content %v", row.ReadAt, row.ContentAt)
	}

	// The build's hash gate drops the unit and touches it; the run ends no_changes.
	if err := s.TouchSources(ctx, ws, []string{string(aKey)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	mustInsertRun(t, s, ws, "manual", "no_changes", time.Now())
	row = ingestRow(t, mustListIngest(t, s, ws), "a.pdf")
	if !row.HasSource || row.ReadAt == nil || row.ReadAt.Before(row.ContentAt) {
		t.Errorf("after the touch: has_source=%v read %v content %v; want read at or after the change",
			row.HasSource, row.ReadAt, row.ContentAt)
	}
}
