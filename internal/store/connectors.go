package store

import (
	"context"
	"fmt"

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
}

// EnabledConnectors returns a workspace's enabled connectors in creation
// order. All of them feed one build: kiln merges every connector's material
// into a single map, so code and documents produce one wiki.
func (s *WikiStore) EnabledConnectors(ctx context.Context, workspaceID string) ([]ConnectorRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, kind, name, config, credential_id, trigger_mode
		FROM connectors
		WHERE workspace_id = $1 AND enabled
		ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: load connectors: %w", err)
	}
	defer rows.Close()

	var out []ConnectorRow
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
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, kind, name, config, credential_id, trigger_mode
		FROM connectors WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("store: load connector: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: load connector: %w", err)
		}
		return nil, fmt.Errorf("store: connector %s: %w", id, ErrNotFound)
	}
	c, err := scanConnector(rows)
	if err != nil {
		return nil, err
	}
	return &c, rows.Err()
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
		&credential, &c.TriggerMode); err != nil {
		return c, fmt.Errorf("store: scan connector: %w", err)
	}
	if credential != nil {
		c.CredentialID = *credential
	}
	return c, nil
}
