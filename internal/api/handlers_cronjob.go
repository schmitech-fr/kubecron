package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/kubecron/kubecron/internal/schedule"
	"github.com/kubecron/kubecron/internal/storage"
)

// resourcesResponse holds the resource request/limit strings for a CronJob.
type resourcesResponse struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

// cronJobResponse is the enriched JSON shape for GET /api/clusters/{clusterID}/cronjobs.
type cronJobResponse struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Schedule  string `json:"schedule"`
	// TimeZone is the CronJob's spec.timeZone, omitted when it declares none.
	// next_run_at is computed in this zone.
	TimeZone  string            `json:"time_zone,omitempty"`
	Suspended bool              `json:"suspended"`
	NextRunAt *time.Time        `json:"next_run_at,omitempty"`
	Resources resourcesResponse `json:"resources"`
	LastRun   interface{}       `json:"last_run,omitempty"`
	Stats7d   interface{}       `json:"stats_7d,omitempty"`
}

// ListCronJobs handles GET /api/clusters/{clusterID}/cronjobs.
func (h *Handler) ListCronJobs(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	ctx := r.Context()

	cronjobs, err := h.store.ListCronJobs(ctx, clusterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list cronjobs")
		return
	}
	// One aggregate read for the whole cluster instead of two queries per
	// CronJob (PERF-2).
	summaries, err := h.store.GetCronJobSummaries(ctx, clusterID, sparklineRuns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list cronjobs")
		return
	}

	resp := make([]cronJobResponse, 0, len(cronjobs))
	for _, cj := range cronjobs {
		item := cronJobResponse{
			ID:        cj.ID,
			Namespace: cj.Namespace,
			Name:      cj.Name,
			Schedule:  cj.Schedule,
			TimeZone:  cj.TZ(),
			Suspended: cj.Suspended,
			Resources: resourcesResponse{
				CPURequest:    derefStr(cj.CPURequest),
				CPULimit:      derefStr(cj.CPULimit),
				MemoryRequest: derefStr(cj.MemoryRequest),
				MemoryLimit:   derefStr(cj.MemoryLimit),
			},
		}

		// Next run is resolved in the CronJob's own zone (DOM-1). It is omitted
		// when the schedule or zone cannot be resolved.
		if cj.Schedule != "" {
			if next, err := schedule.NextRun(cj.Schedule, cj.TZ(), time.Now()); err == nil {
				item.NextRunAt = &next
			}
		}

		if sum := summaries[cj.ID]; sum != nil {
			item.LastRun = sum.LastRun
			item.Stats7d = sum.Stats7d
		}

		resp = append(resp, item)
	}

	writeJSON(w, http.StatusOK, resp)
}

// Suspend handles POST /api/clusters/{clusterID}/cronjobs/{ns}/{name}/suspend.
// It patches the CronJob in Kubernetes to set spec.suspend=true.
func (h *Handler) Suspend(w http.ResponseWriter, r *http.Request) {
	h.patchSuspend(w, r, true)
}

// Resume handles POST /api/clusters/{clusterID}/cronjobs/{ns}/{name}/resume.
// It patches the CronJob in Kubernetes to set spec.suspend=false.
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	h.patchSuspend(w, r, false)
}

// patchSuspend performs the Kubernetes MergePatch to flip spec.suspend and
// writes the HTTP response. Raw K8s errors are logged, never returned to the
// client.
func (h *Handler) patchSuspend(w http.ResponseWriter, r *http.Request, suspend bool) {
	clusterID := r.PathValue("clusterID")
	ns := r.PathValue("ns")
	name := r.PathValue("name")

	cl, ok := h.registry.Get(clusterID)
	if !ok {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}

	suspendValue := "true"
	if !suspend {
		suspendValue = "false"
	}
	patch := []byte(`{"spec":{"suspend":` + suspendValue + `}}`)

	if _, err := cl.Clientset.BatchV1().CronJobs(ns).Patch(
		r.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		slog.Error("failed to patch cronjob suspend",
			"cluster", clusterID, "namespace", ns, "name", name, "suspend", suspend, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to update cronjob")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// triggerResponse is the JSON body returned by the trigger endpoint.
type triggerResponse struct {
	RunID   string `json:"run_id"`
	PodName string `json:"pod_name"`
}

// Trigger handles POST /api/clusters/{clusterID}/cronjobs/{ns}/{name}/trigger.
// It creates a one-off Job derived from the CronJob's JobTemplate, owned by
// the CronJob the way `kubectl create job --from=cronjob/…` owns it: without
// the owner reference the JobHandler does not recognise the Job as a run of
// this CronJob, history limits never collect it and deleting the CronJob
// leaves it behind. BlockOwnerDeletion is left unset so the ServiceAccount
// needs no access to cronjobs/finalizers.
func (h *Handler) Trigger(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ctx := r.Context()

	cl, ok := h.registry.Get(clusterID)
	if !ok {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}

	cronJob, err := cl.Clientset.BatchV1().CronJobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		slog.Error("failed to get cronjob for trigger",
			"cluster", clusterID, "namespace", ns, "name", name, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to get cronjob")
		return
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-manual-",
			Namespace:    ns,
			Labels: map[string]string{
				"kubecron/trigger": "manual",
			},
			Annotations: map[string]string{
				"cronjob.kubernetes.io/instantiate": "manual",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       cronJob.Name,
				UID:        cronJob.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: cronJob.Spec.JobTemplate.Spec,
	}

	created, err := cl.Clientset.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		slog.Error("failed to create triggered job",
			"cluster", clusterID, "namespace", ns, "name", name, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create job")
		return
	}

	runID := string(created.UID)
	cronJobID := fmt.Sprintf("%s/%s/%s", clusterID, ns, name)
	run := storage.JobRun{
		ID:        runID,
		CronJobID: cronJobID,
		PodName:   created.Name,
		Trigger:   "manual",
		Status:    "running",
		StartedAt: time.Now(),
	}
	if err := h.store.UpsertJobRun(ctx, run); err != nil {
		slog.Error("failed to pre-insert triggered run", "run_id", runID, "err", err)
	}

	writeJSON(w, http.StatusCreated, triggerResponse{
		RunID:   runID,
		PodName: created.Name,
	})
}
