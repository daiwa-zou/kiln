package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Org membership management. Roles are viewer < member < owner: viewers
// read, members write content (steering, corrections, reviews, runs), owners
// additionally shape the org itself — connectors, credentials, membership.
// Instance admins bypass all of it, as everywhere else.

// MemberRoles are the assignable roles.
var MemberRoles = []string{"viewer", "member", "owner"}

// MemberRow is one org member as the management surface sees it.
type MemberRow struct {
	UserID    string
	Login     string
	Name      string
	AvatarURL string
	Role      string
	CreatedAt time.Time
}

// ListOrgMembers returns the members of a workspace's org.
func (s *WikiStore) ListOrgMembers(ctx context.Context, workspaceID string) ([]MemberRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.login, coalesce(u.name, ''), coalesce(u.avatar_url, ''),
		       m.role, m.created_at
		FROM org_members m
		JOIN users u ON u.id = m.user_id
		JOIN workspaces w ON w.org_id = m.org_id
		WHERE w.id = $1
		ORDER BY m.created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: list members: %w", err)
	}
	defer rows.Close()

	out := []MemberRow{}
	for rows.Next() {
		var m MemberRow
		if err := rows.Scan(&m.UserID, &m.Login, &m.Name, &m.AvatarURL, &m.Role, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddOrgMember joins an existing user (by login) to the workspace's org.
// The user must already exist — membership grants access, it does not mint
// accounts; people arrive via GitHub sign-in or a CLI-minted token.
func (s *WikiStore) AddOrgMember(ctx context.Context, workspaceID, login, role string) (*MemberRow, error) {
	var m MemberRow
	err := s.pool.QueryRow(ctx, `
		WITH target AS (
		    SELECT id, login, coalesce(name,'') AS name, coalesce(avatar_url,'') AS avatar
		    FROM users WHERE login = $2 ORDER BY created_at LIMIT 1
		), inserted AS (
		    INSERT INTO org_members (org_id, user_id, role)
		    SELECT w.org_id, target.id, $3 FROM workspaces w, target WHERE w.id = $1
		    ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role
		    RETURNING user_id, role, created_at
		)
		SELECT t.id, t.login, t.name, t.avatar, i.role, i.created_at
		FROM inserted i JOIN target t ON t.id = i.user_id`,
		workspaceID, login, role).
		Scan(&m.UserID, &m.Login, &m.Name, &m.AvatarURL, &m.Role, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: user %q: %w", login, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: add member: %w", err)
	}
	return &m, nil
}

// SetMemberRole changes one member's role.
func (s *WikiStore) SetMemberRole(ctx context.Context, workspaceID, userID, role string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE org_members m SET role = $3
		FROM workspaces w
		WHERE w.id = $1 AND m.org_id = w.org_id AND m.user_id = $2`,
		workspaceID, userID, role)
	if err != nil {
		return fmt.Errorf("store: set member role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RemoveOrgMember drops a membership. The user account survives — they may
// belong to other orgs — and their authored corrections keep their name.
func (s *WikiStore) RemoveOrgMember(ctx context.Context, workspaceID, userID string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM org_members m
		USING workspaces w
		WHERE w.id = $1 AND m.org_id = w.org_id AND m.user_id = $2`,
		workspaceID, userID)
	if err != nil {
		return fmt.Errorf("store: remove member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
