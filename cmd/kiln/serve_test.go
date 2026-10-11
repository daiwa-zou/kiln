package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/config"
)

func TestLoopbackAddr(t *testing.T) {
	loopback := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "LOCALHOST:80"}
	for _, a := range loopback {
		if !loopbackAddr(a) {
			t.Errorf("loopbackAddr(%q) = false, want true", a)
		}
	}
	// An empty host binds every interface; with auth disabled that hands the
	// instance to the network, so it must not count as loopback.
	reachable := []string{":8080", "0.0.0.0:8080", "192.168.1.5:8080", "[::]:8080", "example.com:8080", "garbage"}
	for _, a := range reachable {
		if loopbackAddr(a) {
			t.Errorf("loopbackAddr(%q) = true, want false", a)
		}
	}
}

// A listener that fails to start returns at once, but the metrics and worker
// goroutines only exit when the context is cancelled. serveAndDrain must
// cancel it itself, or `kiln serve` hangs on a port-in-use error without
// printing it.
func TestServeAndDrainCancelsAfterServeFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	background := func() chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			<-ctx.Done()
		}()
		return done
	}
	metricsDone, workerDone := background(), background()

	wantErr := errors.New("listen tcp :8080: address already in use")
	drained := false
	result := make(chan error, 1)
	go func() {
		result <- serveAndDrain(func() error { return wantErr }, cancel, metricsDone, workerDone, func() { drained = true })
	}()

	select {
	case err := <-result:
		if !errors.Is(err, wantErr) {
			t.Errorf("err = %v, want %v", err, wantErr)
		}
		if !drained {
			t.Error("onDrain was not called before waiting on the worker")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveAndDrain hung: the failed listener never cancelled the background goroutines")
	}
}

func TestTrustedProxiesLogsListAndWarnings(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	got, err := trustedProxies(&config.Config{TrustedProxies: []string{"10.0.0.5", " 0.0.0.0/0"}}, log)
	if err != nil {
		t.Fatalf("trustedProxies: %v", err)
	}
	if len(got) != 2 || got[0].String() != "10.0.0.5/32" {
		t.Errorf("prefixes = %v", got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "0.0.0.0/0") {
		t.Errorf("no warning for the all-addresses prefix:\n%s", out)
	}
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "10.0.0.5/32") {
		t.Errorf("effective list not logged at info:\n%s", out)
	}

	// An empty list is today's behaviour and says nothing about proxies.
	buf.Reset()
	if got, err := trustedProxies(&config.Config{}, log); err != nil || len(got) != 0 {
		t.Errorf("empty list = %v, %v", got, err)
	}
	if buf.Len() != 0 {
		t.Errorf("empty list logged:\n%s", buf.String())
	}

	if _, err := trustedProxies(&config.Config{TrustedProxies: []string{"nope"}}, log); err == nil {
		t.Error("a bad entry was accepted")
	}
}

func TestServeAndDrainSkipsGoroutinesThatNeverStarted(t *testing.T) {
	err := serveAndDrain(func() error { return nil }, func() {}, nil, nil, func() { t.Error("onDrain called without a worker") })
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}
