package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kubecron/kubecron/internal/storage"
	"github.com/kubecron/kubecron/internal/streamer"
)

// newV1Handler builds a Handler carrying collector metadata, as the server does.
func newV1Handler(t *testing.T) (*Handler, storage.Store) {
	t.Helper()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &Handler{
		store:       store,
		broadcaster: streamer.NewBroadcaster(),
		cacheSynced: func() bool { return true },
		info: CollectorInfo{
			Mode: ModeServer, RetentionDays: 90, LogRetentionDays: 14, SampleIntervalSeconds: 15,
		},
	}, store
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(into); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func TestSplitCronJobID(t *testing.T) {
	tests := []struct {
		in                            string
		wantCluster, wantNS, wantName string
	}{
		{"prod-eu/default/backup", "prod-eu", "default", "backup"},
		// A namespace and a name are DNS labels and a cluster ID a filename
		// stem, so none can contain the separator; three parts is the only
		// well-formed shape.
		{"local/kube-system/cleanup-old-logs", "local", "kube-system", "cleanup-old-logs"},
		// Malformed input yields what is there rather than an error: the run
		// record it accompanies is still worth returning.
		{"orphan", "orphan", "", ""},
		{"", "", "", ""},
	}
	for _, tc := range tests {
		got := splitCronJobID(tc.in)
		if got.ClusterID != tc.wantCluster || got.Namespace != tc.wantNS || got.Name != tc.wantName {
			t.Errorf("splitCronJobID(%q) = %+v", tc.in, got)
		}
		if got.ID != tc.in {
			t.Errorf("splitCronJobID(%q) dropped the verbatim id: %q", tc.in, got.ID)
		}
	}
}

func TestClampLimit(t *testing.T) {
	tests := []struct {
		raw      string
		def, max int
		want     int
	}{
		{"", 100, 1000, 100},
		{"50", 100, 1000, 50},
		{"5000", 100, 1000, 1000}, // capped
		{"0", 100, 1000, 100},     // nonsense falls back rather than returning nothing
		{"-3", 100, 1000, 100},
		{"abc", 100, 1000, 100},
		{"5000", 0, 0, 5000}, // max 0 means uncapped (the log body)
	}
	for _, tc := range tests {
		if got := clampLimit(tc.raw, tc.def, tc.max); got != tc.want {
			t.Errorf("clampLimit(%q, %d, %d) = %d, want %d", tc.raw, tc.def, tc.max, got, tc.want)
		}
	}
}

func TestListClustersV1ReportsObservedSince(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)

	w := httptest.NewRecorder()
	h.ListClustersV1(w, httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var body struct {
		Clusters []clusterV1 `json:"clusters"`
	}
	decodeJSON(t, w, &body)
	if len(body.Clusters) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(body.Clusters))
	}
	// Without this a consumer cannot tell a period nobody was watching from a
	// period in which nothing ran.
	if body.Clusters[0].ObservedSince.IsZero() {
		t.Error("observed_since is zero — a consumer cannot date the start of the record")
	}
}

func TestListCronJobsV1(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	r := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+testClusterID+"/cronjobs", nil)
	r.SetPathValue("clusterID", testClusterID)
	w := httptest.NewRecorder()
	h.ListCronJobsV1(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var body struct {
		ClusterID string      `json:"cluster_id"`
		CronJobs  []cronJobV1 `json:"cronjobs"`
	}
	decodeJSON(t, w, &body)
	if body.ClusterID != testClusterID {
		t.Errorf("cluster_id = %q", body.ClusterID)
	}
	if len(body.CronJobs) != 1 {
		t.Fatalf("want 1 cronjob, got %d", len(body.CronJobs))
	}
	cj := body.CronJobs[0]
	if cj.ID != cjID || cj.Namespace != testNS || cj.Name != testCJName {
		t.Errorf("identity fields wrong: %+v", cj)
	}
	// The consumer must not have to parse the composite id to locate the job.
	if cj.ClusterID != testClusterID {
		t.Errorf("cluster_id = %q, want %q", cj.ClusterID, testClusterID)
	}
	if cj.NextRunAt == nil {
		t.Error("next_run_at missing for a resolvable schedule")
	}
	if cj.LastRun == nil {
		t.Error("last_run missing although a run was seeded")
	}
	if cj.Stats7d == nil {
		t.Error("stats_7d missing although a run was seeded")
	}
}

// TestListCronJobsV1UnresolvableScheduleOmitsNextRun: a wrong countdown is
// worse than an absent one, so an unparseable schedule must yield no field at
// all rather than a zero time.
func TestListCronJobsV1UnresolvableScheduleOmitsNextRun(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	ctx := context.Background()
	if err := store.UpsertCronJob(ctx, storage.CronJob{
		ID:        testClusterID + "/" + testNS + "/broken",
		ClusterID: testClusterID,
		Namespace: testNS,
		Name:      "broken",
		Schedule:  "not a cron expression",
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+testClusterID+"/cronjobs", nil)
	r.SetPathValue("clusterID", testClusterID)
	w := httptest.NewRecorder()
	h.ListCronJobsV1(w, r)

	var body struct {
		CronJobs []cronJobV1 `json:"cronjobs"`
	}
	decodeJSON(t, w, &body)
	if len(body.CronJobs) != 1 {
		t.Fatalf("want 1 cronjob, got %d", len(body.CronJobs))
	}
	if body.CronJobs[0].NextRunAt != nil {
		t.Errorf("next_run_at = %v for an unparseable schedule, want omitted", body.CronJobs[0].NextRunAt)
	}
}

func TestListRunsV1Paginates(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)

	ctx := context.Background()
	base := time.Now().Add(-10 * time.Hour)
	for i := range 5 {
		if err := store.UpsertJobRun(ctx, storage.JobRun{
			ID:        "run-" + string(rune('a'+i)),
			CronJobID: cjID,
			PodName:   "pod",
			Trigger:   "scheduled",
			Status:    "succeeded",
			StartedAt: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("seed run %d: %v", i, err)
		}
	}

	get := func(query string) runsV1Response {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/x?"+query, nil)
		r.SetPathValue("clusterID", testClusterID)
		r.SetPathValue("ns", testNS)
		r.SetPathValue("name", testCJName)
		w := httptest.NewRecorder()
		h.ListRunsV1(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", w.Code)
		}
		var body runsV1Response
		decodeJSON(t, w, &body)
		return body
	}

	first := get("limit=2")
	if len(first.Runs) != 2 {
		t.Fatalf("page 1: want 2 runs, got %d", len(first.Runs))
	}
	// Newest first: the last seeded run has the latest start time.
	if !first.Runs[0].StartedAt.After(first.Runs[1].StartedAt) {
		t.Error("runs are not ordered newest-first")
	}
	if first.NextCursor == "" {
		t.Fatal("a full page must offer a cursor")
	}

	second := get("limit=2&before=" + url.QueryEscape(first.NextCursor))
	if len(second.Runs) != 2 {
		t.Fatalf("page 2: want 2 runs, got %d", len(second.Runs))
	}
	if second.Runs[0].ID == first.Runs[0].ID {
		t.Error("the cursor did not advance")
	}

	last := get("limit=2&before=" + url.QueryEscape(second.NextCursor))
	if len(last.Runs) != 1 {
		t.Fatalf("page 3: want 1 run, got %d", len(last.Runs))
	}
	if last.NextCursor != "" {
		t.Error("a short page is the last page and must offer no cursor")
	}
}

func TestListRunsV1UnknownCronJob(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", "does-not-exist")
	w := httptest.NewRecorder()
	h.ListRunsV1(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

func TestGetRunV1(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+testRunID, nil)
	r.SetPathValue("id", testRunID)
	w := httptest.NewRecorder()
	h.GetRunV1(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var body runV1Response
	decodeJSON(t, w, &body)
	if body.Run == nil || body.Run.ID != testRunID {
		t.Fatalf("run missing or wrong: %+v", body.Run)
	}
	if body.CronJob.ClusterID != testClusterID || body.CronJob.Name != testCJName {
		t.Errorf("cronjob reference wrong: %+v", body.CronJob)
	}
}

func TestGetResourceSamplesV1CarriesIntervalAndSummary(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	ctx := context.Background()
	if err := store.InsertResourceSample(ctx, testRunID, 250, 64<<20); err != nil {
		t.Fatalf("seed sample: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+testRunID+"/samples", nil)
	r.SetPathValue("id", testRunID)
	w := httptest.NewRecorder()
	h.GetResourceSamplesV1(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var body samplesV1Response
	decodeJSON(t, w, &body)
	if len(body.Samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(body.Samples))
	}
	// Without the interval a consumer cannot tell an empty series on a
	// five-second run from a cluster with no Metrics API.
	if body.IntervalSeconds != 15 {
		t.Errorf("interval_seconds = %d, want 15", body.IntervalSeconds)
	}
}

func TestGetResourceSamplesV1UnknownRun(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/nope/samples", nil)
	r.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	h.GetResourceSamplesV1(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

func TestGetLogsV1(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	ctx := context.Background()
	if err := store.BatchInsertLogLines(ctx, testRunID, []string{"line one", "line two", "line three"}); err != nil {
		t.Fatalf("seed logs: %v", err)
	}

	get := func(query string) logsV1Response {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+testRunID+"/logs?"+query, nil)
		r.SetPathValue("id", testRunID)
		w := httptest.NewRecorder()
		h.GetLogsV1(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", w.Code)
		}
		var body logsV1Response
		decodeJSON(t, w, &body)
		return body
	}

	// The whole body by default: a run's log exists nowhere else once the pod
	// is gone, so silently truncating it would be the worst default available.
	all := get("")
	if len(all.Lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(all.Lines))
	}
	if all.Truncated {
		t.Error("truncated=true on a complete body")
	}
	if all.Lines[0].Line != "line one" {
		t.Errorf("lines are not oldest-first: %+v", all.Lines)
	}

	tail := get("limit=2")
	if len(tail.Lines) != 2 || !tail.Truncated {
		t.Errorf("limit=2 gave %d lines, truncated=%v", len(tail.Lines), tail.Truncated)
	}
	if tail.Lines[1].Line != "line three" {
		t.Errorf("a limit must return the tail, got %+v", tail.Lines)
	}
}

// TestGetLogsV1DistinguishesExpiredFromSilent is the constraint the whole shape
// exists for: a run whose log body aged out of retention must not look like a
// run that printed nothing.
func TestGetLogsV1DistinguishesExpiredFromSilent(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	ctx := context.Background()

	// A run that printed nothing: no lines, no recorded size.
	seedRun(t, store, cjID, "succeeded")
	silent := getLogs(t, h, testRunID)
	if silent.Expired {
		t.Error("a run that printed nothing was reported as expired")
	}

	// A run that printed 4 KiB whose lines are gone: size recorded, no lines.
	if err := store.UpsertJobRun(ctx, storage.JobRun{
		ID: "run-expired", CronJobID: cjID, PodName: "pod",
		Trigger: "scheduled", Status: "succeeded", StartedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.AddLogSize(ctx, "run-expired", 4096); err != nil {
		t.Fatalf("seed size: %v", err)
	}
	expired := getLogs(t, h, "run-expired")
	if !expired.Expired {
		t.Error("a run whose captured log body is gone was not reported as expired")
	}
	if expired.LogSizeBytes != 4096 {
		t.Errorf("log_size_bytes = %d, want 4096", expired.LogSizeBytes)
	}
}

func getLogs(t *testing.T, h *Handler, runID string) logsV1Response {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/logs", nil)
	r.SetPathValue("id", runID)
	w := httptest.NewRecorder()
	h.GetLogsV1(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var body logsV1Response
	decodeJSON(t, w, &body)
	return body
}

func TestDailyRunStatsV1(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	r := httptest.NewRequest(http.MethodGet, "/api/v1/x/daily?days=30", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	w := httptest.NewRecorder()
	h.DailyRunStatsV1(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var body struct {
		Days          int                    `json:"days"`
		Daily         []storage.DailyRunStat `json:"daily"`
		ObservedSince *time.Time             `json:"observed_since"`
	}
	decodeJSON(t, w, &body)
	if body.Days != 30 {
		t.Errorf("days = %d, want 30", body.Days)
	}
	if len(body.Daily) != 1 || body.Daily[0].Succeeded != 1 {
		t.Errorf("daily = %+v", body.Daily)
	}
	// A heatmap without this paints the days before the collector existed the
	// same colour as days on which the job genuinely did not fire.
	if body.ObservedSince == nil {
		t.Error("observed_since missing — the heatmap cannot distinguish unwatched days")
	}
}

func TestV1NotFoundNamesTheContract(t *testing.T) {
	h, _ := newV1Handler(t)
	w := httptest.NewRecorder()
	h.v1NotFound(w, httptest.NewRequest(http.MethodGet, "/api/v1/from-a-newer-build", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
	var body map[string]string
	decodeJSON(t, w, &body)
	// A consumer that asked for a route from a newer contract must learn which
	// rung it landed on, not parse an HTML error page.
	if body["api_version"] != APIVersion || body["product"] != "kubecron" {
		t.Errorf("404 body does not identify the contract: %+v", body)
	}
}

// TestListRunsV1TolerantOfUnescapedCursor: a "+" in a query string decodes to a
// space, so a consumer that pastes the cursor in unescaped would otherwise be
// silently served page one forever.
func TestListRunsV1TolerantOfUnescapedCursor(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)

	ctx := context.Background()
	base := time.Now().Add(-10 * time.Hour)
	for i := range 4 {
		if err := store.UpsertJobRun(ctx, storage.JobRun{
			ID:        "run-" + string(rune('a'+i)),
			CronJobID: cjID,
			PodName:   "pod",
			Trigger:   "scheduled",
			Status:    "succeeded",
			StartedAt: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("seed run %d: %v", i, err)
		}
	}

	get := func(rawQuery string) runsV1Response {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
		r.URL.RawQuery = rawQuery
		r.SetPathValue("clusterID", testClusterID)
		r.SetPathValue("ns", testNS)
		r.SetPathValue("name", testCJName)
		w := httptest.NewRecorder()
		h.ListRunsV1(w, r)
		var body runsV1Response
		decodeJSON(t, w, &body)
		return body
	}

	first := get("limit=2")
	if first.NextCursor == "" {
		t.Fatal("expected a cursor")
	}
	// Deliberately NOT escaped — this is the mistake being defended against.
	second := get("limit=2&before=" + first.NextCursor)
	if len(second.Runs) == 0 {
		t.Fatal("unescaped cursor returned nothing")
	}
	if second.Runs[0].ID == first.Runs[0].ID {
		t.Error("unescaped cursor silently reset to page one")
	}
}

// A run far past its CronJob's usual duration is reported stuck, with the
// reason, and the discovery document says the field is meaningful.
func TestListCronJobsV1ReportsStuck(t *testing.T) {
	h, store := newV1Handler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	ctx := context.Background()
	now := time.Now()
	for i := range 5 {
		start := now.Add(-time.Duration(10-i) * time.Hour)
		end := start.Add(4 * time.Minute)
		id := fmt.Sprintf("ok-%d", i)
		if err := store.UpsertJobRun(ctx, storage.JobRun{ID: id, CronJobID: cjID, PodName: id, Trigger: "scheduled", Status: "running", StartedAt: start}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateJobRunStatus(ctx, id, "succeeded", &end, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertJobRun(ctx, storage.JobRun{ID: "slow", CronJobID: cjID, PodName: "slow", Trigger: "scheduled", Status: "running", StartedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+testClusterID+"/cronjobs", nil)
	r.SetPathValue("clusterID", testClusterID)
	w := httptest.NewRecorder()
	h.ListCronJobsV1(w, r)
	var body struct {
		CronJobs []cronJobV1 `json:"cronjobs"`
	}
	decodeJSON(t, w, &body)
	if len(body.CronJobs) != 1 || !body.CronJobs[0].Stuck || !strings.Contains(body.CronJobs[0].StuckReason, "usually 4m00s") {
		t.Fatalf("cronjobs = %+v, want one stuck with its reason", body.CronJobs)
	}

	w = httptest.NewRecorder()
	h.Collector(w, httptest.NewRequest(http.MethodGet, "/api/v1/collector", nil))
	if !strings.Contains(w.Body.String(), `"stuck_detection":true`) {
		t.Errorf("collector document does not advertise stuck_detection: %s", w.Body.String())
	}
}
