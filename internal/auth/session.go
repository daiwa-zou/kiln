package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Browser sessions. Server-side rows rather than signed cookies so they can
// be revoked; the cookie carries a random token whose hash is the row's
// primary key, mirroring how bearer tokens are stored.

// Cookie names. The session cookie is HttpOnly; the CSRF cookie is
// deliberately not, because the double-submit pattern needs the page's own
// JavaScript to read it back into a header — something a cross-site attacker
// cannot do.
const (
	SessionCookie = "kiln_session"
	CSRFCookie    = "kiln_csrf"
	// CSRFHeader must equal the CSRF cookie on any cookie-authenticated
	// mutation.
	CSRFHeader = "X-CSRF-Token"
)

// SessionSource resolves a session cookie's hash to an identity plus the
// session's stored CSRF token, so the middleware validates both in one
// lookup. Nil on the middleware disables cookie auth.
type SessionSource interface {
	IdentityForSession(ctx context.Context, sessionHash string) (Identity, string, error)
}

// IdentityForSession implements SessionSource against the sessions table.
// A browser session acts with the user's full authority: scopes exist to
// narrow automation tokens, not humans, whose power is bounded by role
// checks downstream.
func (s *PGSource) IdentityForSession(ctx context.Context, sessionHash string) (Identity, string, error) {
	var (
		id   Identity
		csrf string
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT s.user_id, u.is_admin, coalesce(s.data->>'csrf', '')
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = $1 AND s.expires_at > now()`,
		sessionHash).Scan(&id.UserID, &id.Admin, &csrf)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, "", ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, "", fmt.Errorf("auth: look up session: %w", err)
	}
	// A browser session acts with the user's full authority, including the
	// admin surfaces their role permits: scopes narrow automation tokens,
	// and a human in a browser is not one.
	id.Scopes = []string{"read", "write", "admin"}
	return id, csrf, nil
}

// CreateSession stores a new session and returns the cookie values: the
// session token (returned exactly once, only its hash is stored) and the
// CSRF token that must accompany mutations.
func CreateSession(ctx context.Context, pool *pgxpool.Pool, userID string, ttl time.Duration) (session, csrf string, err error) {
	plain, hash, err := NewToken()
	if err != nil {
		return "", "", err
	}
	// The CSRF token is random per session and stored alongside it, so
	// rotation is automatic and validation needs no extra secret.
	csrfPlain, _, err := NewToken()
	if err != nil {
		return "", "", err
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, data, expires_at)
		VALUES ($1, $2, jsonb_build_object('csrf', $3::text), now() + $4::interval)`,
		hash, userID, csrfPlain, fmt.Sprintf("%d seconds", int(ttl.Seconds()))); err != nil {
		return "", "", fmt.Errorf("auth: create session: %w", err)
	}
	return plain, csrfPlain, nil
}

// DeleteSession revokes one session by its cookie value.
func DeleteSession(ctx context.Context, pool *pgxpool.Pool, sessionPlain string) error {
	if _, err := pool.Exec(ctx,
		`DELETE FROM sessions WHERE id = $1`, HashToken(sessionPlain)); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// SetSessionCookies writes the pair of cookies a fresh session needs.
// Secure is set from the request's TLS state so localhost development works;
// SameSite=Lax is the baseline CSRF posture, with the double-submit header
// as the enforced layer on top.
func SetSessionCookies(w http.ResponseWriter, r *http.Request, session, csrf string, ttl time.Duration) {
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: session, Path: "/",
		MaxAge: int(ttl.Seconds()), HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: CSRFCookie, Value: csrf, Path: "/",
		MaxAge: int(ttl.Seconds()), HttpOnly: false, Secure: secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookies expires both cookies.
func ClearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{SessionCookie, CSRFCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: name == SessionCookie, SameSite: http.SameSiteLaxMode,
		})
	}
}
