package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// This file is the write side of the human loop: steering documents, page
// corrections, and the review queue. The read side has existed since the
// beginning -- LoadSteering injects all of it into every prompt -- but until
// these methods there was no door into it short of raw psql.

// ReviewRow is one review item as the API lists it.
type ReviewRow struct {
	ID         string
	Kind       string
	Title      string
	Detail     string
	Actions    []string
	Status     string
	PageSlug   string
	CreatedAt  string
	ResolvedAt string
}

// SteeringKinds are the steering documents a workspace can carry, mirroring
// the CHECK constraint on steering_docs.kind.
var SteeringKinds = []string{"purpose", "schema"}

// UpsertSteeringDoc replaces a workspace steering document. The table is
// keyed on (workspace_id, kind), so writing is idempotent.
func (s *WikiStore) UpsertSteeringDoc(ctx context.Context, workspaceID, kind, body, updatedBy string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO steering_docs (workspace_id, kind, body, updated_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (workspace_id, kind) DO UPDATE
		SET body = EXCLUDED.body, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		workspaceID, kind, body, nullable(updatedBy)); err != nil {
		return fmt.Errorf("store: upsert steering %s: %w", kind, err)
	}
	return nil
}

// LoadSteeringDocs returns both steering documents keyed by kind, absent kinds
// as empty strings, so the editing UI renders a stable form.
func (s *WikiStore) LoadSteeringDocs(ctx context.Context, workspaceID string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range SteeringKinds {
		out[k] = ""
	}
	rows, err := s.pool.Query(ctx,
		`SELECT kind, body FROM steering_docs WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: load steering docs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, body string
		if err := rows.Scan(&kind, &body); err != nil {
			return nil, fmt.Errorf("store: scan steering doc: %w", err)
		}
		out[kind] = body
	}
	return out, rows.Err()
}

// CreateCorrection pins a correction to a page, addressed by slug or path.
// Corrections are the designed alternative to editing a page -- an edit would
// be clobbered on the next regeneration, a correction is re-injected into
// every future prompt for that page.
func (s *WikiStore) CreateCorrection(ctx context.Context, workspaceID, pageRef, body, createdBy string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO page_corrections (page_id, body, created_by)
		SELECT p.id, $3, $4
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		  AND (p.slug = $2 OR p.path = $2)
		LIMIT 1
		RETURNING id`,
		workspaceID, pageRef, body, nullable(createdBy)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: create correction: %w", err)
	}
	return id, nil
}

// SetCorrectionActive toggles a correction. Deactivating rather than deleting
// keeps the record of what a human once said about a page.
func (s *WikiStore) SetCorrectionActive(ctx context.Context, workspaceID, correctionID string, active bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE page_corrections c
		SET active = $3
		FROM pages p, wikis w
		WHERE c.id = $2 AND p.id = c.page_id AND w.id = p.wiki_id
		  AND w.workspace_id = $1`,
		workspaceID, correctionID, active)
	if err != nil {
		return fmt.Errorf("store: toggle correction: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCorrections returns a page's corrections, active first, newest first.
func (s *WikiStore) ListCorrections(ctx context.Context, workspaceID, pageRef string) ([]CorrectionRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.body, c.active, to_char(c.created_at, 'YYYY-MM-DD')
		FROM page_corrections c
		JOIN pages p ON p.id = c.page_id
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		  AND (p.slug = $2 OR p.path = $2)
		ORDER BY c.active DESC, c.created_at DESC`,
		workspaceID, pageRef)
	if err != nil {
		return nil, fmt.Errorf("store: list corrections: %w", err)
	}
	defer rows.Close()

	out := []CorrectionRow{}
	for rows.Next() {
		var c CorrectionRow
		if err := rows.Scan(&c.ID, &c.Body, &c.Active, &c.Created); err != nil {
			return nil, fmt.Errorf("store: scan correction: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CorrectionRow is one correction as the API returns it.
type CorrectionRow struct {
	ID      string
	Body    string
	Active  bool
	Created string
}

// ListReviews returns review items for a workspace. Status filters when
// non-empty; "open" is what the UI's inbox shows.
func (s *WikiStore) ListReviews(ctx context.Context, workspaceID, status string, limit, offset int) ([]ReviewRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.kind, r.title, r.detail, r.actions, r.status,
		       coalesce(p.slug, ''),
		       to_char(r.created_at, 'YYYY-MM-DD'),
		       coalesce(to_char(r.resolved_at, 'YYYY-MM-DD'), '')
		FROM review_items r
		LEFT JOIN pages p ON p.id = r.page_id
		WHERE r.workspace_id = $1 AND ($2 = '' OR r.status = $2)
		ORDER BY r.created_at DESC
		LIMIT $3 OFFSET $4`,
		workspaceID, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list reviews: %w", err)
	}
	defer rows.Close()

	out := []ReviewRow{}
	for rows.Next() {
		var (
			rv      ReviewRow
			actions []byte
		)
		if err := rows.Scan(&rv.ID, &rv.Kind, &rv.Title, &rv.Detail, &actions,
			&rv.Status, &rv.PageSlug, &rv.CreatedAt, &rv.ResolvedAt); err != nil {
			return nil, fmt.Errorf("store: scan review: %w", err)
		}
		// Actions ride as jsonb; a decode failure means a hand-edited row, and
		// an empty action list degrades gracefully in the UI.
		_ = json.Unmarshal(actions, &rv.Actions)
		out = append(out, rv)
	}
	return out, rows.Err()
}

// ResolveReview closes an open review item with the action the human chose.
// "approve" resolves to status "approved" -- the status LoadApprovedDeletions
// selects on -- and anything else to "resolved". Only open items can be
// resolved; a second resolution answers ErrNotFound rather than silently
// re-approving something already acted on.
func (s *WikiStore) ResolveReview(ctx context.Context, workspaceID, reviewID, action, resolvedBy string) error {
	status := "resolved"
	if action == "approve" {
		status = "approved"
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE review_items
		SET status = $3, resolved_by = $4, resolved_at = now()
		WHERE id = $2 AND workspace_id = $1 AND status = 'open'`,
		workspaceID, reviewID, status, nullable(resolvedBy))
	if err != nil {
		return fmt.Errorf("store: resolve review: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LoadApprovedDeletions returns the source keys whose deletion a human has
// approved through the review queue. The pipeline merges these into the
// cascade plan; once the cascade drops the source the approval becomes a
// harmless no-op, so nothing needs to be un-approved afterwards.
func (s *WikiStore) LoadApprovedDeletions(ctx context.Context, workspaceID string) ([]diff.Key, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT title FROM review_items
		WHERE workspace_id = $1 AND kind = 'deletion' AND status = 'approved'
		ORDER BY title`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: load approved deletions: %w", err)
	}
	defer rows.Close()

	var out []diff.Key
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return nil, fmt.Errorf("store: scan approved deletion: %w", err)
		}
		out = append(out, diff.Key(title))
	}
	return out, rows.Err()
}

// EnsureDeletionReviews files one open deletion review per disappeared source,
// skipping keys that already have an open or approved item so a source that
// stays missing across runs raises exactly one question. The title is the raw
// source key -- it is both the human-facing summary and what
// LoadApprovedDeletions parses back out.
func (s *WikiStore) EnsureDeletionReviews(ctx context.Context, workspaceID string, cands []jobs.DeletionCandidate) error {
	for _, c := range cands {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO review_items (workspace_id, kind, title, detail, actions)
			SELECT $1, 'deletion', $2, $3, '["approve","keep"]'::jsonb
			WHERE NOT EXISTS (
			    SELECT 1 FROM review_items
			    WHERE workspace_id = $1 AND kind = 'deletion' AND title = $2
			      AND status IN ('open','approved'))`,
			workspaceID, string(c.Key), c.Detail); err != nil {
			return fmt.Errorf("store: ensure deletion review %s: %w", c.Key, err)
		}
	}
	return nil
}

// Backlinks lists live pages that link to the given slug -- the inbound half
// of the graph page_links has materialized since the first import.
func (s *WikiStore) Backlinks(ctx context.Context, workspaceID, slug string) ([]PageInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.path, p.slug, p.type, p.title
		FROM page_links l
		JOIN pages p ON p.id = l.from_page_id
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND l.to_slug = $2 AND p.deleted_at IS NULL
		ORDER BY p.title, p.slug`,
		workspaceID, slug)
	if err != nil {
		return nil, fmt.Errorf("store: backlinks: %w", err)
	}
	defer rows.Close()

	out := []PageInfo{}
	for rows.Next() {
		var p PageInfo
		if err := rows.Scan(&p.Path, &p.Slug, &p.Type, &p.Title); err != nil {
			return nil, fmt.Errorf("store: scan backlink: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// WorkspaceRole reports the caller's role in the workspace's org, or "" when
// they are not a member. Every role except "viewer" may write: the historical
// default role is "member", and treating it as read-only would lock existing
// deployments out of the write API they are upgrading to get.
func (s *WikiStore) WorkspaceRole(ctx context.Context, workspaceID, userID string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx, `
		SELECT m.role
		FROM org_members m
		JOIN workspaces ws ON ws.org_id = m.org_id
		WHERE ws.id = $1 AND m.user_id = $2::uuid`,
		workspaceID, nullable(userID)).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: workspace role: %w", err)
	}
	return role, nil
}
