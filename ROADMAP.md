# Roadmap

Living checklist of potential next steps for KubeCron, roughly ordered by priority.
Items are not committed to any timeline — this is a solo side project.

For *why* these items and in what order, see [`docs/PRODUCT.md`](docs/PRODUCT.md);
for engineering findings behind the AUDIT ids, see [`docs/AUDIT.md`](docs/AUDIT.md).

## Infrastructure & Distribution

- [ ] **Multi-arch Docker builds** — `linux/arm64` was claimed but `docker-publish.yml` builds `linux/amd64` only (no QEMU step); re-add arm64 to the release build _(AUDIT INFRA-3)_
- [x] **Distroless runtime image** — `gcr.io/distroless/static:nonroot` _(v0.0.1)_
- [x] **Helm chart** — parameterized chart with ingress, TLS, resource limits, OIDC secrets, retention _(v0.0.1)_
- [ ] **Publish Helm chart to OCI / Artifact Hub** — push `charts/kubecron` to GHCR OCI registry and submit to Artifact Hub
- [ ] **GitHub Container Registry visibility** — ensure GHCR images are public once the repo is public
- [ ] **Automated GitHub Releases** — workflow that creates a GitHub Release from CHANGELOG entries on tag push
- [ ] **CVE gate in CI** — `govulncheck ./...` on every build _(done v0.5.0)_; image scan on publish still to do. **Must land before digest pinning** (AUDIT SUP-1)
- [ ] **Pin base image digests** — `golang:1.26@sha256:...` and `gcr.io/distroless/static:nonroot@sha256:...` in Dockerfile. Do this *after* the CVE gate: today the floating tag is what keeps the shipped stdlib patched, so pinning first would freeze a vulnerable one in (AUDIT INFRA-1 × SUP-1)

## Features

- [x] **Live log streaming** — SSE-based real-time pod log viewer _(v0.0.1)_
- [x] **Run history** — every execution recorded with status, duration, exit code _(v0.0.1)_
- [x] **Resource tracking** — CPU/memory sampled via metrics-server, avg/max per run _(v0.0.1)_
- [x] **Suspend / Resume / Trigger** — CronJob control from the dashboard _(v0.0.1)_
- [x] **OIDC authentication** _(v0.0.1)_
- [x] **OIDC authorization** — login allow-list (`OIDC_ALLOWED_EMAILS`) + operator role for suspend/resume/trigger (`OIDC_OPERATOR_EMAILS`) _(v0.2.0)_
- [x] **Prometheus metrics** _(v0.0.1)_
- [x] **Missed run detection** — `missed` badge when a CronJob skipped a scheduled tick _(v0.1.0)_
- [x] **Concurrent run detection** — `⚠ concurrent` badge when multiple runs are active simultaneously _(v0.1.0)_
- [x] **Log level colorization** — FATAL/ERROR/WARN/INFO/DEBUG coloring in the log terminal _(v0.1.0)_
- [x] **Log search** — regex filter with match highlighting and line count _(v0.1.0)_
- [x] **Duration sparkline** — 20-run SVG sparkline alongside the 7-day success ratio _(v0.1.0)_
- [x] **Log download** — plain-text `.log` download for any run _(v0.1.0)_
- [x] **Calendar heatmap** — 90-day per-day success/failure heatmap on the run list page _(v0.1.0)_
- [ ] **S3 log storage** — store log lines in S3/object storage instead of SQLite for unlimited retention
- [x] **Global "what is wrong" page** — status-first summary on both the fleet overview and the cluster view: success rate, failures (and how many CronJobs they came from), in-flight and suspended counts, plus rankings by failures / mean duration / peak CPU / peak memory over 24 h, 7 d or 30 d _(v0.3.0, PRODUCT §3.1)_. Overrunning-run detection is still open — see "Stuck / runaway run detection" below
- [ ] **Collector: per-cluster NetworkPolicy in the chart** — a collector should only be reachable from the namespaces that read it; today `mode: server` relies on ClusterIP plus the bearer token (AUDIT SEC-30)
- [ ] **Alerting with log context** — webhook/Slack/Teams/SMTP on failure, missed run and runaway duration, with the failing pod's last log lines attached — the part Alertmanager cannot do _(PRODUCT §3.2)_
- [ ] **Stuck / runaway run detection** — flag a run exceeding `max(p95 × 2, median × 3)` of its own history _(PRODUCT §3.3)_
- [ ] **Daily digest** — one scheduled summary per team: what failed, what slowed down, what stopped running _(PRODUCT §3.4)_
- [ ] **Cross-run log search** — SQLite FTS5 over `log_lines`, searchable across runs and clusters, over logs of pods that no longer exist _(PRODUCT §4.1)_
- [ ] **Failure fingerprinting** — normalise and group error signatures to show recurrence across clusters _(PRODUCT §4.2)_
- [ ] **Right-sizing + cost** — join stored requests/limits against observed peaks; cost per run from `resource_samples` _(PRODUCT §5.1–5.2)_
- [ ] **Grafana dashboard** — pre-built dashboard JSON for `kubecron_*` metrics (16 families as of v0.3.0, incl. `kubecron_cronjob_missed` and `kubecron_runs_active`)
- [x] **Collector mode** — `KUBECRON_MODE=server` runs the same binary headless: read-only versioned `/api/v1`, no HTML, no mutating route, no mutating RBAC. One per cluster on its own ServiceAccount, discovered by Service label and read on demand by a console (KubeDeck) that was not running when a run happened. Contract in `docs/COLLECTOR-API.md` _(v0.4.0)_
- [x] **Pagination** — cursor-based "Load more" for run history; heatmap click-to-filter by day; blue "running" tile indicator _(v0.2.0; the cursor query itself was fixed in v0.4.0 — AUDIT BUG-21)_
- [ ] **CronJob annotations** — display description, owner, runbook link from K8s annotations
- [ ] **Dark / light mode toggle**
- [x] **Timezone-aware schedules** — CronJob `spec.timeZone` honoured in next-run and missed detection; `time/tzdata` embedded for the distroless image _(v0.2.0, AUDIT DOM-1)_
- [x] **Deleted-object cleanup** — CronJobs/clusters removed upstream are soft-deleted: hidden from the UI, Prometheus series dropped, history kept, revived if recreated _(v0.2.0, AUDIT BUG-20)_

## Performance

- [x] **Index the hot read path** — composite index `job_runs(cronjob_id, started_at DESC)` removes the temp-B-tree sort behind every CronJob row; ~154 ms → ~38 ms per render at 500 CronJobs × 500 runs _(v0.2.0, AUDIT PERF-2)_
- [ ] **Object-storage log backend** — move `log_lines` out of SQLite; unlimited retention and removes the single point of data loss _(AUDIT PERF-1, PRODUCT §4.3)_

## Code Quality

- [x] **golangci-lint in CI** _(v0.0.1)_
- [x] **helm lint in CI** _(v0.0.1)_
- [x] **Unit tests (partial)** — schedule (incl. timezone/DST), auth HMAC, storage (incl. migration upgrade path), broadcaster, watcher (Job + CronJob handlers), HTTP handlers (`internal/api`) covered _(v0.2.0)_
- [ ] **Integration tests** — Pod informer event handling with `k8s.io/client-go/kubernetes/fake` (Job and CronJob handlers already covered)
- [x] **Renovate** — automated dependency updates for Go modules and GitHub Actions, `k8s.io/*` grouped, CI no longer skipped for its PRs _(v0.5.0, AUDIT INFRA-5)_
- [x] **seccompProfile: RuntimeDefault** — added to pod security context in `values.yaml` _(v0.2.0, AUDIT INFRA-2)_
- [x] **Security hardening batch** — HTTP server timeouts, generic errors on suspend/resume, nosniff/XFO/Referrer-Policy headers, CSRF `Secure` flag + POST logout, split ClusterRole verbs _(v0.2.0, AUDIT SEC-20/21/23-partial/25/26)_
- [ ] **CSP + HSTS** — remaining SEC-23 headers; CSP requires moving inline scripts to nonce'd blocks
- [ ] **Vendor frontend assets** — embed htmx + Chart.js instead of CDN (SRI/supply chain, air-gapped installs) _(AUDIT SEC-22)_
- [ ] **Pin golangci-lint + add `.golangci.yml`** — CI installs `golangci-lint/cmd/golangci-lint@latest`, the **v1** module path, so it is silently frozen on the final v1 release and will never pick up v2. v2 is stricter by default (it dropped v1's `EXC0001` exclusions for `.*Close` / `.*print(f|ln)?`): running it today reports 15 pre-existing issues. Pin an explicit version and commit a config _(AUDIT INFRA-4)_

## Documentation

- [ ] **Screenshot in README** — dashboard screenshot
- [ ] **OIDC setup guide** — `docs/oidc.md` with Keycloak, Dex, and Google examples
- [ ] **Grafana integration guide** — how to import the bundled dashboard
