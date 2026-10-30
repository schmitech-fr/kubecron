# The KubeCron collector API (`/api/v1`)

The contract another program may depend on. Everything under `/api` without a
version prefix backs KubeCron's own pages and changes with them; everything
under `/api/v1` is a promise.

Written for KubeDeck, which reads a collector as the richest rung of a
degradation ladder, but nothing in it is KubeDeck-specific.

Machine-readable companion: **[`openapi.yaml`](openapi.yaml)** (OpenAPI 3.1) —
generate a client from that, read this for the reasoning it cannot carry.

---

## 1. The two modes

KubeCron is one binary that runs one of two ways. `KUBECRON_MODE` selects which
(`ui` is the default; `standalone` and `collector` are accepted aliases).

| | `ui` — standalone | `server` — collector |
|---|---|---|
| Dashboard, static assets | yes | **no** |
| Unversioned `/api/*` (backs the HTMX fragments) | yes | **no** |
| `/api/v1/*` collector contract | yes | yes |
| Suspend / resume / trigger | yes | **no route registered** |
| RBAC granted by the chart | `patch` on cronjobs, `create` on jobs | `get`/`list`/`watch` only |
| Front door | OIDC for the dashboard; `API_TOKEN` for `/api/v1` when set | `API_TOKEN` bearer (optional) |
| `/metrics` | open | behind the token |
| Informers, sampler, log capture, retention | identical | identical |

**A `ui`-mode instance needs `API_TOKEN` to be readable by a program.** With
OIDC on and no token, `/api/v1` sits behind the session guard and every request
to it is answered with a redirect to the identity provider — which a console
cannot follow, from an Ingress or from a port-forward alike. Set a token and the
contract is guarded by it while the dashboard keeps OIDC.

The collection machinery is the same in both, which is why this is a flag and
not a build tag: a collector that recorded a different set of facts from the
standalone product would be a second product to keep correct. A release can be
switched from one mode to the other and keeps its history.

**Read-only in server mode is structural, not a policy.** The mutating routes
are never registered, and the chart grants no mutating verb, so the claim is
checkable from outside the process. A console performs suspend/resume/trigger
itself, against the Kubernetes API, under its own authorization — two components
able to mutate the same CronJob would be two authorization models that have to
stay in agreement.

---

## 2. Deployment shape

One collector per cluster, watching its own cluster through its own
ServiceAccount:

```bash
helm install kubecron charts/kubecron \
  --namespace kubecron --create-namespace \
  --set mode=server \
  --set clusterID=prod-eu \
  --set api.token="$(openssl rand -hex 32)"
```

No kubeconfigs, no credentials leaving the cluster, RBAC that is read-only.
Fanning out across clusters is the console's job — it already has them all.

The multi-cluster shape still works: mount a directory of kubeconfigs and one
collector watches several clusters from outside. `clusterID` then applies only
to the in-cluster fallback; mounted kubeconfigs name themselves after their
filename.

### Discovery

Keep the Service `ClusterIP` and reach it through a port forward. That needs no
Ingress, no certificate and no public hostname, and it is why a token is
sufficient as a front door.

In server mode the chart labels the Service so a console can find it rather than
be configured with a URL per cluster:

| | |
|---|---|
| label `kubecron.io/collector` | `"true"` |
| label `app.kubernetes.io/component` | `collector` |
| annotation `kubecron.io/api-version` | `v1` |
| annotation `kubecron.io/api-path` | `/api/v1` |
| annotation `kubecron.io/auth` | `bearer` or `none` |
| annotation `kubecron.io/auth-secret` | the Secret holding the token (when `bearer`) |
| annotation `kubecron.io/auth-secret-key` | its key, `api-token` |

A cluster with no such Service has no collector, which is a rung of the ladder,
not an error. Set `service.discoverable=false` to opt out.

**The Service is only labelled when a program can actually read the API** — a
`server` release always, a `ui` release when it has an API token or no OIDC. A
`ui` release with OIDC and no token answers every API request with a redirect to
an identity provider, so advertising it would have a console report a collector
it can never read.

**The token does not have to be configured into the console.** The last two
annotations name the Secret and key that hold it, so a console holding the
operator's kubeconfig reads it from the cluster and nothing is typed anywhere.
That is not a weakening: reading one Secret is strictly less than what that
kubeconfig already grants, and the token was never a barrier against the
operator — it is a barrier against whoever can reach the Service with no cluster
credential at all. A console that cannot read the Secret should say so ("a
collector is here, but I have no token for it") and fall back to the rung
beneath, rather than showing an empty panel.

### Authentication

`Authorization: Bearer <API_TOKEN>` on every request. `/healthz` and `/readyz`
stay open — the kubelet issues them and carries no credential. `/metrics` does
**not**: with no dashboard in front of it, it would otherwise be an
unauthenticated dump of the whole CronJob inventory.

With `API_TOKEN` unset every route answers anonymously. The process says so at
`WARN` on startup and the chart refuses to render an externally-reachable
release in that state (SEC-28).

---

## 3. Versioning, and how to degrade

Call `GET /api/v1/collector` first. It is cheap, and it is the whole
compatibility story:

- `api_versions` lists every contract this build speaks. **If none is
  recognised, fall back to the rung beneath — do not fail.** A degradation
  ladder can absorb an unknown rung; a hard dependency cannot.
- `capabilities` says which panels can be populated at all.
- Unknown JSON fields must be ignored. New fields are added within `v1`; fields
  are not removed or repurposed within a version.
- Any unmatched path under `/api/v1/` returns a JSON 404 naming `product` and
  `api_version`, so a request from a newer contract identifies what it reached
  rather than producing an HTML error page.

---

## 4. What only a collector has

Worth being precise, because it decides whether deploying one is worth it. Live
Kubernetes objects and Prometheus/kube-state-metrics between them already give
schedules, next runs, suspension, and — for as long as Prometheus retains them —
start times, durations and outcomes.

A collector adds four things nothing else has once the Job is garbage-collected:

1. **The log body of a run.** Prometheus holds no log lines. A cluster with Loki
   has them; a cluster with neither has lost them permanently.
2. **Exit codes and retry counts.**
3. **CPU/memory sampled every 15 s while the run was in flight** — metrics-server
   keeps no history, so a forty-second job is invisible to any coarser
   observation.
4. **Runs on clusters with no Prometheus at all.**

---

## 5. Honesty rules the shapes enforce

Absence is never reported as a fact. Three cases a consumer cannot otherwise
distinguish, and the field that distinguishes each:

| Looks like | Actually might be | Told apart by |
|---|---|---|
| A CronJob that never ran | The collector was installed after it last ran | `observed_since` on the cluster, and on the runs and daily responses |
| A run that printed nothing | Its log body aged out of `LOG_RETENTION_DAYS` | `expired` and `log_size_bytes` on the logs response |
| A run that used no CPU | The cluster has no Metrics API, or the run was shorter than one sampling interval | `metrics_enabled` per cluster, `interval_seconds` and `summary` on the samples response |

**There is no backfill.** A collector installed today has history from today.
Anything before `observed_since` was observed by nothing and must be drawn as
unwatched — a distinct state from succeeded, failed and missed. On a calendar
heatmap that is a fourth colour, not an empty square.

---

## 6. Endpoints

All `GET`. All JSON except `logs.txt`.

### `GET /api/v1/collector`

Discovery. Call it first.

```json
{
  "product": "kubecron",
  "version": "0.4.0",
  "api_version": "v1",
  "api_versions": ["v1"],
  "mode": "server",
  "read_only": true,
  "capabilities": {
    "run_history": true, "run_logs": true, "resource_samples": true,
    "live_log_stream": true, "mutations": false
  },
  "retention": { "run_days": 180, "log_days": 14 },
  "sample_interval_seconds": 15,
  "clusters": [
    { "id": "prod-eu", "name": "prod-eu", "metrics_enabled": true,
      "observed_since": "2026-08-01T09:12:44Z" }
  ],
  "server_at": "2026-08-21T09:45:51Z"
}
```

`retention` is what tells a consumer whether an empty query window means
"nothing happened" or "we no longer hold it".

### `GET /api/v1/clusters`

`{ "clusters": [ … ] }` — the same cluster objects as above.

### `GET /api/v1/clusters/{clusterID}/cronjobs`

Every CronJob the collector currently sees in that cluster, each with its last
run and 7-day statistics folded in.

```json
{
  "cluster_id": "prod-eu",
  "cronjobs": [{
    "id": "prod-eu/default/nightly-backup",
    "cluster_id": "prod-eu", "namespace": "default", "name": "nightly-backup",
    "schedule": "0 2 * * *",
    "time_zone": "Europe/Paris",
    "suspended": false,
    "next_run_at": "2026-08-22T00:00:00Z",
    "missed": false,
    "last_successful_time": "2026-08-21T00:00:31Z",
    "resources": { "cpu_request": "250m", "cpu_limit": "1",
                   "memory_request": "512Mi", "memory_limit": "1Gi" },
    "last_run": { … }, "stats_7d": { … },
    "updated_at": "2026-08-21T09:40:00Z"
  }]
}
```

- `next_run_at` is resolved **in the CronJob's own `time_zone`**. A consumer that
  recomputes the schedule must use the same zone or it will disagree with the
  cluster. It is **omitted**, not zeroed, when the schedule or zone cannot be
  resolved — a wrong countdown is worse than an absent one.
- `missed` means the previous scheduled occurrence produced no run *this
  collector saw*. It is only meaningful because something was watching; do not
  derive it from history alone.
- `resources` are Kubernetes quantity strings, empty when unset. Never a
  placeholder — a consumer needs the absence, not a rendering of it.
- Soft-deleted CronJobs are excluded, but their run history stays reachable.

### `GET /api/v1/clusters/{clusterID}/cronjobs/{ns}/{name}/runs`

Run history, newest first.

| Query | |
|---|---|
| `limit` | default 100, max 1000 |
| `before` | the previous page's `next_cursor` — **URL-encode it**, it contains a `+` |

```json
{
  "cronjob_id": "prod-eu/default/nightly-backup",
  "cluster_id": "prod-eu", "namespace": "default", "name": "nightly-backup",
  "runs": [ … ],
  "next_cursor": "2026-08-14T02:00:03.418Z",
  "observed_since": "2026-08-01T09:12:44Z"
}
```

An absent `next_cursor` means the last page. The cursor is opaque and carries
sub-second precision on purpose: truncating it to whole seconds would skip
parallel pods of the same CronJob. An unparseable cursor re-serves page one
rather than erroring (the server also recovers a `+` that was decoded to a space
by a client that forgot to escape it).

A run object:

```json
{
  "id": "b0f1…", "cronjob_id": "prod-eu/default/nightly-backup",
  "pod_name": "nightly-backup-29284560-x7k2n",
  "node_name": "ip-10-0-3-14", "container_image": "backup:1.4.2",
  "trigger": "scheduled",
  "started_at": "2026-08-21T00:00:02Z", "finished_at": "2026-08-21T00:00:31Z",
  "status": "succeeded", "exit_code": 0, "retry_count": 0,
  "duration_ms": 29411, "log_size_bytes": 8213,
  "avg_cpu_millicores": 180, "max_cpu_millicores": 410,
  "avg_memory_bytes": 201326592, "max_memory_bytes": 268435456
}
```

`status` is `running`, `succeeded` or `failed`. `trigger` is `scheduled` or
`manual`. Optional fields are omitted rather than zeroed.

### `GET /api/v1/clusters/{clusterID}/cronjobs/{ns}/{name}/daily`

One row per calendar day — what a heatmap needs, without shipping 130 000 run
records to draw 90 squares. `?days=` defaults to 90, max 365.

```json
{
  "cronjob_id": "…", "cluster_id": "…", "namespace": "…", "name": "…",
  "days": 90,
  "daily": [{ "day": "2026-08-21", "total": 1, "succeeded": 1, "running": 0 }],
  "observed_since": "2026-08-01T09:12:44Z"
}
```

Days absent from `daily` and earlier than `observed_since` are **unwatched**, not
idle.

### `GET /api/v1/runs/{id}`

One run, plus its CronJob's parts split out so the composite `cronjob_id` need
not be parsed.

```json
{ "run": { … }, "cronjob": { "id": "prod-eu/default/nightly-backup",
  "cluster_id": "prod-eu", "namespace": "default", "name": "nightly-backup" } }
```

### `GET /api/v1/runs/{id}/samples`

```json
{
  "run_id": "b0f1…",
  "interval_seconds": 15,
  "samples": [{ "id": 1, "run_id": "b0f1…", "sampled_at": "2026-08-21T00:00:17Z",
                "cpu_millicores": 410, "memory_bytes": 268435456 }],
  "summary": { "avg_cpu_millicores": 180, "max_cpu_millicores": 410,
               "avg_memory_bytes": 201326592, "max_memory_bytes": 268435456 }
}
```

An empty `samples` is **not** a claim that the run used nothing. It means the run
was shorter than `interval_seconds`, or the cluster's `metrics_enabled` is false,
or the raw points aged out — in which case `summary` survives them and says so.

### `GET /api/v1/runs/{id}/logs`

The captured log body. `?limit=N` returns the last N lines; the default is the
whole body, deliberately — a run's log exists nowhere else once the pod is gone,
so silently truncating it would be the worst available default.

```json
{
  "run_id": "b0f1…",
  "lines": [{ "ts": "2026-08-21T00:00:03Z", "line": "starting backup" }],
  "log_size_bytes": 8213,
  "expired": false,
  "truncated": false,
  "running": false
}
```

`expired: true` means the body was captured and has since been removed by log
retention — the difference between "this run printed nothing" and "we no longer
hold what it printed". `running: true` means the body is incomplete and `stream`
follows the rest.

### `GET /api/v1/runs/{id}/logs.txt`

The same body as `text/plain`, for saving to a file. An unknown run is a `404`,
not an empty file.

### `GET /api/v1/runs/{id}/stream`

Server-sent events, for a run still in flight. Long-lived; there is no write
timeout on it.

Optional for a consumer that can reach the live pod itself — but it needs no
`pods/log` RBAC of its own, and it works after the pod is gone, which direct
streaming does not.

---

## 7. `/metrics` — the 16 Prometheus families

A second way in, and a different one: the API answers *about a named CronJob on
demand*, the exporter answers *about everything, on a scrape*. A consumer
building a dashboard usually wants the API; a consumer building alerts wants
these.

**In server mode `/metrics` sits behind the bearer token** (the probes do not).
With no dashboard in front of it, this endpoint is otherwise an anonymous dump
of the whole inventory — AUDIT SEC-29/SEC-30. A Prometheus scraping a
token-protected collector needs an `authorization` block in its scrape config.

Gauges are re-derived from the database every 30 s, so they survive a restart
rather than going blank until each CronJob next fires. Counters and the
histogram stay event-driven — rebuilding them would double-count.

All CronJob-scoped families carry `cluster`, `namespace`, `cronjob`.

| Family | Type | Extra labels | Value |
|---|---|---|---|
| `kubecron_job_runs_total` | counter | `status`, `trigger` | Completed runs |
| `kubecron_job_duration_seconds` | histogram | — | Run duration; buckets 1, 5, 15, 30, 60, 300, 600, 1800 s |
| `kubecron_last_run_timestamp` | gauge | — | Unix time of the last run's start |
| `kubecron_last_run_status` | gauge | — | 0 = success, 1 = failure |
| `kubecron_last_run_duration_seconds` | gauge | — | Wall-clock duration of the most recent finished run |
| `kubecron_last_run_cpu_millicores` | gauge | — | Peak CPU observed during the most recent run |
| `kubecron_last_run_memory_bytes` | gauge | — | Peak memory observed during the most recent run |
| `kubecron_next_run_timestamp` | gauge | — | Unix time of the next scheduled run, resolved in the CronJob's own zone |
| `kubecron_cronjob_suspended` | gauge | — | 1 = suspended |
| `kubecron_cronjob_missed` | gauge | — | 1 = the last scheduled tick produced no run |
| `kubecron_runs_active` | gauge | — | Runs currently executing |
| `kubecron_cluster_cronjobs` | gauge | `cluster` only | Non-deleted CronJobs known for the cluster |
| `kubecron_cluster_metrics_api_available` | gauge | `cluster` only | 1 = the Metrics API answered the last probe |
| `kubecron_build_info` | gauge | `version` | Always 1 |
| `kubecron_http_requests_total` | counter | `route`, `method`, `status` | Requests served |
| `kubecron_http_request_duration_seconds` | histogram | `route` | Handler latency |

The two HTTP families label by **route pattern**, never by path: a path embeds
cluster, namespace, CronJob and run identifiers, so labelling by it would mint a
series per object and grow without bound. Unrouted requests share a single
`unmatched` label for the same reason — an unrouted path is attacker-controlled.

Two traps worth stating, because both read as data and are not:

- `kubecron_cronjob_missed` is only meaningful **because something was
  watching**. It says nothing about any period before the collector was
  installed, and a consumer must not aggregate it across such a period.
- `kubecron_last_run_status` has **no series at all** for a CronJob the
  collector has never seen run. Absent is not zero, and `absent()` is the
  PromQL that says so.

---

## 8. Stability

Within `v1`: fields may be added; existing fields are not removed, renamed or
repurposed; enumerations (`status`, `trigger`, `mode`) may gain members, so
handle an unknown one without crashing.

A breaking change mints `v2`, and `api_versions` lists both while `v1` is still
served. A consumer that pins nothing and degrades on the unknown never needs to
be released in lockstep with a collector — which is the point of keeping this
contract small.
