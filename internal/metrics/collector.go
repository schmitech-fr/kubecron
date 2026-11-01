package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/kubecron/kubecron/internal/runcheck"
	"github.com/kubecron/kubecron/internal/schedule"
	"github.com/kubecron/kubecron/internal/storage"
)

// DefaultCollectInterval is how often the state collector refreshes its gauges.
// It is deliberately coarser than the resource sampler: every value it publishes
// is a slow-moving fact about a CronJob, and the pass costs a handful of indexed
// queries per cluster.
const DefaultCollectInterval = 30 * time.Second

// StateCollector republishes the gauge-valued metrics from the database on a
// ticker.
//
// The event-driven wiring in the watchers only ever *reacts*: RecordCompletion
// fires when a pod finishes, and the CronJob handler fires on informer events.
// That leaves every run-outcome gauge empty from process start until the next
// event happens to occur — for a nightly backup, up to 24 hours during which
// kubecron_last_run_status simply has no series. Alert rules written against it
// silently stop evaluating exactly when a restart makes them most relevant.
//
// Deriving these gauges from stored state instead makes them a function of the
// database rather than of process uptime, so a restart costs one interval of
// staleness rather than a blind window of unbounded length. Counters and the
// duration histogram stay event-driven: they are cumulative, and rebuilding
// them from the database on every pass would double-count.
type StateCollector struct {
	store    storage.Store
	interval time.Duration
}

// NewStateCollector builds a collector reading from store. A non-positive
// interval falls back to DefaultCollectInterval.
func NewStateCollector(store storage.Store, interval time.Duration) *StateCollector {
	if interval <= 0 {
		interval = DefaultCollectInterval
	}
	return &StateCollector{store: store, interval: interval}
}

// Run collects once immediately — so a freshly started process serves populated
// gauges on its first scrape rather than after a full interval — and then on
// every tick until ctx is cancelled.
func (c *StateCollector) Run(ctx context.Context) {
	c.collect(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

// collect runs one full refresh. Errors are logged and abandon only the cluster
// that failed: a single unreadable cluster must not stop the others' gauges
// from being refreshed, since stale gauges are indistinguishable from healthy
// ones at scrape time.
func (c *StateCollector) collect(ctx context.Context) {
	clusters, err := c.store.ListClusters(ctx)
	if err != nil {
		slog.Error("metrics collector: failed to list clusters", "err", err)
		return
	}

	// Running counts are fleet-wide in one query; fetched once per pass rather
	// than per cluster.
	running, err := c.store.CountRunningRuns(ctx)
	if err != nil {
		slog.Error("metrics collector: failed to count running runs", "err", err)
		running = map[string]int{}
	}

	now := time.Now()
	for _, cl := range clusters {
		if err := c.collectCluster(ctx, cl, running, now); err != nil {
			slog.Error("metrics collector: failed to collect cluster",
				"cluster", cl.ID, "err", err)
		}
	}
}

func (c *StateCollector) collectCluster(
	ctx context.Context,
	cl storage.Cluster,
	running map[string]int,
	now time.Time,
) error {
	cronjobs, err := c.store.ListCronJobs(ctx, cl.ID)
	if err != nil {
		return err
	}
	// The duration history is the stuck-run baseline (runcheck.HistoryRuns).
	summaries, err := c.store.GetCronJobSummaries(ctx, cl.ID, runcheck.HistoryRuns)
	if err != nil {
		return err
	}

	ClusterCronJobs.WithLabelValues(cl.ID).Set(float64(len(cronjobs)))
	ClusterMetricsAPIAvailable.WithLabelValues(cl.ID).Set(boolGauge(cl.MetricsEnabled))

	for _, cj := range cronjobs {
		c.collectCronJob(cj, summaries[cj.ID], running[cj.ID], now)
	}
	return nil
}

// collectCronJob publishes every per-CronJob gauge derived from stored state.
func (c *StateCollector) collectCronJob(
	cj storage.CronJob,
	sum *storage.CronJobSummary,
	activeRuns int,
	now time.Time,
) {
	labels := []string{cj.ClusterID, cj.Namespace, cj.Name}

	CronJobSuspended.WithLabelValues(labels...).Set(boolGauge(cj.Suspended))
	RunsActive.WithLabelValues(labels...).Set(float64(activeRuns))

	// A schedule or zone that does not resolve publishes no next-run timestamp
	// at all, rather than one computed in the wrong zone (DOM-1).
	if next, err := schedule.NextRun(cj.Schedule, cj.TZ(), now); err == nil {
		NextRunTimestamp.WithLabelValues(labels...).Set(float64(next.Unix()))
	}

	var last *storage.JobRun
	if sum != nil {
		last = sum.LastRun
	}

	var lastForSchedule *schedule.LastRun
	if last != nil {
		lastForSchedule = &schedule.LastRun{StartedAt: last.StartedAt, Running: last.Status == "running"}
	}
	CronJobMissed.WithLabelValues(labels...).Set(
		boolGauge(schedule.IsMissed(cj.Schedule, cj.TZ(), cj.Suspended, lastForSchedule, now)))

	if last == nil {
		// Never run. Leaving the run-outcome gauges absent is the honest
		// reading: publishing 0 for last_run_status would claim a successful
		// run that never happened.
		return
	}

	LastRunTimestamp.WithLabelValues(labels...).Set(float64(last.StartedAt.Unix()))
	RunStuck.WithLabelValues(labels...).Set(boolGauge(runcheck.Check(last, sum.Durations, now).Stuck))

	// A run still in flight has no outcome, duration, or final resource peak.
	// Its previous values stay published until it finishes rather than being
	// overwritten with zeroes.
	if last.Status == "running" {
		return
	}

	LastRunStatus.WithLabelValues(labels...).Set(boolGauge(last.Status == "failed"))
	if last.DurationMs != nil {
		LastRunDurationSeconds.WithLabelValues(labels...).Set(float64(*last.DurationMs) / 1000)
	}
	if last.MaxCPUMillicores != nil {
		LastRunCPUMillicores.WithLabelValues(labels...).Set(float64(*last.MaxCPUMillicores))
	}
	if last.MaxMemoryBytes != nil {
		LastRunMemoryBytes.WithLabelValues(labels...).Set(float64(*last.MaxMemoryBytes))
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
