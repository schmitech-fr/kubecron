package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fake "k8s.io/client-go/kubernetes/fake"

	"github.com/kubecron/kubecron/internal/cluster"
	"github.com/kubecron/kubecron/internal/storage"
	"github.com/kubecron/kubecron/internal/streamer"
)

const (
	testClusterID = "test-cluster"
	testNS        = "default"
	testCJName    = "my-cronjob"
	testRunID     = "run-abc-00000001"
)

// newTestHandler builds a Handler backed by an in-memory SQLite store.
func newTestHandler(t *testing.T) (*Handler, storage.Store) {
	t.Helper()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return &Handler{
		store:       store,
		broadcaster: streamer.NewBroadcaster(),
		cacheSynced: func() bool { return true },
	}, store
}

// newTestHandlerWithRegistry is like newTestHandler but also returns a populated Registry.
func newTestHandlerWithRegistry(t *testing.T) (*Handler, storage.Store, *cluster.Registry) {
	t.Helper()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	reg := cluster.NewRegistry()
	return &Handler{
		store:       store,
		registry:    reg,
		broadcaster: streamer.NewBroadcaster(),
		cacheSynced: func() bool { return true },
	}, store, reg
}

func seedCluster(t *testing.T, store storage.Store) {
	t.Helper()
	err := store.UpsertCluster(context.Background(), storage.Cluster{
		ID: testClusterID, Name: "Test Cluster", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
}

// seedCronJob inserts a CronJob and returns its ID.
func seedCronJob(t *testing.T, store storage.Store) string {
	t.Helper()
	id := testClusterID + "/" + testNS + "/" + testCJName
	err := store.UpsertCronJob(context.Background(), storage.CronJob{
		ID:        id,
		ClusterID: testClusterID,
		Namespace: testNS,
		Name:      testCJName,
		Schedule:  "*/5 * * * *",
		UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed cronjob: %v", err)
	}
	return id
}

func seedRun(t *testing.T, store storage.Store, cronJobID, status string) {
	t.Helper()
	err := store.UpsertJobRun(context.Background(), storage.JobRun{
		ID:        testRunID,
		CronJobID: cronJobID,
		PodName:   "my-cronjob-xyz",
		Trigger:   "scheduled",
		Status:    status,
		StartedAt: time.Now().Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// ── Observability ─────────────────────────────────────────────────────────────

func TestHealthz(t *testing.T) {
	h, _ := newTestHandler(t)
	w := httptest.NewRecorder()
	h.Healthz(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("want status=ok, got %q", body["status"])
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name     string
		synced   bool
		wantCode int
	}{
		{"synced", true, http.StatusOK},
		{"not synced", false, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, err := storage.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			h := &Handler{store: store, cacheSynced: func() bool { return tc.synced }}
			w := httptest.NewRecorder()
			h.Readyz(w, httptest.NewRequest("GET", "/readyz", nil))
			if w.Code != tc.wantCode {
				t.Fatalf("want %d, got %d", tc.wantCode, w.Code)
			}
		})
	}
}

// ── JSON API: clusters ────────────────────────────────────────────────────────

func TestListClusters_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	w := httptest.NewRecorder()
	h.ListClusters(w, httptest.NewRequest("GET", "/api/clusters", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp []clusterResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 0 {
		t.Errorf("want empty slice, got %d items", len(resp))
	}
}

func TestListClusters_WithData(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)

	w := httptest.NewRecorder()
	h.ListClusters(w, httptest.NewRequest("GET", "/api/clusters", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp []clusterResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(resp))
	}
	if resp[0].ID != testClusterID {
		t.Errorf("want ID=%q, got %q", testClusterID, resp[0].ID)
	}
}

// ── JSON API: cronjobs ────────────────────────────────────────────────────────

func TestListCronJobs_Empty(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)

	r := httptest.NewRequest("GET", "/api/clusters/"+testClusterID+"/cronjobs", nil)
	r.SetPathValue("clusterID", testClusterID)
	w := httptest.NewRecorder()
	h.ListCronJobs(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp []cronJobResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 0 {
		t.Errorf("want empty, got %d", len(resp))
	}
}

func TestListCronJobs_WithData(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	seedCronJob(t, store)

	r := httptest.NewRequest("GET", "/api/clusters/"+testClusterID+"/cronjobs", nil)
	r.SetPathValue("clusterID", testClusterID)
	w := httptest.NewRecorder()
	h.ListCronJobs(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp []cronJobResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 1 {
		t.Fatalf("want 1 cronjob, got %d", len(resp))
	}
	if resp[0].Name != testCJName {
		t.Errorf("want Name=%q, got %q", testCJName, resp[0].Name)
	}
	if resp[0].Schedule != "*/5 * * * *" {
		t.Errorf("want schedule=*/5 * * * *, got %q", resp[0].Schedule)
	}
}

// ── JSON API: runs ────────────────────────────────────────────────────────────

func TestListRuns_CronJobNotFound(t *testing.T) {
	h, _ := newTestHandler(t)

	r := httptest.NewRequest("GET", "/api/clusters/"+testClusterID+"/cronjobs/"+testNS+"/ghost/runs", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", "ghost")
	w := httptest.NewRecorder()
	h.ListRuns(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestListRuns_WithRuns(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	r := httptest.NewRequest("GET", "/api/clusters/"+testClusterID+"/cronjobs/"+testNS+"/"+testCJName+"/runs", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	w := httptest.NewRecorder()
	h.ListRuns(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp []storage.JobRun
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 1 {
		t.Fatalf("want 1 run, got %d", len(resp))
	}
	if resp[0].ID != testRunID {
		t.Errorf("want ID=%q, got %q", testRunID, resp[0].ID)
	}
	if resp[0].Status != "succeeded" {
		t.Errorf("want status=succeeded, got %q", resp[0].Status)
	}
}

// ── Resource samples ──────────────────────────────────────────────────────────

func TestGetResourceSamples(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	r := httptest.NewRequest("GET", "/api/runs/"+testRunID+"/resources", nil)
	r.SetPathValue("id", testRunID)
	w := httptest.NewRecorder()
	h.GetResourceSamples(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.RunID != testRunID {
		t.Errorf("want run_id=%q, got %q", testRunID, resp.RunID)
	}
}

// ── Log download ──────────────────────────────────────────────────────────────

func TestDownloadLogs_Empty(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	r := httptest.NewRequest("GET", "/api/runs/"+testRunID+"/logs.txt", nil)
	r.SetPathValue("id", testRunID)
	w := httptest.NewRecorder()
	h.DownloadLogs(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("want text/plain, got %q", ct)
	}
	if w.Body.Len() != 0 {
		t.Errorf("expected empty body, got %q", w.Body.String())
	}
}

func TestDownloadLogs_WithLines(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")
	if err := store.BatchInsertLogLines(context.Background(), testRunID, []string{"hello world", "second line", "done"}); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/runs/"+testRunID+"/logs.txt", nil)
	r.SetPathValue("id", testRunID)
	w := httptest.NewRecorder()
	h.DownloadLogs(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, line := range []string{"hello world", "second line", "done"} {
		if !strings.Contains(body, line) {
			t.Errorf("response body missing %q", line)
		}
	}
}

// ── CronJob mutations (suspend / resume / trigger) ────────────────────────────

func TestSuspend_ClusterNotFound(t *testing.T) {
	h, _, _ := newTestHandlerWithRegistry(t)

	r := httptest.NewRequest("POST", "/api/clusters/ghost/cronjobs/default/my-job/suspend", nil)
	r.SetPathValue("clusterID", "ghost")
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	w := httptest.NewRecorder()
	h.Suspend(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestSuspend_OK(t *testing.T) {
	h, _, reg := newTestHandlerWithRegistry(t)
	fakeClient := fake.NewSimpleClientset()
	reg.Register(&cluster.ClusterClient{ID: testClusterID, Clientset: fakeClient})

	ctx := context.Background()
	_, err := fakeClient.BatchV1().CronJobs(testNS).Create(ctx,
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: testCJName, Namespace: testNS}},
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/api/clusters/"+testClusterID+"/cronjobs/"+testNS+"/"+testCJName+"/suspend", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	w := httptest.NewRecorder()
	h.Suspend(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResume_OK(t *testing.T) {
	h, _, reg := newTestHandlerWithRegistry(t)
	fakeClient := fake.NewSimpleClientset()
	reg.Register(&cluster.ClusterClient{ID: testClusterID, Clientset: fakeClient})

	ctx := context.Background()
	_, err := fakeClient.BatchV1().CronJobs(testNS).Create(ctx,
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: testCJName, Namespace: testNS}},
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/api/clusters/"+testClusterID+"/cronjobs/"+testNS+"/"+testCJName+"/resume", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	w := httptest.NewRecorder()
	h.Resume(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTrigger_ClusterNotFound(t *testing.T) {
	h, _, _ := newTestHandlerWithRegistry(t)

	r := httptest.NewRequest("POST", "/api/clusters/ghost/cronjobs/default/my-job/trigger", nil)
	r.SetPathValue("clusterID", "ghost")
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	w := httptest.NewRecorder()
	h.Trigger(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestTrigger_OK(t *testing.T) {
	h, store, reg := newTestHandlerWithRegistry(t)
	seedCluster(t, store)
	seedCronJob(t, store)

	fakeClient := fake.NewSimpleClientset()
	reg.Register(&cluster.ClusterClient{ID: testClusterID, Clientset: fakeClient})

	ctx := context.Background()
	_, err := fakeClient.BatchV1().CronJobs(testNS).Create(ctx,
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: testCJName, Namespace: testNS}},
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/api/clusters/"+testClusterID+"/cronjobs/"+testNS+"/"+testCJName+"/trigger", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	w := httptest.NewRecorder()
	h.Trigger(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp triggerResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	// Verify a Job was actually created in the fake cluster.
	jobs, err := fakeClient.BatchV1().Jobs(testNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job in fake cluster, got %d", len(jobs.Items))
	}
	// Without a CronJob owner the JobHandler ignores the Job and history
	// limits never collect it.
	refs := jobs.Items[0].OwnerReferences
	if len(refs) != 1 || refs[0].Kind != "CronJob" || refs[0].Name != testCJName ||
		refs[0].Controller == nil || !*refs[0].Controller {
		t.Errorf("want a controller owner reference to CronJob %q, got %+v", testCJName, refs)
	}
	_ = resp // run_id and pod_name depend on fake UID/name generation
}

// ── UI — HTML smoke tests ─────────────────────────────────────────────────────

func TestDashboard_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	w := httptest.NewRecorder()
	h.Dashboard(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("want text/html, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "KubeCron") {
		t.Error("expected body to contain 'KubeCron'")
	}
}

// With one cluster the fleet summary and that cluster's own summary are the
// same numbers, so "/" must not render a second copy of the cluster view —
// it redirects there instead.
func TestDashboard_SingleClusterRedirects(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)

	w := httptest.NewRecorder()
	h.Dashboard(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", w.Code)
	}
	if got, want := w.Header().Get("Location"), "/clusters/"+testClusterID+"?days=7"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

// The selected window must survive the redirect, or following it would silently
// reset the reader's range back to the default.
func TestDashboard_SingleClusterRedirectKeepsWindow(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)

	w := httptest.NewRecorder()
	h.Dashboard(w, httptest.NewRequest("GET", "/?days=30", nil))
	if got, want := w.Header().Get("Location"), "/clusters/"+testClusterID+"?days=30"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

// With several clusters there is something to aggregate, so the overview
// renders rather than redirecting.
func TestDashboard_MultipleClustersRendersOverview(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	if err := store.UpsertCluster(context.Background(), storage.Cluster{
		ID: "second", Name: "second", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	w := httptest.NewRecorder()
	h.Dashboard(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{testClusterID, "second", "Success rate"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview body missing %q", want)
		}
	}
}

func TestClusterDetail(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	seedCronJob(t, store)

	r := httptest.NewRequest("GET", "/clusters/"+testClusterID, nil)
	r.SetPathValue("clusterID", testClusterID)
	w := httptest.NewRecorder()
	h.ClusterDetail(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), testNS) {
		t.Errorf("expected body to contain namespace %q", testNS)
	}
}

func TestRunsList_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)

	r := httptest.NewRequest("GET", "/clusters/"+testClusterID+"/cronjobs/"+testNS+"/ghost/runs", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", "ghost")
	w := httptest.NewRecorder()
	h.RunsList(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestRunsList_OK(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	r := httptest.NewRequest("GET", "/clusters/"+testClusterID+"/cronjobs/"+testNS+"/"+testCJName+"/runs", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	w := httptest.NewRecorder()
	h.RunsList(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, testRunID) {
		t.Errorf("expected body to contain run ID %q", testRunID)
	}
	if !strings.Contains(body, "*/5 * * * *") {
		t.Errorf("expected body to contain cron schedule")
	}
}

func TestRunDetail_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)

	r := httptest.NewRequest("GET", "/clusters/"+testClusterID+"/cronjobs/"+testNS+"/"+testCJName+"/runs/ghost-id", nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	r.SetPathValue("id", "ghost-id")
	w := httptest.NewRecorder()
	h.RunDetail(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestRunDetail_OK(t *testing.T) {
	h, store := newTestHandler(t)
	seedCluster(t, store)
	cjID := seedCronJob(t, store)
	seedRun(t, store, cjID, "succeeded")

	r := httptest.NewRequest("GET", "/clusters/"+testClusterID+"/cronjobs/"+testNS+"/"+testCJName+"/runs/"+testRunID, nil)
	r.SetPathValue("clusterID", testClusterID)
	r.SetPathValue("ns", testNS)
	r.SetPathValue("name", testCJName)
	r.SetPathValue("id", testRunID)
	w := httptest.NewRecorder()
	h.RunDetail(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "succeeded") {
		t.Errorf("expected body to contain 'succeeded'")
	}
	if !strings.Contains(body, testRunID[:8]) {
		t.Errorf("expected body to contain short run ID %q", testRunID[:8])
	}
}
