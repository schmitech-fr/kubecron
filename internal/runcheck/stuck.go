// Package runcheck decides whether a running CronJob run is stuck.
//
// One rule, shared by the dashboard badge, the kubecron_run_stuck gauge and the
// alert evaluator, so the three can never disagree about the same run.
package runcheck

import (
	"fmt"
	"slices"
	"time"

	"github.com/kubecron/kubecron/internal/storage"
)

const (
	// HistoryRuns is how many recent finished runs form a CronJob's baseline.
	HistoryRuns = 20
	// MinHistory is the fewest finished runs a baseline needs. Below it the
	// duration rule says nothing: two runs are not a habit.
	MinHistory = 5
	// MinRunaway is a floor on the duration threshold, so a job that usually
	// takes two seconds is not called stuck at six.
	MinRunaway = time.Minute
	// WaitGrace is how long a pod may sit in a blocking state before the run is
	// stuck: one CrashLoopBackOff is a restart, five minutes of it is not.
	WaitGrace = 5 * time.Minute
)

// blockingReasons are pod waiting reasons that do not resolve on their own.
// ContainerCreating and PodInitializing are deliberately absent: they are the
// normal path, and a slow one is caught by the duration rule.
var blockingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"Unschedulable":              true,
}

// IsBlockingReason reports whether a pod waiting reason is one that can make a
// run stuck.
func IsBlockingReason(reason string) bool { return blockingReasons[reason] }

// Verdict is the outcome of Check. Reason is a short human sentence, empty
// when the run is not stuck.
type Verdict struct {
	Stuck  bool
	Reason string
}

// Check reports whether run is stuck at now. durationsMs are the CronJob's
// recent finished-run durations, newest first, as storage.GetRecentDurations
// returns them; the running run itself has none, so it is never part of its
// own baseline.
//
// A run is stuck when either:
//   - its pod has been in a blocking waiting state for WaitGrace or longer, or
//   - it has been running longer than max(p95 × 2, median × 3, MinRunaway) of
//     its CronJob's history (PRODUCT §3.3), given MinHistory finished runs.
func Check(run *storage.JobRun, durationsMs []int64, now time.Time) Verdict {
	if run == nil || run.Status != "running" {
		return Verdict{}
	}

	if run.PodWaitingReason != nil && run.PodWaitingSince != nil && IsBlockingReason(*run.PodWaitingReason) {
		if waited := now.Sub(*run.PodWaitingSince); waited >= WaitGrace {
			return Verdict{Stuck: true, Reason: fmt.Sprintf("pod in %s for %s", *run.PodWaitingReason, Human(waited))}
		}
	}

	threshold, ok := Threshold(durationsMs)
	if !ok {
		return Verdict{}
	}
	if elapsed := now.Sub(run.StartedAt); elapsed > threshold {
		median, _ := Median(durationsMs)
		return Verdict{Stuck: true, Reason: fmt.Sprintf("running for %s, usually %s", Human(elapsed), Human(median))}
	}
	return Verdict{}
}

// Threshold returns the duration past which a run of this CronJob is a
// runaway, and false when the history is too short to say.
func Threshold(durationsMs []int64) (time.Duration, bool) {
	if len(durationsMs) < MinHistory {
		return 0, false
	}
	median, _ := Median(durationsMs)
	p95 := percentile(durationsMs, 0.95)
	return max(2*p95, 3*median, MinRunaway), true
}

// Median returns the median of durationsMs, false when it is empty.
func Median(durationsMs []int64) (time.Duration, bool) {
	if len(durationsMs) == 0 {
		return 0, false
	}
	return percentile(durationsMs, 0.5), true
}

// percentile uses the nearest-rank method on a sorted copy: with twenty runs,
// p95 is the 19th, an observed duration rather than an interpolated one.
func percentile(durationsMs []int64, p float64) time.Duration {
	sorted := slices.Clone(durationsMs)
	slices.Sort(sorted)
	rank := int(p*float64(len(sorted))+0.999999) - 1
	rank = min(max(rank, 0), len(sorted)-1)
	return time.Duration(sorted[rank]) * time.Millisecond
}

// Human renders a duration the way the dashboard does: "3m12s", "2h05m".
func Human(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
