package watcher

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubecron/kubecron/internal/metrics"
	"github.com/kubecron/kubecron/internal/sampler"
	"github.com/kubecron/kubecron/internal/storage"
	"github.com/kubecron/kubecron/internal/streamer"
)

// PodHandler receives Pod informer events, triggers log streaming, metrics
// sampling, and status updates.
type PodHandler struct {
	clusterID      string
	store          storage.Store
	streamer       *streamer.Streamer
	sampler        *sampler.Sampler
	clientset      kubernetes.Interface
	metricsEnabled func() bool // read live so the 5-min probe re-enable takes effect
	runIndex       *RunIndex
}

// NewPodHandler creates a PodHandler.
func NewPodHandler(
	clusterID string,
	store storage.Store,
	str *streamer.Streamer,
	smp *sampler.Sampler,
	clientset kubernetes.Interface,
	metricsEnabled func() bool,
	idx *RunIndex,
) *PodHandler {
	return &PodHandler{
		clusterID:      clusterID,
		store:          store,
		streamer:       str,
		sampler:        smp,
		clientset:      clientset,
		metricsEnabled: metricsEnabled,
		runIndex:       idx,
	}
}

// OnAdd handles pods already in Running or terminal phase when the informer
// starts, or when a fast-completing pod is first seen in a terminal state.
func (h *PodHandler) OnAdd(obj interface{}, isInInitialList bool) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	phase := pod.Status.Phase
	if phase != corev1.PodRunning && phase != corev1.PodSucceeded && phase != corev1.PodFailed {
		return
	}
	if isInInitialList && phase == corev1.PodRunning {
		// At startup the Job informer may not have created the run record yet.
		// Retry in a background goroutine so no logs or samples are missed.
		go h.retryOnAdd(pod)
		return
	}
	// Delegate to OnUpdate so the full status/log/sampler logic runs.
	h.OnUpdate(nil, obj)
}

// retryOnAdd waits for the Job handler to create the run record, then calls
// OnUpdate. Gives up after 5 s (10 × 500 ms) — by then the run either exists
// or the informer will fire OnUpdate when the pod transitions to a new phase.
func (h *PodHandler) retryOnAdd(pod *corev1.Pod) {
	jobName, found := jobOwner(pod)
	if !found {
		return
	}
	ctx := context.Background()
	for range 10 {
		if h.findRunID(ctx, jobName) != "" {
			h.OnUpdate(nil, pod)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// OnUpdate handles pod phase transitions.
func (h *PodHandler) OnUpdate(oldObj, newObj interface{}) {
	pod, ok := newObj.(*corev1.Pod)
	if !ok {
		return
	}

	// Only care about pods owned by a Job.
	jobName, found := jobOwner(pod)
	if !found {
		return
	}

	namespace := pod.Namespace
	podName := pod.Name
	ctx := context.Background()

	// Resolve the runID by scanning running runs and matching pod_name = jobName.
	// The job handler stores the job name as the placeholder pod_name when creating
	// the run record, so this lookup is stable.
	runID := h.findRunID(ctx, jobName)
	if runID == "" {
		// Run not tracked yet — ignore.
		return
	}

	// Record why the pod is blocked, on change only: pods are updated far more
	// often than their waiting reason changes.
	if reason := waitingReason(pod); oldObj == nil || reason != waitingReason(asPod(oldObj)) {
		if err := h.store.UpdateJobRunWaiting(ctx, runID, reason, time.Now()); err != nil {
			slog.Warn("failed to update pod waiting reason", "runID", runID, "err", err)
		}
	}

	phase := pod.Status.Phase

	switch phase {
	case corev1.PodRunning:
		nodeName := pod.Spec.NodeName

		// Determine the container image from the first container status, if available.
		containerImage := ""
		if len(pod.Status.ContainerStatuses) > 0 {
			containerImage = pod.Status.ContainerStatuses[0].Image
		}

		// UpdateJobRunNode accepts plain strings (empty string is fine for unknown image).
		if err := h.store.UpdateJobRunNode(ctx, runID, nodeName, containerImage); err != nil {
			slog.Warn("failed to update job run node", "runID", runID, "err", err)
		}

		// Only stream if no logs captured yet (prevents duplicates on pod restarts).
		if run, err := h.store.GetJobRun(ctx, runID); err == nil && run != nil && run.LogSizeBytes == 0 {
			h.streamer.Stream(ctx, h.clientset, namespace, podName, runID)
		}

		// Start resource sampling if metrics are enabled for this cluster.
		if h.sampler != nil && h.metricsEnabled() {
			h.sampler.Start(ctx, h.clusterID, namespace, podName, runID)
		}

	case corev1.PodSucceeded, corev1.PodFailed:
		status := "succeeded"
		if phase == corev1.PodFailed {
			status = "failed"
		}

		finishedAt := time.Now()
		exitCode := 0
		retryCount := 0

		// Extract exit code and restart count from the first container status.
		if len(pod.Status.ContainerStatuses) > 0 {
			cs := pod.Status.ContainerStatuses[0]
			retryCount = int(cs.RestartCount)
			if cs.State.Terminated != nil {
				exitCode = int(cs.State.Terminated.ExitCode)
				if !cs.State.Terminated.FinishedAt.IsZero() {
					finishedAt = cs.State.Terminated.FinishedAt.Time
				}
			}
		}

		if err := h.store.UpdateJobRunStatus(ctx, runID, status, &finishedAt, exitCode, retryCount); err != nil {
			slog.Warn("failed to update job run status", "runID", runID, "status", status, "err", err)
		}

		// Record Prometheus metrics for this run completion.
		if run, err := h.store.GetJobRun(ctx, runID); err == nil && run != nil {
			metrics.RecordCompletion(h.clusterID, run.CronJobID, status, run.Trigger, run.StartedAt, finishedAt)
		}

		// Stream logs for fast-completing pods whose PodRunning event was missed.
		// Only fetch if no logs have been captured yet to avoid duplicates.
		if run, err := h.store.GetJobRun(ctx, runID); err == nil && run != nil && run.LogSizeBytes == 0 {
			h.streamer.Stream(ctx, h.clientset, namespace, podName, runID)
		}

		// Stop resource sampling; finalization is handled inside the sampler.
		if h.sampler != nil {
			h.sampler.Stop(runID)
		}

		// Remove the completed job from the index so we don't hold stale entries.
		h.runIndex.Delete(jobName)
	}
}

// findRunID returns the run ID for the given Kubernetes Job name, checking the
// in-memory RunIndex first (O(1)) before falling back to a DB scan (for runs
// that were already running when kubecron started).
func (h *PodHandler) findRunID(ctx context.Context, jobName string) string {
	if id := h.runIndex.Get(jobName); id != "" {
		return id
	}
	runs, err := h.store.GetRunningRuns(ctx)
	if err != nil {
		slog.Warn("failed to fetch running runs", "err", err)
		return ""
	}
	for _, r := range runs {
		if r.PodName == jobName {
			return r.ID
		}
	}
	return ""
}

// OnDelete is a no-op.
func (h *PodHandler) OnDelete(obj interface{}) {}

// waitingReason returns why pod is blocked: the waiting reason of its first
// waiting container (init containers first, since they run first), or
// "Unschedulable" when the scheduler cannot place it. "" when nothing waits,
// including once the pod has finished.
func waitingReason(pod *corev1.Pod) string {
	if pod == nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return ""
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range statuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				return cs.State.Waiting.Reason
			}
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return corev1.PodReasonUnschedulable
		}
	}
	return ""
}

func asPod(obj interface{}) *corev1.Pod {
	pod, _ := obj.(*corev1.Pod)
	return pod
}

// jobOwner returns the name of the Job that owns the given pod, if any.
func jobOwner(pod *corev1.Pod) (string, bool) {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" {
			return ref.Name, true
		}
	}
	return "", false
}
