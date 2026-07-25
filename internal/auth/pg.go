package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSource resolves tokens against the tokens and users tables.
type PGSource struct {
	Pool *pgxpool.Pool
}

// IdentityForToken looks up a live token by hash.
func (s *PGSource) IdentityForToken(ctx context.Context, tokenHash string) (Identity, error) {
	var (
		id      Identity
		tokenID string
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT t.id, t.user_id, u.is_admin, t.scopes
		FROM tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1
		  AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > now())`,
		tokenHash).Scan(&tokenID, &id.UserID, &id.Admin, &id.Scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, fmt.Errorf("auth: look up token: %w", err)
	}

	// last_used_at is telemetry, not authorization: best-effort, throttled so a
	// busy token is not a write per request.
	_, _ = s.Pool.Exec(ctx, `
		UPDATE tokens SET last_used_at = now()
		WHERE id = $1
		  AND (last_used_at IS NULL OR last_used_at < now() - interval '60 seconds')`,
		tokenID)

	return id, nil
}

// MintRequest describes a token to create. The user and org membership are
// ensured on the way, so bootstrapping a fresh deployment is one command.
type MintRequest struct {
	Login  string
	Org    string
	Name   string
	Scopes []string
	Admin  bool
	// TTL of zero means the token does not expire.
	TTL time.Duration
}

// Mint creates (or finds) the user, ensures org membership, and inserts a
// token, returning the plaintext exactly once.
func Mint(ctx context.Context, pool *pgxpool.Pool, req MintRequest) (string, error) {
	if req.Login == "" {
		return "", errors.New("auth: a login is required")
	}
	if req.Name == "" {
		req.Name = "default"
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{"read"}
	}

	plain, hash, err := NewToken()
	if err != nil {
		return "", err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("auth: begin mint: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// users.login has no unique constraint (GitHub id is the canonical key), so
	// CLI-minted users are matched by login deterministically.
	var userID string
	err = tx.QueryRow(ctx,
		`SELECT id FROM users WHERE login = $1 ORDER BY created_at LIMIT 1`,
		req.Login).Scan(&userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (login, is_admin) VALUES ($1, $2) RETURNING id`,
			req.Login, req.Admin).Scan(&userID); err != nil {
			return "", fmt.Errorf("auth: create user: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("auth: find user: %w", err)
	default:
		if req.Admin {
			if _, err := tx.Exec(ctx,
				`UPDATE users SET is_admin = true, updated_at = now() WHERE id = $1`,
				userID); err != nil {
				return "", fmt.Errorf("auth: promote user: %w", err)
			}
		}
	}

	if req.Org != "" {
		var orgID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO orgs (name, slug) VALUES ($1,$1)
			ON CONFLICT (slug) DO UPDATE SET name = orgs.name
			RETURNING id`, req.Org).Scan(&orgID); err != nil {
			return "", fmt.Errorf("auth: ensure org: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO org_members (org_id, user_id) VALUES ($1,$2)
			ON CONFLICT DO NOTHING`, orgID, userID); err != nil {
			return "", fmt.Errorf("auth: ensure membership: %w", err)
		}
	}

	var expires any
	if req.TTL > 0 {
		expires = time.Now().Add(req.TTL)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tokens (user_id, name, token_hash, scopes, expires_at)
		VALUES ($1,$2,$3,$4,$5)`,
		userID, req.Name, hash, req.Scopes, expires); err != nil {
		return "", fmt.Errorf("auth: insert token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("auth: commit mint: %w", err)
	}
	return plain, nil
}

// Revoke marks a user's named tokens revoked and reports how many it touched.
func Revoke(ctx context.Context, pool *pgxpool.Pool, login, name string) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE tokens SET revoked_at = now()
		WHERE revoked_at IS NULL
		  AND name = $2
		  AND user_id IN (SELECT id FROM users WHERE login = $1)`,
		login, name)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke: %w", err)
	}
	return tag.RowsAffected(), nil
}
