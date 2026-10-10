package store

import (
	"context"
	"testing"
	"time"
)

// The webhook path's store surface: which connectors a push wakes, how long
// ago the bench last built, and the GitHub App installation mirror. None of it
// had a test, and the cooldown's input in particular fails in a direction that
// costs money.

func TestPollDueConnectorsPicksOnlyWhatIsDue(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "acme", "demo", "Demo")
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	mk := func(name, mode string, enabled bool) string {
		t.Helper()
		id, err := s.CreateConnector(ctx, ConnectorRow{
			WorkspaceID: ws, Kind: "git", Name: name,
			Config: map[string]any{"url": "https://github.com/acme/" + name + ".git"},
			// A poll connector with no url would still be selected; the kind
			// and config are here only to make the rows realistic.
			TriggerMode: mode, Enabled: enabled,
		})
		if err != nil {
			t.Fatalf("CreateConnector %s: %v", name, err)
		}
		return id
	}

	never := mk("never-synced", "poll", true)
	fresh := mk("just-synced", "poll", true)
	off := mk("disabled", "poll", false)
	mk("webhook-driven", "webhook", true)

	// Only the freshly synced one carries a timestamp, so it is the only one
	// the interval can exclude.
	if err := s.MarkConnectorSync(ctx, fresh, ""); err != nil {
		t.Fatalf("MarkConnectorSync: %v", err)
	}
	if err := s.MarkConnectorSync(ctx, off, ""); err != nil {
		t.Fatalf("MarkConnectorSync: %v", err)
	}

	due, err := s.PollDueConnectors(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PollDueConnectors: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range due {
		ids[c.ID] = true
	}
	if !ids[never] {
		t.Error("a connector that has never synced is due and was not returned")
	}
	if ids[fresh] {
		t.Error("a connector synced moments ago is not due yet")
	}
	if ids[off] {
		t.Error("a disabled connector was returned")
	}
	if len(due) != 1 {
		t.Errorf("returned %d connectors, want 1 (webhook-driven ones do not poll): %v", len(due), ids)
	}

	// Zero interval: everything with a timestamp is now overdue as well.
	all, err := s.PollDueConnectors(ctx, 0)
	if err != nil {
		t.Fatalf("PollDueConnectors(0): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("with no interval, returned %d, want both enabled poll connectors", len(all))
	}
}

func TestWebhookConnectorsAreOnlyEnabledWebhookOnes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "acme", "demo", "Demo")
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	want, err := s.CreateConnector(ctx, ConnectorRow{
		WorkspaceID: ws, Kind: "git", Name: "watched",
		Config:      map[string]any{"url": "https://github.com/acme/watched.git"},
		TriggerMode: "webhook", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConnector: %v", err)
	}
	for _, c := range []ConnectorRow{
		{Name: "paused", TriggerMode: "webhook", Enabled: false},
		{Name: "polled", TriggerMode: "poll", Enabled: true},
	} {
		c.WorkspaceID, c.Kind = ws, "git"
		c.Config = map[string]any{"url": "https://github.com/acme/" + c.Name + ".git"}
		if _, err := s.CreateConnector(ctx, c); err != nil {
			t.Fatalf("CreateConnector %s: %v", c.Name, err)
		}
	}

	got, err := s.WebhookConnectors(ctx)
	if err != nil {
		t.Fatalf("WebhookConnectors: %v", err)
	}
	if len(got) != 1 || got[0].ID != want {
		t.Fatalf("WebhookConnectors returned %d rows, want just the enabled webhook one", len(got))
	}
	// The handler matches the push against this, so it has to come back.
	if got[0].Config["url"] != "https://github.com/acme/watched.git" {
		t.Errorf("config did not survive the round trip: %v", got[0].Config)
	}
}

func TestLastRunFinishedAtReportsTheMostRecentFinish(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "acme", "demo", "Demo")
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	// A bench that has never built owes no cooldown, and says so with the zero
	// time rather than an error.
	got, err := s.LastRunFinishedAt(ctx, ws)
	if err != nil {
		t.Fatalf("LastRunFinishedAt on an unbuilt bench: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("LastRunFinishedAt = %v, want the zero time before any run", got)
	}

	// A run still in the queue has not finished, so it must not count.
	if _, _, err := s.EnqueueRun(ctx, ws, "manual", ""); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	got, err = s.LastRunFinishedAt(ctx, ws)
	if err != nil {
		t.Fatalf("LastRunFinishedAt with a queued run: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("LastRunFinishedAt = %v, want zero while the only run is unfinished", got)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (workspace_id, trigger, status, started_at, finished_at)
		VALUES ($1, 'manual', 'succeeded', now() - interval '1 hour', now() - interval '1 hour'),
		       ($1, 'manual', 'succeeded', now(), now())`, ws); err != nil {
		t.Fatalf("seed finished runs: %v", err)
	}

	got, err = s.LastRunFinishedAt(ctx, ws)
	if err != nil {
		t.Fatalf("LastRunFinishedAt: %v", err)
	}
	// The newer of the two, not the older or an aggregate of both. Compared
	// with slack because the timestamps come from the database's clock.
	if age := time.Since(got); age > time.Minute {
		t.Errorf("LastRunFinishedAt = %v (%v old), want the finish from moments ago, not the hour-old one", got, age)
	}
}

func TestLastRunFinishedAtReportsFailureRatherThanNoHistory(t *testing.T) {
	// The cooldown reads this. A query that fails leaves the timestamp nil,
	// and answering nil-and-no-error would be indistinguishable from "this
	// bench has never built" -- so the push would enqueue with no not_before
	// and the quiet period the cooldown exists to enforce would silently not
	// happen. The failure has to surface.
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	s := NewWikiStore(pool)

	// workspace_id is a uuid column; a ref that is not one fails in the
	// database rather than matching nothing.
	got, err := s.LastRunFinishedAt(ctx, "not-a-uuid")
	if err == nil {
		t.Fatalf("LastRunFinishedAt = (%v, nil), want an error when the query cannot run", got)
	}
	if !got.IsZero() {
		t.Errorf("LastRunFinishedAt returned %v alongside its error, want the zero time", got)
	}
}

func TestGitHubInstallationMirrorTracksLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	s := NewWikiStore(pool)

	read := func() (login, acctType string, suspended bool, ok bool) {
		t.Helper()
		var suspendedAt *time.Time
		err := pool.QueryRow(ctx, `
			SELECT account_login, account_type, suspended_at
			FROM github_installations WHERE installation_id = $1`, int64(42)).
			Scan(&login, &acctType, &suspendedAt)
		if err != nil {
			return "", "", false, false
		}
		return login, acctType, suspendedAt != nil, true
	}

	if err := s.UpsertGitHubInstallationEvent(ctx, 42, "acme", "Organization", false); err != nil {
		t.Fatalf("UpsertGitHubInstallationEvent: %v", err)
	}
	login, acctType, suspended, ok := read()
	if !ok || login != "acme" || acctType != "Organization" || suspended {
		t.Fatalf("after install: (%q, %q, suspended=%v, found=%v)", login, acctType, suspended, ok)
	}

	// Renaming and suspending both arrive as the same event, and the mirror
	// takes the newest version of every field.
	if err := s.UpsertGitHubInstallationEvent(ctx, 42, "acme-corp", "Organization", true); err != nil {
		t.Fatalf("suspend event: %v", err)
	}
	login, _, suspended, ok = read()
	if !ok || login != "acme-corp" || !suspended {
		t.Fatalf("after suspend: (%q, suspended=%v, found=%v)", login, suspended, ok)
	}

	// Unsuspending has to clear the timestamp, not merely leave the newer
	// event's fields behind it.
	if err := s.UpsertGitHubInstallationEvent(ctx, 42, "acme-corp", "Organization", false); err != nil {
		t.Fatalf("unsuspend event: %v", err)
	}
	if _, _, suspended, _ = read(); suspended {
		t.Error("unsuspending left suspended_at set")
	}

	if err := s.RemoveGitHubInstallation(ctx, 42); err != nil {
		t.Fatalf("RemoveGitHubInstallation: %v", err)
	}
	if _, _, _, ok = read(); ok {
		t.Error("the mirror row survived an uninstall")
	}
	// Uninstalling twice is what a redelivered webhook looks like.
	if err := s.RemoveGitHubInstallation(ctx, 42); err != nil {
		t.Errorf("RemoveGitHubInstallation on an already-removed install: %v", err)
	}
}
