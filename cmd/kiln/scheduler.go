package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/scheduler"
	"github.com/daiwa-zou/kiln/internal/store"
)

func newSchedulerCmd(g *globalFlags) *cobra.Command {
	var once bool

	cmd := &cobra.Command{
		Use:   "scheduler",
		Short: "Run the periodic duties: stale requeue, source polling, GC sweeps, queue depth",
		Long: `Runs kiln's periodic duties in one process for the whole deployment: returning
runs whose worker stopped heartbeating to the queue, enqueueing poll-triggered
sources, the hourly GC sweep (including pending blob deletes), and the
queue-depth gauge.

Run exactly one. Workers run these duties themselves unless
worker.scheduler_duties is false (KILN_WORKER_SCHEDULER_DUTIES=false), so turn
that off on the workers once a scheduler is deployed. Two schedulers, or
workers still running duties beside one, cost duplicate work, never
corruption.

It needs the database and, for blob deletes, object storage; it never reads
the master key or a model key.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			cfg, err := config.Load(config.Options{File: g.configFile, Role: config.RoleScheduler})
			if err != nil {
				return err
			}
			log := observability.NewLogger(firstNonEmpty(g.logLevel, cfg.LogLevel))

			db, err := store.Open(ctx, cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			if err := store.CheckSchemaVersion(ctx, db.Pool); err != nil {
				return err
			}
			return runScheduler(ctx, cfg, schedulerDeps{
				store: store.NewWikiStore(db.Pool), openBlobs: blob.Open, log: log, out: cmd.OutOrStdout(),
			}, once)
		},
	}

	cmd.Flags().BoolVar(&once, "once", false, "run every duty once and exit")

	return cmd
}

// schedulerDeps is what runScheduler needs from the outside world, so the
// command's behaviour is testable without a database or object storage.
type schedulerDeps struct {
	store     scheduler.Store
	openBlobs func(config.Storage) (blob.Store, error)
	log       *slog.Logger
	out       io.Writer
}

// runScheduler runs the duties until ctx is canceled, or once with --once. The
// schema check has already passed; readiness additionally waits for the first
// pass of the duties.
func runScheduler(ctx context.Context, cfg *config.Config, deps schedulerDeps, once bool) error {
	metrics := observability.NewMetrics()
	sc := scheduler.Config{
		Store:               deps.store,
		Log:                 deps.log,
		Metrics:             metrics,
		StaleAfter:          cfg.Worker.StaleAfter,
		SourcePollInterval:  cfg.Worker.SourcePollInterval,
		SoftDeleteRetention: cfg.Storage.SoftDeleteRetention,
		RunRetention:        cfg.Storage.RunArtifactRetention,
	}
	// Without object storage every other duty still runs; the pending blob
	// deletes wait in their table for a process that can reach the store.
	if blobs, err := deps.openBlobs(cfg.Storage); err != nil {
		deps.log.Warn("object storage unavailable; pending blob deletes will wait", "err", err)
	} else {
		sc.Blobs = blobs
	}

	if once {
		// One pass of everything, sweep included: whoever runs --once by hand
		// wants the sweep now, not after a crash-loop guard.
		scheduler.New(sc).Tick(ctx)
		fmt.Fprintln(deps.out, "ran every scheduler duty once")
		return nil
	}

	// A crash-looping pod would otherwise sweep on every restart. Kubernetes'
	// restart backoff tops out at five minutes, so a pod that cannot stay up
	// for a minute never sweeps at all, and the jitter keeps a fleet of
	// restarts from lining up.
	sc.FirstSweepDelay = time.Minute + rand.N(4*time.Minute)
	s := scheduler.New(sc)

	// The scheduler's only listener: metrics, plus probes that read
	// in-process state, so an orchestrator asking costs the database nothing.
	var metricsDone chan struct{}
	if cfg.MetricsAddr != "" {
		fmt.Fprintf(deps.out, "metrics at %s/metrics\n", cfg.MetricsAddr)
		metricsDone = make(chan struct{})
		go func() {
			defer close(metricsDone)
			if err := metrics.ServeMetricsWith(ctx, cfg.MetricsAddr, schedulerProbes(s)); err != nil {
				deps.log.Error("metrics listener stopped", "error", err)
			}
		}()
	}

	fmt.Fprintln(deps.out, "scheduler running: stale requeue, source polling, gc sweep, queue depth")
	err := s.Run(ctx)
	if metricsDone != nil {
		<-metricsDone
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// schedulerProbes answers readiness (the first pass of the duties is done) and
// liveness (every duty loop is making progress) from in-process state.
func schedulerProbes(s *scheduler.Scheduler) map[string]http.Handler {
	probe := func(check func() error) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := check(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "unready", "reason": err.Error()})
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
	}
	return map[string]http.Handler{
		"/readyz": probe(func() error {
			if !s.Ready() {
				return errors.New("the first pass of the scheduler duties has not finished")
			}
			return nil
		}),
		"/healthz": probe(s.Live),
	}
}
