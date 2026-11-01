package runcheck

import (
	"testing"
	"time"

	"github.com/kubecron/kubecron/internal/storage"
)

func ptr[T any](v T) *T { return &v }

func minutes(ms ...int) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = int64(m) * 60_000
	}
	return out
}

func TestThreshold(t *testing.T) {
	tests := []struct {
		name  string
		hist  []int64
		want  time.Duration
		valid bool
	}{
		{"too little history", minutes(4, 4, 4, 4), 0, false},
		// p95 = 4m, median = 4m → max(8m, 12m) = 12m
		{"steady job", minutes(4, 4, 4, 4, 4), 12 * time.Minute, true},
		// p95 = 30m (an outlier), median = 4m → max(60m, 12m) = 60m
		{"one slow outlier", minutes(4, 4, 4, 4, 4, 4, 4, 4, 4, 30), 60 * time.Minute, true},
		// a two-second job is not stuck at six: floor applies
		{"fast job floor", []int64{2000, 2000, 2000, 2000, 2000}, MinRunaway, true},
	}
	for _, tc := range tests {
		got, ok := Threshold(tc.hist)
		if ok != tc.valid || got != tc.want {
			t.Errorf("%s: Threshold = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.valid)
		}
	}
}

func TestCheck(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	hist := minutes(4, 4, 4, 4, 4)

	running := func(elapsed time.Duration) *storage.JobRun {
		return &storage.JobRun{Status: "running", StartedAt: now.Add(-elapsed)}
	}

	tests := []struct {
		name  string
		run   *storage.JobRun
		hist  []int64
		stuck bool
	}{
		{"nil run", nil, hist, false},
		{"finished run is never stuck", &storage.JobRun{Status: "failed", StartedAt: now.Add(-24 * time.Hour)}, hist, false},
		{"within threshold", running(11 * time.Minute), hist, false},
		{"past threshold", running(13 * time.Minute), hist, true},
		{"no history: duration says nothing", running(10 * time.Hour), nil, false},
		{"crash loop past grace, no history needed", func() *storage.JobRun {
			r := running(10 * time.Minute)
			r.PodWaitingReason = ptr("CrashLoopBackOff")
			r.PodWaitingSince = ptr(now.Add(-WaitGrace))
			return r
		}(), nil, true},
		{"crash loop within grace", func() *storage.JobRun {
			r := running(10 * time.Minute)
			r.PodWaitingReason = ptr("CrashLoopBackOff")
			r.PodWaitingSince = ptr(now.Add(-time.Minute))
			return r
		}(), nil, false},
		{"normal waiting reason is not blocking", func() *storage.JobRun {
			r := running(10 * time.Minute)
			r.PodWaitingReason = ptr("ContainerCreating")
			r.PodWaitingSince = ptr(now.Add(-time.Hour))
			return r
		}(), nil, false},
	}
	for _, tc := range tests {
		v := Check(tc.run, tc.hist, now)
		if v.Stuck != tc.stuck {
			t.Errorf("%s: stuck = %v (%q), want %v", tc.name, v.Stuck, v.Reason, tc.stuck)
		}
		if v.Stuck && v.Reason == "" {
			t.Errorf("%s: stuck without a reason", tc.name)
		}
	}
}

func TestHuman(t *testing.T) {
	for d, want := range map[time.Duration]string{
		42 * time.Second:               "42s",
		3*time.Minute + 12*time.Second: "3m12s",
		2*time.Hour + 5*time.Minute:    "2h05m",
	} {
		if got := Human(d); got != want {
			t.Errorf("Human(%v) = %q, want %q", d, got, want)
		}
	}
}
