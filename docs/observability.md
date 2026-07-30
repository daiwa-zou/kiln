# Observability

kiln exposes Prometheus metrics on a **separate listener** from the API,
`:9090` by default (`metrics_addr` / `KILN_METRICS_ADDR`; empty disables it).

The separation is deliberate. Metrics describe the deployment — queue depth,
build outcomes, model spend — not any one tenant's content. Putting them on the
public router would mean either exposing that to every reader or inventing an
authentication scheme Prometheus does not want to use. A second port is
trivially firewalled, never routed through the API's ingress, and is what every
scrape configuration already expects.

Both roles serve it. The API's port reports request traffic; a worker's is its
*only* listener, and the only way to see what builds are doing from outside.

## Scraping

The Helm chart annotates both pod templates for the common
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
| `kiln_pages_total` | counter | `action` |
| `kiln_active_runs` | gauge | — |
| `kiln_queue_depth` | gauge | `status` |

A run is counted as **started when it is claimed**, not when the pipeline
begins. Runs that fail to resolve their sources — a bad connector, unreachable
object storage — never reach the pipeline, and those are exactly the failures
worth alerting on; they finish with status `unresolvable`.

`kiln_run_cost_usd_total` is a first-class metric because kiln spends real
money per run. A build platform whose spend is visible only in a database table
cannot be alerted on, and the cheapest outage to prevent is a budget one.

`no_changes` is the content-hash gate working: the run found nothing to
regenerate and cost nothing. A healthy incremental deployment shows many of
them.

## Alerts worth having

```yaml
groups:
  - name: kiln
    rules:
      # Builds are queuing faster than workers drain them. This is also the
      # signal to autoscale on -- CPU is a poor proxy, because a worker
      # waiting on a model call is idle by every resource measure and busy by
      # the only one that matters.
      - alert: KilnQueueBacklog
        expr: sum(kiln_queue_depth{status="queued"}) > 10
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
`sum(kiln_queue_depth{status="queued"})`. Keep the chart's long scale-down
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
`kiln admin health` probes either from inside the container, which is why the
image needs no curl.
