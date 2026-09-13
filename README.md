# KubeCron

> Monitor and control Kubernetes CronJobs across multiple clusters — live logs, resource tracking, and a clean web dashboard.

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26+-00ADD8.svg)](https://golang.org/)
[![Kubernetes](https://img.shields.io/badge/kubernetes-%E2%89%A51.21-326CE5.svg)](https://kubernetes.io/)

---

> **Alpha software** — Personal project maintained by a single developer with the help of an AI coding agent. Expect rough edges. Contributions welcome, response times may vary.

---

## Why?

CronJobs are invisible by default. You define a schedule, deploy it, and hope it runs correctly — but the only way to know is to `kubectl logs` into the right pod at the right time, across each cluster.

**KubeCron** gives you a single pane of glass:

- **Live log streaming** — watch CronJob pod logs in real time as they execute, via SSE
- **Log tooling** — severity colorization, regex search with highlighting, plain-text `.log` download
- **Run history** — every execution recorded with status, duration, exit code, and retry count
- **History visuals** — duration sparkline per CronJob, 90-day calendar heatmap with click-to-filter, missed/concurrent run badges
- **Resource usage** — CPU and memory sampled every 15 s when metrics-server is available; avg/max computed per run
- **7-day statistics** — success rate, average and max duration per CronJob
- **Next-run countdown** — computed from the cron expression in the CronJob's own `spec.timeZone`, updated live in the browser; missed-run detection uses the same zone
- **Suspend / Resume / Trigger** — control CronJobs directly from the UI without touching kubectl
- **Multi-cluster** — one kubeconfig file per cluster in a directory; all clusters shown in a unified dashboard
- **Focus rankings** — every view leads with success rate, failures, in-flight and suspended counts, then ranks CronJobs by most failures, longest mean duration, and highest peak CPU / memory over 24 h, 7 d or 30 d. Fleet-wide when you run several clusters, per-cluster on the cluster view
- **OIDC authentication** — optional SSO via Keycloak, Dex, Google, or any OIDC provider
- **Prometheus metrics** — 16 metric families at `/metrics` for Grafana integration, including missed-schedule and in-flight-run gauges. Gauges are re-derived from the database every 30 s, so they keep reporting across a restart instead of going blank until each CronJob next fires
- **Collector mode** — the same binary, run headless as a per-cluster recorder that serves a read-only versioned API instead of a dashboard. See below

---

## Two ways to run it

KubeCron is one binary with two modes, selected by `KUBECRON_MODE`. Collection is
identical in both — same informers, same 15-second sampler, same log capture and
retention. Only the HTTP surface differs, and a release can be switched between
them without losing its history.

**`ui`** (the default) is everything above: the dashboard, the API behind it, and
suspend/resume/trigger. Run one, point it at a directory of kubeconfigs, and it
watches your whole fleet.

**`server`** is a headless collector. It serves only the read-only `/api/v1`
contract — no HTML, no mutating routes, and the Helm chart grants it no mutating
RBAC verb, so "read-only" is checkable from outside the process. Deploy one per
cluster, watching its own cluster through its own ServiceAccount, and read it on
demand from a console.

That split exists because a recorder and a console want opposite things. A
recorder has to be running when a job runs: Kubernetes garbage-collects finished
Jobs after `successfulJobsHistoryLimit` (3 by default), which for a job that runs
every minute is a **three-minute** window before its duration, exit code and logs
are gone for good. A console on a laptop that sleeps cannot be that. A Deployment
in the cluster can.

```bash
helm install kubecron charts/kubecron -n kubecron --create-namespace \
  --set mode=server \
  --set clusterID=prod-eu \
  --set api.token="$(openssl rand -hex 32)"
```

Credentials stop travelling — no kubeconfig Secret, RBAC is read-only — and the
Service stays `ClusterIP`, reached by port-forward, so it needs no Ingress and no
certificate. The chart labels it `kubecron.io/collector=true` so a console can
find it instead of being configured with a URL per cluster.

Full wire contract: **[docs/COLLECTOR-API.md](docs/COLLECTOR-API.md)** — with an
OpenAPI 3.1 spec at **[docs/openapi.yaml](docs/openapi.yaml)**.

---

## Requirements

| Requirement | Minimum version |
|---|---|
| Kubernetes | **1.21** (`batch/v1` CronJobs) |
| metrics-server | any (optional, enables live CPU/RAM tracking) |
| Go | 1.26+ (build only) |

---

## Install

### Helm (recommended)

```bash
# Encode your kubeconfig(s) in base64 — one per cluster
KC=$(kubectl config view --minify --raw | base64 -w0)

helm install kubecron oci://ghcr.io/schmitech-fr/charts/kubecron \
  --namespace kubecron --create-namespace \
  --set "kubeconfigs.data.my-cluster=$KC"
```

Or from source:

```bash
git clone https://github.com/schmitech-fr/kubecron.git
cd kubecron

KC=$(kubectl config view --minify --raw | base64 -w0)

helm install kubecron ./charts/kubecron \
  --namespace kubecron --create-namespace \
  --set "kubeconfigs.data.my-cluster=$KC" \
  --set config.retentionDays=30          # keep 30 days of run history
```

Key Helm values:

| Value | Default | Description |
|---|---|---|
| `config.retentionDays` | `90` | Days of run history (metadata) to keep |
| `config.logRetentionDays` | `14` | Days of raw log lines to keep (≤ `retentionDays`) |
| `config.metricsSampleInterval` | `15` | Resource sampling interval (seconds) |
| `persistence.size` | `500Mi` | PVC size for SQLite data |
| `ingress.enabled` | `false` | Expose via Ingress — requires OIDC, see below |
| `oidc.enabled` | `false` | Enable OIDC authentication |
| `security.acknowledgeInsecureExposure` | `false` | Allow external exposure without OIDC (see below) |

> ⚠️ **Exposing KubeCron requires authentication.**
> KubeCron has no authentication of its own: with `oidc.enabled=false`, *every*
> endpoint is anonymous — including `suspend`, `resume` and `trigger`, which act
> on **every cluster whose kubeconfig is mounted**.
>
> The chart therefore **refuses to install** when the service is reachable from
> outside the cluster (`ingress.enabled=true`, or `service.type` other than
> `ClusterIP`) while OIDC is off. Either set `oidc.enabled=true`, or keep the
> default ClusterIP and use `kubectl port-forward`.
>
> If authentication is genuinely enforced upstream — an authenticating proxy, a
> service mesh, a VPN-only ingress — set
> `security.acknowledgeInsecureExposure=true` to proceed deliberately.
>
> Note that `ingress.tls` is empty by default, so configure TLS as well.

Full list of values: [`charts/kubecron/values.yaml`](charts/kubecron/values.yaml).

### Docker Compose (local)

```bash
git clone https://github.com/schmitech-fr/kubecron.git && cd kubecron

# Place kubeconfig files in dev/kubeconfigs/
cp ~/.kube/config dev/kubeconfigs/local.yaml

docker compose up --build
```

Open http://localhost:8080.

### Local dev

```bash
# Requires: Go 1.26+ only — no codegen, no Node.js build step.
# Place kubeconfig files in dev/kubeconfigs/, then:
go run ./cmd/kubecron
```

---

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `KUBECRON_MODE` | `ui` | `ui` serves the dashboard and controls; `server` serves only the read-only `/api/v1` collector API. Aliases: `standalone`, `collector` |
| `API_TOKEN` | _(empty)_ | Bearer token required on every request in `server` mode, except `/healthz` and `/readyz`. Empty = anonymous |
| `CLUSTER_ID` | `local` | Name this cluster reports as when it watches itself through its own ServiceAccount. Stored rows key off it — changing it on a populated database orphans that history |
| `KUBECONFIG_DIR` | `/etc/kubecron/kubeconfigs` | Directory with one kubeconfig file per cluster |
| `DB_PATH` | `/data/kubecron.db` | SQLite database file path |
| `PORT` | `8080` | HTTP listen port |
| `RETENTION_DAYS` | `90` | How many days of run history (metadata) to keep |
| `LOG_RETENTION_DAYS` | `14` | How many days of raw log lines to keep (≤ `RETENTION_DAYS`) |
| `METRICS_SAMPLE_INTERVAL` | `15` | Resource sampling interval in seconds (requires metrics-server) |
| `OIDC_ENABLED` | `false` | Enable OIDC/SSO authentication |
| `OIDC_ISSUER_URL` | _(empty)_ | OIDC provider issuer URL |
| `OIDC_CLIENT_ID` | _(empty)_ | OIDC client ID |
| `OIDC_CLIENT_SECRET` | _(empty)_ | OIDC client secret (store in a K8s Secret) |
| `OIDC_REDIRECT_URL` | _(empty)_ | `https://<host>/auth/callback` — must be HTTPS in production |
| `OIDC_SESSION_KEY` | _(empty)_ | ≥32-char random string for session signing (store in a K8s Secret) |
| `OIDC_ALLOWED_EMAILS` | _(empty)_ | Comma-separated allow-list of emails permitted to log in (empty = any account) |
| `OIDC_OPERATOR_EMAILS` | _(empty)_ | Comma-separated emails allowed to suspend/resume/trigger; others are read-only (empty = all operators) |

See `.env.example` for a commented template.

---

## Architecture

```
Browser → Go HTTP server (port 8080)
            ├── Kubernetes API (informers: CronJob, Job, Pod)
            ├── metrics-server (optional — PodMetrics every 15s)
            ├── SQLite (runs, logs, resource samples)
            └── SSE broadcaster (live log streaming to browser)
```

KubeCron is a **single binary**. It connects directly to each Kubernetes cluster via kubeconfig files using `client-go` informers. No sidecar, no separate database process, no frontend build step.

- **Informers** watch CronJob, Job, and Pod events and update the SQLite database in real time.
- **Log streaming** uses `client-go` `GetLogs(Follow=true)` and broadcasts lines via an in-memory pub/sub to SSE subscribers.
- **Resource sampling** polls the Metrics API every `METRICS_SAMPLE_INTERVAL` seconds per running pod (only on clusters where metrics-server is reachable).
- **HTML** is server-rendered directly in Go (`internal/api/*.go`) — no template engine, no codegen, no runtime template parsing.

---

## Security

- **Minimal RBAC** — the ClusterRole grants only `get`/`list`/`watch` on CronJobs, Jobs, and Pods, plus `patch` on CronJobs and `create` on Jobs (for suspend/resume/trigger). No access to Secrets. In `mode: server` the mutating verbs are not granted at all.
- **Collector mode is read-only structurally** — it registers no mutating route and holds no mutating RBAC verb, so the claim does not rest on configuration being right. Its front door is `API_TOKEN` (bearer) rather than OIDC, because a program cannot complete a browser redirect flow.
- **The chart refuses an unauthenticated public install** — `ingress.enabled=true`, or any `service.type` other than `ClusterIP`, fails the install unless a front door is configured for the mode in use (`oidc.enabled` in `ui`, `api.token` in `server`) or `security.acknowledgeInsecureExposure=true` states the decision deliberately.
- **Distroless runtime image** — `gcr.io/distroless/static:nonroot`, no shell, no package manager.
- **OIDC authentication** — when enabled, all routes require a valid session. The session key is never logged.
- **Authorization** — optionally restrict login to an email allow-list (`OIDC_ALLOWED_EMAILS`) and limit mutating actions (suspend/resume/trigger) to operators (`OIDC_OPERATOR_EMAILS`); everyone else is read-only.
- **No token forwarding** — KubeCron uses its own Service Account, not user tokens. RBAC is enforced at the cluster level.
- **Structured logging** — `log/slog` JSON output, no sensitive data ever logged.

---

## API

### `/api/v1` — the collector contract

Read-only, versioned, and served in **both** modes, so a console can read a
KubeCron you already run rather than needing a second one. This is the only part
of the HTTP surface another program should depend on — full documentation and
compatibility rules in **[docs/COLLECTOR-API.md](docs/COLLECTOR-API.md)**, with an
OpenAPI 3.1 spec at **[docs/openapi.yaml](docs/openapi.yaml)** to generate a client from.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/collector` | Discovery: version, capabilities, retention, clusters. Call this first |
| `GET` | `/api/v1/clusters` | Clusters this collector observes, and since when |
| `GET` | `/api/v1/clusters/{id}/cronjobs` | CronJobs with next run, missed state, last run, 7-day stats |
| `GET` | `/api/v1/clusters/{id}/cronjobs/{ns}/{name}/runs` | Run history, paged (`limit`, `before`) |
| `GET` | `/api/v1/clusters/{id}/cronjobs/{ns}/{name}/daily` | Per-day aggregates for a heatmap (`days`) |
| `GET` | `/api/v1/runs/{id}` | One run, with its CronJob's identity |
| `GET` | `/api/v1/runs/{id}/samples` | Resource sample series + the run's computed summary |
| `GET` | `/api/v1/runs/{id}/logs` | Captured log body (JSON, `limit` for the tail) |
| `GET` | `/api/v1/runs/{id}/logs.txt` | The same body as plain text |
| `GET` | `/api/v1/runs/{id}/stream` | SSE stream of a run still in flight |

### `/api` — the dashboard's own API (`ui` mode only)

Unversioned: it backs the HTMX fragments and changes with them.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/clusters` | List clusters |
| `GET` | `/api/clusters/{id}/cronjobs` | List CronJobs with next-run, last-run, 7-day stats |
| `GET` | `/api/clusters/{id}/cronjobs/{ns}/{name}/runs` | Run history |
| `POST` | `/api/clusters/{id}/cronjobs/{ns}/{name}/suspend` | Suspend a CronJob |
| `POST` | `/api/clusters/{id}/cronjobs/{ns}/{name}/resume` | Resume a CronJob |
| `POST` | `/api/clusters/{id}/cronjobs/{ns}/{name}/trigger` | Trigger a manual run |
| `GET` | `/api/runs/{id}/stream` | SSE stream of live log lines |
| `GET` | `/api/runs/{id}/resources` | Resource sample time-series |
| `GET` | `/api/runs/{id}/logs.txt` | Plain-text log download |

### Both modes

| Method | Path | Description |
|---|---|---|
| `GET` | `/metrics` | Prometheus metrics (behind `API_TOKEN` in `server` mode) |
| `GET` | `/healthz` | Health check (always 200, never authenticated) |
| `GET` | `/readyz` | Readiness (200 when informer caches synced, never authenticated) |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache 2.0 — see [LICENSE](LICENSE).
