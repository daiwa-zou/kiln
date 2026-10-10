package jobs

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/extract"
)

// Figures reach a page in three steps, and the ordering is the whole design.
//
// They are stored *before* generation, not after. What a page may show is
// exactly what has been stored, so the set has to exist and have ids before the
// model is told what it may cite -- and a figure the model referenced but that
// was never stored would be a broken image on a live page. Storing first makes
// that unrepresentable.
//
// Then generation is told which figures a unit owns, with their captions and
// sizes. The model chooses; nothing is auto-inserted. "Include them if they are
// important" is a judgment about the document, and the model reading the
// document is the only thing here in a position to make it.
//
// Finally validation rejects references to figures the unit does not own, the
// same way it rejects wikilinks to pages that do not exist.

// storeFigures uploads this run's recovered figures and records them, returning
// nothing the caller needs: a figure that cannot be stored is a degradation of
// one page, never a reason to fail a build that otherwise read its sources
// fine.
//
// Blob first, row second. A row pointing at bytes that were never written would
// render as a broken image; an orphaned blob is garbage a sweep collects.
//
// Work that is already done is not repeated. This runs on every build, and on
// an established bench almost every build is a poll that changes nothing: with
// hundreds of documents carrying megabytes of pictures each, re-uploading and
// re-recording them all would make an idle poll the most expensive thing the
// service does. One query loads what is already recorded; a figure whose blob
// has a row is not Put again (keys are content digests, so a recorded blob is
// by construction the same bytes), and a document whose recorded set already
// matches is not handed to ReplaceFigures at all.
func (p *Pipeline) storeFigures(ctx context.Context, req BuildRequest, log *slog.Logger) {
	if len(req.Figures) == 0 || p.Blobs == nil {
		return
	}
	keys := sortedFigureKeys(req.Figures)

	// A failed lookup degrades to the old behaviour -- store everything --
	// rather than skipping the figures: redundant work is safe, missing
	// pictures are not.
	recordedBlobs := map[string]bool{}
	recordedBySource := map[string]map[string]FigureRecord{}
	if existing, err := p.Store.FiguresForSources(ctx, req.WorkspaceID, keys); err != nil {
		log.Warn("could not load recorded figures; storing all of them", "err", err)
	} else {
		for _, f := range existing {
			recordedBlobs[f.BlobKey] = true
			if recordedBySource[f.SourceKey] == nil {
				recordedBySource[f.SourceKey] = map[string]FigureRecord{}
			}
			recordedBySource[f.SourceKey][f.SHA256] = f
		}
	}

	for _, key := range keys {
		figs := req.Figures[key]
		records := make([]FigureRecord, 0, len(figs))

		for i, f := range figs {
			blobKey := blob.FigureKey(req.WorkspaceID, f.SHA256)
			if !recordedBlobs[blobKey] {
				if err := p.Blobs.Put(ctx, blobKey, bytes.NewReader(f.Data), int64(len(f.Data))); err != nil {
					log.Warn("figure could not be stored; the page will not show it",
						"unit", key, "figure", f.Ref, "err", err)
					continue
				}
				// Two documents in one run can share a picture.
				recordedBlobs[blobKey] = true
			}
			records = append(records, FigureRecord{
				SourceKey: key, Ref: f.Ref, BlobKey: blobKey,
				ContentType: f.ContentType, Width: f.Width, Height: f.Height,
				SizeBytes: int64(len(f.Data)), Page: f.Page,
				Caption: f.Caption, Ordinal: i, SHA256: f.SHA256,
			})
		}

		if sameFigureSet(recordedBySource[key], records) {
			continue
		}

		orphaned, err := p.Store.ReplaceFigures(ctx, req.WorkspaceID, key, records)
		if err != nil {
			log.Warn("figures could not be recorded", "unit", key, "err", err)
			continue
		}
		// Bytes no surviving figure references. Best effort, after the rows
		// that referenced them are gone: the reverse order would leave a row
		// pointing at nothing.
		for _, gone := range orphaned {
			if err := p.Blobs.Delete(ctx, gone); err != nil {
				log.Warn("superseded figure blob left for GC", "key", gone, "err", err)
			}
		}
	}
}

// sameFigureSet reports whether recording want would change nothing: the same
// pictures, with the same per-document details ReplaceFigures refreshes.
//
// Identity is the digest, as in the store. An empty recorded set matches only
// an empty want, so a document's first figures are always recorded.
func sameFigureSet(recorded map[string]FigureRecord, want []FigureRecord) bool {
	// The same picture can appear twice in one document; the store keeps one
	// row per digest, the last occurrence winning, so compare the same way.
	distinct := make(map[string]FigureRecord, len(want))
	for _, w := range want {
		distinct[w.SHA256] = w
	}
	if len(recorded) != len(distinct) {
		return false
	}
	for _, w := range distinct {
		r, ok := recorded[w.SHA256]
		if !ok || r.Ref != w.Ref || r.Page != w.Page ||
			r.Caption != w.Caption || r.Ordinal != w.Ordinal ||
			r.BlobKey != w.BlobKey {
			return false
		}
	}
	return true
}

// BlobWriter is the half of object storage the pipeline uses: it stores
// figures and frees blobs a cascade released, and never reads. blob.Store
// satisfies it.
type BlobWriter interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Delete(ctx context.Context, key string) error
}

func sortedFigureKeys(m map[string][]extract.Figure) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// figuresByUnit loads, for each unit about to be generated, the figures it may
// put on a page. One query for the run rather than one per unit: units
// generate concurrently, and this is read-only reference data.
func (p *Pipeline) figuresByUnit(ctx context.Context, workspaceID string, keys []diff.Key) (map[diff.Key][]FigureRecord, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	all, err := p.Store.FiguresForSources(ctx, workspaceID, figureSourceKeys(keys))
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	out := make(map[diff.Key][]FigureRecord, len(keys))
	for _, k := range keys {
		if figs := figuresForUnit(all, k); len(figs) > 0 {
			out[k] = figs
		}
	}
	return out, nil
}

// figuresForUnit returns the figures a unit may cite: its own, plus its parent
// document's when the unit is a section.
//
// A section is a span of one document, and the document's figures sit in those
// spans -- restricting a chapter to figures nobody recorded against it would
// mean a split document could never show a picture at all.
func figuresForUnit(all []FigureRecord, key diff.Key) []FigureRecord {
	parent := string(key)
	if i := strings.Index(parent, "#"); i >= 0 {
		parent = parent[:i]
	}
	var out []FigureRecord
	for _, f := range all {
		if f.SourceKey == string(key) || f.SourceKey == parent {
			out = append(out, f)
		}
	}
	return out
}

// allowedFigures is the id set validation checks a page's references against.
//
// Always non-nil when the caller tracks figures at all, including for a unit
// with none: nil would switch the check off, and a unit with no figures citing
// one is exactly the case worth catching.
func allowedFigures(figs []FigureRecord) map[string]bool {
	out := make(map[string]bool, len(figs))
	for _, f := range figs {
		out[f.ID] = true
	}
	return out
}

// figureSourceKeys is the set of source keys whose figures a set of units might
// cite, including each section's parent document.
func figureSourceKeys(keys []diff.Key) []string {
	seen := map[string]bool{}
	var out []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, k := range keys {
		add(string(k))
		if i := strings.Index(string(k), "#"); i >= 0 {
			add(string(k)[:i])
		}
	}
	sort.Strings(out)
	return out
}
