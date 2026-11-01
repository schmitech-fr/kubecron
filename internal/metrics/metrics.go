package metrics

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// JobRunsTotal counts completed job runs by outcome and trigger type.
	JobRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubecron_job_runs_total",
		Help: "Total number of completed job runs.",
	}, []string{"cluster", "namespace", "cronjob", "status", "trigger"})

	// JobDurationSeconds records job run durations.
	JobDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kubecron_job_duration_seconds",
		Help:    "Duration of job runs in seconds.",
		Buckets: []float64{1, 5, 15, 30, 60, 300, 600, 1800},
	}, []string{"cluster", "namespace", "cronjob"})

	// LastRunTimestamp is the Unix timestamp of the most recent run start.
	LastRunTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_last_run_timestamp",
		Help: "Unix timestamp of the last run start for the CronJob.",
	}, []string{"cluster", "namespace", "cronjob"})

	// LastRunStatus is 0 for success, 1 for failure of the most recent run.
	LastRunStatus = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_last_run_status",
		Help: "Status of the last run: 0 = success, 1 = failure.",
	}, []string{"cluster", "namespace", "cronjob"})

	// NextRunTimestamp is the Unix timestamp of the next scheduled run.
	NextRunTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_next_run_timestamp",
		Help: "Unix timestamp of the next scheduled run for the CronJob.",
	}, []string{"cluster", "namespace", "cronjob"})

	// CronJobSuspended is 1 when a CronJob is suspended, 0 otherwise.
	CronJobSuspended = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_cronjob_suspended",
		Help: "Whether the CronJob is suspended (1) or active (0).",
	}, []string{"cluster", "namespace", "cronjob"})

	// LastRunDurationSeconds is the wall-clock duration of the most recent
	// finished run. The histogram above answers "how is this job distributed";
	// this gauge answers "did the last run take too long", which is what an
	// alert rule actually needs and cannot express against a histogram.
	LastRunDurationSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_last_run_duration_seconds",
		Help: "Wall-clock duration of the most recent finished run, in seconds.",
	}, []string{"cluster", "namespace", "cronjob"})

	// RunsActive is the number of runs currently in flight for the CronJob.
	// A value stuck above zero for longer than the job's normal duration is
	// the signal for a hung run; a value above 1 means overlapping executions.
	RunsActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_runs_active",
		Help: "Number of runs currently executing for the CronJob.",
	}, []string{"cluster", "namespace", "cronjob"})

	// CronJobMissed is 1 when the CronJob's last scheduled fire time passed
	// without a run being recorded. The UI has always computed this per row;
	// exporting it makes "my backup silently stopped firing" alertable, which
	// no combination of the other series expresses.
	CronJobMissed = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_cronjob_missed",
		Help: "Whether the CronJob missed its last scheduled run (1) or not (0).",
	}, []string{"cluster", "namespace", "cronjob"})

	// RunStuck is 1 while the CronJob's latest run is stuck: its pod blocked in
	// CrashLoopBackOff, ImagePullBackOff, Unschedulable… past a grace period,
	// or running longer than max(p95 × 2, median × 3) of its own history. The
	// rule lives in runcheck so the badge, this gauge and the alerts agree.
	RunStuck = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_run_stuck",
		Help: "Whether the CronJob's latest run is stuck (1) or not (0).",
	}, []string{"cluster", "namespace", "cronjob"})

	// AlertsSentTotal counts alert deliveries by kind, notifier and result, so
	// a notifier that has started failing is itself alertable.
	AlertsSentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubecron_alerts_sent_total",
		Help: "Alert deliveries, by kind (failed, missed, stuck), notifier and result (ok, error).",
	}, []string{"kind", "notifier", "result"})

	// LastRunCPUMillicores is the peak CPU seen during the most recent run.
	LastRunCPUMillicores = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_last_run_cpu_millicores",
		Help: "Peak CPU observed during the most recent run, in millicores.",
	}, []string{"cluster", "namespace", "cronjob"})

	// LastRunMemoryBytes is the peak memory seen during the most recent run.
	// With the CronJob's declared limit this is what sizing decisions need.
	LastRunMemoryBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_last_run_memory_bytes",
		Help: "Peak memory observed during the most recent run, in bytes.",
	}, []string{"cluster", "namespace", "cronjob"})

	// ClusterCronJobs is the number of live CronJobs known per cluster.
	ClusterCronJobs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_cluster_cronjobs",
		Help: "Number of non-deleted CronJobs known for the cluster.",
	}, []string{"cluster"})

	// ClusterMetricsAPIAvailable reports whether the cluster's Metrics API
	// answered the last probe. When it is 0 the resource gauges above stop
	// being refreshed for that cluster, so alerts on them need this to
	// distinguish "no usage" from "no data".
	ClusterMetricsAPIAvailable = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_cluster_metrics_api_available",
		Help: "Whether the cluster's Metrics API answered the last probe (1) or not (0).",
	}, []string{"cluster"})

	// BuildInfo is the conventional always-1 gauge carrying the build version
	// as a label, so dashboards can annotate a rollout.
	BuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubecron_build_info",
		Help: "Build information; the value is always 1.",
	}, []string{"version"})

	// HTTPRequestsTotal counts served HTTP requests. The route label is the
	// matched pattern, never the raw path, so that per-CronJob URLs cannot
	// blow up cardinality.
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubecron_http_requests_total",
		Help: "Total HTTP requests served, by route pattern and status class.",
	}, []string{"route", "method", "status"})

	// HTTPRequestDurationSeconds records handler latency by route pattern.
	HTTPRequestDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kubecron_http_request_duration_seconds",
		Help:    "HTTP handler latency in seconds, by route pattern.",
		Buckets: []float64{0.005, 0.025, 0.1, 0.25, 1, 2.5, 10},
	}, []string{"route"})
)

// SetBuildInfo publishes the running version as a label on kubecron_build_info.
func SetBuildInfo(version string) {
	BuildInfo.WithLabelValues(version).Set(1)
}

// DeleteCronJobSeries drops every series carrying the given CronJob's labels.
// Without this, deleting a CronJob leaves its gauges frozen at their last value
// forever — a suspended/next-run/last-status reading for an object that no
// longer exists, which alerting rules would keep evaluating (BUG-20).
// DeletePartialMatch is used rather than DeleteLabelValues because the run
// counter and duration histogram carry extra status/trigger labels.
func DeleteCronJobSeries(clusterID, namespace, name string) {
	labels := prometheus.Labels{"cluster": clusterID, "namespace": namespace, "cronjob": name}
	JobRunsTotal.DeletePartialMatch(labels)
	JobDurationSeconds.DeletePartialMatch(labels)
	LastRunTimestamp.DeletePartialMatch(labels)
	LastRunStatus.DeletePartialMatch(labels)
	NextRunTimestamp.DeletePartialMatch(labels)
	CronJobSuspended.DeletePartialMatch(labels)
	LastRunDurationSeconds.DeletePartialMatch(labels)
	RunsActive.DeletePartialMatch(labels)
	CronJobMissed.DeletePartialMatch(labels)
	RunStuck.DeletePartialMatch(labels)
	LastRunCPUMillicores.DeletePartialMatch(labels)
	LastRunMemoryBytes.DeletePartialMatch(labels)
}

// RecordCompletion updates all run-completion metrics.
// cronJobID must have the form "clusterID/namespace/name".
func RecordCompletion(clusterID, cronJobID, status, trigger string, startedAt time.Time, finishedAt time.Time) {
	parts := strings.SplitN(cronJobID, "/", 3)
	if len(parts) != 3 {
		return
	}
	namespace, name := parts[1], parts[2]

	seconds := finishedAt.Sub(startedAt).Seconds()
	JobRunsTotal.WithLabelValues(clusterID, namespace, name, status, trigger).Inc()
	JobDurationSeconds.WithLabelValues(clusterID, namespace, name).Observe(seconds)
	LastRunTimestamp.WithLabelValues(clusterID, namespace, name).Set(float64(startedAt.Unix()))
	LastRunDurationSeconds.WithLabelValues(clusterID, namespace, name).Set(seconds)

	failedVal := 0.0
	if status == "failed" {
		failedVal = 1.0
	}
	LastRunStatus.WithLabelValues(clusterID, namespace, name).Set(failedVal)
}
