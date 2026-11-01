package watcher

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubecron/kubecron/internal/storage"
)

func pendingPod(waiting string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "my-cron-abc123-xyz",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "my-cron-abc123"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if waiting != "" {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}},
		}}
	}
	return pod
}

// The waiting reason drives stuck detection, and its timestamp must mark when
// the pod started waiting, not its latest update — or a pod crash-looping for an
// hour would never pass the grace period.
func TestPodHandler_RecordsWaitingReason(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpsertJobRun(ctx, storage.JobRun{
		ID: "run-1", CronJobID: "c1/default/my-cron", PodName: "my-cron-abc123",
		Trigger: "scheduled", Status: "running", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	idx := NewRunIndex()
	idx.Set("my-cron-abc123", "run-1")
	h := NewPodHandler("c1", store, nil, nil, nil, func() bool { return false }, idx)

	get := func() *storage.JobRun {
		t.Helper()
		r, err := store.GetJobRun(ctx, "run-1")
		if err != nil || r == nil {
			t.Fatalf("GetJobRun: %v", err)
		}
		return r
	}

	h.OnUpdate(nil, pendingPod("ImagePullBackOff"))
	first := get()
	if first.PodWaitingReason == nil || *first.PodWaitingReason != "ImagePullBackOff" || first.PodWaitingSince == nil {
		t.Fatalf("waiting = %v since %v, want ImagePullBackOff with a timestamp", first.PodWaitingReason, first.PodWaitingSince)
	}

	time.Sleep(10 * time.Millisecond)
	// An unchanged reason does not touch the row.
	h.OnUpdate(pendingPod("ImagePullBackOff"), pendingPod("ImagePullBackOff"))
	if same := get(); !same.PodWaitingSince.Equal(*first.PodWaitingSince) {
		t.Errorf("unchanged reason moved since: %v → %v", first.PodWaitingSince, same.PodWaitingSince)
	}

	// A different reason restarts the clock: the grace period is per reason.
	h.OnUpdate(pendingPod("ImagePullBackOff"), pendingPod("ErrImagePull"))
	second := get()
	if second.PodWaitingSince == nil || !second.PodWaitingSince.After(*first.PodWaitingSince) {
		t.Errorf("reason changed but since did not move: %v → %v", first.PodWaitingSince, second.PodWaitingSince)
	}

	// Same reason seen again with no previous object (e.g. after a restart):
	// the stored timestamp is kept.
	h.OnUpdate(nil, pendingPod("ErrImagePull"))
	if third := get(); !third.PodWaitingSince.Equal(*second.PodWaitingSince) {
		t.Errorf("same reason moved since: %v → %v", second.PodWaitingSince, third.PodWaitingSince)
	}

	h.OnUpdate(pendingPod("ErrImagePull"), pendingPod(""))
	if cleared := get(); cleared.PodWaitingReason != nil || cleared.PodWaitingSince != nil {
		t.Errorf("waiting not cleared: %v since %v", cleared.PodWaitingReason, cleared.PodWaitingSince)
	}
}

func TestWaitingReason(t *testing.T) {
	unschedulable := pendingPod("")
	unschedulable.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
	}}
	finished := pendingPod("CrashLoopBackOff")
	finished.Status.Phase = corev1.PodFailed

	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"nil", nil, ""},
		{"container waiting", pendingPod("CrashLoopBackOff"), "CrashLoopBackOff"},
		{"unschedulable", unschedulable, "Unschedulable"},
		{"finished pod waits for nothing", finished, ""},
		{"nothing waiting", pendingPod(""), ""},
	}
	for _, tc := range tests {
		if got := waitingReason(tc.pod); got != tc.want {
			t.Errorf("%s: waitingReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}
