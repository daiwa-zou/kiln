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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) ([]byte, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return pemBytes, &key.PublicKey
}

func TestAppJWTSignsVerifiably(t *testing.T) {
	pemBytes, pub := testKeyPEM(t)
	c := &Client{AppID: 4242, PrivateKey: pemBytes}

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	jwt, err := c.AppJWT(now)
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	// The signature must verify against the corresponding public key.
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	var claims struct {
		Iat, Exp int64
		Iss      int64
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != 4242 {
		t.Errorf("iss = %d", claims.Iss)
	}
	// Backdated a minute for clock skew, valid ten minutes, per GitHub docs.
	if got := now.Unix() - claims.Iat; got != 60 {
		t.Errorf("iat backdated by %ds, want 60", got)
	}
	if got := claims.Exp - now.Unix(); got != 600 {
		t.Errorf("exp %ds ahead, want 600", got)
	}
}

func TestAppJWTAcceptsPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	c := &Client{AppID: 1, PrivateKey: pemBytes}
	if _, err := c.AppJWT(time.Now()); err != nil {
		t.Fatalf("pkcs8 key rejected: %v", err)
	}
}

// fakeGitHub is an httptest server covering every endpoint the client uses.
func fakeGitHub(t *testing.T) (*httptest.Server, *Client) {
	t.Helper()
	pemBytes, _ := testKeyPEM(t)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("client_id") != "cid" || r.FormValue("client_secret") != "shhh" {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("code") == "good-code" {
			_, _ = w.Write([]byte(`{"access_token":"gho_user_token"}`))
			return
		}
		// GitHub reports OAuth errors inside a 200.
		_, _ = w.Write([]byte(`{"error":"bad_verification_code","error_description":"expired"}`))
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_user_token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":77,"login":"daiwa","email":"d@example.com","name":"Daiwa","avatar_url":"https://a.example/x.png"}`))
	})
	mux.HandleFunc("GET /user/installations", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"installations":[
			{"id":901,"account":{"login":"daiwa-zou","type":"User"}},
			{"id":902,"account":{"login":"acme","type":"Organization"},"suspended_at":"2026-07-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("POST /app/installations/901/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") || strings.Count(authz, ".") != 2 {
			http.Error(w, "no app jwt", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_installation_token","expires_at":"2026-07-27T13:00:00Z"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv, &Client{
		ClientID: "cid", ClientSecret: "shhh",
		AppID: 4242, PrivateKey: pemBytes,
		BaseURL: srv.URL, APIBaseURL: srv.URL,
	}
}

func TestOAuthExchangeAndFetches(t *testing.T) {
	_, c := fakeGitHub(t)
	ctx := context.Background()

	token, err := c.ExchangeCode(ctx, "good-code")
	if err != nil || token != "gho_user_token" {
		t.Fatalf("exchange: %q %v", token, err)
	}
	// An OAuth error inside a 200 must fail, not return an empty token.
	if _, err := c.ExchangeCode(ctx, "stale-code"); err == nil ||
		!strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("stale code: %v", err)
	}

	u, err := c.FetchUser(ctx, token)
	if err != nil || u.ID != 77 || u.Login != "daiwa" {
		t.Fatalf("user: %+v %v", u, err)
	}

	installs, err := c.FetchUserInstallations(ctx, token)
	if err != nil || len(installs) != 2 {
		t.Fatalf("installations: %+v %v", installs, err)
	}
	if installs[0].ID != 901 || installs[0].Account.Login != "daiwa-zou" {
		t.Errorf("install[0]: %+v", installs[0])
	}
	if installs[1].SuspendedAt == nil {
		t.Error("suspension not decoded")
	}
}

func TestInstallationToken(t *testing.T) {
	_, c := fakeGitHub(t)

	token, expires, err := c.InstallationToken(context.Background(), 901)
	if err != nil || token != "ghs_installation_token" {
		t.Fatalf("installation token: %q %v", token, err)
	}
	if expires.IsZero() {
		t.Error("expiry not decoded")
	}
}

func TestConfiguredPredicates(t *testing.T) {
	var nilClient *Client
	if nilClient.SignInConfigured() || nilClient.AppConfigured() {
		t.Error("nil client claims to be configured")
	}
	if (&Client{ClientID: "x"}).SignInConfigured() {
		t.Error("client id alone is not sign-in configured")
	}
	if !(&Client{ClientID: "x", ClientSecret: "y"}).SignInConfigured() {
		t.Error("full oauth pair not recognized")
	}
	if !(&Client{AppID: 1, PrivateKey: []byte("k")}).AppConfigured() {
		t.Error("app pair not recognized")
	}
}
