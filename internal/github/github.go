// Package github is kiln's minimal GitHub App client: the OAuth sign-in
// exchange, the user and installation lookups behind it, and app/installation
// tokens for clones.
//
// Hand-rolled over an SDK on purpose: kiln touches five endpoints, and the
// SDK's transitive surface is larger than this file. Base URLs are
// configurable so every path is testable against httptest servers.
package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to GitHub. Zero-value base URLs mean the public github.com.
type Client struct {
	// ClientID and ClientSecret are the GitHub App's OAuth credentials, for
	// user sign-in.
	ClientID     string
	ClientSecret string

	// AppID and PrivateKey identify the App itself, for installation tokens.
	AppID      int64
	PrivateKey []byte // PEM, RSA

	// BaseURL hosts the browser endpoints (login/oauth/*); APIBaseURL hosts
	// the REST API. Split because github.com splits them.
	BaseURL    string
	APIBaseURL string

	HTTP *http.Client
}

// User is the subset of the authenticated-user response kiln stores.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// Installation is one GitHub App installation visible to a user.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

// SignInConfigured reports whether the OAuth half is usable.
func (c *Client) SignInConfigured() bool {
	return c != nil && c.ClientID != "" && c.ClientSecret != ""
}

// AppConfigured reports whether installation tokens can be minted.
func (c *Client) AppConfigured() bool {
	return c != nil && c.AppID != 0 && len(c.PrivateKey) > 0
}

// AuthorizeURL is where the browser goes to start sign-in.
func (c *Client) AuthorizeURL(state string) string {
	q := url.Values{"client_id": {c.ClientID}, "state": {state}}
	return c.base() + "/login/oauth/authorize?" + q.Encode()
}

// ExchangeCode swaps an OAuth callback code for a user access token.
func (c *Client) ExchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"code":          {code},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := c.do(req, &out); err != nil {
		return "", fmt.Errorf("github: exchange code: %w", err)
	}
	if out.AccessToken == "" {
		// GitHub reports OAuth errors inside a 200 response.
		return "", fmt.Errorf("github: exchange refused: %s %s", out.Error, out.ErrorDesc)
	}
	return out.AccessToken, nil
}

// FetchUser returns the authenticated user for a user access token.
func (c *Client) FetchUser(ctx context.Context, accessToken string) (*User, error) {
	req, err := c.apiRequest(ctx, http.MethodGet, "/user", accessToken)
	if err != nil {
		return nil, err
	}
	var u User
	if err := c.do(req, &u); err != nil {
		return nil, fmt.Errorf("github: fetch user: %w", err)
	}
	if u.ID == 0 || u.Login == "" {
		return nil, fmt.Errorf("github: user response missing id or login")
	}
	return &u, nil
}

// FetchUserInstallations lists the App installations the user can access,
// which is what maps a signed-in human to the repositories kiln may read.
func (c *Client) FetchUserInstallations(ctx context.Context, accessToken string) ([]Installation, error) {
	req, err := c.apiRequest(ctx, http.MethodGet, "/user/installations", accessToken)
	if err != nil {
		return nil, err
	}
	var out struct {
		Installations []Installation `json:"installations"`
	}
	if err := c.do(req, &out); err != nil {
		return nil, fmt.Errorf("github: fetch installations: %w", err)
	}
	return out.Installations, nil
}

// InstallationToken mints a short-lived token for one installation. These
// supersede stored PATs for GitHub clones: they expire in an hour and are
// scoped to the installation's repositories, so there is nothing durable to
// leak.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, time.Time, error) {
	jwt, err := c.AppJWT(time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := c.apiRequest(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", installationID), "")
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.do(req, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("github: installation token: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("github: installation token response empty")
	}
	return out.Token, out.ExpiresAt, nil
}

// AppJWT builds the App's RS256 JWT: 10 minutes of validity, issued 60
// seconds in the past to absorb clock skew, exactly as GitHub documents.
func (c *Client) AppJWT(now time.Time) (string, error) {
	key, err := parsePrivateKey(c.PrivateKey)
	if err != nil {
		return "", err
	}

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": c.AppID,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)

	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: sign app jwt: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("github: private key is not PEM")
	}
	// GitHub ships PKCS#1; tolerate PKCS#8 for keys that were re-wrapped.
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github: private key is %T, want RSA", parsed)
	}
	return key, nil
}

func (c *Client) apiRequest(ctx context.Context, method, path, accessToken string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	return req, nil
}

func (c *Client) do(req *http.Request, into any) error {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, res.StatusCode, firstLine(string(body)))
	}
	return json.Unmarshal(body, into)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return "https://github.com"
}

func (c *Client) apiBase() string {
	if c.APIBaseURL != "" {
		return strings.TrimSuffix(c.APIBaseURL, "/")
	}
	return "https://api.github.com"
}
