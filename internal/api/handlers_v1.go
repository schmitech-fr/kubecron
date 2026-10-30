package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kubecron/kubecron/internal/schedule"
	"github.com/kubecron/kubecron/internal/storage"
	"github.com/kubecron/kubecron/internal/version"
)

// The /api/v1 collector contract.
//
// This is the only part of KubeCron that another program is invited to depend
// on. Everything under /api (unversioned) backs KubeCron's own pages and may
// change with them; everything under /api/v1 is a promise.
//
// The consumer is KubeDeck, which reads a collector as the last and richest
// rung of a degradation ladder — live Kubernetes objects, then Prometheus and
// kube-state-metrics, then a collector. Two properties follow from that and
// shape every response below:
//
//   - A consumer that meets a version it does not know must be able to fall
//     back to the rung beneath it rather than fail. Hence GET /api/v1/collector,
//     which is cheap enough to call first and says what this build can answer.
//   - Absence must never be reported as a fact. A CronJob with no runs recorded
//     because the collector was deployed yesterday is a different answer from a
//     CronJob that has not run, and a run whose log body has aged out of
//     retention is a different answer from a run that printed nothing. Every
//     shape here carries what it takes to tell those apart.
const (
	// APIVersion is the collector contract this build speaks.
	APIVersion = "v1"

	// v1DefaultRunLimit and v1MaxRunLimit bound the run history page size.
	// Unbounded is not offered: a CronJob that runs every minute accumulates
	// ~260 000 rows over the default 180-day retention.
	v1DefaultRunLimit = 100
	v1MaxRunLimit     = 1000

	// v1DefaultHeatmapDays is the window GET .../daily covers by default —
	// the 90-day calendar heatmap KubeDeck draws.
	v1DefaultHeatmapDays = 90
)

// CollectorInfo describes the deployment to a consumer. It is assembled in
// cmd/kubecron from the same configuration the collector actually runs with,
// rather than restated here, so it cannot drift from the truth.
type CollectorInfo struct {
	// Mode is "ui" or "server". A collector in "ui" mode still answers /api/v1
	// (the data is identical) but is not read-only: it also serves the
	// dashboard and the suspend/resume/trigger routes.
	Mode Mode
	// RetentionDays is how long run metadata is kept; LogRetentionDays how long
	// the log bodies are. A consumer needs both to know whether a gap in its
	// query window is "nothing happened" or "we no longer hold it".
	RetentionDays    int
	LogRetentionDays int
	// SampleIntervalSeconds is the resource-sampling period. It sets the
	// resolution of the samples series, and tells a consumer what a run shorter
	// than one interval can be expected to yield: nothing.
	SampleIntervalSeconds int
}

// ── Discovery ─────────────────────────────────────────────────────────────────

type collectorCapabilities struct {
	// RunHistory: run records past the life of the Job object.
	RunHistory bool `json:"run_history"`
	// RunLogs: the log body of a finished run, past the life of the Pod. No
	// other source in KubeDeck's ladder has these.
	RunLogs bool `json:"run_logs"`
	// ResourceSamples: CPU/memory sampled while a run was in flight, at
	// SampleIntervalSeconds resolution.
	ResourceSamples bool `json:"resource_samples"`
	// LiveLogStream: SSE tail of a run that is still executing.
	LiveLogStream bool `json:"live_log_stream"`
	// Mutations: suspend/resume/trigger. False in server mode, by design —
	// KubeDeck performs those itself against the Kubernetes API.
	Mutations bool `json:"mutations"`
}

type collectorRetention struct {
	RunDays int `json:"run_days"`
	LogDays int `json:"log_days"`
}

type collectorResponse struct {
	Product      string                `json:"product"`
	Version      string                `json:"version"`
	APIVersion   string                `json:"api_version"`
	APIVersions  []string              `json:"api_versions"`
	Mode         string                `json:"mode"`
	ReadOnly     bool                  `json:"read_only"`
	Capabilities collectorCapabilities `json:"capabilities"`
	Retention    collectorRetention    `json:"retention"`
	// SampleIntervalSeconds is the resource sampling period in seconds.
	SampleIntervalSeconds int `json:"sample_interval_seconds"`
	// Clusters is what this collector observes. A collector deployed per
	// cluster reports exactly one; a collector fed a directory of kubeconfigs
	// reports several.
	Clusters []clusterV1 `json:"clusters"`
	ServerAt time.Time   `json:"server_at"`
}

// Collector handles GET /api/v1/collector.
//
// It is the entry point of the contract: a consumer calls it first, decides
// from api_versions whether it can talk to this build at all, and from
// capabilities which of its own panels it can populate.
func (h *Handler) Collector(w http.ResponseWriter, r *http.Request) {
	clusters, err := h.store.ListClusters(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list clusters")
		return
	}

	writeJSON(w, http.StatusOK, collectorResponse{
		Product:     "kubecron",
		Version:     version.Version,
		APIVersion:  APIVersion,
		APIVersions: []string{APIVersion},
		Mode:        string(h.info.Mode),
		ReadOnly:    !h.info.Mode.AllowsMutation(),
		Capabilities: collectorCapabilities{
			RunHistory:      true,
			RunLogs:         true,
			ResourceSamples: true,
			LiveLogStream:   true,
			Mutations:       h.info.Mode.AllowsMutation(),
		},
		Retention: collectorRetention{
			RunDays: h.info.RetentionDays,
			LogDays: h.info.LogRetentionDays,
		},
		SampleIntervalSeconds: h.info.SampleIntervalSeconds,
		Clusters:              toClustersV1(clusters),
		ServerAt:              time.Now().UTC(),
	})
}

// ── Clusters ──────────────────────────────────────────────────────────────────

type clusterV1 struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// MetricsEnabled reports whether the Metrics API answered at the last probe
	// on this cluster. When false, runs on it carry no resource samples — and
	// that is a missing measurement, not an idle workload.
	MetricsEnabled bool `json:"metrics_enabled"`
	// ObservedSince is the first moment this collector saw the cluster. Nothing
	// before it was recorded by anybody, so a consumer must draw that period as
	// unobserved rather than as an absence of runs.
	ObservedSince time.Time `json:"observed_since"`
}

func toClustersV1(in []storage.Cluster) []clusterV1 {
	out := make([]clusterV1, 0, len(in))
	for _, c := range in {
		out = append(out, clusterV1{
			ID:             c.ID,
			Name:           c.Name,
			MetricsEnabled: c.MetricsEnabled,
			ObservedSince:  c.CreatedAt,
		})
	}
	return out
}

// ListClustersV1 handles GET /api/v1/clusters.
func (h *Handler) ListClustersV1(w http.ResponseWriter, r *http.Request) {
	clusters, err := h.store.ListClusters(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list clusters")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": toClustersV1(clusters)})
}

// ── CronJobs ──────────────────────────────────────────────────────────────────

type cronJobV1 struct {
	ID        string `json:"id"`
	ClusterID string `json:"cluster_id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Schedule  string `json:"schedule"`
	// TimeZone is spec.timeZone, empty when the CronJob declares none. NextRunAt
	// is resolved in it; a consumer that recomputes the schedule itself must use
	// the same zone or it will disagree with the cluster.
	TimeZone  string `json:"time_zone"`
	Suspended bool   `json:"suspended"`
	// NextRunAt is nil when the schedule or the zone cannot be resolved. It is
	// deliberately omitted rather than approximated: a wrong countdown is worse
	// than an absent one.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	// Missed reports that the previous scheduled occurrence produced no run this
	// collector saw. It is only meaningful because the collector was watching —
	// a consumer must not derive it from history alone.
	Missed             bool              `json:"missed"`
	LastSuccessfulTime *time.Time        `json:"last_successful_time,omitempty"`
	Resources          resourcesV1       `json:"resources"`
	LastRun            *storage.JobRun   `json:"last_run,omitempty"`
	Stats7d            *storage.RunStats `json:"stats_7d,omitempty"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

// resourcesV1 carries the pod template's requests and limits verbatim as
// Kubernetes quantity strings ("250m", "512Mi"), empty when unset. Unlike the
// UI's shape it never substitutes an em-dash: a consumer needs the absence, not
// a rendering of it.
type resourcesV1 struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ListCronJobsV1 handles GET /api/v1/clusters/{clusterID}/cronjobs.
func (h *Handler) ListCronJobsV1(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	ctx := r.Context()

	cronjobs, err := h.store.ListCronJobs(ctx, clusterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list cronjobs")
		return
	}
	// One aggregate read for the whole cluster rather than three queries per
	// CronJob (PERF-2) — the same read path the dashboard uses.
	summaries, err := h.store.GetCronJobSummaries(ctx, clusterID, sparklineRuns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list cronjobs")
		return
	}

	now := time.Now()
	out := make([]cronJobV1, 0, len(cronjobs))
	for _, cj := range cronjobs {
		item := cronJobV1{
			ID:                 cj.ID,
			ClusterID:          cj.ClusterID,
			Namespace:          cj.Namespace,
			Name:               cj.Name,
			Schedule:           cj.Schedule,
			TimeZone:           cj.TZ(),
			Suspended:          cj.Suspended,
			LastSuccessfulTime: cj.LastSuccessfulTime,
			UpdatedAt:          cj.UpdatedAt,
			Resources: resourcesV1{
				CPURequest:    strOrEmpty(cj.CPURequest),
				CPULimit:      strOrEmpty(cj.CPULimit),
				MemoryRequest: strOrEmpty(cj.MemoryRequest),
				MemoryLimit:   strOrEmpty(cj.MemoryLimit),
			},
		}
		if cj.Schedule != "" {
			if next, err := schedule.NextRun(cj.Schedule, cj.TZ(), now); err == nil {
				item.NextRunAt = &next
			}
		}
		if sum := summaries[cj.ID]; sum != nil {
			item.LastRun = sum.LastRun
			item.Stats7d = sum.Stats7d
		}
		// Computed outside the summary branch: a CronJob that has never run at
		// all is the clearest missed case there is, and it has no summary.
		item.Missed = schedule.IsMissed(cj.Schedule, cj.TZ(), cj.Suspended, lastRunForSchedule(item.LastRun), now)
		out = append(out, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"cluster_id": clusterID,
		"cronjobs":   out,
	})
}

// ── Run history ───────────────────────────────────────────────────────────────

type runsV1Response struct {
	CronJobID string           `json:"cronjob_id"`
	ClusterID string           `json:"cluster_id"`
	Namespace string           `json:"namespace"`
	Name      string           `json:"name"`
	Runs      []storage.JobRun `json:"runs"`
	// NextCursor is the value to pass back as ?before= for the following page.
	// Empty means the last page was reached.
	NextCursor string `json:"next_cursor,omitempty"`
	// ObservedSince repeats the cluster's first-seen moment on every page, so a
	// consumer paging backwards knows where the record stops being a record and
	// starts being an absence of one.
	ObservedSince *time.Time `json:"observed_since,omitempty"`
}

// ListRunsV1 handles GET /api/v1/clusters/{clusterID}/cronjobs/{ns}/{name}/runs.
//
// Paged newest-first. ?limit= (default 100, max 1000) and ?before= (an RFC3339
// timestamp, normally the previous page's next_cursor).
func (h *Handler) ListRunsV1(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ctx := r.Context()

	cj, err := h.store.GetCronJobByName(ctx, clusterID, ns, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to look up cronjob")
		return
	}
	if cj == nil {
		writeError(w, http.StatusNotFound, "cronjob not found")
		return
	}

	limit := clampLimit(r.URL.Query().Get("limit"), v1DefaultRunLimit, v1MaxRunLimit)
	runs, err := h.store.ListJobRunsPaged(ctx, cj.ID, parseRunCursor(r.URL.Query().Get("before")), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}

	resp := runsV1Response{
		CronJobID: cj.ID,
		ClusterID: cj.ClusterID,
		Namespace: cj.Namespace,
		Name:      cj.Name,
		Runs:      runs,
	}
	// A short page is the last page. A full one may or may not be, and paying
	// for one extra empty request is cheaper than a COUNT on every call.
	//
	// Full precision (RFC3339Nano), because the cursor selects runs strictly
	// older than it: truncating to whole seconds would skip every other run
	// that started in the same second as this page's last row.
	if len(runs) == limit {
		resp.NextCursor = runCursor(runs[len(runs)-1])
	}
	if c := h.clusterByID(ctx, clusterID); c != nil {
		resp.ObservedSince = &c.CreatedAt
	}

	writeJSON(w, http.StatusOK, resp)
}

// DailyRunStatsV1 handles
// GET /api/v1/clusters/{clusterID}/cronjobs/{ns}/{name}/daily.
//
// One row per calendar day, which is what a calendar heatmap needs. It exists
// as its own endpoint rather than as a client-side fold of the runs list
// because folding 90 days of a per-minute CronJob client-side means shipping
// 130 000 run records to draw 90 squares.
func (h *Handler) DailyRunStatsV1(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ctx := r.Context()

	cj, err := h.store.GetCronJobByName(ctx, clusterID, ns, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to look up cronjob")
		return
	}
	if cj == nil {
		writeError(w, http.StatusNotFound, "cronjob not found")
		return
	}

	days := clampLimit(r.URL.Query().Get("days"), v1DefaultHeatmapDays, 365)
	stats, err := h.store.GetDailyRunStats(ctx, cj.ID, days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get daily run stats")
		return
	}
	if stats == nil {
		stats = []storage.DailyRunStat{}
	}

	resp := map[string]any{
		"cronjob_id": cj.ID,
		"cluster_id": cj.ClusterID,
		"namespace":  cj.Namespace,
		"name":       cj.Name,
		"days":       days,
		"daily":      stats,
	}
	// Days before the collector existed hold no runs because nobody was
	// watching. Without this a consumer cannot tell them from days on which the
	// CronJob genuinely did not fire, and would paint a healthy job as dark.
	if c := h.clusterByID(ctx, clusterID); c != nil {
		resp["observed_since"] = c.CreatedAt
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── One run ───────────────────────────────────────────────────────────────────

type runV1Response struct {
	Run *storage.JobRun `json:"run"`
	// CronJob locates the run without making the consumer parse the composite
	// cronjob_id. The parts are exact: a cluster ID is a kubeconfig filename
	// stem and a namespace and name are DNS labels, so none of them can contain
	// the separator.
	CronJob cronJobRefV1 `json:"cronjob"`
}

type cronJobRefV1 struct {
	ID        string `json:"id"`
	ClusterID string `json:"cluster_id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// splitCronJobID splits a "clusterID/namespace/name" CronJob ID. A malformed
// ID yields whatever parts are present rather than an error: the run record is
// still worth returning.
func splitCronJobID(id string) cronJobRefV1 {
	ref := cronJobRefV1{ID: id}
	parts := strings.SplitN(id, "/", 3)
	if len(parts) > 0 {
		ref.ClusterID = parts[0]
	}
	if len(parts) > 1 {
		ref.Namespace = parts[1]
	}
	if len(parts) > 2 {
		ref.Name = parts[2]
	}
	return ref
}

// GetRunV1 handles GET /api/v1/runs/{id}.
func (h *Handler) GetRunV1(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := h.store.GetJobRun(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, runV1Response{Run: run, CronJob: splitCronJobID(run.CronJobID)})
}

// ── Resource samples ──────────────────────────────────────────────────────────

type samplesV1Response struct {
	RunID string `json:"run_id"`
	// IntervalSeconds is the sampling period this collector runs at. A run
	// shorter than it can legitimately have no samples at all.
	IntervalSeconds int                      `json:"interval_seconds"`
	Samples         []storage.ResourceSample `json:"samples"`
	// Summary is the aggregate the collector computed when the run finished.
	// It may be present with an empty Samples series once retention has removed
	// the raw points.
	Summary samplesSummaryV1 `json:"summary"`
}

type samplesSummaryV1 struct {
	AvgCPUMillicores *int64 `json:"avg_cpu_millicores,omitempty"`
	MaxCPUMillicores *int64 `json:"max_cpu_millicores,omitempty"`
	AvgMemoryBytes   *int64 `json:"avg_memory_bytes,omitempty"`
	MaxMemoryBytes   *int64 `json:"max_memory_bytes,omitempty"`
}

// GetResourceSamplesV1 handles GET /api/v1/runs/{id}/samples.
//
// An empty series is not a claim that the run used nothing. It means one of:
// the run was shorter than the sampling interval, the Metrics API was
// unavailable on that cluster (see metrics_enabled in GET /api/v1/clusters), or
// the samples have aged out. The summary distinguishes the last case.
func (h *Handler) GetResourceSamplesV1(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	run, err := h.store.GetJobRun(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	samples, err := h.store.GetResourceSamples(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get resource samples")
		return
	}
	if samples == nil {
		samples = []storage.ResourceSample{}
	}

	writeJSON(w, http.StatusOK, samplesV1Response{
		RunID:           id,
		IntervalSeconds: h.info.SampleIntervalSeconds,
		Samples:         samples,
		Summary: samplesSummaryV1{
			AvgCPUMillicores: run.AvgCPUMillicores,
			MaxCPUMillicores: run.MaxCPUMillicores,
			AvgMemoryBytes:   run.AvgMemoryBytes,
			MaxMemoryBytes:   run.MaxMemoryBytes,
		},
	})
}

// ── Logs ──────────────────────────────────────────────────────────────────────

type logLineV1 struct {
	Ts   time.Time `json:"ts"`
	Line string    `json:"line"`
}

type logsV1Response struct {
	RunID string      `json:"run_id"`
	Lines []logLineV1 `json:"lines"`
	// LogSizeBytes is what the collector recorded for this run when it
	// streamed it, and it is kept for the run's whole life.
	LogSizeBytes int64 `json:"log_size_bytes"`
	// Expired says the log body was captured and has since been removed by
	// log retention. It is the difference between "this run printed nothing"
	// and "we no longer hold what it printed", which no consumer can otherwise
	// tell apart.
	Expired bool `json:"expired"`
	// Truncated says a ?limit= was applied and the returned lines are the tail.
	Truncated bool `json:"truncated"`
	// Running says the run is still executing, so the body is incomplete;
	// GET /api/v1/runs/{id}/stream follows the rest.
	Running bool `json:"running"`
}

// GetLogsV1 handles GET /api/v1/runs/{id}/logs.
//
// ?limit=N returns the last N lines. The whole body is returned by default: a
// run's log is the one thing in the ladder that exists nowhere else, so
// silently sending part of it would be the worst possible default.
func (h *Handler) GetLogsV1(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	run, err := h.store.GetJobRun(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}

	limit := clampLimit(r.URL.Query().Get("limit"), 0, 0)
	lines, err := h.store.GetLogLinesTail(ctx, id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get logs")
		return
	}

	out := make([]logLineV1, 0, len(lines))
	for _, l := range lines {
		out = append(out, logLineV1{Ts: l.Ts, Line: l.Line})
	}

	writeJSON(w, http.StatusOK, logsV1Response{
		RunID:        id,
		Lines:        out,
		LogSizeBytes: run.LogSizeBytes,
		Expired:      len(lines) == 0 && run.LogSizeBytes > 0,
		Truncated:    limit > 0 && len(lines) == limit,
		Running:      run.Status == "running",
	})
}

// DownloadLogsV1 handles GET /api/v1/runs/{id}/logs.txt — the same body as
// plain text, for an operator saving a run's output to a file.
func (h *Handler) DownloadLogsV1(w http.ResponseWriter, r *http.Request) {
	h.DownloadLogs(w, r)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// clusterByID looks up one cluster in the (small, single-digit) cluster list.
// A dedicated query would be a migration's worth of ceremony for a table with
// one row per kubeconfig.
func (h *Handler) clusterByID(ctx context.Context, id string) *storage.Cluster {
	clusters, err := h.store.ListClusters(ctx)
	if err != nil {
		return nil
	}
	for i := range clusters {
		if clusters[i].ID == id {
			return &clusters[i]
		}
	}
	return nil
}

// clampLimit parses a positive integer query parameter, falling back to def on
// anything unparseable and capping at max. A max of 0 means uncapped.
func clampLimit(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}

// v1NotFound answers every unmatched /api/v1 path with a machine-readable
// error naming the version this build speaks, so a consumer that asked for a
// route from a newer contract learns which rung it has landed on instead of
// parsing an HTML 404 page.
func (h *Handler) v1NotFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error":       fmt.Sprintf("no such endpoint in collector API %s", APIVersion),
		"api_version": APIVersion,
		"product":     "kubecron",
	})
}
