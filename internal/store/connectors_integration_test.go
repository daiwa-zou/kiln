package store

import (
	"context"
	"errors"
	"testing"
)

func TestConnectorsLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	gitID, err := s.CreateConnector(ctx, ConnectorRow{
		WorkspaceID: ws, Kind: "git", Name: "repo",
		Config:  map[string]any{"path": "/srv/repo"},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create git: %v", err)
	}
	webID, err := s.CreateConnector(ctx, ConnectorRow{
		WorkspaceID: ws, Kind: "web", Name: "docs site",
		Config: map[string]any{"urls": []any{"https://example.com"}},
		// Disabled: EnabledConnectors must not see it.
	})
	if err != nil {
		t.Fatalf("create web: %v", err)
	}

	// EnabledConnectors feeds the build, so a paused source must not appear.
	enabled, err := s.EnabledConnectors(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || enabled[0].ID != gitID {
		t.Fatalf("enabled = %+v, want just the git connector", enabled)
	}

	// ConnectorByID round-trips what CreateConnector stored, defaults included.
	got, err := s.ConnectorByID(ctx, gitID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "git" || got.Name != "repo" || got.Config["path"] != "/srv/repo" {
		t.Fatalf("by id = %+v", got)
	}
	if got.TriggerMode != "manual" {
		t.Errorf("trigger mode defaulted to %q, want manual", got.TriggerMode)
	}
	if got.LastSyncedAt != nil || got.LastError != "" {
		t.Errorf("unsynced connector reports a sync: %+v", got)
	}
	if _, err := s.ConnectorByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id: err = %v, want ErrNotFound", err)
	}

	// A failed sync records when and why; the reader-facing Ingestion page
	// shows both.
	if err := s.MarkConnectorSync(ctx, gitID, "clone failed: no route to host"); err != nil {
		t.Fatalf("mark failed sync: %v", err)
	}
	got, _ = s.ConnectorByID(ctx, gitID)
	if got.LastSyncedAt == nil || got.LastError != "clone failed: no route to host" {
		t.Fatalf("after failed sync: %+v", got)
	}

	// A clean sync clears the error rather than leaving a stale one on the
	// page after the problem is fixed.
	if err := s.MarkConnectorSync(ctx, gitID, ""); err != nil {
		t.Fatalf("mark clean sync: %v", err)
	}
	got, _ = s.ConnectorByID(ctx, gitID)
	if got.LastError != "" {
		t.Fatalf("clean sync left error %q", got.LastError)
	}

	// Enabling the web connector by patch makes it feed the next build.
	on := true
	if err := s.UpdateConnector(ctx, ws, webID, ConnectorPatch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	enabled, _ = s.EnabledConnectors(ctx, ws)
	if len(enabled) != 2 {
		t.Fatalf("enabled after resume = %+v", enabled)
	}
	// Creation order, so the build reads sources in the order they were added.
	if enabled[0].ID != gitID || enabled[1].ID != webID {
		t.Errorf("order = [%s %s], want creation order", enabled[0].Name, enabled[1].Name)
	}
}
