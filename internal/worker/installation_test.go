package worker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daiwa-zou/kiln/internal/github"
	"github.com/daiwa-zou/kiln/internal/store"
)

// installationWorker wires a worker whose GitHub App is fully configured and
// whose token endpoint counts how often it is asked for a token. The clone
// itself is allowed to fail (the URL is unroutable); what matters here is
// whether the ownership check let the worker get as far as minting.
func installationWorker(t *testing.T, st *fakeStore) (*Worker, *atomic.Int32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	var minted atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		minted.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(api.Close)

	return &Worker{
		Store: st,
		GitHub: &github.Client{
			AppID: 7, PrivateKey: pemKey, APIBaseURL: api.URL, HTTP: api.Client(),
		},
	}, &minted
}

func installationConnector() store.ConnectorRow {
	return store.ConnectorRow{
		ID: "c1", WorkspaceID: "ws1", Kind: "git", Name: "code",
		Config: map[string]any{"url": "https://github.com/acme/private.git", "installation_id": float64(4242)},
	}
}

func TestCloneRefusesInstallationNoMemberHolds(t *testing.T) {
	st := &fakeStore{installOK: false}
	w, minted := installationWorker(t, st)

	_, _, _, err := w.cloneRemote(context.Background(), installationConnector(), "https://github.com/acme/private.git")
	if err == nil || !strings.Contains(err.Error(), "no current member") {
		t.Fatalf("err = %v, want a refusal naming membership", err)
	}
	if minted.Load() != 0 {
		t.Errorf("a token was requested for an installation nobody behind the workspace holds")
	}
	if len(st.installChecked) != 1 || st.installChecked[0] != 4242 {
		t.Errorf("checked installations = %v, want [4242]", st.installChecked)
	}
}

func TestCloneAllowsInstallationAMemberHolds(t *testing.T) {
	st := &fakeStore{installOK: true}
	w, minted := installationWorker(t, st)

	_, _, _, err := w.cloneRemote(context.Background(), installationConnector(), "https://github.com/acme/private.git")
	if err == nil {
		t.Fatal("expected the stubbed token endpoint to fail the clone")
	}
	if strings.Contains(err.Error(), "no current member") {
		t.Fatalf("a linked installation was refused: %v", err)
	}
	if minted.Load() != 1 {
		t.Errorf("token requests = %d, want 1", minted.Load())
	}
}

func TestCloneInstallationCheckFailsClosed(t *testing.T) {
	st := &fakeStore{installErr: errors.New("db down")}
	w, minted := installationWorker(t, st)

	_, _, _, err := w.cloneRemote(context.Background(), installationConnector(), "https://github.com/acme/private.git")
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want the store error surfaced", err)
	}
	if minted.Load() != 0 {
		t.Errorf("a token was requested although the check could not run")
	}
}

func TestCloneSkipsInstallationCheckWithAuthDisabled(t *testing.T) {
	// Loopback dev: nobody signs in, so user_installations is empty and a
	// membership check would lock out the only way to use an App there.
	st := &fakeStore{installOK: false}
	w, minted := installationWorker(t, st)
	w.AuthDisabled = true

	_, _, _, _ = w.cloneRemote(context.Background(), installationConnector(), "https://github.com/acme/private.git")
	if len(st.installChecked) != 0 {
		t.Errorf("auth-disabled worker consulted the store: %v", st.installChecked)
	}
	if minted.Load() != 1 {
		t.Errorf("token requests = %d, want 1", minted.Load())
	}
}
