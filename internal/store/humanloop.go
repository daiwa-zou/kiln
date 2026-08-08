package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

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
	ID       string
	Kind     string
	Title    string
	Detail   string
	Actions  []string
	Status   string
	PageSlug string
	// Research is what a research run found, empty until one has run. It sits
	// beside Detail rather than replacing it: the question and the reading done
	// against it are two different things, and a human answering the question
	// wants both.
	Research   string
	ResearchAt string
	// Researching reports a research run queued or in flight for this item.
	// Derived from the run queue rather than stored on the item, so it cannot
	// outlive the run it describes.
	Researching bool
	CreatedAt   string
	ResolvedAt  string
}

// ResearchableKinds are the review kinds a research run can answer.
//
// Every one of them is a question about the *material*: two sources disagree,
// a claim could not be corroborated, a page is referred to and never written.
// Re-reading the corpus is exactly what settles those. The kinds left out are
// questions about the deployment -- a deletion needs authority rather than
// evidence, and no amount of reading tells the wiki whether a budget should be
// raised.
var ResearchableKinds = []string{"contradiction", "uncertain", "gap"}

// ErrNotResearchable marks a review item whose kind no amount of reading can
// answer, so the caller can say which of the two refusals happened.
var ErrNotResearchable = errors.New("store: review kind cannot be researched")

// ErrRunActive marks an enqueue that lost to a build already queued or running
// on the same workspace. Distinct from a failure: nothing is wrong, the request
// simply has to wait for the slot.
var ErrRunActive = errors.New("store: workspace already has an active run")

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
		SELECT c.id, c.body, c.active, c.created_at
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
		var (
			c         CorrectionRow
			createdAt time.Time
		)
		if err := rows.Scan(&c.ID, &c.Body, &c.Active, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan correction: %w", err)
		}
		c.Created = instant(&createdAt)
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
		       coalesce(p.slug, ''), r.research, r.research_at,
		       EXISTS (SELECT 1 FROM runs rn
		               WHERE rn.review_id = r.id AND rn.status IN ('queued','running')),
		       r.created_at, r.resolved_at
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
			rv                     ReviewRow
			actions                []byte
			createdAt              time.Time
			researchAt, resolvedAt *time.Time
		)
		if err := rows.Scan(&rv.ID, &rv.Kind, &rv.Title, &rv.Detail, &actions,
			&rv.Status, &rv.PageSlug, &rv.Research, &researchAt, &rv.Researching,
			&createdAt, &resolvedAt); err != nil {
			return nil, fmt.Errorf("store: scan review: %w", err)
		}
		rv.CreatedAt = instant(&createdAt)
		rv.ResearchAt = instant(researchAt)
		rv.ResolvedAt = instant(resolvedAt)
		// Actions ride as jsonb; a decode failure means a hand-edited row, and
		// an empty action list degrades gracefully in the UI.
		_ = json.Unmarshal(actions, &rv.Actions)
		out = append(out, rv)
	}
	return out, rows.Err()
}

// ResolveReview closes an open review item with the action the human chose.
// "approve" resolves to status "approved" -- the status LoadApprovedDeletions
// selects on -- and anything else to "resolved". A second resolution answers
// ErrNotFound rather than silently re-approving something already acted on.
//
// An item with research in flight is resolvable like any other: a human who
// has decided does not wait for a reader that no longer has a question to
// answer. The research run is dropped with it while it is still queued,
// because the money it would spend buys an answer to a settled question; a run
// already claimed is left alone, and RecordResearch discards its findings when
// it lands on an item that is no longer open.
func (s *WikiStore) ResolveReview(ctx context.Context, workspaceID, reviewID, action, resolvedBy string) error {
	status := "resolved"
	if action == "approve" {
		status = "approved"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin resolve review: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	tag, err := tx.Exec(ctx, `
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

	if _, err := tx.Exec(ctx, `
		DELETE FROM runs WHERE review_id = $1 AND status = 'queued'`, reviewID); err != nil {
		return fmt.Errorf("store: drop queued research run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit resolve review: %w", err)
	}
	return nil
}

// instant renders a timestamp as an unambiguous UTC RFC3339 string, empty when
// there is none.
//
// Dates used to go out as 'YYYY-MM-DD' from to_char, which reads fine in SQL
// and is wrong in a browser: a bare date parses as *UTC* midnight, so a review
// filed an hour ago rendered as "yesterday" for every reader west of UTC, and
// the exact timestamp the UI promises on hover did not exist to show. Runs have
// always sent the full instant; this is the same rule everywhere.
func instant(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ResearchQuestion is the review item behind a research run, as the worker
// reads it back after claiming the run.
type ResearchQuestion struct {
	ReviewID string
	Kind     string
	Title    string
	Detail   string
	// PageSlug is the page the flag was raised against, empty when the item is
	// about the bench rather than one page.
	PageSlug string
}

// RequestResearch queues a research run for one review item: the worker
// re-reads the bench's sources with this single question in hand and writes
// back what it found.
//
// The item itself is not touched. A question with a worker reading for it is
// still an open question -- nobody has answered it yet -- so it keeps its place
// in the inbox, and the run row is the whole record that a read is in flight.
//
// The two refusals are distinguishable on purpose. A kind no amount of reading
// can settle is ErrNotResearchable and always will be. A workspace whose one
// active-run slot is taken is ErrRunActive, which is a wait rather than a
// fault: the partial unique index that collapses webhook storms onto a single
// run would otherwise quietly attach this request to a build that is never
// going to answer anything, and the caller would report success for work that
// will not happen. A second click on the same card lands there too, which is
// the right answer to it.
func (s *WikiStore) RequestResearch(ctx context.Context, workspaceID, reviewID string) (runID string, err error) {
	var kind string
	err = s.pool.QueryRow(ctx, `
		SELECT kind FROM review_items
		WHERE id = $2 AND workspace_id = $1 AND status = 'open'`,
		workspaceID, reviewID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: load review for research: %w", err)
	}
	if !slices.Contains(ResearchableKinds, kind) {
		return "", ErrNotResearchable
	}

	err = s.pool.QueryRow(ctx, `
		INSERT INTO runs (workspace_id, trigger, status, review_id)
		VALUES ($1, 'research', 'queued', $2)
		ON CONFLICT (workspace_id) WHERE status IN ('queued','running') DO NOTHING
		RETURNING id`, workspaceID, reviewID).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrRunActive
	}
	if err != nil {
		return "", fmt.Errorf("store: enqueue research run: %w", err)
	}
	return runID, nil
}

// ResearchQuestionFor returns the review item a claimed run exists to answer,
// or ErrNotFound when the run is an ordinary build. The worker calls this
// after claiming rather than carrying the question on the queue row: a run
// waits in the queue for as long as the bench is busy, and the question is
// worth reading at the moment it is answered.
func (s *WikiStore) ResearchQuestionFor(ctx context.Context, runID string) (*ResearchQuestion, error) {
	var q ResearchQuestion
	err := s.pool.QueryRow(ctx, `
		SELECT ri.id, ri.kind, ri.title, ri.detail, coalesce(p.slug, '')
		FROM runs r
		JOIN review_items ri ON ri.id = r.review_id
		LEFT JOIN pages p ON p.id = ri.page_id
		WHERE r.id = $1`, runID).Scan(&q.ReviewID, &q.Kind, &q.Title, &q.Detail, &q.PageSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: load research question: %w", err)
	}
	return &q, nil
}

// RecordResearch writes back what a research run found.
//
// Findings are evidence, not authority. The item keeps its place in the queue
// with the reading attached so a human still makes the call -- unless the pass
// reported the question conclusively settled, which is the one case where
// leaving it in the inbox would ask someone to re-answer a question that no
// longer has two sides.
//
// Scoped to items still open, so a human who answered while the worker was
// reading keeps the last word and a research run that outlived its question
// writes nothing.
func (s *WikiStore) RecordResearch(ctx context.Context, reviewID, findings string, resolved bool) error {
	status := "open"
	if resolved {
		status = "resolved"
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE review_items
		SET research = $2,
		    research_at = now(),
		    status = $3,
		    resolved_at = CASE WHEN $3 = 'resolved' THEN now() ELSE resolved_at END
		WHERE id = $1 AND status = 'open'`,
		reviewID, findings, status); err != nil {
		return fmt.Errorf("store: record research: %w", err)
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
