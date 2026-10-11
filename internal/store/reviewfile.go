package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Review items filed from outside the pipeline -- by an agent over MCP, or
// anyone with a key -- and the human promotion that turns a proposed
// correction into a pinned one. Filing never changes a page: every item lands
// in the queue as a question, and the one route from a proposal to a prompt is
// PromoteReview, which a person calls with the text they confirmed.

// Flood caps on open items filed by API keys. Per key, so one runaway agent
// cannot starve another; per bench, so many keys together cannot bury the
// inbox. Items a person files from a session are not counted -- the caps
// exist because a loop is cheap for a program and not for a human.
const (
	AgentOpenCapPerKey   = 20
	AgentOpenCapPerBench = 200
)

// ErrAgentCap refuses a filing that would push a key, or the bench, past its
// cap on open agent-filed items. Wrapped with which one, for the message.
var ErrAgentCap = errors.New("store: too many open agent-filed review items")

// ErrPageGone refuses a promotion whose page was deleted after the proposal
// was filed: there is nothing left to pin the correction to.
var ErrPageGone = errors.New("store: the page this review is about no longer exists")

// FileReviewParams is one filing. Kind is decided by the caller against a
// fixed allowlist; the store trusts it, which is why nothing outside the
// filing handler may call this.
type FileReviewParams struct {
	WorkspaceID string
	Kind        string
	// PageRef is a slug or wiki-relative path, resolved to a live page in
	// WorkspaceID or refused. Ignored for a gap, which is about a page that
	// does not exist; the caller checks the gap list instead.
	PageRef string
	// Title is ignored for corrections, whose title is derived from the page
	// so that two proposals for one page read alike in the inbox.
	Title  string
	Detail string
	// Proposed is the correction text; empty for questions.
	Proposed string
	// Origin is the display name of whoever filed it -- free text, never
	// decided on. OriginKind is what filed it (OriginKey, OriginSession or
	// OriginLocal), and only key-filed items count toward the caps.
	// OriginToken is the filing key's id, which the per-key cap counts.
	Origin      string
	OriginKind  string
	OriginToken string
}

// Origin kinds. A key is a program and is capped; a session or an authless
// (loopback-only) instance is a person at a keyboard.
const (
	OriginKey     = "key"
	OriginSession = "session"
	OriginLocal   = "local"
)

// Outcomes of FileAgentReview.
const (
	FiledNew         = "filed"
	FiledAlreadyOpen = "already_open"
)

// FileAgentReview files one review item against a page, deduplicated against
// the open queue and bounded by the caps, all under one per-bench lock so the
// count a cap reads is the count it enforces.
//
// Dedupe is on (kind, title, page), plus the proposed text for corrections:
// asking the same question about the same page twice asks once, while two
// different corrections to one page are two proposals a human should see.
// A duplicate is answered before the caps, since it adds nothing.
func (s *WikiStore) FileAgentReview(ctx context.Context, p FileReviewParams) (id, outcome string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("store: begin file review: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('kiln:review-file:' || $1))`, p.WorkspaceID); err != nil {
		return "", "", fmt.Errorf("store: lock review filing: %w", err)
	}

	// The page must be live and in this bench. Joining through wikis to the
	// workspace is the cross-bench check: a ref naming another bench's page
	// finds nothing here, exactly like a ref naming no page at all.
	var pageID *string
	title := p.Title
	if p.Kind != "gap" {
		var pid, slug string
		err = tx.QueryRow(ctx, `
			SELECT p.id, p.slug
			FROM pages p
			JOIN wikis w ON w.id = p.wiki_id
			WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
			  AND (p.slug = $2 OR p.path = $2)
			ORDER BY (p.slug = $2) DESC
			LIMIT 1`, p.WorkspaceID, p.PageRef).Scan(&pid, &slug)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrNotFound
		}
		if err != nil {
			return "", "", fmt.Errorf("store: resolve review page: %w", err)
		}
		pageID = &pid
		if p.Kind == "correction" {
			title = "Proposed correction: " + slug
		}
	}

	err = tx.QueryRow(ctx, `
		SELECT id FROM review_items
		WHERE workspace_id = $1 AND status = 'open'
		  AND kind = $2 AND title = $3 AND page_id IS NOT DISTINCT FROM $4
		  AND ($2 <> 'correction' OR proposed = $5)
		LIMIT 1`, p.WorkspaceID, p.Kind, title, pageID, p.Proposed).Scan(&id)
	switch {
	case err == nil:
		return id, FiledAlreadyOpen, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", "", fmt.Errorf("store: dedupe review: %w", err)
	}

	if p.OriginKind == OriginKey {
		var perKey, perBench int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE origin_token = $2), count(*)
			FROM review_items
			WHERE workspace_id = $1 AND status = 'open' AND origin_kind = 'key'`,
			p.WorkspaceID, nullable(p.OriginToken)).Scan(&perKey, &perBench); err != nil {
			return "", "", fmt.Errorf("store: count agent reviews: %w", err)
		}
		switch {
		case perKey >= AgentOpenCapPerKey:
			return "", "", fmt.Errorf("%w: this key has %d open items in the bench", ErrAgentCap, perKey)
		case perBench >= AgentOpenCapPerBench:
			return "", "", fmt.Errorf("%w: the bench has %d open items filed by keys", ErrAgentCap, perBench)
		}
	}

	// Dismiss is the only action: an agent's question has no source to accept
	// and no deletion to approve, and a proposed correction is acted on by
	// promotion, a separate route that carries the text a human confirmed.
	if err := tx.QueryRow(ctx, `
		INSERT INTO review_items
		    (workspace_id, page_id, kind, title, detail, proposed, origin, origin_kind, origin_token, actions)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '["dismiss"]'::jsonb)
		RETURNING id`,
		p.WorkspaceID, pageID, p.Kind, title, p.Detail, p.Proposed, p.Origin, p.OriginKind,
		nullable(p.OriginToken)).Scan(&id); err != nil {
		return "", "", fmt.Errorf("store: insert review: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("store: commit file review: %w", err)
	}
	return id, FiledNew, nil
}

// PromoteReview pins a proposed correction with the text a human confirmed,
// and approves the review, in one transaction: a pinned correction with its
// proposal still open would invite a second promotion, and an approved
// proposal with nothing pinned would be a decision that did nothing.
//
// text is what the person read and confirmed, which may differ from what was
// proposed; the proposal itself is never what gets pinned. proposed_by records
// who proposed it, so the prompt can say the text began with an agent.
func (s *WikiStore) PromoteReview(ctx context.Context, workspaceID, reviewID, text, promotedBy string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin promote review: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var pageID, origin string
	err = tx.QueryRow(ctx, `
		UPDATE review_items
		SET status = 'approved', resolved_by = $3, resolved_at = now()
		WHERE id = $2 AND workspace_id = $1 AND status = 'open'
		  AND kind = 'correction' AND page_id IS NOT NULL
		RETURNING page_id, origin`,
		workspaceID, reviewID, nullable(promotedBy)).Scan(&pageID, &origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: approve proposal: %w", err)
	}

	// A proposal whose page carries no filer still began outside the member
	// who is promoting it, so the provenance label must not come out empty.
	if origin == "" {
		origin = "unknown"
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO page_corrections (page_id, body, created_by, proposed_by)
		SELECT p.id, $2, $3, $4
		FROM pages p
		WHERE p.id = $1 AND p.deleted_at IS NULL
		RETURNING id`,
		pageID, text, nullable(promotedBy), origin).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPageGone
	}
	if err != nil {
		return "", fmt.Errorf("store: pin promoted correction: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit promote review: %w", err)
	}
	return id, nil
}
