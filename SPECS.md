# KubeCron — Claude Code Specification

> **Historical.** The original build specification. It no longer describes the code (templ, a 7-day retention, …); see `README.md`, `CLAUDE.md` and `docs/`.

## Project Overview

KubeCron is a self-hosted, single-binary Go application deployed on a Kubernetes "hub" cluster
that aggregates, streams, and historizes logs from CronJobs across multiple Kubernetes clusters.
It exposes a real-time web UI, a REST API, and Prometheus metrics.

---

## Technical Stack

| Layer | Choice | Rationale |
|---|---|---|
| Language | Go 1.23+ | `net/http` enhanced routing, `slog`, ranging over integers |
| Kubernetes client | `k8s.io/client-go` | Informers, SharedInformerFactory, typed clients |
| HTML templating | `a-h/templ` | Type-safe, compiled templates — no runtime panics |
| Frontend interactivity | HTMX 2.x (CDN) | No build step, SSE support native |
| CSS | Tailwind CDN + custom CSS vars | Zero build step, utility-first |
| Database | `modernc.org/sqlite` | Pure Go, zero CGO, single-file DB |
| Migrations | `golang-migrate/migrate` | SQL migration files, embedded via `embed.FS` |
| Metrics | `prometheus/client_golang` | Standard `/metrics` endpoint |
| Logging | `log/slog` (stdlib) | Structured JSON logging, zero dependency |
| Config | `github.com/caarlos0/env/v11` | Env-based config, no YAML overhead |
| Container | Multi-stage → `gcr.io/distroless/static` | Minimal attack surface, no shell |
| Metrics client | `k8s.io/metrics` | PodMetrics API for CPU/RAM sampling |
| Cron parser | `github.com/robfig/cron/v3` | Next-run computation from schedule expression |

---

## Project Structure

```
kubecron/
├── cmd/
│   └── kubecron/
│       └── main.go                  # Wiring, signal handling, graceful shutdown
├── internal/
│   ├── cluster/
│   │   ├── manager.go               # Loads kubeconfigs, builds per-cluster clients
│   │   ├── registry.go              # Thread-safe map[clusterID]*ClusterClient
│   │   └── client.go                # ClusterClient struct (clientset + informerFactory)
│   ├── watcher/
│   │   ├── controller.go            # Top-level controller: starts watchers per cluster
│   │   ├── cronjob.go               # Informer on CronJobs → upsert DB, detect job launches
│   │   ├── job.go                   # Informer on Jobs → link to CronJob, detect pods
│   │   └── pod.go                   # Informer on Pods → trigger log stream on Running
│   ├── sampler/
│   │   ├── metrics_probe.go         # Probe Metrics API availability per cluster at startup
│   │   └── resource_sampler.go      # Poll PodMetrics every 15s during run → resource_samples
│   ├── schedule/
│   │   └── next.go                  # Compute next run timestamp from cron expression (robfig/cron/v3)
│   ├── streamer/
│   │   ├── logstream.go             # Streams pod logs line-by-line via client-go
│   │   └── broadcaster.go           # In-memory pub/sub: run_id → []chan string (SSE fan-out)
│   ├── storage/
│   │   ├── db.go                    # SQLite init, WAL mode, connection pool
│   │   ├── models.go                # Go structs mirroring DB tables
│   │   ├── queries.go               # All SQL read/write functions
│   │   └── retention.go             # Hourly cleanup goroutine (7-day window)
│   ├── metrics/
│   │   └── metrics.go               # Prometheus collectors registration + update helpers
│   ├── api/
│   │   ├── server.go                # `net/http` server, route registration (Go 1.22 patterns)
│   │   ├── middleware.go            # slog request logger, recover, CORS
│   │   ├── handlers_cluster.go      # GET /api/clusters
│   │   ├── handlers_cronjob.go      # GET|POST cronjob routes
│   │   ├── handlers_runs.go         # GET run history + stats
│   │   └── handlers_sse.go          # GET /api/runs/{id}/stream (SSE)
│   └── ui/
│       ├── templates/
│       │   ├── layout.templ         # Base layout, nav
│       │   ├── dashboard.templ      # Cluster overview cards
│       │   ├── cronjob_list.templ   # CronJob table with status badges + next run countdown
│       │   ├── run_detail.templ     # Live log terminal + resource mini-charts
│       │   └── stats.templ          # Success/failure/duration/resource charts (Chart.js CDN)
│       └── static/
│           └── app.css              # Custom CSS variables, terminal font
├── migrations/
│   ├── 000001_init.up.sql
│   ├── 000001_init.down.sql
│   ├── 000002_resource_samples.up.sql   # In case schema evolves
│   └── 000002_resource_samples.down.sql
├── k8s/
│   ├── namespace.yaml
│   ├── serviceaccount.yaml
│   ├── clusterrole.yaml
│   ├── clusterrolebinding.yaml
│   ├── secret-kubeconfigs-example.yaml
│   ├── pvc.yaml
│   ├── deployment.yaml
│   └── service.yaml
├── Dockerfile
├── .dockerignore
├── go.mod
└── go.sum
```

---

## Database Schema

File: `migrations/000001_init.up.sql`

```sql
CREATE TABLE clusters (
  id              TEXT PRIMARY KEY,
  name            TEXT NOT NULL,
  metrics_enabled INTEGER NOT NULL DEFAULT 0, -- 0 until Metrics API probe succeeds
  created_at      DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE cronjobs (
  id                   TEXT PRIMARY KEY,
  cluster_id           TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
  namespace            TEXT NOT NULL,
  name                 TEXT NOT NULL,
  schedule             TEXT NOT NULL,
  suspended            INTEGER NOT NULL DEFAULT 0,
  -- Resource requests/limits captured from jobTemplate.spec.containers[0]
  cpu_request          TEXT,   -- e.g. "100m"
  cpu_limit            TEXT,   -- e.g. "500m"
  memory_request       TEXT,   -- e.g. "128Mi"
  memory_limit         TEXT,   -- e.g. "512Mi"
  last_successful_time DATETIME,
  updated_at           DATETIME NOT NULL DEFAULT (datetime('now')),
  UNIQUE(cluster_id, namespace, name)
);

CREATE TABLE job_runs (
  id               TEXT PRIMARY KEY,
  cronjob_id       TEXT NOT NULL REFERENCES cronjobs(id) ON DELETE CASCADE,
  pod_name         TEXT NOT NULL,
  node_name        TEXT,                       -- filled when pod is scheduled
  container_image  TEXT,                       -- image used by the main container
  trigger          TEXT NOT NULL CHECK(trigger IN ('scheduled', 'manual')),
  started_at       DATETIME NOT NULL,
  finished_at      DATETIME,
  status           TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running','succeeded','failed')),
  exit_code        INTEGER,                    -- container exit code from pod status
  retry_count      INTEGER NOT NULL DEFAULT 0, -- pod restarts before success/fail
  log_size_bytes   INTEGER NOT NULL DEFAULT 0, -- running total, updated during stream
  -- Resource usage (NULL if metrics-server unavailable on that cluster)
  avg_cpu_millicores   INTEGER,
  max_cpu_millicores   INTEGER,
  avg_memory_bytes     INTEGER,
  max_memory_bytes     INTEGER,
  -- Computed
  duration_ms INTEGER GENERATED ALWAYS AS (
    CASE WHEN finished_at IS NOT NULL
    THEN CAST((julianday(finished_at) - julianday(started_at)) * 86400000 AS INTEGER)
    ELSE NULL END
  ) STORED
);

CREATE TABLE resource_samples (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id          TEXT NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
  sampled_at      DATETIME NOT NULL,
  cpu_millicores  INTEGER NOT NULL,
  memory_bytes    INTEGER NOT NULL
);

CREATE TABLE log_lines (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id TEXT NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
  ts     DATETIME NOT NULL DEFAULT (datetime('now')),
  line   TEXT NOT NULL
);

CREATE INDEX idx_log_lines_run        ON log_lines(run_id);
CREATE INDEX idx_resource_samples_run  ON resource_samples(run_id);
CREATE INDEX idx_job_runs_cronjob      ON job_runs(cronjob_id);
CREATE INDEX idx_job_runs_started      ON job_runs(started_at);
CREATE INDEX idx_cronjobs_cluster      ON cronjobs(cluster_id);
```

---

## Core Components — Implementation Details

### 1. `internal/cluster/manager.go`

- At startup, read all kubeconfigs from a directory mounted from a K8s Secret (default: `/etc/kubecron/kubeconfigs/`)
- Each file in the directory = one cluster. Filename (without extension) = `clusterID`
- For each kubeconfig: build a `*rest.Config`, then `*kubernetes.Clientset`, then `cache.SharedInformerFactory`
- Register each cluster in `registry.go`
- Upsert cluster into SQLite on load

```go
type Config struct {
  KubeconfigDir         string `env:"KUBECONFIG_DIR"          envDefault:"/etc/kubecron/kubeconfigs"`
  DBPath                string `env:"DB_PATH"                 envDefault:"/data/kubecron.db"`
  Port                  int    `env:"PORT"                    envDefault:"8080"`
  RetentionDays         int    `env:"RETENTION_DAYS"          envDefault:"7"`
  MetricsSampleInterval int    `env:"METRICS_SAMPLE_INTERVAL" envDefault:"15"` // seconds
}
```

### 2. `internal/watcher/controller.go`

Start one goroutine per cluster that runs:
```
SharedInformerFactory.Start(stopCh)
→ CronJobInformer  (AddEventHandler: OnAdd, OnUpdate)
→ JobInformer      (AddEventHandler: OnAdd, OnUpdate)
→ PodInformer      (AddEventHandler: OnUpdate — watch for phase Running/Succeeded/Failed)
```

**CronJob Informer** (`cronjob.go`):
- On Add/Update: upsert into `cronjobs` table (schedule, suspended flag)
- Extract `cpu_request`, `cpu_limit`, `memory_request`, `memory_limit` from `jobTemplate.spec.containers[0].resources`
- Update `last_successful_time` from `cronjob.Status.LastSuccessfulTime`

**Job Informer** (`job.go`):
- On Add: if `ownerReferences` contains a CronJob → create `job_runs` row with status=`running`
- If label `kubecron/trigger=manual` → set trigger=`manual`, else `scheduled`

**Pod Informer** (`pod.go`):
- On Update, when `pod.Status.Phase == Running` and pod owned by a tracked Job:
  - Set `job_runs.node_name` from `pod.Spec.NodeName`
  - Set `job_runs.container_image` from `pod.Spec.Containers[0].Image`
  - Call `streamer.Stream(ctx, clusterID, pod)`
  - Start `sampler.Start(ctx, clusterID, namespace, podName, runID)` if cluster has `metrics_enabled=1`
- On Update, when phase is `Succeeded` or `Failed`:
  - Update `job_runs.status` + `finished_at`
  - Set `exit_code` from `pod.Status.ContainerStatuses[0].State.Terminated.ExitCode`
  - Set `retry_count` from `pod.Status.ContainerStatuses[0].RestartCount`
  - Call `sampler.Stop(runID)` → compute + persist avg/max CPU and memory into `job_runs`

### 3.5 `internal/sampler/metrics_probe.go`

At cluster registration time, attempt to call the Metrics API:
```go
metricsClient.MetricsV1beta1().PodMetricses("default").List(ctx, metav1.ListOptions{Limit: 1})
```
- If success → set `clusters.metrics_enabled = 1` in DB + in-memory registry
- If error (not found / forbidden) → log at Info level, leave `metrics_enabled = 0`
- Re-probe every 5 minutes in background (cluster may install metrics-server later)

### 3.6 `internal/sampler/resource_sampler.go`

```go
func (s *Sampler) Start(ctx context.Context, clusterID, namespace, podName, runID string) {
  ticker := time.NewTicker(s.interval) // default 15s
  go func() {
    defer ticker.Stop()
    for {
      select {
      case <-ctx.Done():
        s.finalize(runID)
        return
      case <-ticker.C:
        pm, err := s.metricsClient.MetricsV1beta1().
          PodMetricses(namespace).Get(ctx, podName, metav1.GetOptions{})
        if err != nil { continue }
        cpu    := pm.Containers[0].Usage.Cpu().MilliValue()
        memory := pm.Containers[0].Usage.Memory().Value()
        s.store.InsertResourceSample(runID, cpu, memory)
      }
    }
  }()
}

// finalize: SELECT avg/max from resource_samples WHERE run_id=? → UPDATE job_runs
func (s *Sampler) finalize(runID string) { ... }
```

### 3.7 `internal/schedule/next.go`

```go
// Returns the next N scheduled times after `after` for a given cron expression
func NextRuns(expr string, after time.Time, n int) ([]time.Time, error) {
  schedule, err := cron.ParseStandard(expr)
  // ...
  times := make([]time.Time, n)
  t := after
  for i := range n {
    t = schedule.Next(t)
    times[i] = t
  }
  return times, nil
}
```

Used by:
- `GET /api/clusters/{clusterID}/cronjobs` → includes `next_run_at` per CronJob
- The CronJob list UI → shows countdown to next run

### 3. `internal/streamer/logstream.go`

```go
func (s *Streamer) Stream(ctx context.Context, clusterID, namespace, podName, runID string) {
  opts := &corev1.PodLogOptions{Follow: true, Timestamps: true}
  stream, _ := client.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(ctx)
  scanner := bufio.NewScanner(stream)
  for scanner.Scan() {
    line := scanner.Text()
    s.storage.InsertLogLine(runID, line)
    s.broadcaster.Publish(runID, line)
  }
}
```

- Run in its own goroutine, context cancelled when pod terminates
- Deduplicate: track `runID → streaming bool` to avoid double-streaming

### 4. `internal/streamer/broadcaster.go`

```go
type Broadcaster struct {
  mu   sync.RWMutex
  subs map[string][]chan string  // runID → subscriber channels
}

func (b *Broadcaster) Subscribe(runID string) (ch chan string, unsub func())
func (b *Broadcaster) Publish(runID, line string)
func (b *Broadcaster) Close(runID string)  // called when stream ends
```

### 5. `internal/api/handlers_sse.go`

```
GET /api/runs/{id}/stream
```

1. Check if run exists in DB. If `status != running` → replay all lines from SQLite and close.
2. If `status == running` → subscribe to broadcaster, write `data: {line}\n\n` per event, flush.
3. On client disconnect → unsub.

SSE headers:
```
Content-Type: text/event-stream
Cache-Control: no-cache
X-Accel-Buffering: no
```

### 6. `internal/metrics/metrics.go`

Register at init, update at event points:

```go
// Run lifecycle
kubecron_job_runs_total              CounterVec   [cluster, namespace, cronjob, status, trigger]
kubecron_job_duration_seconds        HistogramVec [cluster, namespace, cronjob]
                                     // Buckets: 1s, 5s, 15s, 30s, 60s, 300s, 600s, 1800s, +Inf

// CronJob state
kubecron_cronjob_suspended           GaugeVec     [cluster, namespace, cronjob]  // 0 or 1
kubecron_last_run_timestamp          GaugeVec     [cluster, namespace, cronjob]  // Unix
kubecron_last_run_status             GaugeVec     [cluster, namespace, cronjob]  // 0=ok 1=fail
kubecron_next_run_timestamp          GaugeVec     [cluster, namespace, cronjob]  // Unix

// Resource usage (only set when metrics_enabled=1 on cluster)
kubecron_job_cpu_millicores_avg      GaugeVec     [cluster, namespace, cronjob]
kubecron_job_cpu_millicores_peak     GaugeVec     [cluster, namespace, cronjob]
kubecron_job_memory_bytes_avg        GaugeVec     [cluster, namespace, cronjob]
kubecron_job_memory_bytes_peak       GaugeVec     [cluster, namespace, cronjob]

// Operational
kubecron_active_streams              Gauge        []
kubecron_cluster_metrics_enabled     GaugeVec     [cluster]  // 0 or 1
kubecron_log_size_bytes_total        CounterVec   [cluster, namespace, cronjob]
```

### 7. `internal/api/server.go`

Use Go 1.22+ `net/http` enhanced routing (method + path pattern):

```go
mux.HandleFunc("GET /api/clusters",                                          h.ListClusters)
mux.HandleFunc("GET /api/clusters/{clusterID}/cronjobs",                     h.ListCronJobs)
mux.HandleFunc("GET /api/clusters/{clusterID}/cronjobs/{ns}/{name}/runs",    h.ListRuns)
mux.HandleFunc("GET /api/runs/{id}/stream",                                  h.StreamLogs)
mux.HandleFunc("GET /api/runs/{id}/resources",                               h.GetResourceSamples) // time-series for chart
mux.HandleFunc("POST /api/clusters/{clusterID}/cronjobs/{ns}/{name}/suspend",  h.Suspend)
mux.HandleFunc("POST /api/clusters/{clusterID}/cronjobs/{ns}/{name}/resume",   h.Resume)
mux.HandleFunc("POST /api/clusters/{clusterID}/cronjobs/{ns}/{name}/trigger",  h.Trigger)
mux.Handle("GET /metrics",                                                   promhttp.Handler())
mux.HandleFunc("GET /healthz",                                               h.Healthz)
mux.HandleFunc("GET /readyz",                                                h.Readyz)
mux.HandleFunc("GET /",                                                      h.Dashboard)
mux.HandleFunc("GET /clusters/{clusterID}",                                  h.ClusterDetail)
mux.HandleFunc("GET /runs/{id}",                                             h.RunDetail)
```

---

## API Contracts

### `GET /api/clusters`
```json
[
  {
    "id": "prod-eu",
    "name": "prod-eu",
    "cronjob_count": 12,
    "running_count": 1,
    "metrics_enabled": true
  }
]
```

### `GET /api/clusters/{clusterID}/cronjobs`
```json
[
  {
    "id": "uuid",
    "namespace": "default",
    "name": "backup-db",
    "schedule": "0 2 * * *",
    "suspended": false,
    "next_run_at": "2026-05-03T02:00:00Z",
    "resources": {
      "cpu_request": "100m",
      "cpu_limit": "500m",
      "memory_request": "128Mi",
      "memory_limit": "512Mi"
    },
    "last_run": {
      "id": "uuid",
      "status": "succeeded",
      "trigger": "scheduled",
      "started_at": "2026-05-01T02:00:00Z",
      "duration_ms": 4200,
      "exit_code": 0,
      "node_name": "node-eu-3",
      "container_image": "myrepo/backup:v1.4.2",
      "log_size_bytes": 18240,
      "avg_cpu_millicores": 210,
      "max_cpu_millicores": 480,
      "avg_memory_bytes": 134217728,
      "max_memory_bytes": 201326592
    },
    "stats_7d": {
      "total": 7,
      "succeeded": 6,
      "failed": 1,
      "avg_duration_ms": 3900,
      "max_duration_ms": 8100,
      "avg_cpu_millicores": 198,
      "avg_memory_bytes": 130023424
    }
  }
]
```

### `GET /api/runs/{id}/resources`
```json
{
  "run_id": "uuid",
  "samples": [
    { "sampled_at": "2026-05-01T02:00:15Z", "cpu_millicores": 180, "memory_bytes": 128974848 },
    { "sampled_at": "2026-05-01T02:00:30Z", "cpu_millicores": 340, "memory_bytes": 145678912 }
  ]
}
```

### `POST /api/.../trigger` → `201 Created`
```json
{ "run_id": "uuid", "pod_name": "backup-db-manual-1234" }
```

---

## UI Design

**Aesthetic direction**: Industrial terminal — dark background (`#0a0a0f`), monospace
accents, neon green highlights (`#00ff88`), sharp grid lines. Think "ops dashboard",
not "SaaS startup". Feels like it belongs in a datacenter.

**Pages**:

1. **Dashboard** (`/`)
   - Cluster cards: name, CronJob count, running badge, `metrics_enabled` indicator
   - HTMX polling every 10s (`hx-get` + `hx-swap`) for live cluster state

2. **Cluster Detail** (`/clusters/{id}`)
   - Table of CronJobs: name, namespace, schedule, **next run countdown** (JS `setInterval`), last status badge, avg duration
   - Resource requests/limits shown as small badges (cpu/mem)
   - Suspend/Resume button (HTMX `hx-post` + toast)
   - Trigger button → confirm modal → POST → redirect to run detail

3. **Run Detail** (`/runs/{id}`)
   - Header: CronJob name, status badge, trigger type, duration, exit code, retry count, node name, image
   - **Resource mini-charts** (Chart.js, line chart): CPU millicores + Memory bytes over time, fetched from `/api/runs/{id}/resources`
     - Only rendered if `avg_cpu_millicores` is non-null (metrics-server available)
     - If run is live: poll `/api/runs/{id}/resources` every 15s via HTMX
   - **Log size** displayed in header (e.g. "18.2 KB")
   - Terminal-style log viewer: black bg, monospace, auto-scroll
   - If `status=running`: HTMX SSE (`hx-ext="sse"`, `sse-connect="/api/runs/{id}/stream"`)
   - If `status=succeeded/failed`: static replay from DB

4. **Fonts**: `JetBrains Mono` (CDN) for log terminal + code elements. `Syne` for headings.

5. **No JavaScript frameworks** — HTMX + Chart.js (CDN) + minimal vanilla JS for SSE auto-scroll and countdown timers.

---

## Kubernetes Manifests (`k8s/`)

### `pvc.yaml`
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: kubecron-data
  namespace: kubecron
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 5Gi
```

### `secret-kubeconfigs-example.yaml`
```yaml
# Each key = clusterID, value = base64 kubeconfig
apiVersion: v1
kind: Secret
metadata:
  name: kubecron-kubeconfigs
  namespace: kubecron
type: Opaque
data:
  prod-eu: <base64-kubeconfig>
  staging-us: <base64-kubeconfig>
```

### `deployment.yaml`
```yaml
spec:
  replicas: 1
  template:
    spec:
      serviceAccountName: kubecron
      containers:
      - name: kubecron
        image: kubecron:latest
        ports:
        - containerPort: 8080
        env:
        - name: DB_PATH
          value: /data/kubecron.db
        - name: KUBECONFIG_DIR
          value: /etc/kubecron/kubeconfigs
        volumeMounts:
        - name: data
          mountPath: /data
        - name: kubeconfigs
          mountPath: /etc/kubecron/kubeconfigs
          readOnly: true
        livenessProbe:
          httpGet: { path: /healthz, port: 8080 }
        readinessProbe:
          httpGet: { path: /readyz, port: 8080 }
        resources:
          requests: { cpu: 100m, memory: 128Mi }
          limits:   { cpu: 500m, memory: 512Mi }
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: kubecron-data
      - name: kubeconfigs
        secret:
          secretName: kubecron-kubeconfigs
```

### RBAC
```yaml
# ClusterRole needs: get/list/watch on cronjobs, jobs, pods, pods/log
# + patch on cronjobs (suspend)
# + create on jobs (trigger)
# + get/list on pods.metrics (metrics.k8s.io API group) for resource sampling
# Applied per spoke cluster via kubeconfig with sufficient permissions
```

---

## Dockerfile

```dockerfile
# Stage 1: generate templ
FROM golang:1.23-alpine AS templ
RUN go install github.com/a-h/templ/cmd/templ@latest
WORKDIR /app
COPY . .
RUN templ generate

# Stage 2: build
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY --from=templ /app .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o kubecron ./cmd/kubecron

# Stage 3: minimal runtime
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/kubecron /kubecron
ENTRYPOINT ["/kubecron"]
```

---

## `main.go` — Bootstrap Order

```
1. Parse config via env
2. Init slog (JSON handler, Info level)
3. Open SQLite, run migrations
4. Start retention goroutine
5. Load kubeconfigs → ClusterManager.Start()
6. Per cluster: probe Metrics API → set metrics_enabled flag
7. Start watcher controller per cluster
8. Start resource sampler (if metrics_enabled) per active run at startup (resume on restart)
9. Start HTTP server (with /healthz, /readyz)
10. Wait for SIGTERM/SIGINT → graceful shutdown (context cancel, drain HTTP)
```

Graceful shutdown timeout: 30s.

---

## Additional Requirements

- **`/healthz`**: always 200. **`/readyz`**: 200 only after all informer caches have synced (`cache.WaitForCacheSync`)
- **Error handling**: all DB errors logged via `slog`, never panic in handlers — return structured JSON error `{"error": "..."}` with appropriate HTTP status
- **Concurrency**: all DB writes go through a single `*sql.DB` with `PRAGMA journal_mode=WAL` and `PRAGMA busy_timeout=5000`
- **Idempotency**: watcher OnAdd handlers must be idempotent (use `INSERT OR REPLACE`)
- **Log lines storage**: batch inserts every 100ms or 50 lines (whichever comes first) to avoid SQLite write contention during heavy log streams
- **log_size_bytes**: incremented atomically (via `UPDATE job_runs SET log_size_bytes = log_size_bytes + ? WHERE id = ?`) after each batch insert
- **Resource sampler graceful degradation**: if Metrics API returns an error mid-run (transient), log at Warn and retry next tick — do not abort the sampler goroutine
- **Restart recovery**: on startup, query `SELECT id FROM job_runs WHERE status = 'running'` — mark them as `failed` with a note `exit_code = -1` (process was killed mid-run, logs are partial)
- **next_run_at**: computed in-process via `schedule.NextRuns()`, never stored in DB — always fresh on API response
- **go.mod module name**: `github.com/kubecron/kubecron`
- **Minimum Go version in go.mod**: `go 1.23`

---

## What NOT to implement

- No authentication (out of scope — use K8s ingress auth or a reverse proxy)
- No multi-tenancy
- No alerting (use Prometheus Alertmanager on top of the exposed metrics)
- No log search/full-text (SQLite FTS can be added later as an extension)