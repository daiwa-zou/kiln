package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// TokenInfo is one API token as its owner may see it. The token itself is
// absent by construction: only a hash is stored, and the plaintext exists for
// exactly one response, at mint time.
type TokenInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"-"`
	LastUsed  *time.Time
	ExpiresAt *time.Time
}

// MintForUser issues a token for an existing user and returns the plaintext
// once.
//
// Deliberately narrower than Mint, which the CLI uses: that one creates users
// and org memberships and can grant admin, because an operator at a shell is
// bootstrapping a deployment. This is a signed-in person asking for a key for
// their own agent, so it attaches to the caller's own user and grants only the
// scopes it is handed -- there is no way to name a different user, an org, or
// admin through it.
func MintForUser(ctx context.Context, pool *pgxpool.Pool, userID, name string, scopes []string, ttl time.Duration) (string, TokenInfo, error) {
	if userID == "" {
		return "", TokenInfo{}, errors.New("auth: a user is required to mint a token")
	}
	if name == "" {
		name = "agent"
	}
	if len(scopes) == 0 {
		scopes = []string{"read"}
	}

	plain, hash, err := NewToken()
	if err != nil {
		return "", TokenInfo{}, err
	}
	var expires any
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}

	var info TokenInfo
	if err := pool.QueryRow(ctx, `
		INSERT INTO tokens (user_id, name, token_hash, scopes, expires_at)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, name, scopes, created_at, expires_at`,
		userID, name, hash, scopes, expires,
	).Scan(&info.ID, &info.Name, &info.Scopes, &info.CreatedAt, &info.ExpiresAt); err != nil {
		return "", TokenInfo{}, fmt.Errorf("auth: mint token for user: %w", err)
	}
	return plain, info, nil
}

// ListForUser returns a user's live tokens, newest first. Revoked and expired
// ones are omitted: the list answers "what can reach my benches right now",
// and a key that cannot is noise in that answer.
func ListForUser(ctx context.Context, pool *pgxpool.Pool, userID string) ([]TokenInfo, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, scopes, created_at, last_used_at, expires_at
		FROM tokens
		WHERE user_id = $1
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list tokens: %w", err)
	}
	defer rows.Close()

	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.ID, &t.Name, &t.Scopes, &t.CreatedAt, &t.LastUsed, &t.ExpiresAt); err != nil {
			return nil, fmt.Errorf("auth: scan token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ErrNoSuchToken is returned when a revoke matches nothing the caller owns.
var ErrNoSuchToken = errors.New("auth: no such token")

// RevokeID revokes one of a user's own tokens.
//
// Ownership is part of the statement rather than a check beforehand: a token
// id belonging to someone else must be indistinguishable from one that never
// existed, or the endpoint answers "that key is not yours" and becomes a way
// to confirm ids.
func RevokeID(ctx context.Context, pool *pgxpool.Pool, userID, tokenID string) error {
	tag, err := pool.Exec(ctx, `
		UPDATE tokens SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, tokenID, userID)
	if err != nil {
		// A malformed uuid can only come from an id the caller invented, so it
		// is the same answer as one that does not exist.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return ErrNoSuchToken
		}
		return fmt.Errorf("auth: revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchToken
	}
	return nil
}
