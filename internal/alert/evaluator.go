package alert

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/kubecron/kubecron/internal/metrics"
	"github.com/kubecron/kubecron/internal/runcheck"
	"github.com/kubecron/kubecron/internal/schedule"
	"github.com/kubecron/kubecron/internal/storage"
)

const (
	// DefaultInterval is how often the evaluator looks for something to report.
	DefaultInterval = 30 * time.Second
	// failureLookback bounds how far back a failure may still be reported: far
	// enough to cover a restart, short enough that switching alerting on does
	// not replay the history.
	failureLookback = time.Hour
	// failureSettle delays a failure alert so the streamer has flushed the
	// pod's last log lines, which are the point of the alert.
	failureSettle = 30 * time.Second
	// maxAttempts is how many passes an alert is retried while every notifier
	// fails, before it is recorded anyway rather than retried forever.
	maxAttempts = 5
	// maxLineBytes caps one log line in an alert.
	maxLineBytes = 1000
)

// Evaluator finds failed, missed and stuck runs in the store and delivers
// each one once.
type Evaluator struct {
	store     storage.Store
	notifiers []Notifier
	kinds     map[Kind]bool
	logLines  int
	baseURL   string
	attempts  map[string]int
	now       func() time.Time
}

// NewEvaluator builds an evaluator from cfg. It returns nil, nil when no
// destination is configured: alerting is off.
func NewEvaluator(store storage.Store, cfg Config) (*Evaluator, error) {
	notifiers, err := cfg.Notifiers()
	if err != nil {
		return nil, err
	}
	kinds, err := cfg.Kinds()
	if err != nil {
		return nil, err
	}
	if len(notifiers) == 0 || len(kinds) == 0 {
		return nil, nil
	}
	return &Evaluator{
		store:     store,
		notifiers: notifiers,
		kinds:     kinds,
		logLines:  max(cfg.LogLines, 0),
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		attempts:  map[string]int{},
		now:       time.Now,
	}, nil
}

// Names lists the configured notifiers, for the startup log.
func (e *Evaluator) Names() []string {
	out := make([]string, len(e.notifiers))
	for i, n := range e.notifiers {
		out[i] = n.Name()
	}
	return out
}

// Run evaluates on every tick until ctx is cancelled.
func (e *Evaluator) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.evaluate(ctx)
		}
	}
}

// evaluate runs one pass. Each kind is independent: an error in one is logged
// and does not stop the others.
func (e *Evaluator) evaluate(ctx context.Context) {
	now := e.now()
	if e.kinds[KindFailed] {
		if err := e.failed(ctx, now); err != nil {
			slog.Error("alerts: failed-run pass", "err", err)
		}
	}
	if e.kinds[KindStuck] {
		if err := e.stuck(ctx, now); err != nil {
			slog.Error("alerts: stuck-run pass", "err", err)
		}
	}
	if e.kinds[KindMissed] {
		if err := e.missed(ctx, now); err != nil {
			slog.Error("alerts: missed-run pass", "err", err)
		}
	}
}

func (e *Evaluator) failed(ctx context.Context, now time.Time) error {
	runs, err := e.store.ListFailedRunsSince(ctx, now.Add(-failureLookback))
	if err != nil {
		return err
	}
	for i := range runs {
		run := &runs[i]
		if run.FinishedAt == nil || run.FinishedAt.After(now.Add(-failureSettle)) {
			continue
		}
		ev, ok := e.runEvent(ctx, KindFailed, run, now)
		if !ok {
			continue
		}
		ev.Summary = "Failed"
		if run.DurationMs != nil {
			ev.Summary += " after " + runcheck.Human(time.Duration(*run.DurationMs)*time.Millisecond)
		}
		if run.ExitCode != nil {
			ev.Summary += fmt.Sprintf(" with exit code %d", *run.ExitCode)
		}
		if usual, ok := e.usualDuration(ctx, run.CronJobID); ok {
			ev.Summary += "; usually takes " + runcheck.Human(usual)
		}
		ev.Summary += "."
		e.deliver(ctx, run.ID, ev)
	}
	return nil
}

func (e *Evaluator) stuck(ctx context.Context, now time.Time) error {
	runs, err := e.store.GetRunningRuns(ctx)
	if err != nil {
		return err
	}
	for i := range runs {
		run := &runs[i]
		durations, err := e.store.GetRecentDurations(ctx, run.CronJobID, runcheck.HistoryRuns)
		if err != nil {
			return err
		}
		v := runcheck.Check(run, durations, now)
		if !v.Stuck {
			continue
		}
		ev, ok := e.runEvent(ctx, KindStuck, run, now)
		if !ok {
			continue
		}
		ev.Summary = "Stuck: " + v.Reason + "."
		e.deliver(ctx, run.ID, ev)
	}
	return nil
}

func (e *Evaluator) missed(ctx context.Context, now time.Time) error {
	clusters, err := e.store.ListClusters(ctx)
	if err != nil {
		return err
	}
	for _, cl := range clusters {
		cronjobs, err := e.store.ListCronJobs(ctx, cl.ID)
		if err != nil {
			return err
		}
		summaries, err := e.store.GetCronJobSummaries(ctx, cl.ID, 1)
		if err != nil {
			return err
		}
		for _, cj := range cronjobs {
			var last *storage.JobRun
			if sum := summaries[cj.ID]; sum != nil {
				last = sum.LastRun
			}
			var forSchedule *schedule.LastRun
			if last != nil {
				forSchedule = &schedule.LastRun{StartedAt: last.StartedAt, Running: last.Status == "running"}
			}
			if !schedule.IsMissed(cj.Schedule, cj.TZ(), cj.Suspended, forSchedule, now) {
				continue
			}

			// One alert per silence: the subject is the run the silence
			// follows, so it changes — and may alert again — only once a new
			// run has happened in between.
			subject, since := cj.ID+"@never", "No run recorded yet"
			if last != nil {
				subject = cj.ID + "@" + last.ID
				since = "Last run started " + runcheck.Human(now.Sub(last.StartedAt)) + " ago"
			}
			sched := cj.Schedule
			if tz := cj.TZ(); tz != "" {
				sched += " (" + tz + ")"
			}
			ev := Event{
				Kind: KindMissed, Cluster: cl.ID, Namespace: cj.Namespace, CronJob: cj.Name, At: now,
				Summary: fmt.Sprintf("Missed its scheduled run: %s, schedule %s.", since, sched),
			}
			if e.baseURL != "" {
				ev.URL = e.baseURL + cronJobPath(cl.ID, cj.Namespace, cj.Name) + "/runs"
			}
			e.deliver(ctx, subject, ev)
		}
	}
	return nil
}

// runEvent fills the parts of an event that come from the run itself.
func (e *Evaluator) runEvent(ctx context.Context, kind Kind, run *storage.JobRun, now time.Time) (Event, bool) {
	parts := strings.SplitN(run.CronJobID, "/", 3)
	if len(parts) != 3 {
		return Event{}, false
	}
	ev := Event{
		Kind: kind, Cluster: parts[0], Namespace: parts[1], CronJob: parts[2],
		RunID: run.ID, StartedAt: &run.StartedAt, FinishedAt: run.FinishedAt, ExitCode: run.ExitCode,
		At: now,
	}
	if e.baseURL != "" {
		ev.URL = e.baseURL + cronJobPath(parts[0], parts[1], parts[2]) + "/runs/" + url.PathEscape(run.ID)
	}
	if e.logLines > 0 {
		lines, err := e.store.GetLogLinesTail(ctx, run.ID, e.logLines)
		if err != nil {
			slog.Warn("alerts: failed to read log tail", "run_id", run.ID, "err", err)
		}
		for _, l := range lines {
			line := l.Line
			if len(line) > maxLineBytes {
				line = line[:maxLineBytes] + "…"
			}
			ev.LogTail = append(ev.LogTail, line)
		}
	}
	return ev, true
}

// usualDuration is the median of the CronJob's recent finished runs, when
// there are enough of them to call it usual.
func (e *Evaluator) usualDuration(ctx context.Context, cronJobID string) (time.Duration, bool) {
	durations, err := e.store.GetRecentDurations(ctx, cronJobID, runcheck.HistoryRuns)
	if err != nil || len(durations) < runcheck.MinHistory {
		return 0, false
	}
	return runcheck.Median(durations)
}

// deliver sends ev to every notifier unless it already went out, and records
// it once at least one notifier took it. When all of them fail it is retried
// on the next passes, up to maxAttempts, then recorded anyway: an alert that
// can never be delivered must not be retried, and logged, forever.
func (e *Evaluator) deliver(ctx context.Context, subject string, ev Event) {
	key := string(ev.Kind) + "|" + subject
	sent, err := e.store.AlertSent(ctx, string(ev.Kind), subject)
	if err != nil {
		slog.Error("alerts: failed to read delivery state", "kind", ev.Kind, "err", err)
		return
	}
	if sent {
		return
	}

	delivered := 0
	for _, n := range e.notifiers {
		if err := n.Send(ctx, ev); err != nil {
			metrics.AlertsSentTotal.WithLabelValues(string(ev.Kind), n.Name(), "error").Inc()
			slog.Warn("alerts: delivery failed", "notifier", n.Name(), "kind", ev.Kind,
				"cluster", ev.Cluster, "namespace", ev.Namespace, "cronjob", ev.CronJob, "err", err)
			continue
		}
		metrics.AlertsSentTotal.WithLabelValues(string(ev.Kind), n.Name(), "ok").Inc()
		delivered++
	}

	e.attempts[key]++
	if delivered == 0 && e.attempts[key] < maxAttempts {
		return
	}
	delete(e.attempts, key)
	if err := e.store.RecordAlert(ctx, string(ev.Kind), subject, e.now()); err != nil {
		slog.Error("alerts: failed to record delivery", "kind", ev.Kind, "err", err)
	}
}

func cronJobPath(cluster, namespace, name string) string {
	return "/clusters/" + url.PathEscape(cluster) + "/cronjobs/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)
}
