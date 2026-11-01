package alert

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubecron/kubecron/internal/storage"
)

type fakeNotifier struct {
	events []Event
	fail   bool
}

func (f *fakeNotifier) Name() string { return "fake" }
func (f *fakeNotifier) Send(_ context.Context, e Event) error {
	if f.fail {
		return errors.New("down")
	}
	f.events = append(f.events, e)
	return nil
}

const cjID = "c1/default/backup"

// harness is a store with one cluster and one CronJob, an evaluator wired to a
// fake notifier, and a clock the test moves.
type harness struct {
	t     *testing.T
	ctx   context.Context
	store storage.Store
	ev    *Evaluator
	note  *fakeNotifier
	now   time.Time
}

func newHarness(t *testing.T, schedule string, kinds ...Kind) *harness {
	t.Helper()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := store.UpsertCluster(ctx, storage.Cluster{ID: "c1", Name: "c1", CreatedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCronJob(ctx, storage.CronJob{
		ID: cjID, ClusterID: "c1", Namespace: "default", Name: "backup",
		Schedule: schedule, UpdatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: ctx, store: store, note: &fakeNotifier{}, now: now}
	h.ev = &Evaluator{
		store: store, notifiers: []Notifier{h.note}, kinds: map[Kind]bool{},
		logLines: 3, baseURL: "https://kc.example", attempts: map[string]int{},
		now: func() time.Time { return h.now },
	}
	for _, k := range kinds {
		h.ev.kinds[k] = true
	}
	return h
}

// run inserts a run that started ago and, unless status is running, finished
// after took with the given exit code.
func (h *harness) run(id, status string, ago, took time.Duration, exit int) {
	h.t.Helper()
	start := h.now.Add(-ago)
	if err := h.store.UpsertJobRun(h.ctx, storage.JobRun{
		ID: id, CronJobID: cjID, PodName: id, Trigger: "scheduled", Status: "running", StartedAt: start,
	}); err != nil {
		h.t.Fatal(err)
	}
	if status == "running" {
		return
	}
	end := start.Add(took)
	if err := h.store.UpdateJobRunStatus(h.ctx, id, status, &end, exit, 0); err != nil {
		h.t.Fatal(err)
	}
}

func TestFailed_SentOnceWithLogsAndBaseline(t *testing.T) {
	h := newHarness(t, "0 0 1 1 *", KindFailed)
	for i := range 5 {
		h.run("ok-"+string(rune('a'+i)), "succeeded", time.Duration(10-i)*time.Hour, 4*time.Minute, 0)
	}
	h.run("bad", "failed", 10*time.Minute, 3*time.Minute+12*time.Second, 1)
	if err := h.store.BatchInsertLogLines(h.ctx, "bad", []string{"one", "two", "three", "boom"}); err != nil {
		t.Fatal(err)
	}

	h.ev.evaluate(h.ctx)
	h.ev.evaluate(h.ctx)

	if len(h.note.events) != 1 {
		t.Fatalf("want exactly one alert across two passes, got %d", len(h.note.events))
	}
	e := h.note.events[0]
	if e.Kind != KindFailed || e.CronJob != "backup" || e.RunID != "bad" {
		t.Errorf("event = %+v", e)
	}
	if !strings.Contains(e.Summary, "3m12s") || !strings.Contains(e.Summary, "exit code 1") || !strings.Contains(e.Summary, "usually takes 4m00s") {
		t.Errorf("summary = %q", e.Summary)
	}
	if strings.Join(e.LogTail, ",") != "two,three,boom" {
		t.Errorf("log tail = %v, want the last 3 lines", e.LogTail)
	}
	if e.URL != "https://kc.example/clusters/c1/cronjobs/default/backup/runs/bad" {
		t.Errorf("url = %q", e.URL)
	}

	// A restart is a new evaluator: the delivery is remembered in the store.
	h.ev.attempts = map[string]int{}
	h.ev.evaluate(h.ctx)
	if len(h.note.events) != 1 {
		t.Errorf("re-sent after restart: %d alerts", len(h.note.events))
	}
}

func TestFailed_IgnoresRestartMarkSettleAndOldFailures(t *testing.T) {
	h := newHarness(t, "0 0 1 1 *", KindFailed)
	// Marked failed by startup recovery: KubeCron was not watching, the job
	// did not necessarily fail.
	h.run("restart-mark", "running", 5*time.Minute, 0, 0)
	if err := h.store.MarkRunFailed(h.ctx, "restart-mark"); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute) // past the settle delay for the mark
	// Finished too recently: its last log lines may not be flushed yet.
	h.run("just-now", "failed", 20*time.Second, 10*time.Second, 1)
	// Older than the lookback: switching alerts on must not replay history.
	h.run("old", "failed", 3*time.Hour, time.Minute, 1)

	h.ev.evaluate(h.ctx)
	if len(h.note.events) != 0 {
		t.Fatalf("want no alert, got %+v", h.note.events)
	}

	h.now = h.now.Add(time.Minute)
	h.ev.evaluate(h.ctx)
	if len(h.note.events) != 1 || h.note.events[0].RunID != "just-now" {
		t.Errorf("after settling, want just-now alerted once, got %+v", h.note.events)
	}
}

func TestStuck(t *testing.T) {
	h := newHarness(t, "0 0 1 1 *", KindStuck)
	for i := range 5 {
		h.run("ok-"+string(rune('a'+i)), "succeeded", time.Duration(10-i)*time.Hour, 4*time.Minute, 0)
	}
	h.run("slow", "running", 13*time.Minute, 0, 0) // threshold is 12m

	h.ev.evaluate(h.ctx)
	h.ev.evaluate(h.ctx)
	if len(h.note.events) != 1 || h.note.events[0].Kind != KindStuck || h.note.events[0].RunID != "slow" {
		t.Fatalf("want one stuck alert for slow, got %+v", h.note.events)
	}
	if !strings.Contains(h.note.events[0].Summary, "usually 4m00s") {
		t.Errorf("summary = %q", h.note.events[0].Summary)
	}
}

func TestMissed_OncePerSilence(t *testing.T) {
	h := newHarness(t, "*/5 * * * *", KindMissed)
	h.run("last", "succeeded", 2*time.Hour, time.Minute, 0)

	h.ev.evaluate(h.ctx)
	h.ev.evaluate(h.ctx)
	if len(h.note.events) != 1 || h.note.events[0].Kind != KindMissed {
		t.Fatalf("want one missed alert, got %+v", h.note.events)
	}
	if h.note.events[0].URL != "https://kc.example/clusters/c1/cronjobs/default/backup/runs" {
		t.Errorf("url = %q", h.note.events[0].URL)
	}

	// A run happens, then the CronJob goes silent again: that is a new silence.
	h.run("next", "succeeded", 0, time.Minute, 0)
	h.now = h.now.Add(2 * time.Hour)
	h.ev.evaluate(h.ctx)
	if len(h.note.events) != 2 {
		t.Errorf("want a second alert for the second silence, got %d", len(h.note.events))
	}
}

func TestDeliver_RetriesThenGivesUp(t *testing.T) {
	h := newHarness(t, "0 0 1 1 *", KindFailed)
	h.run("bad", "failed", 10*time.Minute, time.Minute, 1)
	h.note.fail = true

	for range maxAttempts - 1 {
		h.ev.evaluate(h.ctx)
	}
	if sent, _ := h.store.AlertSent(h.ctx, string(KindFailed), "bad"); sent {
		t.Fatal("recorded before maxAttempts while every notifier failed")
	}

	// The notifier comes back before the last attempt: the alert goes out.
	h.note.fail = false
	h.ev.evaluate(h.ctx)
	if len(h.note.events) != 1 {
		t.Fatalf("want delivery on recovery, got %d", len(h.note.events))
	}

	// One that never succeeds is recorded after maxAttempts and dropped.
	h.run("bad2", "failed", 10*time.Minute, time.Minute, 1)
	h.note.fail = true
	for range maxAttempts {
		h.ev.evaluate(h.ctx)
	}
	if sent, _ := h.store.AlertSent(h.ctx, string(KindFailed), "bad2"); !sent {
		t.Error("not recorded after maxAttempts failed deliveries")
	}
}

func TestNewEvaluator_OffWithoutDestination(t *testing.T) {
	ev, err := NewEvaluator(nil, Config{On: []string{"failed"}})
	if err != nil || ev != nil {
		t.Errorf("no destination: got %v, %v; want nil, nil", ev, err)
	}
	if _, err := NewEvaluator(nil, Config{On: []string{"exploded"}, WebhookURL: "http://x"}); err == nil {
		t.Error("unknown ALERT_ON kind accepted")
	}
	if _, err := NewEvaluator(nil, Config{SMTPHost: "mail"}); err == nil {
		t.Error("SMTP without from/to accepted")
	}
}
