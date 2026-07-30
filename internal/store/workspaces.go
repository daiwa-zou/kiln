package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrOrgForbidden means the caller may not create workspaces in an org that
// already exists and does not belong to them. Distinct from ErrNotFound so the
// API can answer 404 either way without the handler having to decide: a user
// learning "this org exists but is not yours" is a tenant-enumeration oracle.
var ErrOrgForbidden = errors.New("store: org belongs to someone else")

// ErrWorkspaceExists means the slug is taken inside the org.
var ErrWorkspaceExists = errors.New("store: workspace already exists")

// CreateWorkspace makes an org (if needed), a workspace, and its wiki, and
// binds the creator to the org as an owner.
//
// The membership is the point. EnsureWorkspace -- the path `kiln build` and
// `admin token create` use -- creates the org/workspace chain with no members
// at all, which is why a user who signs in through GitHub has historically
// landed on an empty list with nothing to click: every workspace was invisible
// to them, and they had no way to make one.
//
// Authorization happens inside the transaction rather than in the handler.
// Creating a workspace in an org that already exists requires owning that org;
// otherwise any authenticated user could plant a workspace in another tenant's
// org and, being its creator, manage it.
func (s *WikiStore) CreateWorkspace(ctx context.Context, orgSlug, wsSlug, name, userID string, instanceAdmin bool) (workspaceID string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin create workspace: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// Serialize creators racing on the same new org slug: without it, two
	// requests both see "no org" and one loses on the unique index with a
	// constraint error rather than joining the org that just appeared.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('kiln:org:' || $1))`, orgSlug); err != nil {
		return "", fmt.Errorf("store: lock org slug: %w", err)
	}

	var orgID string
	err = tx.QueryRow(ctx, `SELECT id FROM orgs WHERE slug = $1`, orgSlug).Scan(&orgID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// New org: the creator owns it.
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (name, slug) VALUES ($1, $1) RETURNING id`, orgSlug).Scan(&orgID); err != nil {
			return "", fmt.Errorf("store: create org: %w", err)
		}
		if userID != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')
				ON CONFLICT (org_id, user_id) DO UPDATE SET role = 'owner'`,
				orgID, userID); err != nil {
				return "", fmt.Errorf("store: grant org ownership: %w", err)
			}
		}
	case err != nil:
		return "", fmt.Errorf("store: look up org: %w", err)
	default:
		// Existing org: only an owner (or an instance admin) may add to it.
		if !instanceAdmin {
			var role string
			err := tx.QueryRow(ctx,
				`SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`,
				orgID, userID).Scan(&role)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && role != "owner") {
				return "", ErrOrgForbidden
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return "", fmt.Errorf("store: check org membership: %w", err)
			}
		}
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO workspaces (org_id, name, slug) VALUES ($1, $2, $3)
		ON CONFLICT (org_id, slug) DO NOTHING
		RETURNING id`, orgID, name, wsSlug).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrWorkspaceExists
	}
	if err != nil {
		return "", fmt.Errorf("store: create workspace: %w", err)
	}

	// The wiki row is created up front so the reader has something to resolve
	// against before the first build, rather than 404ing on a bench that
	// visibly exists.
	if _, err := tx.Exec(ctx,
		`INSERT INTO wikis (workspace_id) VALUES ($1) ON CONFLICT (workspace_id) DO NOTHING`,
		workspaceID); err != nil {
		return "", fmt.Errorf("store: create wiki: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit create workspace: %w", err)
	}
	return workspaceID, nil
}
