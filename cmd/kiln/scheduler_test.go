package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/scheduler"
	"github.com/daiwa-zou/kiln/internal/store"
)

// schedStore is a scheduler.Store that counts each duty's calls. QueueDepth
// can be slowed, to show --once waits for a duty before returning.
type schedStore struct {
	mu                               sync.Mutex
	requeues, polls, sweeps, samples int
	blobSweeps                       int
	depthDelay                       time.Duration
}

func (s *schedStore) RequeueStaleRuns(context.Context, time.Duration) ([]store.StaleRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requeues++
	return nil, nil
}

func (s *schedStore) PollDueConnectors(context.Context, time.Duration) ([]store.ConnectorRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	return nil, nil
}

func (s *schedStore) EnqueueRun(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (s *schedStore) Sweep(context.Context, time.Duration, time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweeps++
	return 0, nil
}

func (s *schedStore) SweepBlobs(context.Context, store.BlobDeleter) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobSweeps++
	return 0, 0, nil
}

func (s *schedStore) PendingBlobDeletes(context.Context) (int, error) { return 0, nil }

func (s *schedStore) QueueDepth(context.Context) (map[string]int, error) {
	time.Sleep(s.depthDelay)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples++
	return map[string]int{"queued": 0}, nil
}

func schedulerConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Worker.StaleAfter = 15 * time.Minute
	cfg.Worker.SourcePollInterval = 10 * time.Minute
	return cfg
}

func noBlobs(config.Storage) (blob.Store, error) { return nil, errors.New("no bucket configured") }

// C11: --once runs every duty a single time -- the sweep included, and a slow
// duty to completion -- and a missing object store is a warning, not a stop.
func TestRunSchedulerOnce(t *testing.T) {
	st := &schedStore{depthDelay: 50 * time.Millisecond}
	var logs, out bytes.Buffer
	deps := schedulerDeps{store: st, openBlobs: noBlobs,
		log: slog.New(slog.NewTextHandler(&logs, nil)), out: &out}

	if err := runScheduler(context.Background(), schedulerConfig(), deps, true); err != nil {
		t.Fatalf("runScheduler --once: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.requeues != 1 || st.polls != 1 || st.sweeps != 1 || st.samples != 1 {
		t.Errorf("requeues=%d polls=%d sweeps=%d samples=%d, want one of each",
			st.requeues, st.polls, st.sweeps, st.samples)
	}
	if st.blobSweeps != 0 {
		t.Errorf("blob sweep ran %d times without object storage", st.blobSweeps)
	}
	if !strings.Contains(logs.String(), "object storage unavailable") {
		t.Errorf("missing object storage was not warned about:\n%s", logs.String())
	}
	if !strings.Contains(out.String(), "once") {
		t.Errorf("output = %q", out.String())
	}
}

// freeAddr returns a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func probe(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

// The long-running mode serves metrics and the in-process probes on the
// metrics listener, goes ready after the first pass, uses the blob store when
// it opens, and stops cleanly on cancel.
func TestRunSchedulerServesProbes(t *testing.T) {
	st := &schedStore{}
	cfg := schedulerConfig()
	cfg.MetricsAddr = freeAddr(t)
	var out bytes.Buffer
	deps := schedulerDeps{store: st, openBlobs: func(config.Storage) (blob.Store, error) { return nil, nil },
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), out: &out}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runScheduler(ctx, cfg, deps, false) }()

	base := "http://" + cfg.MetricsAddr
	deadline := time.Now().Add(3 * time.Second)
	for {
		if code, _ := probe(t, base+"/readyz"); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduler never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code, body := probe(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d %s", code, body)
	}
	if _, body := probe(t, base+"/metrics"); !strings.Contains(body, `kiln_scheduler_last_success_timestamp_seconds{duty="queue_depth"}`) {
		t.Error("metrics do not show the queue-depth duty succeeding")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runScheduler after cancel = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runScheduler did not stop on cancel")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	// The sweep waits out its startup delay, so a short run never reaches it.
	if st.sweeps != 0 {
		t.Errorf("swept %d times inside the startup delay", st.sweeps)
	}
	if !strings.Contains(out.String(), "metrics at "+cfg.MetricsAddr) {
		t.Errorf("output = %q", out.String())
	}
}

// C12: the command exists with its one flag.
func TestSchedulerFlags(t *testing.T) {
	cmd, _, err := newRootCmd().Find([]string{"scheduler"})
	if err != nil || cmd.Name() != "scheduler" {
		t.Fatalf("scheduler command not found: %v", err)
	}
	if cmd.Flags().Lookup("once") == nil {
		t.Error("scheduler has no --once flag")
	}
}

// An unready scheduler fails its probe with a reason `kiln admin health` can
// print, as the API's /readyz does.
func TestSchedulerReadinessProbeSaysWhy(t *testing.T) {
	s := scheduler.New(scheduler.Config{Store: &schedStore{}})
	rec := httptest.NewRecorder()
	schedulerProbes(s)["/readyz"].ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"reason":"the first pass`) {
		t.Errorf("/readyz before Run = %d %s, want 503 with a reason", rec.Code, rec.Body.String())
	}
}
