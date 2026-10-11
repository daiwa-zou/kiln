package store

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// fakeBlobs is an object store that records what was deleted and can be told to
// refuse a key. Like every real driver, deleting an absent key succeeds.
type fakeBlobs struct {
	deleted []string
	refuse  map[string]bool
	calls   map[string]int
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{refuse: map[string]bool{}, calls: map[string]int{}}
}

func (f *fakeBlobs) Delete(_ context.Context, key string) error {
	f.calls[key]++
	if f.refuse[key] {
		return errors.New("object store unavailable")
	}
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeBlobs) sortedDeleted() []string {
	out := append([]string(nil), f.deleted...)
	sort.Strings(out)
	return out
}

// pendingKeys lists the queue, sorted.
func pendingKeys(t *testing.T, js *WikiStore) []string {
	t.Helper()
	rows, err := js.Pool().Query(context.Background(),
		`SELECT blob_key FROM pending_blob_deletes ORDER BY blob_key`)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestDeleteWorkspaceQueuesFigureBlobs: only the uploads' keys used to be
// collected, so every picture extracted from a deleted bench's documents
// leaked. Now both are returned and both are queued, in the transaction that
// removes the rows naming them.
func TestDeleteWorkspaceQueuesFigureBlobs(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	uploadFile(t, js, ws, "report.pdf")
	key := string(diff.DocKey(diff.UploadOrigin("report.pdf")))
	if _, err := js.ReplaceFigures(ctx, ws, key, []jobs.FigureRecord{
		figure(key, "p1-i0", "sha-1", "ws/"+ws+"/figures/sha-1"),
		figure(key, "p2-i0", "sha-2", "ws/"+ws+"/figures/sha-2"),
	}); err != nil {
		t.Fatal(err)
	}

	keys, err := js.DeleteWorkspace(ctx, ws)
	if err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	want := []string{
		"ws/" + ws + "/uploads/report.pdf",
		"ws/" + ws + "/figures/sha-1",
		"ws/" + ws + "/figures/sha-2",
	}
	if !sameKeys(keys, want) {
		t.Errorf("returned keys = %v, want %v", keys, want)
	}
	if got := pendingKeys(t, js); !sameKeys(got, want) {
		t.Errorf("queued = %v, want %v", got, want)
	}

	// The bench is gone, and so is every row that could have vouched for the
	// bytes -- which is exactly why the queue must not depend on them.
	blobs := newFakeBlobs()
	done, failed, err := js.SweepBlobs(ctx, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if done != 3 || failed != 0 {
		t.Errorf("sweep done=%d failed=%d, want 3/0", done, failed)
	}
	if !sameKeys(blobs.deleted, want) {
		t.Errorf("deleted %v, want %v", blobs.sortedDeleted(), want)
	}
	if left := pendingKeys(t, js); len(left) != 0 {
		t.Errorf("queue not drained: %v", left)
	}
}

// TestSweepKeepsABlobThatIsReferencedAgain: figure keys are content digests, so
// a picture dropped by one document can be claimed by another before the sweep
// runs. Deleting it then would blank a live page.
func TestSweepKeepsABlobThatIsReferencedAgain(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	uploadFile(t, js, ws, "a.pdf")
	uploadFile(t, js, ws, "b.pdf")
	uploadFile(t, js, ws, "c.pdf")
	a := string(diff.DocKey(diff.UploadOrigin("a.pdf")))
	b := string(diff.DocKey(diff.UploadOrigin("b.pdf")))
	c := string(diff.DocKey(diff.UploadOrigin("c.pdf")))

	// a records three pictures, then drops all of them: three queued deletes.
	if _, err := js.ReplaceFigures(ctx, ws, a, []jobs.FigureRecord{
		figure(a, "1", "s-dead", "blob-dead"),
		figure(a, "2", "s-again", "blob-again"),
		figure(a, "3", "s-late", "blob-late"),
	}); err != nil {
		t.Fatal(err)
	}
	orphaned, err := js.ReplaceFigures(ctx, ws, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 3 {
		t.Fatalf("orphaned %v, want all three", orphaned)
	}
	if got := pendingKeys(t, js); len(got) != 3 {
		t.Fatalf("queued %v, want all three", got)
	}

	// b records one of them again through the normal path, which withdraws
	// the queued delete.
	if _, err := js.ReplaceFigures(ctx, ws, b, []jobs.FigureRecord{
		figure(b, "1", "s-again", "blob-again"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := pendingKeys(t, js); !sameKeys(got, []string{"blob-dead", "blob-late"}) {
		t.Errorf("after re-reference the queue holds %v, want the other two", got)
	}

	// Another one is claimed by a path that does not know about the queue (a
	// row written some other way), which is what the sweep's own re-check is for.
	if _, err := js.Pool().Exec(ctx, `
		INSERT INTO figures (workspace_id, source_key, ref, blob_key, content_type,
		                     width, height, size_bytes, sha256)
		VALUES ($1, $2, 'x', 'blob-late', 'image/png', 1, 1, 1, 's-late')`, ws, c); err != nil {
		t.Fatal(err)
	}

	blobs := newFakeBlobs()
	done, failed, err := js.SweepBlobs(ctx, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if !sameKeys(blobs.deleted, []string{"blob-dead"}) {
		t.Errorf("deleted %v, want only blob-dead: a referenced blob was freed", blobs.sortedDeleted())
	}
	if done != 2 || failed != 0 {
		t.Errorf("done=%d failed=%d, want 2/0 (one deleted, one cleared)", done, failed)
	}
	// The re-referenced row is cleared, not left to be checked forever.
	if left := pendingKeys(t, js); len(left) != 0 {
		t.Errorf("queue not drained: %v", left)
	}
}

// TestSweepRetriesAFailedDelete: a refused delete leaves its row behind with the
// failure on it, does not hold up the others, and succeeds on a later sweep.
func TestSweepRetriesAFailedDelete(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	id := uploadFile(t, js, ws, "notes.md")
	if _, _, err := js.DeleteFile(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	// A second pending blob, queued directly, so there is something for the
	// failing one to (not) starve.
	tx, err := js.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueBlobDeletes(ctx, tx, []string{"blob-other"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fileBlob := "ws/" + ws + "/uploads/notes.md"
	if got := pendingKeys(t, js); !sameKeys(got, []string{fileBlob, "blob-other"}) {
		t.Fatalf("queued = %v", got)
	}

	blobs := newFakeBlobs()
	blobs.refuse[fileBlob] = true
	done, failed, err := js.SweepBlobs(ctx, blobs)
	if err != nil {
		t.Fatalf("a refused delete must not fail the sweep: %v", err)
	}
	if done != 1 || failed != 1 {
		t.Errorf("first sweep done=%d failed=%d, want 1/1", done, failed)
	}
	if got := pendingKeys(t, js); !sameKeys(got, []string{fileBlob}) {
		t.Errorf("after the failure the queue holds %v, want just the refused blob", got)
	}
	var attempts int
	var lastErr *string
	if err := js.Pool().QueryRow(ctx,
		`SELECT attempts, last_error FROM pending_blob_deletes WHERE blob_key = $1`, fileBlob).
		Scan(&attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr == nil || *lastErr == "" {
		t.Errorf("failure not recorded: attempts=%d last_error=%v", attempts, lastErr)
	}

	// The store recovers; the next sweep finishes the job.
	delete(blobs.refuse, fileBlob)
	done, failed, err = js.SweepBlobs(ctx, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if done != 1 || failed != 0 {
		t.Errorf("second sweep done=%d failed=%d, want 1/0", done, failed)
	}
	if blobs.calls[fileBlob] != 2 {
		t.Errorf("refused blob was tried %d times, want 2", blobs.calls[fileBlob])
	}
	if left := pendingKeys(t, js); len(left) != 0 {
		t.Errorf("queue not drained after recovery: %v", left)
	}
}

// hangingBlobs is an object store that never answers a delete; only the
// caller's context ends the call.
type hangingBlobs struct{ calls int }

func (h *hangingBlobs) Delete(ctx context.Context, _ string) error {
	h.calls++
	<-ctx.Done()
	return ctx.Err()
}

// TestSweepBoundsEachBlobDelete: an object store that hangs costs each key its
// own timeout, recorded as a failed attempt, rather than the whole sweep.
func TestSweepBoundsEachBlobDelete(t *testing.T) {
	js, _ := jobStore(t)
	saved := blobDeleteTimeout
	blobDeleteTimeout = 20 * time.Millisecond
	t.Cleanup(func() { blobDeleteTimeout = saved })

	// The sweep's own budget is far longer than the per-key cap: without the
	// cap the first key would hang until this ran out.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := js.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueBlobDeletes(ctx, tx, []string{"blob-a", "blob-b"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	blobs := &hangingBlobs{}
	started := time.Now()
	done, failed, err := js.SweepBlobs(ctx, blobs)
	if err != nil {
		t.Fatalf("a hung delete must not fail the sweep: %v", err)
	}
	if done != 0 || failed != 2 || blobs.calls != 2 {
		t.Errorf("done=%d failed=%d calls=%d, want 0/2/2: every key tried, each cut off", done, failed, blobs.calls)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("sweep took %s; the per-key timeout did not bound it", took)
	}
	var attempts int
	if err := js.Pool().QueryRow(ctx,
		`SELECT min(attempts) FROM pending_blob_deletes`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want each timed-out key recorded once", attempts)
	}
	if n, err := js.PendingBlobDeletes(ctx); err != nil || n != 2 {
		t.Errorf("PendingBlobDeletes = %d, %v; want both keys still queued", n, err)
	}
}

// TestBlobOrphaningPathsQueueTheirBlobs covers the remaining places a blob
// loses its last reference: a file replaced, a file deleted (with the blobs its
// cascade released), a connector deleted, and a source dropped by an approved
// deletion in an import.
func TestBlobOrphaningPathsQueueTheirBlobs(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	// Replacing a file orphans its previous blob.
	if _, _, err := js.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "r.md", BlobKey: "blob-r1", SizeBytes: 1, SHA256: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, replaced, err := js.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "r.md", BlobKey: "blob-r2", SizeBytes: 1, SHA256: "r2"}); err != nil {
		t.Fatal(err)
	} else if replaced != "blob-r1" {
		t.Fatalf("replaced = %q", replaced)
	}
	if got := pendingKeys(t, js); !sameKeys(got, []string{"blob-r1"}) {
		t.Errorf("replace queued %v, want blob-r1", got)
	}

	// Deleting a document queues its own blob and its figures' blobs.
	alphaID, _ := seedTwoDocumentWiki(t, js, ws)
	alphaKey := string(diff.DocKey(diff.UploadOrigin("alpha.md")))
	if _, err := js.ReplaceFigures(ctx, ws, alphaKey, []jobs.FigureRecord{
		figure(alphaKey, "1", "s-alpha", "blob-fig-alpha"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := js.DeleteFile(ctx, ws, alphaID); err != nil {
		t.Fatal(err)
	}
	got := pendingKeys(t, js)
	for _, want := range []string{"blob-alpha", "blob-fig-alpha"} {
		if !contains(got, want) {
			t.Errorf("file delete did not queue %s; queue = %v", want, got)
		}
	}
	// blob-corpus is still claimed by beta's source, so it was never orphaned.
	if contains(got, "blob-corpus") {
		t.Errorf("queued blob-corpus, which a live source still references: %v", got)
	}

	// Deleting a connector queues the blobs only its sources referenced.
	conn, err := js.CreateConnector(ctx, ConnectorRow{
		WorkspaceID: ws, Kind: "git", Name: "repo", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertSources: []diff.SourceRecord{{
			Key: diff.ModuleKey("m"), InputHash: "h", ConnectorID: conn,
			BlobKeys: []string{"blob-conn"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.DeleteConnector(ctx, ws, conn); err != nil {
		t.Fatal(err)
	}
	if !contains(pendingKeys(t, js), "blob-conn") {
		t.Errorf("connector delete did not queue blob-conn: %v", pendingKeys(t, js))
	}

	// An approved deletion dropped by Import queues what the cascade released
	// and the dropped source's figures.
	uploadFile(t, js, ws, "gone.pdf")
	goneKey := diff.DocKey(diff.UploadOrigin("gone.pdf"))
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertSources: []diff.SourceRecord{{
			Key: goneKey, InputHash: "g", BlobKeys: []string{"blob-gone"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReplaceFigures(ctx, ws, string(goneKey), []jobs.FigureRecord{
		figure(string(goneKey), "1", "s-gone", "blob-fig-gone"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		DropSources: []diff.Key{goneKey},
		DeleteBlobs: []string{"blob-gone"},
	}); err != nil {
		t.Fatal(err)
	}
	got = pendingKeys(t, js)
	for _, want := range []string{"blob-gone", "blob-fig-gone"} {
		if !contains(got, want) {
			t.Errorf("approved deletion did not queue %s; queue = %v", want, got)
		}
	}
	if figs, _ := js.FiguresForSources(ctx, ws, []string{string(goneKey)}); len(figs) != 0 {
		t.Errorf("dropped source kept its figures: %+v", figs)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
