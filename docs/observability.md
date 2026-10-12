# Observability

kiln exposes Prometheus metrics on a **separate listener** from the API,
`127.0.0.1:9090` by default (`metrics_addr` / `KILN_METRICS_ADDR`; empty
disables it). The endpoint is unauthenticated, so the default is loopback only;
set `metrics_addr` to `:9090` where Prometheus scrapes from another host or pod
(the Helm chart and the compose file already do).

The separation is deliberate. Metrics describe the deployment — queue depth,
build outcomes, model spend — not any one tenant's content. Putting them on the
public router would mean either exposing that to every reader or inventing an
authentication scheme Prometheus does not want to use. A second port is
trivially firewalled, never routed through the API's ingress, and is what every
scrape configuration already expects.

Every role serves it. The API's port reports request traffic; a worker's is its
*only* listener, and the only way to see what builds are doing from outside.
The scheduler's (`kiln scheduler`) carries the queue-depth gauge, the stale
requeue counter and the duty metrics below, plus its readiness and liveness
probes.

## Scraping

The Helm chart annotates every pod template for the common
`prometheus.io/scrape` discovery:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "9090"
prometheus.io/path: /metrics
```

With Prometheus Operator, target the pods by their component label instead:

```yaml
selector:
  matchLabels:
    app.kubernetes.io/name: kiln
podMetricsEndpoints:
  - port: metrics
```

## What is measured

### Requests

| Metric | Type | Labels |
| --- | --- | --- |
| `kiln_http_requests_total` | counter | `method`, `route`, `status` |
| `kiln_http_request_duration_seconds` | histogram | `method`, `route` |
| `kiln_http_requests_in_flight` | gauge | — |

`route` is the router's **pattern**, never the concrete path —
`/api/v1/workspaces/{workspace}/pages`, not the workspace and page you asked
for. Page slugs and tenant ids are unbounded, and unbounded label values are
how a monitoring system becomes the outage it was installed to catch. `status`
is bucketed by class (`2xx`, `4xx`, `5xx`) for the same reason.

### Builds

| Metric | Type | Labels |
| --- | --- | --- |
| `kiln_runs_started_total` | counter | `trigger` |
| `kiln_runs_finished_total` | counter | `status` |
| `kiln_run_duration_seconds` | histogram | `status` |
| `kiln_run_cost_usd_total` | counter | — |
| `kiln_runs_stale_requeued_total` | counter | — |
| `kiln_pages_total` | counter | `action` |
| `kiln_documents_skipped_total` | counter | `kind`, `class` |
| `kiln_active_runs` | gauge | — |
| `kiln_queue_depth` | gauge | `status` |

A run is counted as **started when it is claimed**, not when the pipeline
begins. Runs that fail to resolve their sources — a bad connector, unreachable
object storage — never reach the pipeline, and those are exactly the failures
worth alerting on; they finish with status `unresolvable`.

`kiln_run_cost_usd_total` is a first-class metric because kiln spends real
money per run. A build platform whose spend is visible only in a database table
cannot be alerted on, and the cheapest outage to prevent is a budget one.

`kiln_runs_stale_requeued_total` counts running builds returned to the queue
because their worker stopped refreshing its claim. A worker heartbeats for as
long as its build lives, so a long build never lands here; a non-zero rate means
workers are dying or losing the database mid-build, and the requeued build is
paid for again. The worker's log names each run and the claim it lapsed from.
The displaced worker, if it is still alive, finishes its side with status
`claim_lost` on `kiln_runs_finished_total`: it wrote nothing to the run, which
now belongs to the worker that took it.

`no_changes` is the content-hash gate working: the run found nothing to
regenerate and cost nothing. A healthy incremental deployment shows many of
them.

`kiln_documents_skipped_total` counts documents a sync could not read: `kind`
is `upload` or `web`, and `class` is one of `no_text`, `extractor_missing`,
`unsupported_type`, `timed_out` and `extractor_error`, drawn from an
allow-list so the label set stays bounded. A steady `no_text` rate is users
uploading scans; `extractor_missing` on a fresh deployment is an image missing
a tool. The worker's log names each skipped file with its run, workspace and
class, and for uploads the skip is also recorded on the file row, which is what
the Ingest view shows as "not read".

`kiln_queue_depth` and `kiln_runs_stale_requeued_total` come from whichever
process runs the scheduler duties: the scheduler pod when one is deployed (the
Helm chart and compose default), otherwise every worker. The gauge is the
instance-wide count, not a per-process share, so aggregate it with `max`, never
`sum` -- with duties in N workers, `sum` reads N times the real queue.

### Scheduler duties

| Metric | Type | Labels |
| --- | --- | --- |
| `kiln_scheduler_last_success_timestamp_seconds` | gauge | `duty` |
| `kiln_scheduler_errors_total` | counter | `duty` |

`duty` is one of `requeue_stale`, `poll_sources`, `sweep` (the GC sweep and
the pending blob deletes) and `queue_depth`. Each duty runs on its own period
-- 30s, 1m, 1h and 10s -- with its own timeout (1m, the sweep 10m), so a slow
one delays only itself. A failed attempt is logged with its `duty` (an error,
or a warning for `queue_depth`) and counted; a timeout is logged as an error.

The last-success timestamp is what to alert on. It is stamped when the
process starts and then on every success, so a duty that never succeeds goes
stale from boot. Errors show a duty failing; only a timestamp that stops
advancing, or a series that is absent, shows a duty that is not running at
all -- workers configured to leave the duties to a scheduler that was never
deployed.

The scheduler pod's probes live on the metrics port and read in-process state
only: `/readyz` turns ready once the first pass of the duties has finished
with at least one success (the first sweep waits out a one-to-five-minute
startup delay and is not part of it) -- or after two minutes without one, with
a warning, so a database outage does not fail a rollout -- and `/healthz` fails when a duty loop has not finished an attempt in
three periods plus its timeout. Neither queries the database, so an outage
shows in the duty metrics, not as restarts.

## Alerts worth having

```yaml
groups:
  - name: kiln
    rules:
      # Builds are queuing faster than workers drain them. This is also the
      # signal to autoscale on -- CPU is a poor proxy, because a worker
      # waiting on a model call is idle by every resource measure and busy by
      # the only one that matters. max, not sum: every process that samples
      # the gauge reports the whole queue.
      - alert: KilnQueueBacklog
        expr: max(kiln_queue_depth{status="queued"}) > 10
        for: 15m

      # Sources are misconfigured: these runs never reached the pipeline.
      - alert: KilnRunsUnresolvable
        expr: increase(kiln_runs_finished_total{status="unresolvable"}[1h]) > 3

      - alert: KilnRunsFailing
        expr: |
          sum(rate(kiln_runs_finished_total{status="failed"}[30m]))
            / sum(rate(kiln_runs_finished_total[30m])) > 0.2
        for: 30m

      # Spend rate, not total: a runaway loop shows here long before the
      # monthly bill does.
      - alert: KilnSpendSpike
        expr: rate(kiln_run_cost_usd_total[1h]) * 3600 > 20

      - alert: KilnAPIErrors
        expr: |
          sum(rate(kiln_http_requests_total{status="5xx"}[10m]))
            / sum(rate(kiln_http_requests_total[10m])) > 0.05
        for: 10m
```

### Scheduler alerts

The duties fail quietly by nature -- nothing breaks the hour a sweep stops --
so these belong in every deployment. As a Prometheus Operator `PrometheusRule`
(the `groups` block alone works as a plain rules file):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: kiln-scheduler
spec:
  groups:
    - name: kiln-scheduler
      rules:
        # No process is running the duties at all: no scheduler pod scraped,
        # and no worker running them either. Stale runs block their benches,
        # polls stop, nothing is swept.
        - alert: KilnSchedulerAbsent
          expr: absent(kiln_scheduler_last_success_timestamp_seconds{duty="queue_depth"})
          for: 10m
          annotations:
            summary: No kiln process is running the scheduler duties.
            description: >-
              Check the kiln-scheduler Deployment is Ready, or set
              scheduler.enabled=false to hand the duties back to the workers.

        # A duty has stopped succeeding, or never has: the timestamp starts
        # at process start, so a duty failing from boot goes stale too.
        # Thresholds are a few periods each: requeue every 30s, poll every
        # 1m, queue depth every 10s, sweep hourly.
        - alert: KilnSchedulerDutyStale
          expr: |
            time() - max by (duty) (kiln_scheduler_last_success_timestamp_seconds{duty=~"requeue_stale|poll_sources|queue_depth"}) > 600
          for: 5m
          annotations:
            summary: kiln scheduler duty {{ $labels.duty }} has not succeeded for 10 minutes.

        - alert: KilnSchedulerSweepStale
          expr: |
            time() - max(kiln_scheduler_last_success_timestamp_seconds{duty="sweep"}) > 3 * 3600
          annotations:
            summary: The kiln GC sweep has not succeeded for three hours.

        # Errors, per duty period: the frequent duties fail many times an
        # hour, the sweep at most once.
        - alert: KilnSchedulerErrors
          expr: increase(kiln_scheduler_errors_total{duty!="sweep"}[30m]) > 5
          annotations:
            summary: kiln scheduler duty {{ $labels.duty }} is failing.

        - alert: KilnSchedulerSweepErrors
          expr: increase(kiln_scheduler_errors_total{duty="sweep"}[3h]) >= 2
          annotations:
            summary: The kiln GC sweep failed twice in three hours.
```

`max by (duty)` keeps these correct whether one scheduler or several workers
publish the series. Each process stamps its last-success series when it
starts, so a restart restarts the staleness clock: a scheduler that
crash-loops faster than the thresholds shows up as not Ready (and in your
pod-restart alerts) rather than here.

## Autoscaling workers on queue depth

CPU-based autoscaling underserves an LLM-bound queue. With the Prometheus
Adapter, scale on the queue instead:

```yaml
worker:
  autoscaling:
    enabled: true
    minReplicas: 1
    maxReplicas: 20
```

then replace the chart's CPU metric with an external one backed by
`max(kiln_queue_depth{status="queued"})`. Keep the chart's long scale-down
stabilization window: evicting a worker mid-build pays for the same pages
twice.

## Logs

Structured JSON via `log/slog`, at `log_level` (`KILN_LOG_LEVEL`). Every build
line carries `run` and `workspace`, and every request carries a request id, so
a run can be followed end to end across the API and the worker that claimed it.

## Health

| Endpoint | Meaning |
| --- | --- |
| `/healthz` | The process is alive. Answers even while Postgres is down. |
| `/readyz` | The database is reachable; safe to route traffic here. |

They differ on purpose: gating restarts on database reachability turns a
Postgres blip into a cluster-wide restart loop that makes recovery slower.
The scheduler serves the same two paths on its metrics port with its own
meanings (see [Scheduler duties](#scheduler-duties)).
`kiln admin health` probes either from inside the container, which is why the
image needs no curl.
