package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/publish"
	"github.com/daiwa-zou/kiln/internal/store"
)

// Publishing runs after a build, not as part of it.
//
// A push that fails must never fail the build. The wiki is already committed to
// Postgres by then and is correct and readable; a mirror that could not be
// written is a degraded copy of something that still exists, and turning that
// into a failed run would make an unreachable GitHub look like a broken bench.
// So the outcome lands on the target row, where it shows up next to the
// configuration that caused it, the same way a connector's sync error does.

// publishAfterBuild mirrors the bench's wiki to its configured repository.
//
// Best effort throughout, and silent when nothing is configured -- which is the
// common case, since publishing is opt-in per bench.
func (w *Worker) publishAfterBuild(ctx context.Context, workspaceID, slug, trigger string, log *slog.Logger) {
	target, err := w.Store.PublishTargetFor(ctx, workspaceID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		log.Error("publish target lookup failed", "error", err)
		return
	}
	if !target.Enabled {
		return
	}

	log = log.With("repo", target.RepoURL, "branch", target.Branch)
	commit, err := w.pushWiki(ctx, workspaceID, slug, trigger, target)

	switch {
	case errors.Is(err, publish.ErrNothingToPush):
		// The repository already matched. Recorded as a success with no new
		// commit: nothing is wrong, and stamping an error here would make an
		// idle bench look permanently broken.
		log.Info("wiki already published; nothing to push")
		if merr := w.Store.MarkPublished(ctx, workspaceID, "", ""); merr != nil {
			log.Error("recording publish outcome failed", "error", merr)
		}
	case err != nil:
		log.Error("publishing the wiki failed", "error", err)
		if merr := w.Store.MarkPublished(ctx, workspaceID, "", err.Error()); merr != nil {
			log.Error("recording publish failure failed", "error", merr)
		}
	default:
		log.Info("wiki published", "commit", commit)
		if merr := w.Store.MarkPublished(ctx, workspaceID, commit, ""); merr != nil {
			log.Error("recording publish outcome failed", "error", merr)
		}
	}
}

// pushWiki assembles the wiki as it currently stands and pushes it.
//
// Read from the database rather than from the run's own output, so what lands
// in the repository is the whole wiki rather than the pages this run happened
// to touch. An incremental build that rewrote one page must still publish a
// complete, browsable tree.
func (w *Worker) pushWiki(ctx context.Context, workspaceID, slug, trigger string, target store.PublishTarget) (string, error) {
	pages, err := w.Store.LoadPages(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("load pages: %w", err)
	}

	in := publish.Input{Bench: slug, Pages: pages}
	for _, a := range []struct {
		kind string
		into *string
	}{
		{"index", &in.Index},
		{"overview", &in.Overview},
		{"log", &in.Log},
	} {
		body, err := w.Store.LoadArtifact(ctx, workspaceID, a.kind)
		if err != nil {
			// An artifact that will not load costs navigation, not the
			// publish: the pages themselves are the substance.
			continue
		}
		*a.into = body
	}

	figures, err := w.publishableFigures(ctx, workspaceID)
	if err != nil {
		// Pages still publish; their images render as broken links in the
		// repository, which is visible and fixable, unlike no publish at all.
		return "", err
	}
	in.Figures = figures

	token, err := w.publishToken(ctx, target)
	if err != nil {
		return "", err
	}

	res, err := publish.Push(ctx, publish.PushOptions{
		Remote:  target.RepoURL,
		Branch:  target.Branch,
		Prefix:  target.PathPrefix,
		Token:   token,
		Files:   publish.Tree(in),
		Message: commitMessage(slug, trigger, len(pages)),
	})
	if err != nil {
		return "", err
	}
	return res.Commit, nil
}

// publishableFigures loads the bench's images so referenced ones can be checked
// in alongside the pages that use them.
func (w *Worker) publishableFigures(ctx context.Context, workspaceID string) ([]publish.Figure, error) {
	if w.Blobs == nil {
		return nil, nil
	}
	rows, err := w.Store.ListFigures(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list figures: %w", err)
	}

	out := make([]publish.Figure, 0, len(rows))
	for _, f := range rows {
		data, err := readBlob(ctx, w.Blobs, f.BlobKey)
		if err != nil {
			// One unreadable image is not a reason to withhold the wiki. Its
			// page keeps the unresolved reference, which reads as a broken
			// image rather than as a silently missing figure.
			continue
		}
		out = append(out, publish.Figure{ID: f.ID, ContentType: f.ContentType, Data: data})
	}
	return out, nil
}

// publishToken decrypts the target's credential through openCredential, which
// is the only call path in kiln that turns ciphertext back into a secret.
//
// A target with no credential pushes unauthenticated. That works only for a
// repository accepting anonymous writes; GitHub rejects it, and the rejection
// lands on the target row as a plain authentication error, which diagnoses
// itself better than refusing to try would.
func (w *Worker) publishToken(ctx context.Context, target store.PublishTarget) (string, error) {
	if target.CredentialID == "" {
		return "", nil
	}
	token, err := w.openCredential(ctx, target.CredentialID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(token), nil
}

// readBlob loads one blob whole. Figures are bounded by the extractor's byte
// budget, so buffering is safe here in a way it would not be for an upload.
func readBlob(ctx context.Context, blobs blob.Store, key string) ([]byte, error) {
	rc, err := blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// commitMessage says what the build did, so the repository's history reads as
// a record of the bench rather than a wall of identical subjects.
func commitMessage(slug, trigger string, pages int) string {
	subject := fmt.Sprintf("Update %s wiki (%d pages)", slug, pages)
	if slug == "" {
		subject = fmt.Sprintf("Update wiki (%d pages)", pages)
	}
	body := fmt.Sprintf("Published by kiln at %s", time.Now().UTC().Format(time.RFC3339))
	if trigger != "" {
		body += fmt.Sprintf("\nTrigger: %s", trigger)
	}
	return subject + "\n\n" + body + "\n"
}
