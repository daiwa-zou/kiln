package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/connector"
)

// eventually polls cond until it holds or the deadline passes.
func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSyncDoesNotLeakConnections(t *testing.T) {
	// Every Sync builds its own transport, so the keep-alive connections it
	// opens are reachable only through that transport. If Sync does not close
	// them, each poll interval strands one (plus goroutines and an fd) until
	// the remote gives up, which a server with no idle timeout never does.
	var open atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte("# Notes\n\nSome text."))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := &Connector{allowPrivate: true, Extractors: testExtractors()} // tests must reach loopback
	cfg := connector.Config{"urls": []any{srv.URL + "/a.md", srv.URL + "/b.md"}}

	// Let goroutines from earlier tests wind down before taking the baseline.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	const syncs = 8
	for i := range syncs {
		set, err := c.Sync(context.Background(), cfg, t.TempDir())
		if err != nil {
			t.Fatalf("Sync %d: %v", i, err)
		}
		if payload := PayloadOf(set); payload == nil || len(payload.Docs) != 2 {
			t.Fatalf("Sync %d: payload = %+v, want 2 docs", i, payload)
		}
	}

	// Closing is asynchronous on the server side, so wait for it to settle
	// rather than asserting the instant Sync returns.
	if !eventually(3*time.Second, func() bool { return open.Load() == 0 }) {
		t.Errorf("%d connections still open on the server after %d Syncs; Sync leaves its keep-alive connections behind", open.Load(), syncs)
	}

	// Goroutines are noisy (the runtime, the test server), so allow slack well
	// below the ~2 per leaked connection a leak across 8 Syncs would add.
	const slack = 6
	if !eventually(3*time.Second, func() bool { return runtime.NumGoroutine() <= before+slack }) {
		t.Errorf("goroutines = %d after %d Syncs, started at %d (slack %d)", runtime.NumGoroutine(), syncs, before, slack)
	}
}
