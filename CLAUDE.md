# KubeCron — CLAUDE.md

Context file for Claude Code. Covers architecture, commands, conventions, and definition of done.

---

## Project Overview

KubeCron is a **single-binary Go application** that monitors Kubernetes CronJob executions across multiple clusters. It provides live log streaming, run history, resource tracking, and CronJob control (suspend/resume/trigger) via a server-rendered web UI.

**Two modes, one binary** (`KUBECRON_MODE`, default `ui`):

- **`ui`** (aliases: `standalone`) — the product above: dashboard, the JSON API behind its HTMX fragments, and the CronJob controls.
- **`server`** (aliases: `collector`) — headless recorder. Serves only the read-only `/api/v1` contract; registers no HTML route and no mutating route, and the chart grants it no mutating RBAC verb. Meant to run one-per-cluster on its own ServiceAccount and be read on demand by a console (KubeDeck) that was not running when a run happened.

Collection is identical in both modes — same informers, sampler, log capture, retention. Only route registration and the middleware chain differ (`internal/api/server.go`). Contract: `docs/COLLECTOR-API.md`.

- **Language**: Go 1.26, stdlib `net/http` router (Go 1.22+ enhanced routing), `log/slog` structured logging
- **UI rendering**: Raw HTML via `fmt.Fprintf` in `internal/api/html.go` + `html_components.go` — no template engine, no Node.js build step
- **Database**: SQLite via `modernc.org/sqlite` (pure Go, no CGO, WAL mode, embedded migrations)
- **Kubernetes**: `k8s.io/client-go` informers (CronJob, Job, Pod), MetricsClient for resource sampling
- **Frontend**: HTMX 2.x (CDN) + custom CSS (`internal/ui/static/app.css`, embedded) + Chart.js (CDN) — no Node.js build step
- **Auth**: OIDC optional (`coreos/go-oidc/v3` + `golang.org/x/oauth2`); HMAC-signed session cookie, 24 h TTL
- **Metrics**: Prometheus via `prometheus/client_golang`; 16 wired metric families exposed at `/metrics`. Gauges are re-derived from the DB by `metrics.StateCollector` every 30 s so they survive a restart; counters/histograms stay event-driven
- **Infra**: Two-stage Dockerfile (`golang:1.27-alpine` build → `gcr.io/distroless/static:nonroot`), CI with golangci-lint + SBOM + cosign; images currently `linux/amd64` only (arm64 planned — AUDIT INFRA-3)

---

## Repository Layout

```
cmd/kubecron/
  main.go                     # Bootstrap, config parsing (caarlos0/env), service wiring, graceful shutdown

internal/
  api/
    server.go                 # HTTP server setup, route registration
    middleware.go             # Logging, recovery, auth middleware
    csrf.go                   # CSRF double-submit cookie protection
    rate_limiter.go           # Fixed-window rate limiter (login, trigger)
    handlers_cluster.go       # Dashboard, ClusterDetail, NamespaceDetail pages
    handlers_cronjob.go       # Suspend/resume/trigger endpoints
    handlers_runs.go          # Run list, run detail, SSE stream, log download
    handlers_sse.go           # GET /api/runs/{id}/stream (SSE log streaming)
    handlers_v1.go            # The /api/v1 collector contract (read-only, versioned)
    mode.go                   # Mode (ui | server) and what each permits
    bearer.go                 # BearerAuth — server mode's front door (API_TOKEN)
    html.go                   # Shared HTML head/foot, nav, breadcrumb, countdown, statusBadge
    html_components.go        # Reusable CronJob row, run row, action buttons, table headers
    html_log.go               # logSearchBar, logSearchJS
    html_chart.go             # sparklineSVG, heatmapHTML
    html_overview.go          # statTile, topList, rangeSwitchFor, sectionHeading, byte/CPU formatting
  auth/
    auth.go                   # OIDC discovery, login/callback/logout, session management
  cluster/
    manager.go                # Load kubeconfigs, build per-cluster clients
    client.go                 # ClusterClient (Clientset + InformerFactory + MetricsClient)
    registry.go               # Thread-safe cluster registry
  watcher/
    controller.go             # Per-cluster informer controller
    cronjob.go                # CronJob informer event handlers
    job.go                    # Job informer event handlers (run tracking)
    pod.go                    # Pod informer event handlers (logs, metrics, status)
    run_index.go              # Thread-safe jobName→runID map (O(1) lookup for PodHandler)
  streamer/
    logstream.go              # Stream pod logs via client-go
    broadcaster.go            # In-memory pub/sub for SSE (run_id → subscribers)
  sampler/
    metrics_probe.go          # Probe Metrics API availability at startup + re-probe every 5 min
    resource_sampler.go       # Poll PodMetrics every METRICS_SAMPLE_INTERVAL s
  storage/
    db.go                     # SQLite connection, WAL mode, migrations
    models.go                 # Go structs for DB tables
    queries.go                # All SQL operations (upsert, list, update, delete)
    retention.go              # Background cleanup (delete runs older than RETENTION_DAYS)
  metrics/
    metrics.go                # Prometheus collectors (16 families, all wired)
    collector.go              # StateCollector: re-derives gauges from stored state every 30 s (OBS-3)
  schedule/
    next.go                   # Compute next N runs from cron expression
  ui/
    static/                   # Embedded CSS (app.css)

docs/
  AUDIT.md                    # Engineering audit, pass history and finding ids
  PRODUCT.md                  # Product strategy: dependency levers, phase plan, anti-goals
  COLLECTOR-API.md            # The /api/v1 wire contract consumed by KubeDeck
  openapi.yaml                # OpenAPI 3.1 of /api/v1 — machine-readable companion

migrations/                   # SQL files embedded in binary via embed.FS
charts/kubecron/              # Helm chart (cluster installs)
  Chart.yaml
  values.yaml
  templates/
dev/kubeconfigs/              # Local dev kubeconfigs (gitignored content, directory committed)
```

---

## Key Commands

```bash
# Build
go build ./...

# Vet & lint
go vet ./...
golangci-lint run

# Test
go test ./...

# Run locally (kubeconfigs in dev/kubeconfigs/)
go run ./cmd/kubecron

# Docker Compose (live reload)
docker compose up --build
```

---

## Environment Variables

See `.env.example`. Key variables:

| Variable | Default | Description |
|---|---|---|
| `KUBECRON_MODE` | `ui` | `ui` serves the dashboard + controls; `server` serves only the read-only `/api/v1` collector API. Aliases: `standalone`, `collector` |
| `API_TOKEN` | _(empty)_ | Bearer token required on every request in `server` mode (except `/healthz`, `/readyz`). Empty = anonymous |
| `CLUSTER_ID` | `local` | Name this cluster reports as when watching itself via its own ServiceAccount. Every stored row keys off it |
| `KUBECONFIG_DIR` | `/etc/kubecron/kubeconfigs` | One kubeconfig file per cluster |
| `DB_PATH` | `/data/kubecron.db` | SQLite database file |
| `PORT` | `8080` | HTTP listen port |
| `RETENTION_DAYS` | `90` | Days of run history (job_runs) to keep |
| `LOG_RETENTION_DAYS` | `14` | Days of raw log lines to keep (run metadata kept until RETENTION_DAYS) |
| `METRICS_SAMPLE_INTERVAL` | `15` | Resource sampling interval (seconds) |
| `OIDC_ENABLED` | `false` | Enable OIDC/SSO |
| `OIDC_ISSUER_URL` | _(empty)_ | OIDC provider issuer URL |
| `OIDC_CLIENT_ID` | _(empty)_ | OIDC client ID |
| `OIDC_CLIENT_SECRET` | _(empty)_ | OIDC client secret (store in K8s Secret) |
| `OIDC_REDIRECT_URL` | _(empty)_ | `https://<host>/auth/callback` — must be HTTPS |
| `OIDC_SESSION_KEY` | _(empty)_ | Any string ≥32 chars; SHA-256 derived internally (store in K8s Secret) |
| `OIDC_ALLOWED_EMAILS` | _(empty)_ | Comma-separated allow-list of emails permitted to log in (empty = any account) |
| `OIDC_OPERATOR_EMAILS` | _(empty)_ | Comma-separated emails allowed to suspend/resume/trigger; others read-only (empty = all operators) |

---

## Database Schema

Tables: `clusters`, `cronjobs`, `job_runs`, `resource_samples`, `log_lines`.

- `clusters` — one row per kubeconfig file; `metrics_enabled` flag set at startup and re-probed every 5 min; `deleted_at` set when the kubeconfig is removed
- `cronjobs` — synced from informer; stores schedule, `time_zone` (from `spec.timeZone`), suspended state, resource requests/limits, `deleted_at`
- `job_runs` — one row per CronJob execution; status: `running` → `succeeded`/`failed`; stores avg/max CPU+RAM
- `resource_samples` — raw PodMetrics samples (15 s interval) linked to a `job_run`
- `log_lines` — streamed pod log lines linked to a `job_run`

Migrations are embedded SQL files in `migrations/` applied at startup via `embed.FS`.

**Soft delete:** listings (`ListCronJobs`, `ListClusters`) filter on `deleted_at IS NULL`; `GetCronJobByName` deliberately does not, so existing links to the run history of a deleted CronJob keep working. Any new listing query must filter too.

**Indexes:** the CronJob-row read path (`GetLastJobRun`, `GetRunStats7d`, `GetRecentDurations`) depends on `idx_job_runs_cronjob_started(cronjob_id, started_at DESC)` to avoid a temp-B-tree sort per query. Check `EXPLAIN QUERY PLAN` before changing those queries' `WHERE`/`ORDER BY`.

---

## Security Model

- **Service Account RBAC** — `get`/`list`/`watch` on CronJobs, Jobs, Pods; `patch` on CronJobs; `create` on Jobs. No Secrets access. In `mode: server` the chart grants **only** the read verbs: collector mode registers no mutating route, and dropping the verbs makes that claim checkable from outside the process.
- **Collector front door** — `mode: server` uses `API_TOKEN` (bearer), not OIDC: an unauthenticated request under OIDC is answered with a 302 to an identity provider, which a machine client cannot follow. `/healthz` and `/readyz` stay open for the kubelet; `/metrics` does not (SEC-29 — with no dashboard in front of it, it would be an anonymous inventory dump).
- **Distroless image** — `gcr.io/distroless/static:nonroot`; runs as non-root.
- **No CGO** — `CGO_ENABLED=0`; pure Go binary.
- **OIDC session** — signed session cookie; session key stored in K8s Secret; never logged.
- **Authorization** — optional login allow-list (`OIDC_ALLOWED_EMAILS`) and operator role (`OIDC_OPERATOR_EMAILS`) restricting suspend/resume/trigger; others read-only.
- **CSRF & rate limiting** — double-submit cookie validated on every POST; fixed-window rate limits on `/auth/login` (10/min) and trigger (20/min) per source IP.
- **Kubeconfig data** — never logged server-side.

---

## Known Issues & Backlog

Full detail, evidence, and history: `docs/AUDIT.md` (IDs below reference it).

### Security — Medium Priority

- **SEC-22** — htmx/Chart.js/fonts loaded from CDNs without SRI; breaks air-gapped installs. Fix: vendor into `internal/ui/static/`.

### Security — Low Priority

- **SEC-23 (partial)** — nosniff/XFO/Referrer-Policy shipped; CSP (needs inline-script nonce refactor) and HSTS still missing.
- **SEC-24** — rate limiter proxy-blind (`RemoteAddr` behind ingress = collective lockout) with unbounded bucket map.
- **SEC-29** — in `mode: ui`, `/metrics` is auth-exempt and discloses the full cluster/namespace/CronJob inventory, schedules, run outcomes and resource usage. Keep it off the public Ingress; scope the scrape with a NetworkPolicy. (In `mode: server` it sits behind `API_TOKEN` — see SEC-30.)
- **SEC-30** — `mode: server` defaults to anonymous: with `API_TOKEN` unset, `/api/v1` and `/metrics` disclose the CronJob inventory, run outcomes and **captured log bodies** to anyone who can reach the Service. Mitigated (chart guard on external exposure, startup `slog.Warn`, token covers `/metrics`), not closed — a ClusterIP install is still anonymous by default.
- **INFRA-1** — base images use floating tags. Fix: pin with `@sha256:...` — but **only after SUP-1**, or the pin freezes a vulnerable stdlib into every release.

### Infra & CI — Medium Priority

- **SUP-1** — no CVE gate in CI. `govulncheck ./...` reported 14 reachable stdlib advisories at the `go 1.26.0` floor; the release image escaped them only because the base tag floats. The build now uses `golang:1.27-alpine` (CI and Dockerfile, 2026-08-21), but **`go.mod` still declares `go 1.26.0`**, so the floor — and what govulncheck measures against — is unchanged. Add `govulncheck` to `ci.yml` and an image scan to `docker-publish.yml` **before** pinning digests (INFRA-1).
- **INFRA-3** — release images are `linux/amd64` only; the arm64 claim is not implemented in `docker-publish.yml` (no QEMU step). Implement or drop the claim.
- **INFRA-5** — CI is skipped entirely for `renovate[bot]`; Renovate already has nine open branches, so its PRs would merge untested.

### Performance — Medium Priority

- **PERF-1** — log lines stored in SQLite; `log_lines` can grow large before retention. Planned: S3 log storage backend.

### Testing — Medium Priority

- **TEST-1** — schedule (incl. timezone/DST), auth, storage (incl. the migration upgrade path and run paging), broadcaster, watcher (Job + CronJob handlers), HTTP handlers, mode routing and the `/api/v1` contract covered; `go test -race` runs in CI (`ci.yml`). Still missing: PodHandler, sampler, Streamer, cluster.Manager; no coverage threshold.

### Resolved (archived)

- **BUG-22** — missed-run detection was silently off for anything sparser than daily _(v0.4.0)_. `PrevRun` scanned back a fixed 25 hours, so a weekly CronJob had no occurrence in the window on six days out of seven and a monthly one on twenty-nine days out of thirty; it returned an error, `IsMissed` read that as "cannot say" and answered **false**. A healthy monthly job and one silent for seventy days gave the same answer, so nothing looked wrong. The window is now derived from the schedule's own cadence and expanded — which also cut the per-minute case from ~1 500 `Next()` calls per row to ~3. Second half: when the most recent occurrence is still inside `MissedGracePeriod`, `IsMissed` steps back to the one before rather than answering false, because a per-minute CronJob is inside *some* occurrence's grace period at every instant and could therefore never be reported missed at all. **The step-back only applies when a run exists to compare against** — with none, the earlier occurrence may predate the CronJob, and a job created three minutes ago must not be flagged for a tick from before it existed. Found while porting this package to KubeDeck; the two implementations were then diffed over 13 scenarios and agree on all of them.

- **BUG-21** — run-history paging returned nothing past the first page _(v0.4.0)_. The driver stores a `time.Time` in Go's `time.Time.String()` layout, which is not ISO 8601, so SQLite's `datetime()` returned NULL and `datetime(started_at) < datetime(?)` was NULL for every row — the UI's "Load more" silently stopped at 50 runs. `ListJobRunsPaged` now takes a `time.Time` and compares directly. **Do not wrap `started_at` in a SQLite date function**; comparisons against it are lexicographic, which is what `ORDER BY started_at` already relied on.
- **SEC-31** — collector mode cannot mutate _(v0.4.0)_. Enforced twice: the mutating routes are never registered, and the chart grants no `patch`/`create` verb in `mode: server`.

- **DOM-1** — `spec.timeZone` now honoured end to end _(v0.2.0)_. `schedule.{Parse,NextRun,PrevRun}` take an IANA zone; `cmd/kubecron` imports `time/tzdata` because distroless ships no zoneinfo. An unresolvable schedule or zone renders `unresolved` rather than a wrong countdown.
- **BUG-20** — deleted CronJobs/clusters are soft-deleted _(v0.2.0)_: hidden from listings, Prometheus series dropped, history preserved, revived on recreation. Caught by `OnDelete`, by a post-cache-sync `Reconcile` (for deletions that happened while KubeCron was down), and by `MarkClustersDeletedExcept` on kubeconfig load.
- **PERF-2** — fixed by indexing, not batching _(v0.2.0)_. Migration 000006's `job_runs(cronjob_id, started_at DESC)` removes the temp-B-tree sort behind each row read. A window-function batch was measured at 4–16× slower and rejected; see the comment above `GetCronJobSummaries` before attempting it again.
- **OBS-3 / OBS-4** — gauge metrics survive restarts _(v0.3.0)_. They were previously written only by live watcher events, so a fresh process served no `last_run_status` series at all until each CronJob next fired. `metrics.StateCollector` now re-derives them from the DB every 30 s; counters and the histogram stay event-driven because rebuilding them would double-count. Ten further families added — see `internal/metrics/metrics.go`.
- **INFRA-4** — lint is deterministic _(v0.3.0)_. CI installed golangci-lint from the **v1** module path, where `@latest` can never resolve v2, so CI silently froze on the last v1 while local v2 reported 15 findings CI never saw. `.golangci.yml` pins the semantics; CI installs `/v2/`.
- **MAINT-2** — missed-run detection lives once, in `schedule.IsMissed` _(v0.3.0)_, shared by the UI badge and `kubecron_cronjob_missed`.
- **SEC-28** — the chart can no longer ship an unauthenticated externally-reachable install _(v0.3.0)_. `kubecron.validateExposure` in `_helpers.tpl` (called from `deployment.yaml`, the one template that always renders) fails the install when `ingress.enabled=true` **or** `service.type != ClusterIP` while `oidc.enabled=false`; `security.acknowledgeInsecureExposure=true` is the deliberate escape hatch. Backed by a startup `slog.Warn` (for raw manifests / Compose / `go run`, which no chart guard can reach) and a `NOTES.txt` banner. **Any new exposure path added to the chart must be added to that helper.**

---

## Code Conventions

- **No CGO**: always build with `CGO_ENABLED=0`.
- **UI rendering**: all HTML is generated in `internal/api/html.go`, `html_components.go`, `html_log.go`, `html_chart.go`, and `html_overview.go` via `fmt.Fprintf`. No template engine. When adding a new UI component, add a helper to `html_components.go` (reusable row/card) or the appropriate thematic file rather than inlining HTML in handlers.
- **SQL**: all queries in `internal/storage/queries.go`. New tables = new migration file in `migrations/`.
- **Schedules**: never evaluate a cron expression without its zone. Every `schedule` entry point takes an IANA zone name; pass `cj.TZ()`. On error, show nothing rather than a wrong time — a wrong countdown is worse than an absent one.
- **Error handling**: never return raw K8s API or DB errors to HTTP clients. Log with `slog.Error()`, return 500.
- **Secrets**: never log kubeconfig paths or content, OIDC secrets, or session keys.
- **Versioning**: update `CHANGELOG.md` and the `Version` constant in `internal/version/version.go` on every release.
- **RBAC**: any new K8s API access must be added to `charts/kubecron/templates/clusterrole.yaml`. A mutating verb belongs in the `mode: ui` branch only.
- **Modes**: a new route is registered in `registerUI` (dashboard, unversioned `/api/*`, anything mutating) or in `registerCollectorAPI` (`/api/v1`, GET-only, served in both modes). Never both, and never a mutating route in the second — see `docs/COLLECTOR-API.md`.
- **The `/api/v1` contract is a promise**: fields may be added within `v1`, never removed, renamed or repurposed. A breaking change mints `v2` and `api_versions` lists both. Absence is never reported as fact — a shape that can be empty must carry the field that says *why* (`observed_since`, `expired`, `metrics_enabled`). Any change to a `/api/v1` shape must land in `docs/openapi.yaml` **and** `docs/COLLECTOR-API.md` in the same commit.
- **Docker images**: published only on `*.*.*` tag push — `git tag 0.3.0 && git push origin 0.3.0`.

---

## CI/CD Notes

- `ci.yml` runs on push/PR to `main`: `go build`, `go vet`, `go test` (+ `-race`), `golangci-lint`, `helm lint`. Skipped for `renovate[bot]` (see AUDIT INFRA-5).
- `docker-publish.yml` pushes to `ghcr.io/schmitech-fr/kubecron` on **every push to `main`** (tags `main`, `<commit-sha>`) and on `*.*.*` tag push (tags `latest`, `<git-tag>`, `<commit-sha>`).
- Platforms: `linux/amd64` only for now (arm64 pending — AUDIT INFRA-3).
- SBOM generated per release image with `anchore/sbom-action` (SPDX); release images signed with `sigstore/cosign` (keyless, OIDC-based).

---

## Definition of Done (Release Checklist)

### Build & quality
- [ ] `go build ./...` — no errors
- [ ] `go vet ./...` — no warnings
- [ ] `go test ./...` — all tests pass
- [ ] `golangci-lint run` — no lint errors
- [ ] `helm lint charts/kubecron` — no errors

### Version bump
- [ ] `internal/version/version.go` — update `Version` constant
- [ ] `charts/kubecron/Chart.yaml` — bump `appVersion` to match

### Helm chart
- [ ] If env vars changed: update `charts/kubecron/values.yaml` + `templates/deployment.yaml`
- [ ] If RBAC changed: update `charts/kubecron/templates/clusterrole.yaml` (both mode branches)
- [ ] Render both modes, not just the default:
      `helm template t charts/kubecron` and `helm template t charts/kubecron --set mode=server`
- [ ] If a new exposure path was added, extend `kubecron.validateExposure` (SEC-28)

### Documentation
- [ ] `docs/COLLECTOR-API.md` **and** `docs/openapi.yaml` — update together if any `/api/v1` shape changed
- [ ] `CHANGELOG.md` — all changes documented under the new version with today's date
- [ ] `ROADMAP.md` — mark shipped items as `[x]` with the version tag
- [ ] `README.md` — update if user-facing features, env vars, or install steps changed
- [ ] `CLAUDE.md` — move resolved Known Issues to an archived section

### Git workflow
1. All changes committed on a feature branch
2. PR reviewed and merged into `main`
3. Tag pushed from `main`:
   ```bash
   git tag 0.X.Y && git push origin 0.X.Y
   ```
   The `docker-publish.yml` workflow triggers automatically on `*.*.*` tag push.

### Common pitfalls
- Release image tags (`latest`, `<version>`) publish **only on tag push** — the tag must exactly match the version bump (`main`/`<sha>` tags also publish on every main push)
- `helm lint` is now in CI (`helm-lint` job) — run it locally first to avoid a wasted CI run
- New Prometheus metrics must be wired (incremented/set) in the appropriate watcher or handler — declaring them in `metrics.go` is not enough

<!-- rtk-instructions v2 -->
# RTK (Rust Token Killer) - Token-Optimized Commands

## Golden Rule

**Always prefix commands with `rtk`**. If RTK has a dedicated filter, it uses it. If not, it passes through unchanged. This means RTK is always safe to use.

**Important**: Even in command chains with `&&`, use `rtk`:
```bash
# ❌ Wrong
git add . && git commit -m "msg" && git push

# ✅ Correct
rtk git add . && rtk git commit -m "msg" && rtk git push
```

## RTK Commands by Workflow

### Build & Compile (80-90% savings)
```bash
rtk cargo build         # Cargo build output
rtk cargo check         # Cargo check output
rtk cargo clippy        # Clippy warnings grouped by file (80%)
rtk tsc                 # TypeScript errors grouped by file/code (83%)
rtk lint                # ESLint/Biome violations grouped (84%)
rtk prettier --check    # Files needing format only (70%)
rtk next build          # Next.js build with route metrics (87%)
```

### Test (60-99% savings)
```bash
rtk cargo test          # Cargo test failures only (90%)
rtk go test             # Go test failures only (90%)
rtk jest                # Jest failures only (99.5%)
rtk vitest              # Vitest failures only (99.5%)
rtk playwright test     # Playwright failures only (94%)
rtk pytest              # Python test failures only (90%)
rtk rake test           # Ruby test failures only (90%)
rtk rspec               # RSpec test failures only (60%)
rtk test <cmd>          # Generic test wrapper - failures only
```

### Git (59-80% savings)
```bash
rtk git status          # Compact status
rtk git log             # Compact log (works with all git flags)
rtk git diff            # Compact diff (80%)
rtk git show            # Compact show (80%)
rtk git add             # Ultra-compact confirmations (59%)
rtk git commit          # Ultra-compact confirmations (59%)
rtk git push            # Ultra-compact confirmations
rtk git pull            # Ultra-compact confirmations
rtk git branch          # Compact branch list
rtk git fetch           # Compact fetch
rtk git stash           # Compact stash
rtk git worktree        # Compact worktree
```

Note: Git passthrough works for ALL subcommands, even those not explicitly listed.

### GitHub (26-87% savings)
```bash
rtk gh pr view <num>    # Compact PR view (87%)
rtk gh pr checks        # Compact PR checks (79%)
rtk gh run list         # Compact workflow runs (82%)
rtk gh issue list       # Compact issue list (80%)
rtk gh api              # Compact API responses (26%)
```

### JavaScript/TypeScript Tooling (70-90% savings)
```bash
rtk pnpm list           # Compact dependency tree (70%)
rtk pnpm outdated       # Compact outdated packages (80%)
rtk pnpm install        # Compact install output (90%)
rtk npm run <script>    # Compact npm script output
rtk npx <cmd>           # Compact npx command output
rtk prisma              # Prisma without ASCII art (88%)
rtk uv run <cmd>        # Compact uv project command output
```

### Files & Search (60-75% savings)
```bash
rtk ls <path>           # Tree format, compact (65%)
rtk read <file>         # Code reading with filtering (60%)
rtk grep <pattern>      # Search grouped by file (75%). Format flags (-c, -l, -L, -o, -Z) run raw.
rtk find <pattern>      # Find grouped by directory (70%)
```

### Analysis & Debug (70-90% savings)
```bash
rtk err <cmd>           # Filter errors only from any command
rtk log <file>          # Deduplicated logs with counts
rtk json <file>         # JSON structure without values
rtk deps                # Dependency overview
rtk env                 # Environment variables compact
rtk summary <cmd>       # Smart summary of command output
rtk diff                # Ultra-compact diffs
```

### Infrastructure (85% savings)
```bash
rtk docker ps           # Compact container list
rtk docker images       # Compact image list
rtk docker logs <c>     # Deduplicated logs
rtk kubectl get         # Compact resource list
rtk kubectl logs        # Deduplicated pod logs
```

### Network (65-70% savings)
```bash
rtk curl <url>          # Compact HTTP responses (70%)
rtk wget <url>          # Compact download output (65%)
```

### Meta Commands
```bash
rtk gain                # View token savings statistics
rtk gain --history      # View command history with savings
rtk discover            # Analyze Claude Code sessions for missed RTK usage
rtk proxy <cmd>         # Run command without filtering (for debugging)
rtk init                # Add RTK instructions to CLAUDE.md
rtk init --global       # Add RTK to ~/.claude/CLAUDE.md
```

## Token Savings Overview

| Category | Commands | Typical Savings |
|----------|----------|-----------------|
| Tests | vitest, playwright, cargo test | 90-99% |
| Build | next, tsc, lint, prettier | 70-87% |
| Git | status, log, diff, add, commit | 59-80% |
| GitHub | gh pr, gh run, gh issue | 26-87% |
| Package Managers | pnpm, npm, npx | 70-90% |
| Files | ls, read, grep, find | 60-75% |
| Infrastructure | docker, kubectl | 85% |
| Network | curl, wget | 65-70% |

Overall average: **60-90% token reduction** on common development operations.
<!-- /rtk-instructions -->