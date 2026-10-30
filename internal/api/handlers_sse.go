package api

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kubecron/kubecron/internal/storage"
)

// runStatusPayload is the JSON shape sent as SSE "status" events.
type runStatusPayload struct {
	Status    string `json:"status"`
	Duration  string `json:"duration"`
	ExitCode  *int   `json:"exit_code"`
	LogSizeKB string `json:"log_size_kb"`
	AvgCPUm   *int64 `json:"avg_cpu_m,omitempty"`
	MaxCPUm   *int64 `json:"max_cpu_m,omitempty"`
	AvgMemMB  *int64 `json:"avg_mem_mb,omitempty"`
	MaxMemMB  *int64 `json:"max_mem_mb,omitempty"`
}

func makeStatusPayload(run *storage.JobRun) runStatusPayload {
	dur := "running…"
	if run.DurationMs != nil {
		dur = fmtDuration(*run.DurationMs)
	}
	p := runStatusPayload{
		Status:    run.Status,
		Duration:  dur,
		ExitCode:  run.ExitCode,
		LogSizeKB: fmt.Sprintf("%.1f KB", float64(run.LogSizeBytes)/1024),
	}
	if run.AvgCPUMillicores != nil {
		p.AvgCPUm = run.AvgCPUMillicores
	}
	if run.MaxCPUMillicores != nil {
		p.MaxCPUm = run.MaxCPUMillicores
	}
	if run.AvgMemoryBytes != nil {
		mb := *run.AvgMemoryBytes / 1048576
		p.AvgMemMB = &mb
	}
	if run.MaxMemoryBytes != nil {
		mb := *run.MaxMemoryBytes / 1048576
		p.MaxMemMB = &mb
	}
	return p
}

func writeStatusEvent(w http.ResponseWriter, f http.Flusher, run *storage.JobRun) {
	b, _ := json.Marshal(makeStatusPayload(run))
	fmt.Fprintf(w, "event: status\ndata: %s\n\n", b)
	f.Flush()
}

// StreamLogs handles GET /api/runs/{id}/stream.
//
// For running runs it:
//  1. Subscribes to the broadcaster FIRST (so no lines are missed).
//  2. Replays all lines already in the DB (no duplicates because publish
//     always happens after the DB insert in the same goroutine).
//  3. Forwards live lines from the broadcaster.
//  4. Sends periodic "status" events every 3 s.
//
// For finished runs it replays the stored lines then closes.
func (h *Handler) StreamLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	run, err := h.store.GetJobRun(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to look up run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Send current status immediately so the client can initialise its UI.
	writeStatusEvent(w, flusher, run)

	const sseLogLimit = 5000
	if run.Status != "running" {
		lines, _ := h.store.GetLogLinesTail(ctx, id, sseLogLimit)
		for _, l := range lines {
			writeLogLine(w, l.Line)
		}
		fmt.Fprintf(w, "event: done\ndata: {}\n\n")
		flusher.Flush()
		return
	}

	// Subscribe BEFORE reading history so no lines can slip through the gap.
	ch, unsub := h.broadcaster.Subscribe(id)
	defer unsub()

	// Replay the most recent lines already persisted to DB.
	hist, _ := h.store.GetLogLinesTail(ctx, id, sseLogLimit)
	for _, l := range hist {
		writeLogLine(w, l.Line)
	}
	flusher.Flush()

	// Tick every second for live status updates and to detect run completion.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	// broadcastDone is set when the log stream closes. The pod watcher
	// (UpdateJobRunStatus) and the log streamer are independent goroutines,
	// so the DB may not reflect the final status yet when the broadcaster
	// closes. We keep ticking for up to 5 s to catch the status update.
	var broadcastDone bool
	var broadcastAt time.Time

	for {
		select {
		case line, ok := <-ch:
			if !ok {
				ch = nil // nil channel blocks forever — remove from select
				broadcastDone = true
				broadcastAt = time.Now()
			} else {
				writeLogLine(w, line)
				flusher.Flush()
			}

		case <-ticker.C:
			r2, err := h.store.GetJobRun(ctx, id)
			if err != nil || r2 == nil {
				continue
			}
			writeStatusEvent(w, flusher, r2)
			finished := r2.Status != "running" ||
				(broadcastDone && time.Since(broadcastAt) > 5*time.Second)
			if finished {
				fmt.Fprintf(w, "event: done\ndata: {}\n\n")
				flusher.Flush()
				return
			}

		case <-ctx.Done():
			return
		}
	}
}

// writeLogLine sends one log line as an SSE data event.
//
// A carriage return is an SSE line terminator, and log output keeps embedded
// ones (progress bars rewrite a line with "\r"). Left raw, everything after it
// was parsed as a new SSE field: the visible line was cut short, and a pod
// printing "\revent: done\r" could end or forge events in the viewer. It is
// sent as a character reference instead, which decodes back to "\r" in both
// the text and the data-raw attribute.
func writeLogLine(w io.Writer, line string) {
	esc := strings.ReplaceAll(html.EscapeString(line), "\r", "&#13;")
	fmt.Fprintf(w, "data: <div class=\"ll\" data-raw=\"%s\">%s</div>\n\n", esc, esc)
}
