package store

import (
	"context"
	"fmt"

	"github.com/daiwa-zou/kiln/internal/github"
)

// UpsertGitHubUser records a signed-in GitHub user, keyed on their immutable
// GitHub id so login renames follow along. The very first user a fresh
// deployment sees becomes the instance admin: someone has to be able to
// configure connectors, and the person who just installed kiln is the only
// candidate. Every later user starts unprivileged.
func (s *WikiStore) UpsertGitHubUser(ctx context.Context, u *github.User) (userID string, admin bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("store: begin user upsert: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var bootstrap bool
	if err := tx.QueryRow(ctx,
		`SELECT NOT EXISTS (SELECT 1 FROM users)`).Scan(&bootstrap); err != nil {
		return "", false, fmt.Errorf("store: check first user: %w", err)
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO users (github_user_id, login, email, name, avatar_url, is_admin)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (github_user_id) DO UPDATE SET
			login = EXCLUDED.login,
			email = EXCLUDED.email,
			name = EXCLUDED.name,
			avatar_url = EXCLUDED.avatar_url,
			updated_at = now()
		RETURNING id, is_admin`,
		u.ID, u.Login, nullable(u.Email), nullable(u.Name), nullable(u.AvatarURL), bootstrap,
	).Scan(&userID, &admin); err != nil {
		return "", false, fmt.Errorf("store: upsert github user: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return userID, admin, nil
}

// SyncUserInstallations mirrors the App installations a user can reach.
// GitHub is the source of ACL truth for git-backed workspaces; this mirror is
// what later maps a webhook's installation to the workspaces it may build.
// The set is replaced wholesale: a revoked installation must disappear here
// the next time the user signs in.
func (s *WikiStore) SyncUserInstallations(ctx context.Context, userID string, installs []github.Installation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin installation sync: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	if _, err := tx.Exec(ctx,
		`DELETE FROM user_installations WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("store: clear user installations: %w", err)
	}

	for _, inst := range installs {
		var ghRowID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO github_installations (installation_id, account_login, account_type, suspended_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (installation_id) DO UPDATE SET
				account_login = EXCLUDED.account_login,
				account_type = EXCLUDED.account_type,
				suspended_at = EXCLUDED.suspended_at,
				updated_at = now()
			RETURNING id`,
			inst.ID, inst.Account.Login, inst.Account.Type, inst.SuspendedAt,
		).Scan(&ghRowID); err != nil {
			return fmt.Errorf("store: upsert installation %d: %w", inst.ID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_installations (user_id, installation_id, synced_at)
			VALUES ($1, $2, now())
			ON CONFLICT (user_id, installation_id) DO UPDATE SET synced_at = now()`,
			userID, ghRowID); err != nil {
			return fmt.Errorf("store: link installation %d: %w", inst.ID, err)
		}
	}

	return tx.Commit(ctx)
}

// UserHasInstallation reports whether the user can reach a GitHub App
// installation, going by the mirror SyncUserInstallations keeps. This is the
// ownership check behind connector configs: an installation id is a small
// integer anyone can guess, and naming one in a connector makes the worker
// mint a token for it, so the id is only honored for someone GitHub itself
// says can use it.
func (s *WikiStore) UserHasInstallation(ctx context.Context, userID string, installationID int64) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM user_installations ui
			JOIN github_installations gi ON gi.id = ui.installation_id
			WHERE ui.user_id = $1 AND gi.installation_id = $2
		)`, userID, installationID).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: check user installation: %w", err)
	}
	return ok, nil
}

// InstallationServesWorkspace reports whether an installation is linked to a
// user who currently stands behind the workspace: a member of its org, or an
// instance admin (who bypass org membership everywhere else). It is the
// run-time twin of UserHasInstallation: the API checks who is writing the
// connector, this checks that someone who still belongs there vouches for the
// installation when the clone actually happens. Removing the member who
// configured it, or revoking their access on GitHub and having them sign in
// again, therefore stops the connector minting tokens.
func (s *WikiStore) InstallationServesWorkspace(ctx context.Context, workspaceID string, installationID int64) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM github_installations gi
			JOIN user_installations ui ON ui.installation_id = gi.id
			JOIN users u ON u.id = ui.user_id
			WHERE gi.installation_id = $2
			  AND (u.is_admin OR EXISTS (
			        SELECT 1
			        FROM org_members m
			        JOIN workspaces w ON w.org_id = m.org_id
			        WHERE w.id = $1 AND m.user_id = u.id))
		)`, workspaceID, installationID).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: check workspace installation: %w", err)
	}
	return ok, nil
}

// UserProfile is what /api/v1/me returns: enough for the UI to show who is
// signed in.
type UserProfile struct {
	ID        string
	Login     string
	Name      string
	AvatarURL string
	Admin     bool
}

// LoadUserProfile returns one user's public fields.
func (s *WikiStore) LoadUserProfile(ctx context.Context, userID string) (*UserProfile, error) {
	var p UserProfile
	err := s.pool.QueryRow(ctx, `
		SELECT id, login, coalesce(name, ''), coalesce(avatar_url, ''), is_admin
		FROM users WHERE id = $1`, userID).
		Scan(&p.ID, &p.Login, &p.Name, &p.AvatarURL, &p.Admin)
	if err != nil {
		return nil, fmt.Errorf("store: load user profile: %w", err)
	}
	return &p, nil
}
