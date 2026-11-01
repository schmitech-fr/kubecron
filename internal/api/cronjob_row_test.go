package api

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // named zones must resolve without host zoneinfo

	"github.com/kubecron/kubecron/internal/storage"
)

// TestBuildCronJobRow_MissedDetectionHonoursTimeZone is the DOM-1 regression
// test at the level the bug was visible: a healthy CronJob flagged "missed".
//
// The CronJob runs at 04:00 Tokyo, i.e. 19:00 UTC the previous day. At 05:00 UTC
// its last run (19:00:30 UTC yesterday) is the most recent scheduled tick, so
// nothing was missed. Evaluated in UTC instead, the previous tick looks like
// 04:00 UTC today — an hour before "now" and *after* the last run — so the row
// wrongly claims the job skipped a run.
func TestBuildCronJobRow_MissedDetectionHonoursTimeZone(t *testing.T) {
	now := time.Date(2024, 1, 15, 5, 0, 0, 0, time.UTC)
	lastRun := &storage.JobRun{
		ID:        "run-1",
		CronJobID: "c1/default/tokyo-cron",
		Status:    "succeeded",
		StartedAt: time.Date(2024, 1, 14, 19, 0, 30, 0, time.UTC), // 04:00:30 JST
	}
	summary := &storage.CronJobSummary{LastRun: lastRun}

	base := storage.CronJob{
		ID:        "c1/default/tokyo-cron",
		ClusterID: "c1",
		Namespace: "default",
		Name:      "tokyo-cron",
		Schedule:  "0 4 * * *",
	}

	withZone := base
	withZone.TimeZone = new("Asia/Tokyo")
	row := buildCronJobRow("c1", withZone, summary, nil, now)
	if row.IsMissed {
		t.Error("CronJob that ran at its last Tokyo tick must not be flagged missed")
	}
	if row.ScheduleError {
		t.Error("schedule and zone are both valid — ScheduleError must be false")
	}

	// Same inputs without the zone: this is the wrong verdict the fix removes.
	// Asserting it keeps the test honest about what the zone actually changes.
	rowNoZone := buildCronJobRow("c1", base, summary, nil, now)
	if !rowNoZone.IsMissed {
		t.Error("expected the zone-blind evaluation to produce the false 'missed' verdict")
	}
}

// TestBuildCronJobRow_NextRunHonoursTimeZone verifies the countdown target is
// the instant Kubernetes will actually fire the job.
func TestBuildCronJobRow_NextRunHonoursTimeZone(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cj := storage.CronJob{
		ID: "c1/default/ny-cron", ClusterID: "c1", Namespace: "default", Name: "ny-cron",
		Schedule: "0 4 * * *", TimeZone: new("America/New_York"),
	}

	row := buildCronJobRow("c1", cj, nil, nil, now)

	want := time.Date(2024, 1, 16, 9, 0, 0, 0, time.UTC) // 04:00 EST
	if !row.NextRun.Equal(want) {
		t.Errorf("NextRun = %v, want %v", row.NextRun.UTC(), want)
	}
}

// TestBuildCronJobRow_UnresolvableSchedule verifies an unknown zone yields no
// countdown and no missed claim, rather than a confidently wrong one.
func TestBuildCronJobRow_UnresolvableSchedule(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cj := storage.CronJob{
		ID: "c1/default/bad-tz", ClusterID: "c1", Namespace: "default", Name: "bad-tz",
		Schedule: "0 4 * * *", TimeZone: new("Not/AZone"),
	}

	row := buildCronJobRow("c1", cj, nil, nil, now)

	if !row.ScheduleError {
		t.Error("expected ScheduleError for an unknown time zone")
	}
	if !row.NextRun.IsZero() {
		t.Errorf("expected no next run, got %v", row.NextRun)
	}
	if row.IsMissed {
		t.Error("an unresolvable schedule must not be reported as missed")
	}

	html := renderCronJobRow(row)
	if !strings.Contains(html, "unresolved") {
		t.Error("expected the row to show an 'unresolved' marker instead of a countdown")
	}
	if strings.Contains(html, `class="countdown"`) {
		t.Error("expected no countdown element for an unresolvable schedule")
	}
}

// TestRenderCronJobRow_ShowsTimeZone verifies the zone is surfaced in the UI —
// a schedule means nothing without the zone it is read in.
func TestRenderCronJobRow_ShowsTimeZone(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cj := storage.CronJob{
		ID: "c1/default/ny-cron", ClusterID: "c1", Namespace: "default", Name: "ny-cron",
		Schedule: "0 4 * * *", TimeZone: new("America/New_York"),
	}

	html := renderCronJobRow(buildCronJobRow("c1", cj, nil, nil, now))
	if !strings.Contains(html, "America/New_York") {
		t.Error("expected the CronJob's time zone to be rendered")
	}

	cj.TimeZone = nil
	html = renderCronJobRow(buildCronJobRow("c1", cj, nil, nil, now))
	if strings.Contains(html, "spec.timeZone") {
		t.Error("expected no zone annotation for a CronJob that declares none")
	}
}

// TestBuildCronJobRow_ConcurrentAndSummary covers the plumbing of the batched
// read into the row: summaries are looked up by CronJob ID and a nil summary is
// tolerated.
func TestBuildCronJobRow_ConcurrentAndSummary(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cj := storage.CronJob{
		ID: "c1/default/cron", ClusterID: "c1", Namespace: "default", Name: "cron",
		Schedule: "*/5 * * * *",
	}

	sum := &storage.CronJobSummary{
		LastRun:   &storage.JobRun{ID: "r1", Status: "running", StartedAt: now},
		Stats7d:   &storage.RunStats{Total: 3, Succeeded: 2, Failed: 1},
		Durations: []int64{3000, 2000, 1000},
	}
	running := map[string]int{"c1/default/cron": 2}

	row := buildCronJobRow("c1", cj, sum, running, now)
	if !row.IsConcurrent {
		t.Error("expected IsConcurrent with two running runs")
	}
	if row.Stats7d == nil || row.Stats7d.Total != 3 {
		t.Errorf("expected stats to be carried over, got %+v", row.Stats7d)
	}
	if len(row.Durations) != 3 {
		t.Errorf("expected 3 durations, got %d", len(row.Durations))
	}

	// A CronJob with no runs yet has no summary entry.
	bare := buildCronJobRow("c1", cj, nil, nil, now)
	if bare.LastRun != nil || bare.Stats7d != nil || bare.Durations != nil {
		t.Error("expected an empty row for a CronJob with no summary")
	}
	if bare.IsConcurrent {
		t.Error("expected IsConcurrent=false with no running runs")
	}
}

// A run past its CronJob's runaway threshold shows a stuck badge carrying the
// reason; the same run within it shows none.
func TestBuildCronJobRow_StuckBadge(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cj := storage.CronJob{ID: "c1/ns/job", ClusterID: "c1", Namespace: "ns", Name: "job", Schedule: "0 0 1 1 *"}
	history := []int64{240_000, 240_000, 240_000, 240_000, 240_000} // 4 min each → threshold 12 min

	for _, tc := range []struct {
		elapsed time.Duration
		badge   bool
	}{{11 * time.Minute, false}, {13 * time.Minute, true}} {
		sum := &storage.CronJobSummary{
			LastRun:   &storage.JobRun{ID: "r", Status: "running", StartedAt: now.Add(-tc.elapsed)},
			Durations: history,
		}
		html := renderCronJobRow(buildCronJobRow("c1", cj, sum, nil, now))
		if got := strings.Contains(html, ">stuck</span>"); got != tc.badge {
			t.Errorf("elapsed %v: stuck badge = %v, want %v", tc.elapsed, got, tc.badge)
		}
		if tc.badge && !strings.Contains(html, `title="running for 13m00s, usually 4m00s"`) {
			t.Errorf("badge carries no reason: %s", html)
		}
	}
}
