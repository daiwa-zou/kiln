package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/extract"
	"github.com/daiwa-zou/kiln/internal/store"
)

// The file surface: browser uploads of source documents. Files are the one
// place a tenant hands kiln raw bytes rather than a pointer to them, so the
// path from request to blob store is deliberately short -- sanitize the name,
// refuse what extract cannot read, stream to storage, record the row. The
// worker materializes these rows at sync time; nothing here touches the
// pipeline.
//
// Upload and delete are member-level writes, not admin actions: contributing a
// document is content work, like pushing to a connected repo, and nothing
// destructive can result -- pages derived from a deleted file still leave
// through the deletion-review queue.

// FileStore is the workspace-file surface; *store.WikiStore implements it.
type FileStore interface {
	ListFiles(ctx context.Context, workspaceID string) ([]store.FileRow, error)
	CreateFile(ctx context.Context, f store.FileRow) (id, replacedBlobKey string, err error)
	DeleteFile(ctx context.Context, workspaceID, id string) (blobKey string, err error)
}

const (
	// maxUploadBytes bounds one request: the largest document extract will
	// read, plus headroom for multipart framing.
	maxUploadBytes = extract.MaxExtractBytes + 1<<20
	// uploadTimeout replaces the API's 30-second request timeout for uploads
	// only; a 32 MiB document on a slow uplink is a legitimate request.
	uploadTimeout = 5 * time.Minute
	// uploadDebounce delays an auto-enqueued build so a batch of files
	// becomes one run, mirroring the webhook push debounce.
	uploadDebounce = 2 * time.Minute
	// maxUploadPathBytes bounds the stored relative path.
	maxUploadPathBytes = 512

	errNoBlobStore = "uploads are unavailable: object storage is not configured (set storage.* and restart)"
)

// fileJSON shapes one file row for responses.
func fileJSON(f store.FileRow) map[string]any {
	return map[string]any{
		"id": f.ID, "path": f.Path, "size": f.SizeBytes, "sha256": f.SHA256,
		"contentType": f.ContentType,
		"uploaded":    f.CreatedAt.UTC().Format(time.RFC3339),
		"updated":     f.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	rows, err := s.Files.ListFiles(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, f := range rows {
		out = append(out, fileJSON(f))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFileUpload streams one multipart file into the blob store and records
// it. One file per request on purpose: it keeps the failure unit obvious (the
// UI loops and reports per file) and the handler free of partial-batch states.
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := s.guardWrite(w, r, maxUploadBytes)
	if !ok {
		return
	}
	if s.Blobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": errNoBlobStore})
		return
	}

	// The route's 5-minute chi timeout governs the handler; these extend the
	// per-connection transport deadlines to match, without loosening the
	// server-wide backstops for every other request. Best effort: a transport
	// that cannot do this (as in some tests) just keeps its defaults.
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(uploadTimeout)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected a multipart/form-data body"})
		return
	}

	// Fields arrive in order; an optional "path" field must precede "file".
	relPath := ""
	var part *multipart.Part
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadReadError(w, err)
			return
		}
		if p.FormName() == "path" {
			buf, err := io.ReadAll(io.LimitReader(p, maxUploadPathBytes+1))
			if err != nil {
				s.uploadReadError(w, err)
				return
			}
			relPath = string(buf)
			continue
		}
		if p.FormName() == "file" {
			part = p
			break
		}
	}
	if part == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `multipart body needs a "file" part`})
		return
	}
	if relPath == "" {
		relPath = path.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
	}
	relPath, err = sanitizeUploadPath(relPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if extract.DetectFormat(relPath) == extract.FormatUnknown {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unsupported file type %q; supported: %s",
				path.Ext(relPath), supportedExtensions()),
		})
		return
	}

	// Stream to the blob store under a fresh server-made key, hashing and
	// counting on the way through. The row is written after the bytes land;
	// on a row failure the orphan blob is removed, so neither side can point
	// at the other's absence.
	fileID, err := newFileID()
	if err != nil {
		s.fail(w, err)
		return
	}
	key := blob.FileKey(ws.ID, fileID)
	hasher := sha256.New()
	counter := &countingReader{r: io.TeeReader(part, hasher)}
	if err := s.Blobs.Put(r.Context(), key, counter, -1); err != nil {
		s.uploadReadError(w, err)
		return
	}

	rowID, replacedBlob, err := s.Files.CreateFile(r.Context(), store.FileRow{
		WorkspaceID: ws.ID, Path: relPath, BlobKey: key,
		SizeBytes:   counter.n,
		ContentType: part.Header.Get("Content-Type"),
		SHA256:      hex.EncodeToString(hasher.Sum(nil)),
		UploadedBy:  uid,
	})
	if err != nil {
		s.deleteBlobQuietly(r.Context(), key)
		s.fail(w, err)
		return
	}
	if replacedBlob != "" {
		s.deleteBlobQuietly(r.Context(), replacedBlob)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id": rowID, "path": relPath, "size": counter.n,
		"sha256":   hex.EncodeToString(hasher.Sum(nil)),
		"replaced": replacedBlob != "",
		"build":    s.maybeEnqueueUploadBuild(r.Context(), ws),
	})
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.guardWrite(w, r, maxResolveBytes)
	if !ok {
		return
	}
	blobKey, err := s.Files.DeleteFile(r.Context(), ws.ID, chi.URLParam(r, "id"))
	if err != nil {
		s.failOrNotFound(w, err, "file not found")
		return
	}
	if s.Blobs != nil {
		s.deleteBlobQuietly(r.Context(), blobKey)
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": chi.URLParam(r, "id"), "status": "deleted"})
}

// maybeEnqueueUploadBuild starts a debounced build when the bench opted in by
// giving its upload connector the build-on-change trigger. The default is a
// nudge, not a run: builds spend real money, and the decision to spend sits
// with the trigger mode the owner configured. Failures degrade to "pending" --
// the upload itself succeeded, and the UI's Build button still works.
func (s *Server) maybeEnqueueUploadBuild(ctx context.Context, ws store.WorkspaceRow) string {
	if s.Runs == nil || s.Admin == nil {
		return "pending"
	}
	conns, err := s.Admin.ListConnectors(ctx, ws.ID)
	if err != nil {
		return "pending"
	}
	for _, c := range conns {
		if c.Kind != "upload" || !c.Enabled || c.TriggerMode != "webhook" {
			continue
		}
		if refused, err := s.refuseOverBudget(ctx, ws); err != nil || refused != "" {
			return "pending"
		}
		if _, _, err := s.Runs.EnqueueRunOpts(ctx, ws.ID, "upload", c.ID,
			store.EnqueueOptions{NotBefore: time.Now().Add(uploadDebounce)}); err != nil {
			return "pending"
		}
		return "queued"
	}
	return "pending"
}

// uploadReadError translates a failure while draining the request body: the
// MaxBytesReader cap reads as 413, everything else as a 400 -- by the time we
// are streaming, a failure is the client's connection or framing, not ours.
func (s *Server) uploadReadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("file exceeds the %d MiB upload limit", extract.MaxExtractBytes>>20),
		})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload failed: " + err.Error()})
}

func (s *Server) deleteBlobQuietly(ctx context.Context, key string) {
	// Best effort on an uncancelable context: the row decision has been made;
	// a failed blob delete is an orphan for GC, not a request failure.
	if err := s.Blobs.Delete(context.WithoutCancel(ctx), key); err != nil && s.Log != nil {
		s.Log.Error("deleting blob failed", "key", key, "err", err)
	}
}

// sanitizeUploadPath normalizes a client-supplied relative path and refuses
// anything that could escape the staging directory the worker materializes
// into. The result is exactly what the worker joins under its temp root.
func sanitizeUploadPath(name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("file name is empty")
	}
	if len(name) > maxUploadPathBytes {
		return "", fmt.Errorf("file path is longer than %d bytes", maxUploadPathBytes)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("file path contains control characters")
		}
	}
	if strings.HasPrefix(name, "/") {
		return "", errors.New("file path must be relative")
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("file path escapes the upload root")
	}
	return cleaned, nil
}

// supportedExtensions lists what DetectFormat recognizes, for error messages.
func supportedExtensions() string {
	exts := []string{
		".md", ".mdx", ".markdown", ".txt", ".text", ".rst", ".org", ".pdf",
		".docx", ".doc", ".pptx", ".ppt", ".xlsx", ".xls", ".odt", ".odp",
		".ods", ".epub", ".rtf", ".html", ".htm",
	}
	sort.Strings(exts)
	return strings.Join(exts, " ")
}

// newFileID mints a random UUIDv4. Generated server-side rather than by the
// database so the blob key exists before the row insert it appears in.
func newFileID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("api: mint file id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
