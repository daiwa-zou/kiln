package jobs

import (
	"log/slog"

	"github.com/daiwa-zou/kiln/internal/wiki"
)

// A page a human deleted has to stay deleted, and the pipeline enforces that in
// two places for two different reasons.
//
// The prompt tells the agent (appendSuppressions), which is what stops a run
// paying to write a page that would only be thrown away, and gives it the
// chance to put the material somewhere useful instead. This filter is the
// backstop: a prompt is guidance, and a page that slipped past it would undo a
// human's deletion on the next build with nobody noticing.
//
// Neither is redundant. Without the prompt the run wastes money; without the
// filter the deletion does not hold.

// dropSuppressed removes pages a human deleted from what a run is about to
// import, logging each one so an agent repeatedly trying to write a deleted
// page is visible rather than silently absorbed.
func dropSuppressed(written []wiki.Page, suppressed map[string]string, log *slog.Logger) []wiki.Page {
	if len(suppressed) == 0 || len(written) == 0 {
		return written
	}
	out := make([]wiki.Page, 0, len(written))
	for _, p := range written {
		if _, gone := suppressed[p.Slug]; gone {
			log.Info("discarded a page a human deleted",
				"slug", p.Slug, "path", p.Path,
				"reason", suppressed[p.Slug])
			continue
		}
		out = append(out, p)
	}
	return out
}
