package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/store"
)

// fakeHooks scripts the webhook store surface.
type fakeHooks struct {
	mu         sync.Mutex
	connectors []store.ConnectorRow
	finished   map[string]time.Time
	enqueued   []string
	opts       []store.EnqueueOptions
	installs   map[int64]string // id -> last action applied
}

func newFakeHooks() *fakeHooks {
	return &fakeHooks{finished: map[string]time.Time{}, installs: map[int64]string{}}
}

func (f *fakeHooks) WebhookConnectors(context.Context) ([]store.ConnectorRow, error) {
	return f.connectors, nil
}
func (f *fakeHooks) LastRunFinishedAt(_ context.Context, ws string) (time.Time, error) {
	return f.finished[ws], nil
}
func (f *fakeHooks) EnqueueRunOpts(_ context.Context, ws, trigger, connectorID string, opts store.EnqueueOptions) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, ws+"/"+trigger)
	f.opts = append(f.opts, opts)
	return "r1", true, nil
}
func (f *fakeHooks) UpsertGitHubInstallationEvent(_ context.Context, id int64, _, _ string, suspended bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if suspended {
		f.installs[id] = "suspended"
	} else {
		f.installs[id] = "active"
	}
	return nil
}
func (f *fakeHooks) RemoveGitHubInstallation(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs[id] = "removed"
	return nil
}

const hookSecret = "whsec_test"

func hookServer(t *testing.T, hooks *fakeHooks, cooldown time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer((&Server{
		Store: &fakeReadStore{}, Writes: (*fakeWrites)(nil),
		Hooks: hooks, WebhookSecret: []byte(hookSecret), WebhookCooldown: cooldown,
	}).Router())
	t.Cleanup(srv.Close)
	return srv
}

// deliver posts one webhook and returns the status code, closing the body.
func deliver(t *testing.T, srv *httptest.Server, event string, body []byte, sign bool) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/hooks/github", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("Content-Type", "application/json")
	if sign {
		mac := hmac.New(sha256.New, []byte(hookSecret))
		mac.Write(body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func gitConnector(ws, url string) store.ConnectorRow {
	return store.ConnectorRow{
		ID: "c-" + ws, WorkspaceID: ws, Kind: "git", Name: ws,
		TriggerMode: "webhook", Enabled: true,
		Config: map[string]any{"url": url},
	}
}

func TestWebhookSignatureGate(t *testing.T) {
	hooks := newFakeHooks()
	srv := hookServer(t, hooks, 0)
	body := []byte(`{"repository":{"full_name":"daiwa-zou/kiln"}}`)

	// The M3 verification pair: invalid HMAC → 401, valid → 202.
	if code := deliver(t, srv, "push", body, false); code != http.StatusUnauthorized {
		t.Errorf("unsigned delivery = %d, want 401", code)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged signature = %d, want 401", res.StatusCode)
	}
	if code := deliver(t, srv, "push", body, true); code != http.StatusAccepted {
		t.Errorf("valid signature = %d, want 202", code)
	}
	if len(hooks.enqueued) != 0 {
		t.Errorf("no connector matches, but enqueued %v", hooks.enqueued)
	}
}

func TestWebhookPushMapsRepoToConnector(t *testing.T) {
	hooks := newFakeHooks()
	hooks.connectors = []store.ConnectorRow{
		gitConnector("ws-kiln", "https://github.com/daiwa-zou/kiln.git"),
		gitConnector("ws-other", "https://github.com/acme/other"),
		// Manual connectors must not react to pushes even for the same repo:
		// WebhookConnectors already filters, so this fake stays honest by
		// only containing webhook-mode rows.
	}
	srv := hookServer(t, hooks, 0)

	body := []byte(`{"repository":{"full_name":"daiwa-zou/kiln"}}`)
	if code := deliver(t, srv, "push", body, true); code != http.StatusAccepted {
		t.Fatalf("push = %d", code)
	}
	if len(hooks.enqueued) != 1 || hooks.enqueued[0] != "ws-kiln/webhook" {
		t.Errorf("enqueued = %v, want exactly ws-kiln", hooks.enqueued)
	}
}

func TestWebhookCooldownSchedulesInsteadOfDropping(t *testing.T) {
	hooks := newFakeHooks()
	hooks.connectors = []store.ConnectorRow{
		gitConnector("ws-kiln", "https://github.com/daiwa-zou/kiln"),
	}
	finished := time.Now().Add(-10 * time.Second)
	hooks.finished["ws-kiln"] = finished
	srv := hookServer(t, hooks, time.Minute)

	// A push inside the cooldown still enqueues — nothing is dropped — but
	// carries not_before so the claim waits out the quiet period, and the
	// pushed head rides along for incremental routing.
	body := []byte(`{"after":"abc123","repository":{"full_name":"daiwa-zou/kiln"}}`)
	if code := deliver(t, srv, "push", body, true); code != http.StatusAccepted {
		t.Fatalf("push = %d", code)
	}
	if len(hooks.enqueued) != 1 {
		t.Fatalf("push inside cooldown enqueued %v, want 1 scheduled run", hooks.enqueued)
	}
	got := hooks.opts[0]
	if got.RefTo != "abc123" {
		t.Errorf("refTo = %q, want the pushed head", got.RefTo)
	}
	if want := finished.Add(time.Minute); !got.NotBefore.Equal(want) {
		t.Errorf("notBefore = %v, want finished+cooldown %v", got.NotBefore, want)
	}

	// Outside the cooldown the push is claimable immediately.
	hooks.finished["ws-kiln"] = time.Now().Add(-2 * time.Minute)
	if code := deliver(t, srv, "push", body, true); code != http.StatusAccepted {
		t.Fatalf("push = %d", code)
	}
	if len(hooks.enqueued) != 2 || !hooks.opts[1].NotBefore.IsZero() {
		t.Errorf("push outside cooldown: enqueued=%v opts=%+v", hooks.enqueued, hooks.opts)
	}
}

func TestWebhookInstallationLifecycle(t *testing.T) {
	hooks := newFakeHooks()
	srv := hookServer(t, hooks, 0)

	for action, want := range map[string]string{
		"created":   "active",
		"suspend":   "suspended",
		"unsuspend": "active",
		"deleted":   "removed",
	} {
		body := []byte(`{"action":"` + action + `","installation":{"id":901,"account":{"login":"daiwa-zou","type":"User"}}}`)
		if code := deliver(t, srv, "installation", body, true); code != http.StatusAccepted {
			t.Fatalf("%s = %d", action, code)
		}
		if hooks.installs[901] != want {
			t.Errorf("%s: state = %q, want %q", action, hooks.installs[901], want)
		}
	}
}

func TestWebhookPingAndOversize(t *testing.T) {
	hooks := newFakeHooks()
	srv := hookServer(t, hooks, 0)

	if code := deliver(t, srv, "ping", []byte(`{"zen":"Keep it simple."}`), true); code != http.StatusOK {
		t.Errorf("ping = %d, want 200", code)
	}
	big := bytes.Repeat([]byte("a"), maxWebhookBytes+1)
	if code := deliver(t, srv, "push", big, true); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize = %d, want 413", code)
	}
}

func TestRepoMatches(t *testing.T) {
	cases := []struct {
		url, repo string
		want      bool
	}{
		{"https://github.com/daiwa-zou/kiln.git", "daiwa-zou/kiln", true},
		{"https://github.com/daiwa-zou/kiln", "daiwa-zou/kiln", true},
		{"https://github.com/daiwa-zou/kiln/", "daiwa-zou/kiln", true},
		{"https://github.com/Daiwa-Zou/Kiln.git", "daiwa-zou/kiln", true},
		{"https://github.com/daiwa-zou/kiln2", "daiwa-zou/kiln", false},
		{"https://github.com/acme/kiln", "daiwa-zou/kiln", false},
		{"", "daiwa-zou/kiln", false},
		{"https://github.com/daiwa-zou/kiln", "", false},
	}
	for _, tc := range cases {
		if got := repoMatches(tc.url, tc.repo); got != tc.want {
			t.Errorf("repoMatches(%q, %q) = %v", tc.url, tc.repo, got)
		}
	}
}

// fakeReadStore satisfies Store minimally; webhook tests never read content.
type fakeReadStore struct{ Store }

// TestWebhookPushStormQueuesOneRun is the roadmap's storm verification
// against the real queue: many signed pushes for one repository leave
// exactly one queued run, because the active-run index debounces at insert.
func TestWebhookPushStormQueuesOneRun(t *testing.T) {
	_, js, wsID := testServer(t) // skips without KILN_TEST_DATABASE_URL

	connectorID, err := js.CreateConnector(context.Background(), store.ConnectorRow{
		WorkspaceID: wsID, Kind: "git", Name: "kiln",
		Config:      map[string]any{"url": "https://github.com/daiwa-zou/kiln.git"},
		TriggerMode: "webhook", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = connectorID

	// Rebuild the router with hooks wired onto the same store. No cooldown,
	// so only the index is doing the debouncing.
	hooked := httptest.NewServer((&Server{
		Store: js, Writes: js, Runs: js, Admin: js,
		Hooks: js, WebhookSecret: []byte(hookSecret),
		DB: nil,
	}).Router())
	t.Cleanup(hooked.Close)

	body := []byte(`{"repository":{"full_name":"daiwa-zou/kiln"}}`)
	for i := 0; i < 8; i++ {
		if code := deliver(t, hooked, "push", body, true); code != http.StatusAccepted {
			t.Fatalf("push %d = %d", i, code)
		}
	}

	var queued int
	if err := js.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE workspace_id = $1 AND status = 'queued'`,
		wsID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("queued runs after storm = %d, want exactly 1", queued)
	}
	var trigger string
	if err := js.Pool().QueryRow(context.Background(),
		`SELECT trigger FROM runs WHERE workspace_id = $1 LIMIT 1`, wsID).Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	if trigger != "webhook" {
		t.Errorf("trigger = %q, want webhook", trigger)
	}
}
