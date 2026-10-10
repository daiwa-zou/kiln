package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// An import commits what a run produced, and a run can be minutes long. In
// that time the world it read can change underneath it: someone deletes an
// uploaded document, or an admin deletes the connector a source came from.
// Both deletes are explicit and final, and the run has no way to know -- it
// planned against a snapshot.
//
// The advisory lock only orders the two transactions. It stops them
// interleaving, but it does not make the second one aware of the first, so an
// import that lands after a delete would re-insert the source row, re-write
// its pages, and undo the deletion. The fence below is the awareness: under
// the same lock, the import checks that the producer of each source still
// exists, and leaves out the ones that do not.
//
// The alternative considered was a per-workspace epoch that every explicit
// delete bumps and every import compares against. It was rejected because it
// can only answer "did anything get deleted", and the only safe response to
// that is to abandon the whole import -- throwing away pages that are perfectly
// good, for money already spent. Checking the producers directly answers the
// question that matters ("does the thing this source came from still exist")
// per source, so everything else lands, and it needs no schema change and no
// state that has to be kept in step with the deletes.

// lockImports takes the per-workspace lock that serializes imports with the
// explicit deletes that would otherwise race them. Transaction-scoped: it
// releases on commit or rollback, so no path can forget to.
func lockImports(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('kiln:import:' || $1))`, workspaceID); err != nil {
		return fmt.Errorf("store: acquire import lock: %w", err)
	}
	return nil
}

// uploadNamespace is the key prefix of an uploaded document's source records.
const uploadNamespace = diff.PrefixDoc + ":upload:"

// uploadPathCandidates returns the file paths a source key could belong to, or
// nil when the key is not an uploaded document's. A document's section units
// are keyed "<doc key>#<section>", and a path may itself contain '#', so every
// split point is a candidate and the caller asks which one is a real file.
func uploadPathCandidates(key string) []string {
	rest, ok := strings.CutPrefix(key, uploadNamespace)
	if !ok {
		return nil
	}
	out := []string{rest}
	for i := 0; i < len(rest); i++ {
		if rest[i] == '#' {
			out = append(out, rest[:i])
		}
	}
	return out
}

// uploadSourceLive reports whether an uploaded document's source key still has
// a file row. Keys outside the upload namespace are never judged here.
func uploadSourceLive(key string, files map[string]bool) bool {
	cands := uploadPathCandidates(key)
	if cands == nil {
		return true
	}
	for _, c := range cands {
		if files[c] {
			return true
		}
	}
	return false
}

// uploadFilePaths loads the paths of a workspace's uploaded files.
func uploadFilePaths(ctx context.Context, q queryer, workspaceID string) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT path FROM workspace_files WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: load file paths: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: scan file path: %w", err)
		}
		out[p] = true
	}
	return out, rows.Err()
}

// liveConnectors returns which of the given connector ids still exist in the
// workspace, and share-locks those rows until the transaction ends.
//
// The lock is what closes the gap the advisory lock leaves: a connector with
// no sources yet is deleted without touching the import lock's critical
// section in a way this import could see, and the share lock makes that delete
// wait for this import to commit instead of silently cascading away the
// sources it is about to write.
func liveConnectors(ctx context.Context, tx pgx.Tx, workspaceID string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	// Compared as text so a malformed id reads as "no such connector" rather
	// than as a cast error that fails the import.
	rows, err := tx.Query(ctx, `
		SELECT id::text FROM connectors
		WHERE workspace_id = $1 AND id::text = ANY($2)
		FOR SHARE`, workspaceID, ids)
	if err != nil {
		return nil, fmt.Errorf("store: check connectors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan connector id: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// fencedImport is an import request with everything removed whose producer
// disappeared while the run was in flight.
type fencedImport struct {
	req jobs.ImportRequest
	// regen are surviving sources that share a page with a removed one. Their
	// flag is set after the sources are written, because writing a source
	// clears it.
	regen []string
	// pagesDropped says the page set no longer matches what the run built its
	// index and overview from.
	pagesDropped bool
}

// fenceImport removes, from an import, the sources whose producer is gone and
// the pages only those sources wrote. Everything else is untouched, so a
// deletion costs the run exactly the output that was about to resurrect it.
//
// Must run inside the import transaction, after the import lock is held: the
// answers are only true for as long as no delete can commit, and the lock is
// what guarantees that.
func fenceImport(ctx context.Context, tx pgx.Tx, in jobs.ImportRequest) (fencedImport, error) {
	out := fencedImport{req: in}
	if len(in.UpsertSources) == 0 {
		return out, nil
	}

	var (
		files      map[string]bool
		connectors map[string]bool
	)
	needFiles := false
	var connIDs []string
	seenConn := map[string]bool{}
	for _, s := range in.UpsertSources {
		if uploadPathCandidates(string(s.Key)) != nil {
			needFiles = true
		}
		if s.ConnectorID != "" && !seenConn[s.ConnectorID] {
			seenConn[s.ConnectorID] = true
			connIDs = append(connIDs, s.ConnectorID)
		}
	}
	var err error
	if needFiles {
		if files, err = uploadFilePaths(ctx, tx, in.WorkspaceID); err != nil {
			return out, err
		}
	}
	if connectors, err = liveConnectors(ctx, tx, in.WorkspaceID, connIDs); err != nil {
		return out, err
	}

	var survivors, dead []diff.SourceRecord
	for _, s := range in.UpsertSources {
		switch {
		case !uploadSourceLive(string(s.Key), files):
			dead = append(dead, s)
		case s.ConnectorID != "" && !connectors[s.ConnectorID]:
			dead = append(dead, s)
		default:
			survivors = append(survivors, s)
		}
	}
	if len(dead) == 0 {
		return out, nil
	}
	out.req.UpsertSources = survivors

	// A page is owed to the removed sources alone only if no live source
	// claims it too. Live means: kept in this import, or already on record and
	// not being replaced or dropped by it.
	replaced := map[diff.Key]bool{}
	for _, s := range in.UpsertSources {
		replaced[s.Key] = true
	}
	for _, k := range in.DropSources {
		replaced[k] = true
	}
	claimants := map[string][]string{}
	for _, s := range survivors {
		for _, p := range s.FilesWritten {
			claimants[p] = append(claimants[p], string(s.Key))
		}
	}
	onRecord, err := loadSources(ctx, tx, in.WorkspaceID)
	if err != nil {
		return out, err
	}
	for _, s := range onRecord {
		if replaced[s.Key] {
			continue
		}
		for _, p := range s.FilesWritten {
			claimants[p] = append(claimants[p], string(s.Key))
		}
	}

	deadPages := map[string]bool{}
	for _, s := range dead {
		for _, p := range s.FilesWritten {
			deadPages[p] = true
		}
	}
	regen := map[string]bool{}
	pages := in.UpsertPages[:0:0]
	for _, p := range in.UpsertPages {
		if !deadPages[p.Path] {
			pages = append(pages, p)
			continue
		}
		owners := claimants[p.Path]
		if len(owners) == 0 {
			out.pagesDropped = true
			continue
		}
		// Shared with a live source: the page stays, but its prose may have
		// been written with the departed document in view, so the live owners
		// rewrite it next run -- the same treatment the delete cascade gives
		// a shared page.
		pages = append(pages, p)
		for _, o := range owners {
			regen[o] = true
		}
	}
	out.req.UpsertPages = pages
	for k := range regen {
		out.regen = append(out.regen, k)
	}
	return out, nil
}

// flagSourcesForRegeneration marks sources whose shared pages were written
// with a departed document in view.
func flagSourcesForRegeneration(ctx context.Context, tx pgx.Tx, workspaceID string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sources SET needs_regen = TRUE, updated_at = now()
		WHERE workspace_id = $1 AND key = ANY($2)`, workspaceID, keys); err != nil {
		return fmt.Errorf("store: flag sources for regeneration: %w", err)
	}
	return nil
}
