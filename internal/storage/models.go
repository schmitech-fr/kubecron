package storage

import "time"

type Cluster struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	MetricsEnabled bool       `json:"metrics_enabled"`
	CreatedAt      time.Time  `json:"created_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}

type CronJob struct {
	ID        string `json:"id"`
	ClusterID string `json:"cluster_id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Schedule  string `json:"schedule"`
	// TimeZone mirrors CronJob spec.timeZone (IANA name). nil means the CronJob
	// declares no zone, in which case the schedule is evaluated server-local —
	// the same rule the Kubernetes CronJob controller applies.
	TimeZone           *string    `json:"time_zone,omitempty"`
	Suspended          bool       `json:"suspended"`
	CPURequest         *string    `json:"cpu_request,omitempty"`
	CPULimit           *string    `json:"cpu_limit,omitempty"`
	MemoryRequest      *string    `json:"memory_request,omitempty"`
	MemoryLimit        *string    `json:"memory_limit,omitempty"`
	LastSuccessfulTime *time.Time `json:"last_successful_time,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at"`
	// DeletedAt is set when the object is no longer present in the cluster.
	// Soft-deleted CronJobs are hidden from listings but keep their run history.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// TZ returns the CronJob's IANA time-zone name, or "" when it declares none.
func (c CronJob) TZ() string {
	if c.TimeZone == nil {
		return ""
	}
	return *c.TimeZone
}

type JobRun struct {
	ID               string     `json:"id"`
	CronJobID        string     `json:"cronjob_id"`
	PodName          string     `json:"pod_name"`
	NodeName         *string    `json:"node_name,omitempty"`
	ContainerImage   *string    `json:"container_image,omitempty"`
	Trigger          string     `json:"trigger"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	Status           string     `json:"status"`
	ExitCode         *int       `json:"exit_code,omitempty"`
	RetryCount       int        `json:"retry_count"`
	LogSizeBytes     int64      `json:"log_size_bytes"`
	DurationMs       *int64     `json:"duration_ms,omitempty"`
	AvgCPUMillicores *int64     `json:"avg_cpu_millicores,omitempty"`
	MaxCPUMillicores *int64     `json:"max_cpu_millicores,omitempty"`
	AvgMemoryBytes   *int64     `json:"avg_memory_bytes,omitempty"`
	MaxMemoryBytes   *int64     `json:"max_memory_bytes,omitempty"`
	// PodWaitingReason is why the run's pod is blocked (CrashLoopBackOff,
	// ImagePullBackOff, Unschedulable, …), nil when it is not waiting.
	// PodWaitingSince is when that reason was first seen.
	PodWaitingReason *string    `json:"pod_waiting_reason,omitempty"`
	PodWaitingSince  *time.Time `json:"pod_waiting_since,omitempty"`
}

type ResourceSample struct {
	ID            int64     `json:"id"`
	RunID         string    `json:"run_id"`
	SampledAt     time.Time `json:"sampled_at"`
	CPUMillicores int64     `json:"cpu_millicores"`
	MemoryBytes   int64     `json:"memory_bytes"`
}

type LogLine struct {
	ID    int64     `json:"id"`
	RunID string    `json:"run_id"`
	Ts    time.Time `json:"ts"`
	Line  string    `json:"line"`
}

type RunStats struct {
	Total            int    `json:"total"`
	Succeeded        int    `json:"succeeded"`
	Failed           int    `json:"failed"`
	AvgDurationMs    *int64 `json:"avg_duration_ms,omitempty"`
	MaxDurationMs    *int64 `json:"max_duration_ms,omitempty"`
	AvgCPUMillicores *int64 `json:"avg_cpu_millicores,omitempty"`
	AvgMemoryBytes   *int64 `json:"avg_memory_bytes,omitempty"`
}

// CronJobSummary is everything a CronJob table row needs beyond the CronJob
// row itself. Rendering a cluster or namespace page used to issue three
// queries per CronJob (last run, 7-day stats, recent durations), re-run every
// 10 s by the HTMX poll of every open tab — 3N queries per render. Summaries
// for a whole cluster are now fetched in a fixed number of queries instead
// (PERF-2).
type CronJobSummary struct {
	LastRun   *JobRun
	Stats7d   *RunStats
	Durations []int64
}

// DailyRunStat holds aggregated run statistics for a single calendar day.
type DailyRunStat struct {
	Day       string `json:"day"`
	Total     int    `json:"total"`
	Succeeded int    `json:"succeeded"`
	Running   int    `json:"running"`
}

// FleetStats aggregates the whole fleet — every cluster, every namespace — over
// a rolling window. It answers "is anything on fire right now" in one query set,
// which is what the overview page leads with.
type FleetStats struct {
	Clusters       int `json:"clusters"`
	Namespaces     int `json:"namespaces"`
	CronJobs       int `json:"cronjobs"`
	Suspended      int `json:"suspended"`
	Runs           int `json:"runs"`
	Succeeded      int `json:"succeeded"`
	Failed         int `json:"failed"`
	Running        int `json:"running"`
	FailingCronJob int `json:"failing_cronjobs"` // distinct CronJobs with >=1 failure in the window
}

// SuccessRate returns the share of finished runs that succeeded, in percent.
// Running runs are excluded: they have no outcome yet, and counting them as
// failures would make every busy window look like a partial outage.
func (f FleetStats) SuccessRate() float64 {
	finished := f.Succeeded + f.Failed
	if finished == 0 {
		return 0
	}
	return float64(f.Succeeded) / float64(finished) * 100
}

// RankMetric names a column the fleet-wide top-N ranking can order by. The set
// is closed because the value it selects is interpolated into the SQL text —
// callers pass a RankMetric, never a raw string, so no caller-controlled data
// reaches the query.
type RankMetric string

const (
	RankByCPU      RankMetric = "cpu"      // peak CPU millicores observed
	RankByMemory   RankMetric = "memory"   // peak memory bytes observed
	RankByDuration RankMetric = "duration" // mean wall-clock run duration
	RankByFailures RankMetric = "failures" // number of failed runs
)

// CronJobRank is one entry in a fleet-wide top-N list. Value carries the metric
// the list was ranked by, in the unit that metric implies (millicores, bytes,
// milliseconds, or a plain count for failures).
type CronJobRank struct {
	CronJobID string `json:"cronjob_id"`
	ClusterID string `json:"cluster_id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Runs      int    `json:"runs"`
	Failed    int    `json:"failed"`
	Value     int64  `json:"value"`
}
