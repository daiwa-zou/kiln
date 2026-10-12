package worker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// docSkipSink returns the OnDocSkips callback for one build: it records which
// uploads the sync could not read, logs them, and counts them.
//
// What the store gets is a class token per path, never the extractor's text;
// the row is tied to the sha256 of the bytes that were staged (staged), so a
// file replaced while the build ran is left alone. readPaths are the staged
// files the sync did read -- staged minus skipped -- so a skip recorded by an
// earlier build clears once the document reads. Web skips are logged only:
// a fetched page has no row to record on.
//
// The extractor's own text is for the operator log, after sanitizeSkipRaw. A
// new skip (the store reports the path changed) logs at Warn; a repeat, which
// every rebuild of a bench with a scan produces, at Debug. No review item is
// filed: the unreadable-blob precedent (reportUnreadableDocuments) is a
// storage incident, and a scan is a fact about the document that the Ingest
// page now shows.
//
// Nothing here can fail the run. A store error is logged and the build
// proceeds: the pages are still worth writing, and the Ingest page falls back
// to "not read, cause not recorded" for the file.
func (w *Worker) docSkipSink(run *store.QueuedRun, connectors jobs.SourceConnectors, jail string, staged map[string]string, log *slog.Logger) func(context.Context, []jobs.DocSkip) {
	return func(ctx context.Context, skips []jobs.DocSkip) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), skipSinkTimeout)
		defer cancel()

		// Persist first, so the log can say which skips are news.
		var fileSkips []store.FileSkip
		skippedPath := map[string]bool{}
		for _, s := range skips {
			if s.Kind != connector.KindUpload {
				continue
			}
			skippedPath[s.Subject] = true
			// An upload connector reading a worker-local folder has no file
			// rows; its skips are logged like everyone else's and recorded
			// nowhere.
			if sha, ok := staged[s.Subject]; ok {
				fileSkips = append(fileSkips, store.FileSkip{Path: s.Subject, SHA256: sha, Class: s.Class})
			}
		}
		var readPaths []string
		for path := range staged {
			if !skippedPath[path] {
				readPaths = append(readPaths, path)
			}
		}
		sort.Strings(readPaths)

		// recorded says the store answered, so a path absent from changed is
		// a repeat. Without an answer every skip is treated as news: louder
		// is the safe side of a failed write.
		recorded := false
		changed := map[string]bool{}
		if len(fileSkips) > 0 || len(readPaths) > 0 {
			paths, err := w.Store.RecordFileSkips(ctx, run.WorkspaceID, fileSkips, readPaths, time.Now())
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

		lines, more := 0, 0
		emit := func(level slog.Level, msg string, attrs ...any) {
			if lines >= skipLogLines {
				more++
				return
			}
			lines++
			log.Log(ctx, level, msg, attrs...)
		}
		for _, s := range skips {
			raw := sanitizeSkipRaw(s.Raw, jail)
			if s.Kind == connector.KindWeb {
				emit(slog.LevelWarn, "page skipped", "connector", connectors.Web,
					"host", s.Subject, "class", s.Class, "reason", raw)
				w.metrics().DocumentSkipped(s.Kind, s.Class)
				continue
			}
			level := slog.LevelWarn
			if recorded && !changed[s.Subject] {
				level = slog.LevelDebug
			}
			emit(level, "document skipped", "path", s.Subject, "class", s.Class, "reason", raw)
			if !recorded || changed[s.Subject] {
				w.metrics().DocumentSkipped(s.Kind, s.Class)
			}
		}
		if more > 0 {
			log.Warn("more documents skipped", "count", more)
		}
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
