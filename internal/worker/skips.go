package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
)

// skipSinkTimeout bounds the sink's one write. The sink runs between the sync
// and the build on a context that is detached from the run's: a slow database
// must not hold the build, and a build that was just interrupted must still
// get its skips recorded.
const skipSinkTimeout = 5 * time.Second

// skipLogLines caps how many documents one run names in the log before it
// summarises the rest. A bench of two hundred scans is one incident, not two
// hundred.
const skipLogLines = 20

// maxSkipRawRunes caps the extractor text one log line carries.
const maxSkipRawRunes = 300

// maxSkipSeen bounds the worker's memory of unrecorded skips; past it the set
// starts over and the next rebuild logs each once more. Benches with folders
// of thousands of scans are not what the set is for.
const maxSkipSeen = 4096

// skipSeenKey identifies one unrecorded skip: a document (path or host) of
// one connector kind on one bench, skipped for one reason.
type skipSeenKey struct {
	workspace, kind, subject, class string
}

// skipSeenSet is the worker's memory of unrecorded skips (Worker.skipSeen).
// Nothing removes a key: a skip that clears (the scan is replaced by a text
// version) and later recurs with the same class stays seen, so it is logged
// at Debug and not counted again, for the life of the process or until the
// maxSkipSeen reset. A restart forgets everything and logs each once more.
type skipSeenSet struct {
	mu   sync.Mutex
	seen map[skipSeenKey]struct{}
}

// markSkipSeen records a skip the store has no row for and reports whether
// this process had seen it before. The store answers that question for
// uploads in files mode (RecordFileSkips returns the changed paths); this is
// the same answer, held in memory, for the documents it cannot.
func (w *Worker) markSkipSeen(k skipSeenKey) (news bool) {
	if w.skipSeen == nil {
		w.skipSeen = &skipSeenSet{}
	}
	s := w.skipSeen
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) >= maxSkipSeen {
		s.seen = nil
	}
	if s.seen == nil {
		s.seen = map[skipSeenKey]struct{}{}
	}
	if _, seen := s.seen[k]; seen {
		return false
	}
	s.seen[k] = struct{}{}
	return true
}

// docSkipSink returns the OnDocSkips callback for one build: it records what
// the sync staged and could not read, logs the skips, and counts them.
//
// staged is the sha256 of every uploaded file the worker materialized, by
// stored path (nil for a path-mode folder, which has no rows). The store gets
// the stamp for each staged row and a class token per skipped path, never the
// extractor's text; both are tied to the sha256 of the bytes that were
// staged, so a file replaced while the build ran is left alone. Web skips are
// logged only: a fetched page has no row to record on. No review item is
// filed: the unreadable-blob precedent (reportUnreadableDocuments) is a
// storage incident, and a scan is a fact about the document that the Ingest
// page now shows.
//
// Nothing here can fail the run. A store error is logged and the build
// proceeds: the pages are still worth writing, and a document the build read
// still gets its source row from the import.
func (w *Worker) docSkipSink(run *store.QueuedRun, connectors jobs.SourceConnectors, jail string, staged map[string]string, log *slog.Logger) func(context.Context, []jobs.DocSkip) {
	return func(ctx context.Context, skips []jobs.DocSkip) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), skipSinkTimeout)
		defer cancel()
		// Persist first, so the log can say which skips are news.
		news := w.persistSkips(ctx, run, staged, skips, log)
		w.logSkips(ctx, connectors, jail, skips, news, log)
	}
}

// persistSkips writes the sync's outcome to the file rows and decides which
// skips are news, counting those on the metric. A skip the store has a row
// for is news when the store says the path's skip changed (or when the store
// could not say: louder is the safe side of a failed write); one with no row
// is news the first time this process sees it (markSkipSeen). news[i] answers
// for skips[i].
func (w *Worker) persistSkips(ctx context.Context, run *store.QueuedRun, staged map[string]string, skips []jobs.DocSkip, log *slog.Logger) (news []bool) {
	var fileSkips []store.FileSkip
	for _, s := range skips {
		// An upload connector reading a worker-local folder has no file rows;
		// its skips are logged like everyone else's and recorded nowhere.
		if sha, ok := staged[s.Subject]; ok && s.Kind == connector.KindUpload {
			fileSkips = append(fileSkips, store.FileSkip{Path: s.Subject, SHA256: sha, Class: s.Class})
		}
	}

	recorded := false
	changed := map[string]bool{}
	if len(fileSkips) > 0 || len(staged) > 0 {
		paths, err := w.Store.RecordFileSkips(ctx, run.WorkspaceID, fileSkips, staged, time.Now())
		if err != nil {
			log.Error("recording document skips failed; the Ingest page will not show why they were not read",
				"error", err, "count", len(fileSkips))
		} else {
			recorded = true
			for _, p := range paths {
				changed[p] = true
			}
		}
	}

	news = make([]bool, len(skips))
	for i, s := range skips {
		if _, hasRow := staged[s.Subject]; hasRow && s.Kind == connector.KindUpload {
			news[i] = !recorded || changed[s.Subject]
		} else {
			news[i] = w.markSkipSeen(skipSeenKey{run.WorkspaceID, s.Kind, s.Subject, s.Class})
		}
		if news[i] {
			w.metrics().DocumentSkipped(s.Kind, s.Class)
		}
	}
	return news
}

// logSkips writes the operator's view of the skips: a new one at Warn, a
// repeat (which every rebuild of a bench with a scan produces) at Debug. News
// goes first, so the cap on lines never hides a new skip behind old ones.
//
// The extractor's own text reaches the log only through sanitizeSkipRaw,
// which is the one place it is made fit for a line: the connector and the
// pipeline hand it on as written, so there is one owner of what a log may
// carry about a user's file.
func (w *Worker) logSkips(ctx context.Context, connectors jobs.SourceConnectors, jail string, skips []jobs.DocSkip, news []bool, log *slog.Logger) {
	// Only a line that reaches the log spends the cap: a Debug repeat at the
	// default level costs nothing, so twenty stable scans never push a new
	// one into the summary. The summary names what it folded by class.
	lines := 0
	moreByClass := map[string]int{}
	emit := func(level slog.Level, class, msg string, attrs ...any) {
		if !log.Enabled(ctx, level) {
			return
		}
		if lines >= skipLogLines {
			moreByClass[class]++
			return
		}
		lines++
		log.Log(ctx, level, msg, attrs...)
	}
	for _, wantNews := range []bool{true, false} {
		for i, s := range skips {
			if news[i] != wantNews {
				continue
			}
			level := slog.LevelDebug
			if wantNews {
				level = slog.LevelWarn
			}
			raw := sanitizeSkipRaw(s.Raw, jail)
			if s.Kind == connector.KindWeb {
				emit(level, s.Class, "page skipped", "connector", connectors.Web,
					"host", s.Subject, "class", s.Class, "reason", raw)
				continue
			}
			emit(level, s.Class, "document skipped", "path", s.Subject, "class", s.Class, "reason", raw)
		}
	}
	if len(moreByClass) > 0 {
		more, classes := 0, make([]string, 0, len(moreByClass))
		for class, n := range moreByClass {
			more += n
			classes = append(classes, fmt.Sprintf("%s=%d", class, n))
		}
		sort.Strings(classes)
		log.Warn("more documents skipped", "count", more, "classes", strings.Join(classes, " "))
	}
}

// sanitizeSkipRaw makes extractor text about a user's file fit for one log
// line: the first line only; staging paths reduced to the path inside the
// staging directory; URLs reduced to scheme and host; control and format
// runes (a NUL, a bidi override that would reorder the line in a terminal)
// and invalid UTF-8 dropped; whitespace collapsed; capped at maxSkipRawRunes.
// The text is logged, never persisted: the class is what the row gets.
func sanitizeSkipRaw(raw, jail string) string {
	s := raw
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	s = stripStagingPaths(s, jail)
	s = jobs.RedactURLs(s)

	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
			// Invalid UTF-8: a converter's stderr is bytes, not text.
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			// A space rather than nothing, so a tab still separates words
			// once whitespace is collapsed below.
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	s = strings.Join(strings.Fields(b.String()), " ")
	if runes := []rune(s); len(runes) > maxSkipRawRunes {
		s = string(runes[:maxSkipRawRunes])
	}
	return s
}

// stripStagingPaths reduces each staging path in s to the path inside the
// staging directory. jail is the directory the worker staged uploads into;
// the pipeline's own staging (kiln-docs-*) is created under the temp directory
// and is not visible from here, so any token under the temp directory loses
// that first entry as well. A converter quotes the file it failed on, and the
// host's temp layout is nothing a log reader needs.
func stripStagingPaths(s, jail string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		fields[i] = stripStagingPath(f, jail)
	}
	return strings.Join(fields, " ")
}

func stripStagingPath(tok, jail string) string {
	if jail != "" {
		if tok == jail {
			return ""
		}
		if idx := strings.Index(tok, jail+"/"); idx >= 0 {
			return tok[:idx] + tok[idx+len(jail)+1:]
		}
	}
	for _, root := range tempRoots() {
		idx := strings.Index(tok, root+"/")
		if idx < 0 {
			continue
		}
		rest := tok[idx+len(root)+1:]
		// The temp directory's own entry is the staging directory; what is
		// inside it is the document's path.
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[j+1:]
		} else {
			rest = ""
		}
		return tok[:idx] + rest
	}
	return tok
}

// tempRoots is where MkdirTemp puts staging: the temp directory as configured
// and as resolved (macOS keeps it behind a symlink), and the conventional
// /tmp whatever TMPDIR says.
func tempRoots() []string {
	roots := []string{"/private/tmp", "/tmp"}
	if t := filepath.Clean(os.TempDir()); t != "/" {
		if resolved, err := filepath.EvalSymlinks(t); err == nil && resolved != t {
			roots = append([]string{resolved}, roots...)
		}
		roots = append([]string{t}, roots...)
	}
	return roots
}
