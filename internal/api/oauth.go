package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/github"
	"github.com/daiwa-zou/kiln/internal/store"
)

// GitHub sign-in. The flow is the standard OAuth dance with a state cookie:
// /auth/github/login mints a random state, drops it in a short-lived HttpOnly
// cookie, and redirects to GitHub; the callback requires the returned state
// to equal the cookie, exchanges the code, upserts the user, mirrors their
// App installations, and issues the session + CSRF cookie pair.

// UserStore is what sign-in needs from persistence.
type UserStore interface {
	UpsertGitHubUser(ctx context.Context, u *github.User) (userID string, admin bool, err error)
	SyncUserInstallations(ctx context.Context, userID string, installs []github.Installation) error
	LoadUserProfile(ctx context.Context, userID string) (*store.UserProfile, error)
}

const (
	stateCookie = "kiln_oauth_state"
	stateTTL    = 10 * time.Minute
)

// mountAuthRoutes wires the sign-in endpoints. They sit outside the API auth
// wrap by nature: their whole job is to create the credentials the wrap
// checks. Callers guarantee GitHub, Users, and SessionPool are non-nil.
func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		s.fail(w, err)
		return
	}
	state := hex.EncodeToString(buf[:])

	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/auth",
		MaxAge: int(stateTTL.Seconds()), HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.GitHub.AuthorizeURL(state), http.StatusFound)
}

func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	// The state proves this callback belongs to a login this browser
	// started; without it, an attacker could complete the dance with their
	// own code and log the victim into the attacker's account.
	c, err := r.Cookie(stateCookie)
	returned := r.URL.Query().Get("state")
	if err != nil || returned == "" ||
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(returned)) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state mismatch; restart sign-in"})
		return
	}
	// One-shot: a replayed callback must not mint a second session.
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/auth", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing code"})
		return
	}

	ctx := r.Context()
	accessToken, err := s.GitHub.ExchangeCode(ctx, code)
	if err != nil {
		s.failAuth(w, err)
		return
	}
	ghUser, err := s.GitHub.FetchUser(ctx, accessToken)
	if err != nil {
		s.failAuth(w, err)
		return
	}

	userID, _, err := s.Users.UpsertGitHubUser(ctx, ghUser)
	if err != nil {
		s.fail(w, err)
		return
	}

	// Installation sync is best-effort: a hiccup here must not block
	// sign-in, and the mirror refreshes on every login anyway.
	if installs, err := s.GitHub.FetchUserInstallations(ctx, accessToken); err == nil {
		if serr := s.Users.SyncUserInstallations(ctx, userID, installs); serr != nil && s.Log != nil {
			s.Log.Error("installation sync failed", "err", serr)
		}
	} else if s.Log != nil {
		s.Log.Warn("listing user installations failed", "err", err)
	}

	session, csrf, err := auth.CreateSession(ctx, s.SessionPool, userID, s.SessionTTL)
	if err != nil {
		s.fail(w, err)
		return
	}
	auth.SetSessionCookies(w, r, session, csrf, s.SessionTTL)
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout revokes the session. Mounted outside the auth wrap (a client
// with a half-broken session must still be able to sign out), so it checks
// the CSRF pair itself before acting.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil && c.Value != "" {
		csrfCookie, cerr := r.Cookie(auth.CSRFCookie)
		if cerr != nil || csrfCookie.Value == "" ||
			r.Header.Get(auth.CSRFHeader) != csrfCookie.Value {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or mismatched CSRF token"})
			return
		}
		if err := auth.DeleteSession(r.Context(), s.SessionPool, c.Value); err != nil {
			s.fail(w, err)
			return
		}
	}
	auth.ClearSessionCookies(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// handleMe tells the UI who is calling. Mounted inside the auth wrap.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		// Auth disabled: there is no user, and pretending otherwise would
		// make the UI render a phantom account.
		writeJSON(w, http.StatusOK, map[string]any{"anonymous": true, "admin": true})
		return
	}
	profile, err := s.Users.LoadUserProfile(r.Context(), id.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": profile.ID, "login": profile.Login, "name": profile.Name,
		"avatarUrl": profile.AvatarURL, "admin": profile.Admin,
	})
}

// failAuth logs the detail and answers 502: the failure happened upstream at
// GitHub, and the caller can retry sign-in.
func (s *Server) failAuth(w http.ResponseWriter, err error) {
	if s.Log != nil {
		s.Log.Error("github sign-in failed", "err", err)
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub sign-in failed; try again"})
}
