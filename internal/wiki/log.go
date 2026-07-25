package wiki

import (
	"fmt"
	"strings"
)

// LogHeader seeds a new log file.
const LogHeader = "# Wiki Log"

// LogEntry is one run's record.
type LogEntry struct {
	Date    string // YYYY-MM-DD
	Action  string // build | research | prune
	Subject string // workspace name, optionally with a ref
	Ref     string // commit sha or equivalent, optional
	Lines   []string
}

// RenderLogEntry formats a single entry.
//
// The "## [YYYY-MM-DD] action | subject" prefix is deliberate: it keeps the log
// greppable with ordinary tools, so `grep "^## \[" log.md | tail -5` gives the
// last five runs without any parsing.
func RenderLogEntry(e LogEntry) string {
	var b strings.Builder

	subject := e.Subject
	if e.Ref != "" {
		subject = fmt.Sprintf("%s @ %s", e.Subject, e.Ref)
	}
	fmt.Fprintf(&b, "## [%s] %s | %s\n", e.Date, e.Action, subject)

	if len(e.Lines) > 0 {
		b.WriteString("\n")
		for _, line := range e.Lines {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	return b.String()
}

// SummarizeChanges builds the standard bullet lines for a build entry. Counts
// of zero are omitted so an entry states only what actually happened.
func SummarizeChanges(created, updated, deleted int) []string {
	var parts []string
	if created > 0 {
		parts = append(parts, fmt.Sprintf("%d created", created))
	}
	if updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", updated))
	}
	if deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", deleted))
	}
	if len(parts) == 0 {
		return []string{"No page changes"}
	}
	return []string{"Pages: " + strings.Join(parts, ", ")}
}
