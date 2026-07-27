package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WebhookConnectors returns every enabled connector that syncs on webhook
// events. The set is small (one row per watched repository), so repo matching
// happens in Go where URL parsing is honest, instead of in SQL patterns.
func (s *WikiStore) WebhookConnectors(ctx context.Context) ([]ConnectorRow, error) {
	return s.queryConnectors(ctx, `WHERE enabled AND trigger_mode = 'webhook'`)
}

// PollDueConnectors returns enabled poll connectors whose last sync is older
// than the interval (or that have never synced). The worker turns each into
// an enqueue; the active-run index keeps duplicates impossible.
func (s *WikiStore) PollDueConnectors(ctx context.Context, olderThan time.Duration) ([]ConnectorRow, error) {
	return s.queryConnectors(ctx, `
		WHERE enabled AND trigger_mode = 'poll'
		  AND (last_synced_at IS NULL OR last_synced_at < now() - make_interval(secs => $1))`,
		olderThan.Seconds())
}

// LastRunFinishedAt returns when the workspace's most recent run finished,
// or the zero time when none has. This is the webhook completion cooldown's
// input.
func (s *WikiStore) LastRunFinishedAt(ctx context.Context, workspaceID string) (time.Time, error) {
	var finished *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(finished_at) FROM runs WHERE workspace_id = $1`, workspaceID).Scan(&finished)
	if errors.Is(err, pgx.ErrNoRows) || finished == nil {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: last run finished: %w", err)
	}
	return *finished, nil
}

// UpsertGitHubInstallationEvent mirrors an installation lifecycle event.
func (s *WikiStore) UpsertGitHubInstallationEvent(ctx context.Context, installationID int64, accountLogin, accountType string, suspended bool) error {
	var suspendedAt any
	if suspended {
		suspendedAt = time.Now()
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO github_installations (installation_id, account_login, account_type, suspended_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (installation_id) DO UPDATE SET
			account_login = EXCLUDED.account_login,
			account_type = EXCLUDED.account_type,
			suspended_at = EXCLUDED.suspended_at,
			updated_at = now()`,
		installationID, accountLogin, accountType, suspendedAt); err != nil {
		return fmt.Errorf("store: upsert installation event: %w", err)
	}
	return nil
}

// RemoveGitHubInstallation drops the mirror row for an uninstalled App.
// User links cascade away; workspaces and their pages stay, because content
// destruction is always a human decision.
func (s *WikiStore) RemoveGitHubInstallation(ctx context.Context, installationID int64) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM github_installations WHERE installation_id = $1`, installationID); err != nil {
		return fmt.Errorf("store: remove installation: %w", err)
	}
	return nil
}
