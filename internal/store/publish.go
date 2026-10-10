package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PublishTarget is where a bench's wiki is mirrored to.
//
// The mirror image of ConnectorRow, and modelled the same way on purpose: a
// connector says where material comes from, this says where the finished wiki
// goes. Both carry a credential, an enabled flag, and the last outcome, so a
// failure shows up on the page where it was configured.
type PublishTarget struct {
	ID          string
	WorkspaceID string
	Kind        string
	RepoURL     string
	Branch      string
	// PathPrefix is the subdirectory the wiki owns; empty is the repository
	// root. Only that subtree is replaced by a push.
	PathPrefix   string
	CredentialID string
	Enabled      bool

	LastCommit      string
	LastPublishedAt *time.Time
	LastError       string

	// PublishedRevision is the wiki revision the repository last matched, or
	// zero when it has never been published to (or the target was reconfigured
	// since). The worker compares it with the wiki's current revision to skip a
	// push that cannot change anything.
	PublishedRevision int64
}

// PublishTargetPatch is a partial update. Nil fields are left alone, so the UI
// can toggle enabled without resending the repository URL.
type PublishTargetPatch struct {
	RepoURL      *string
	Branch       *string
	PathPrefix   *string
	CredentialID *string
	Enabled      *bool
}

// PublishTargetFor returns a workspace's target, or ErrNotFound when the bench
// does not publish.
func (s *WikiStore) PublishTargetFor(ctx context.Context, workspaceID string) (PublishTarget, error) {
	var (
		t         PublishTarget
		cred      *string
		lastErr   *string
		published *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, kind, repo_url, branch, path_prefix,
		       credential_id::text, enabled, last_commit, last_published_at, last_error,
		       published_revision
		FROM publish_targets WHERE workspace_id = $1`, workspaceID).
		Scan(&t.ID, &t.WorkspaceID, &t.Kind, &t.RepoURL, &t.Branch, &t.PathPrefix,
			&cred, &t.Enabled, &t.LastCommit, &published, &lastErr,
			&t.PublishedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublishTarget{}, ErrNotFound
	}
	if err != nil {
		return PublishTarget{}, fmt.Errorf("store: load publish target: %w", err)
	}
	if cred != nil {
		t.CredentialID = *cred
	}
	if lastErr != nil {
		t.LastError = *lastErr
	}
	t.LastPublishedAt = published
	return t, nil
}

// SetPublishTarget creates or replaces a workspace's target.
//
// Upsert rather than insert-or-patch because there is exactly one per bench:
// configuring publishing twice means changing where it publishes, not adding a
// second destination.
func (s *WikiStore) SetPublishTarget(ctx context.Context, t PublishTarget) (string, error) {
	kind := t.Kind
	if kind == "" {
		kind = "github"
	}
	branch := t.Branch
	if branch == "" {
		branch = "main"
	}

	var id string
	// The last outcome is cleared on reconfiguration: an error from the old
	// repository says nothing about the new one, and leaving it would show a
	// stale failure against a target that has never been tried. The same goes for
	// the revision the repository matched: it described the old destination.
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO publish_targets
			(workspace_id, kind, repo_url, branch, path_prefix, credential_id, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (workspace_id) DO UPDATE SET
			kind = EXCLUDED.kind,
			repo_url = EXCLUDED.repo_url,
			branch = EXCLUDED.branch,
			path_prefix = EXCLUDED.path_prefix,
			credential_id = EXCLUDED.credential_id,
			enabled = EXCLUDED.enabled,
			last_error = NULL,
			published_revision = 0,
			updated_at = now()
		RETURNING id`,
		t.WorkspaceID, kind, t.RepoURL, branch, t.PathPrefix,
		nullable(t.CredentialID), t.Enabled).Scan(&id); err != nil {
		return "", fmt.Errorf("store: set publish target: %w", err)
	}
	return id, nil
}

// PatchPublishTarget applies a partial update, leaving unset fields alone.
func (s *WikiStore) PatchPublishTarget(ctx context.Context, workspaceID string, p PublishTargetPatch) error {
	var credentialSet bool
	var credential *string
	if p.CredentialID != nil {
		credentialSet = true
		if *p.CredentialID != "" {
			credential = p.CredentialID
		}
	}

	// Moving the destination invalidates what is known about it: the new
	// repository, branch or path has not been pushed to, whatever revision the
	// old one matched.
	destinationChanged := p.RepoURL != nil || p.Branch != nil || p.PathPrefix != nil

	tag, err := s.pool.Exec(ctx, `
		UPDATE publish_targets SET
			published_revision = CASE WHEN $8 THEN 0 ELSE published_revision END,
			repo_url    = coalesce($2, repo_url),
			branch      = coalesce($3, branch),
			path_prefix = coalesce($4, path_prefix),
			credential_id = CASE WHEN $5 THEN $6::uuid ELSE credential_id END,
			enabled     = coalesce($7, enabled),
			updated_at  = now()
		WHERE workspace_id = $1`,
		workspaceID, p.RepoURL, p.Branch, p.PathPrefix,
		credentialSet, credential, p.Enabled, destinationChanged)
	if err != nil {
		return fmt.Errorf("store: patch publish target: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePublishTarget stops a bench publishing. The repository is left exactly
// as it was: kiln put those files there, but it does not own the repository,
// and quietly emptying someone's repo because they turned off a mirror would
// be indefensible.
func (s *WikiStore) DeletePublishTarget(ctx context.Context, workspaceID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM publish_targets WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return fmt.Errorf("store: delete publish target: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// WikiRevision is the counter that moves whenever the set of live pages or the
// derived artifacts change, and zero for a bench that has no wiki yet.
func (s *WikiStore) WikiRevision(ctx context.Context, workspaceID string) (int64, error) {
	var rev int64
	err := s.pool.QueryRow(ctx,
		`SELECT revision FROM wikis WHERE workspace_id = $1`, workspaceID).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: load wiki revision: %w", err)
	}
	return rev, nil
}

// MarkPublished records the outcome of one push.
//
// A success stamps the commit and the wiki revision the repository now matches
// and clears the error; a failure records it and leaves the last successful
// commit and revision in place, so the UI can say both "this broke" and "here is
// what is currently published", and the next build tries again.
func (s *WikiStore) MarkPublished(ctx context.Context, workspaceID, commit string, revision int64, publishErr string) error {
	if publishErr != "" {
		if _, err := s.pool.Exec(ctx, `
			UPDATE publish_targets SET last_error = $2, updated_at = now()
			WHERE workspace_id = $1`, workspaceID, publishErr); err != nil {
			return fmt.Errorf("store: mark publish failure: %w", err)
		}
		return nil
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE publish_targets
		SET last_commit = coalesce(nullif($2, ''), last_commit),
		    last_published_at = now(),
		    last_error = NULL,
		    published_revision = $3,
		    updated_at = now()
		WHERE workspace_id = $1`, workspaceID, commit, revision); err != nil {
		return fmt.Errorf("store: mark published: %w", err)
	}
	return nil
}
