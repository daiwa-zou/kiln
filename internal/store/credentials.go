package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Credentials are write-only through the API: sealed ciphertext goes in,
// metadata comes out, and the plaintext is reconstructed in exactly one place
// -- the worker, at sync time. No store method returns plaintext, because none
// receives the keyring.

// CredentialMeta is what listings expose: enough to pick a credential for a
// connector, nothing an attacker could use.
type CredentialMeta struct {
	ID        string
	Kind      string
	CreatedAt time.Time
}

// SealedCredential is a stored secret as the worker consumes it.
type SealedCredential struct {
	ID         string
	OrgID      string
	Kind       string
	Ciphertext []byte
	Nonce      []byte
}

// CreateCredential stores a sealed secret for an org.
func (s *WikiStore) CreateCredential(ctx context.Context, orgID, kind string, ciphertext, nonce []byte) (string, error) {
	var id string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO credentials (org_id, kind, ciphertext, nonce)
		VALUES ($1, $2, $3, $4)
		RETURNING id`, orgID, kind, ciphertext, nonce).Scan(&id); err != nil {
		return "", fmt.Errorf("store: create credential: %w", err)
	}
	return id, nil
}

// ListCredentialMeta lists an org's credentials without their secrets.
func (s *WikiStore) ListCredentialMeta(ctx context.Context, orgID string) ([]CredentialMeta, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, created_at FROM credentials
		WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	defer rows.Close()

	out := []CredentialMeta{}
	for rows.Next() {
		var c CredentialMeta
		if err := rows.Scan(&c.ID, &c.Kind, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan credential: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCredential removes a credential. Connectors referencing it fall back
// to unauthenticated syncs (credential_id is ON DELETE SET NULL), which fail
// visibly on private remotes rather than silently using a stale secret.
func (s *WikiStore) DeleteCredential(ctx context.Context, orgID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM credentials WHERE org_id = $1 AND id = $2`, orgID, id)
	if err != nil {
		return fmt.Errorf("store: delete credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LoadSealedCredential returns the ciphertext for the worker to open at sync
// time. Callers outside the worker have no business here.
func (s *WikiStore) LoadSealedCredential(ctx context.Context, id string) (*SealedCredential, error) {
	var c SealedCredential
	err := s.pool.QueryRow(ctx, `
		SELECT id, org_id, kind, ciphertext, nonce
		FROM credentials WHERE id = $1`, id).
		Scan(&c.ID, &c.OrgID, &c.Kind, &c.Ciphertext, &c.Nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: load credential: %w", err)
	}
	return &c, nil
}

// CredentialInOrg reports whether a credential belongs to an org. The schema
// does not force a connector's credential into the connector's own org, so
// the API must: without this check, any admin-writable connector row could
// point at another tenant's secret and have the worker decrypt it.
func (s *WikiStore) CredentialInOrg(ctx context.Context, orgID, id string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM credentials WHERE id = $1 AND org_id = $2)`,
		id, orgID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: credential in org: %w", err)
	}
	return ok, nil
}

// ResealCredentials rewrites every credential through the supplied functions
// — open under the old master key, seal under the new — in one transaction,
// so rotation is atomic: either every credential moves to the new key or
// none does. This is the only sanctioned path to key rotation; there is
// deliberately no plaintext export.
func (s *WikiStore) ResealCredentials(ctx context.Context,
	open func(ciphertext, nonce []byte) ([]byte, error),
	seal func(plaintext []byte) (ciphertext, nonce []byte, err error),
) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin reseal: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	rows, err := tx.Query(ctx, `SELECT id, ciphertext, nonce FROM credentials FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("store: list credentials for reseal: %w", err)
	}
	type sealed struct {
		id        string
		ct, nonce []byte
	}
	var all []sealed
	for rows.Next() {
		var c sealed
		if err := rows.Scan(&c.id, &c.ct, &c.nonce); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan credential: %w", err)
		}
		all = append(all, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, c := range all {
		plaintext, err := open(c.ct, c.nonce)
		if err != nil {
			return 0, fmt.Errorf("store: credential %s does not open under the old key: %w", c.id, err)
		}
		ct, nonce, err := seal(plaintext)
		if err != nil {
			return 0, fmt.Errorf("store: reseal credential %s: %w", c.id, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE credentials SET ciphertext = $2, nonce = $3, updated_at = now()
			WHERE id = $1`, c.id, ct, nonce); err != nil {
			return 0, fmt.Errorf("store: update credential %s: %w", c.id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(all), nil
}

// OrgOfWorkspace maps a workspace to its org, which is where credentials live.
func (s *WikiStore) OrgOfWorkspace(ctx context.Context, workspaceID string) (string, error) {
	var orgID string
	err := s.pool.QueryRow(ctx,
		`SELECT org_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: org of workspace: %w", err)
	}
	return orgID, nil
}
