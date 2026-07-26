package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectorRow is one configured source, as the worker consumes it. The config
// is API-writable data, never operator input: everything read from it must be
// validated by the caller (the permitted-roots check for local paths).
type ConnectorRow struct {
	ID           string
	WorkspaceID  string
	Kind         string
	Name         string
	Config       map[string]any
	CredentialID string
	TriggerMode  string
	Enabled      bool
	LastSyncedAt *time.Time
	LastError    string
	CreatedAt    time.Time
}

// EnabledConnectors returns a workspace's enabled connectors in creation
// order. All of them feed one build: kiln merges every connector's material
// into a single map, so code and documents produce one wiki.
func (s *WikiStore) EnabledConnectors(ctx context.Context, workspaceID string) ([]ConnectorRow, error) {
	return s.queryConnectors(ctx, `WHERE workspace_id = $1 AND enabled`, workspaceID)
}

// ListConnectors returns every connector on a workspace, enabled or not, for
// the admin CRUD surface.
func (s *WikiStore) ListConnectors(ctx context.Context, workspaceID string) ([]ConnectorRow, error) {
	return s.queryConnectors(ctx, `WHERE workspace_id = $1`, workspaceID)
}

func (s *WikiStore) queryConnectors(ctx context.Context, where string, args ...any) ([]ConnectorRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, kind, name, config, credential_id, trigger_mode,
		       enabled, last_synced_at, coalesce(last_error, ''), created_at
		FROM connectors `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: load connectors: %w", err)
	}
	defer rows.Close()

	out := []ConnectorRow{}
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConnectorByID returns one connector, for runs pinned to a specific source.
func (s *WikiStore) ConnectorByID(ctx context.Context, id string) (*ConnectorRow, error) {
	rows, err := s.queryConnectors(ctx, `WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("store: connector %s: %w", id, ErrNotFound)
	}
	return &rows[0], nil
}

// CreateConnector registers a source on a workspace.
func (s *WikiStore) CreateConnector(ctx context.Context, c ConnectorRow) (string, error) {
	cfg, err := json.Marshal(orEmptyMap(c.Config))
	if err != nil {
		return "", fmt.Errorf("store: encode connector config: %w", err)
	}
	var id string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO connectors (workspace_id, kind, name, config, credential_id, trigger_mode, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		c.WorkspaceID, c.Kind, c.Name, cfg, nullable(c.CredentialID),
		orDefault(c.TriggerMode, "manual"), c.Enabled).Scan(&id); err != nil {
		return "", fmt.Errorf("store: create connector: %w", err)
	}
	return id, nil
}

// ConnectorPatch is a partial update; nil fields are left unchanged.
type ConnectorPatch struct {
	Name         *string
	Config       *map[string]any
	CredentialID *string // empty string clears it
	TriggerMode  *string
	Enabled      *bool
}

// UpdateConnector applies a patch, scoped to the workspace so an id from
// another tenant is indistinguishable from a missing one.
func (s *WikiStore) UpdateConnector(ctx context.Context, workspaceID, id string, p ConnectorPatch) error {
	var cfg any
	if p.Config != nil {
		encoded, err := json.Marshal(orEmptyMap(*p.Config))
		if err != nil {
			return fmt.Errorf("store: encode connector config: %w", err)
		}
		cfg = encoded
	}
	var credential any
	credentialSet := p.CredentialID != nil
	if credentialSet && *p.CredentialID != "" {
		credential = *p.CredentialID
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE connectors SET
			name          = coalesce($3, name),
			config        = coalesce($4, config),
			credential_id = CASE WHEN $5 THEN $6::uuid ELSE credential_id END,
			trigger_mode  = coalesce($7, trigger_mode),
			enabled       = coalesce($8, enabled),
			updated_at    = now()
		WHERE workspace_id = $1 AND id = $2`,
		workspaceID, id, p.Name, cfg, credentialSet, credential, p.TriggerMode, p.Enabled)
	if err != nil {
		return fmt.Errorf("store: update connector: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteConnector removes a connector. Its sources cascade away in the
// schema; the pages they produced stay until the deletion review flow
// removes them, because destruction is always a human decision.
func (s *WikiStore) DeleteConnector(ctx context.Context, workspaceID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM connectors WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("store: delete connector: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkConnectorSync records the outcome of a connector's most recent sync.
func (s *WikiStore) MarkConnectorSync(ctx context.Context, id, syncErr string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE connectors
		SET last_synced_at = now(), last_error = $2, updated_at = now()
		WHERE id = $1`, id, nullable(syncErr)); err != nil {
		return fmt.Errorf("store: mark connector sync: %w", err)
	}
	return nil
}

func scanConnector(rows pgx.Rows) (ConnectorRow, error) {
	var (
		c          ConnectorRow
		credential *string
	)
	if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Kind, &c.Name, &c.Config,
		&credential, &c.TriggerMode, &c.Enabled, &c.LastSyncedAt,
		&c.LastError, &c.CreatedAt); err != nil {
		return c, fmt.Errorf("store: scan connector: %w", err)
	}
	if credential != nil {
		c.CredentialID = *credential
	}
	return c, nil
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
