package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kubecron/kubecron/internal/storage"
)

const runsPageSize = 50

// ── JSON ─────────────────────────────────────────────────────────────────────

func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ctx := r.Context()

	cj, err := h.store.GetCronJobByName(ctx, clusterID, ns, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to look up cronjob")
		return
	}
	if cj == nil {
		writeError(w, http.StatusNotFound, "cronjob not found")
		return
	}
	runs, err := h.store.ListJobRuns(ctx, cj.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

type resourceSamplesResponse struct {
	RunID   string `json:"run_id"`
	Samples any    `json:"samples"`
}

func (h *Handler) GetResourceSamples(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	samples, err := h.store.GetResourceSamples(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get resource samples")
		return
	}
	writeJSON(w, http.StatusOK, resourceSamplesResponse{RunID: id, Samples: samples})
}

func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	if h.cacheSynced() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
}

// ── UI — Run list ─────────────────────────────────────────────────────────────

func (h *Handler) RunsList(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ctx := r.Context()
	day := r.URL.Query().Get("day")

	cj, err := h.store.GetCronJobByName(ctx, clusterID, ns, name)
	if err != nil || cj == nil {
		http.Error(w, "cronjob not found", http.StatusNotFound)
		return
	}

	var runs []storage.JobRun
	if day != "" {
		runs, err = h.store.ListJobRunsByDay(ctx, cj.ID, day)
	} else {
		runs, err = h.store.ListJobRunsPaged(ctx, cj.ID, time.Time{}, runsPageSize)
	}
	if err != nil {
		http.Error(w, "failed to load runs", http.StatusInternalServerError)
		return
	}

	dailyStats, _ := h.store.GetDailyRunStats(ctx, cj.ID, 90)
	allCJs, _ := h.store.ListCronJobs(ctx, clusterID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, htmlHeadSidebar(clusterID, buildNsSidebar(clusterID, allCJs, ns), h.nav(ctx, clusterID)))
	fmt.Fprint(w, `<div class="page-content">`)
	fmt.Fprint(w, breadcrumb(
		`<a href="/">clusters</a>`,
		`<a href="/clusters/`+esc(clusterID)+`">`+esc(clusterID)+`</a>`,
		`<a href="/clusters/`+esc(clusterID)+`/cronjobs/`+esc(ns)+`">`+esc(ns)+`</a>`,
		`<span>`+esc(name)+`</span>`,
	))
	fmt.Fprintf(w, `<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:1.25rem;">
	  <h1 style="font-family:var(--font-mono);color:var(--accent);font-size:1.25rem;margin:0;">%s</h1>
	  <button class="btn btn-primary"
	    hx-post="/api/clusters/%s/cronjobs/%s/%s/trigger"
	    hx-confirm="Trigger a manual run?"
	    hx-swap="none"
	    hx-on::after-request="if(event.detail.successful){var d=JSON.parse(event.detail.xhr.responseText);window.location='/clusters/%s/cronjobs/%s/%s/runs/'+d.run_id;}else{showToast('Trigger failed',false);}">
	    ▶ Run now
	  </button>
	</div>`,
		esc(cj.Schedule), esc(clusterID), esc(ns), esc(name), esc(clusterID), esc(ns), esc(name))

	// 90-day history heatmap card.
	fmt.Fprint(w, `<div class="card" style="margin-bottom:1rem;">`)
	fmt.Fprint(w, `<div class="section-label">90-day history</div>`)
	fmt.Fprint(w, heatmapHTML(dailyStats, 90))
	fmt.Fprint(w, `</div>`)

	// Day filter chip.
	if day != "" {
		clearURL := "/clusters/" + url.PathEscape(clusterID) + "/cronjobs/" + url.PathEscape(ns) + "/" + url.PathEscape(name) + "/runs"
		fmt.Fprintf(w, `<div style="margin-bottom:0.75rem;display:flex;align-items:center;gap:0.5rem;font-family:var(--font-mono);font-size:0.85rem;">
  <span style="background:rgba(66,153,225,0.15);border:1px solid rgba(66,153,225,0.4);color:#4299e1;padding:2px 10px;border-radius:12px;">%s</span>
  <a href="%s" style="color:var(--muted);text-decoration:none;" title="Clear filter">✕ clear</a>
</div>`, esc(day), esc(clearURL))
	}

	if len(runs) == 0 {
		fmt.Fprint(w, `<div class="card" style="text-align:center;padding:3rem;color:var(--muted);font-family:var(--font-mono);">No runs yet.</div>`)
	} else {
		fmt.Fprintf(w, `<div class="card" style="padding:0;overflow:hidden;"><table>
<thead><tr>
  <th>ID</th><th>Trigger</th><th>Status</th>
  <th>Started</th><th>Duration</th><th>Exit code</th><th>Node</th>
</tr></thead><tbody id="runs-tbody">`)
		for _, run := range runs {
			fmt.Fprint(w, renderRunRow(run, clusterID, ns, name))
		}
		fmt.Fprint(w, `</tbody></table></div>`)
		if day == "" && len(runs) == runsPageSize {
			cursor := runCursor(runs[len(runs)-1])
			fmt.Fprint(w, runLoadMoreWrap(clusterID, ns, name, cursor, ""))
		} else {
			fmt.Fprint(w, `<div id="load-more-wrap"></div>`)
		}
	}
	fmt.Fprint(w, `</div>`) // page-content
	fmt.Fprint(w, htmlFootSidebar)
}

// RunsListMore handles HTMX "Load more" requests for the run list.
// It returns only <tr> rows (appended to #runs-tbody) + OOB update for the load-more button.
func (h *Handler) RunsListMore(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ctx := r.Context()
	before := parseRunCursor(r.URL.Query().Get("before"))
	day := r.URL.Query().Get("day")

	cj, err := h.store.GetCronJobByName(ctx, clusterID, ns, name)
	if err != nil || cj == nil {
		http.Error(w, "cronjob not found", http.StatusNotFound)
		return
	}

	var runs []storage.JobRun
	if day != "" {
		runs, err = h.store.ListJobRunsByDay(ctx, cj.ID, day)
	} else {
		runs, err = h.store.ListJobRunsPaged(ctx, cj.ID, before, runsPageSize)
	}
	if err != nil {
		http.Error(w, "failed to load runs", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, run := range runs {
		fmt.Fprint(w, renderRunRow(run, clusterID, ns, name))
	}
	// OOB: update or clear the load-more button.
	if day == "" && len(runs) == runsPageSize {
		cursor := runCursor(runs[len(runs)-1])
		fmt.Fprint(w, runLoadMoreOOB(clusterID, ns, name, cursor, ""))
	} else {
		fmt.Fprint(w, runLoadMoreOOB(clusterID, ns, name, "", ""))
	}
}

// ── UI — Run detail ───────────────────────────────────────────────────────────

func (h *Handler) RunDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	run, err := h.store.GetJobRun(ctx, id)
	if err != nil {
		http.Error(w, "failed to load run", http.StatusInternalServerError)
		return
	}
	if run == nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	shortID := id
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	isRunning := run.Status == "running"

	clusterID := r.PathValue("clusterID")
	ns := r.PathValue("ns")
	name := r.PathValue("name")

	allCJs, _ := h.store.ListCronJobs(ctx, clusterID)
	runsURL := "/clusters/" + esc(clusterID) + "/cronjobs/" + esc(ns) + "/" + esc(name) + "/runs"

	fmt.Fprint(w, htmlHeadSidebar(clusterID, buildNsSidebar(clusterID, allCJs, ns), h.nav(ctx, clusterID)))
	fmt.Fprint(w, `<div class="page-content">`)
	fmt.Fprint(w, breadcrumb(
		`<a href="/">clusters</a>`,
		`<a href="/clusters/`+esc(clusterID)+`">`+esc(clusterID)+`</a>`,
		`<a href="/clusters/`+esc(clusterID)+`/cronjobs/`+esc(ns)+`">`+esc(ns)+`</a>`,
		`<a href="`+runsURL+`">`+esc(name)+`</a>`,
		`<span>`+esc(shortID)+`…</span>`,
	))

	// ── Metadata card ─────────────────────────────────────────────────────────
	duration := "running…"
	if run.DurationMs != nil {
		duration = fmtDuration(*run.DurationMs)
	}
	logSize := fmt.Sprintf("%.1f KB", float64(run.LogSizeBytes)/1024)

	exitDisplay := ""
	if run.ExitCode != nil {
		col := "var(--green)"
		if *run.ExitCode != 0 {
			col = "var(--red)"
		}
		exitDisplay = fmt.Sprintf(`<span class="meta-kv">exit <strong id="run-exit" style="color:%s;">%d</strong></span>`, col, *run.ExitCode)
	} else if isRunning {
		exitDisplay = `<span class="meta-kv" id="run-exit-wrap" style="display:none">exit <strong id="run-exit"></strong></span>`
	}

	// CPU / RAM — shown for running (will be populated by SSE) and for completed runs with data.
	resDisplay := ""
	if isRunning {
		resDisplay = `<span class="meta-kv" id="run-res-wrap" style="display:none">` +
			`cpu avg <strong id="run-avg-cpu">—</strong> peak <strong id="run-max-cpu">—</strong>` +
			` &nbsp;·&nbsp; ` +
			`mem avg <strong id="run-avg-mem">—</strong> peak <strong id="run-max-mem">—</strong>` +
			`</span>`
	} else if run.AvgCPUMillicores != nil {
		avgCPU := fmt.Sprintf("%dm", *run.AvgCPUMillicores)
		maxCPU := avgCPU
		if run.MaxCPUMillicores != nil {
			maxCPU = fmt.Sprintf("%dm", *run.MaxCPUMillicores)
		}
		avgMem, maxMem := "—", "—"
		if run.AvgMemoryBytes != nil {
			avgMem = fmt.Sprintf("%d MiB", *run.AvgMemoryBytes/1048576)
		}
		if run.MaxMemoryBytes != nil {
			maxMem = fmt.Sprintf("%d MiB", *run.MaxMemoryBytes/1048576)
		}
		resDisplay = fmt.Sprintf(
			`<span class="meta-kv" id="run-res-wrap">`+
				`cpu avg <strong id="run-avg-cpu">%s</strong> peak <strong id="run-max-cpu">%s</strong>`+
				` &nbsp;·&nbsp; `+
				`mem avg <strong id="run-avg-mem">%s</strong> peak <strong id="run-max-mem">%s</strong>`+
				`</span>`,
			esc(avgCPU), esc(maxCPU), esc(avgMem), esc(maxMem),
		)
	}

	fmt.Fprintf(w, `<div class="card" style="margin-bottom:1rem;">
  <div style="display:flex;flex-wrap:wrap;gap:1rem 1.5rem;align-items:center;">
    <span id="run-status">%s</span>
    <span class="meta-kv">trigger <strong style="color:var(--accent);">%s</strong></span>
    <span class="meta-kv">duration <strong id="run-duration" style="color:var(--accent);">%s</strong></span>
    %s
    %s`,
		statusBadge(run.Status),
		esc(run.Trigger),
		esc(duration),
		exitDisplay,
		resDisplay,
	)

	if run.NodeName != nil {
		fmt.Fprintf(w, `<span class="meta-kv muted">node %s</span>`, esc(*run.NodeName))
	}
	if run.ContainerImage != nil {
		fmt.Fprintf(w, `<span class="meta-kv muted">image %s</span>`, esc(*run.ContainerImage))
	}
	fmt.Fprintf(w, `<span id="run-logsize" class="meta-kv muted">%s</span>`, esc(logSize))
	fmt.Fprint(w, `</div></div>`)

	// ── Resource charts (completed runs with samples) ──────────────────────────
	if !isRunning && run.AvgCPUMillicores != nil {
		fmt.Fprintf(w, `<div class="card" style="margin-bottom:1rem;">
  <div class="section-label">Resource usage</div>
  <div style="display:grid;grid-template-columns:1fr 1fr;gap:1rem;">
    <div class="chart-container"><canvas id="cpuChart"></canvas></div>
    <div class="chart-container"><canvas id="memChart"></canvas></div>
  </div>
</div>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.9"></script>
<script>
(function(){
  var chartOpts = function(label, data, labels, color) {
    return { type:'line', data:{ labels:labels, datasets:[{ label:label, data:data,
      borderColor:color, backgroundColor:color+'22', fill:true, tension:0.3, pointRadius:2 }] },
      options:{ responsive:true, maintainAspectRatio:false,
        scales:{ y:{ beginAtZero:true, grid:{color:'#1e1e2e'}, ticks:{color:'#718096'} },
                 x:{ grid:{color:'#1e1e2e'}, ticks:{color:'#718096', maxTicksLimit:8} } },
        plugins:{ legend:{ labels:{color:'#e2e8f0'} } } } };
  };
  fetch('/api/runs/%s/resources').then(function(r){return r.json();}).then(function(d){
    if (!d.samples || !d.samples.length) return;
    var labels = d.samples.map(function(s){return new Date(s.sampled_at).toLocaleTimeString();});
    var cpu = d.samples.map(function(s){return s.cpu_millicores;});
    var mem = d.samples.map(function(s){return Math.round(s.memory_bytes/1048576);});
    new Chart(document.getElementById('cpuChart'), chartOpts('CPU (m)', cpu, labels, '#6c8ef7'));
    new Chart(document.getElementById('memChart'), chartOpts('Memory (MiB)', mem, labels, '#48bb78'));
  });
})();
</script>`, esc(id))
	}

	// ── Log terminal ──────────────────────────────────────────────────────────
	fmt.Fprint(w, `<div class="card"><div class="section-label">Logs</div>`)
	fmt.Fprint(w, logSearchBar(id))

	if isRunning {
		// Empty terminal — the EventSource below fills it (historical + live).
		fmt.Fprint(w, `<div id="log-term" class="log-terminal"></div>`)
		fmt.Fprintf(w, `<script>
(function(){
  var term = document.getElementById('log-term');
  var es = new EventSource('/api/runs/%s/stream');

  es.onmessage = function(e) {
    if (!e.data) return;
    term.insertAdjacentHTML('beforeend', e.data);
  };

  es.addEventListener('status', function(e) {
    var d = JSON.parse(e.data);
    var badge = {running:'badge-running',succeeded:'badge-success',failed:'badge-failed'};
    var cls = badge[d.status] || 'badge-muted';
    var st = document.getElementById('run-status');
    if (st) st.innerHTML = '<span class="badge '+cls+'">'+d.status+'</span>';
    var dur = document.getElementById('run-duration');
    if (dur) dur.textContent = d.duration || 'running…';
    if (d.exit_code !== null && d.exit_code !== undefined) {
      var ew = document.getElementById('run-exit-wrap');
      if (ew) ew.style.display = '';
      var ex = document.getElementById('run-exit');
      if (ex) { ex.textContent = d.exit_code; ex.style.color = d.exit_code===0?'var(--green)':'var(--red)'; }
    }
    if (d.avg_cpu_m != null) {
      var rw = document.getElementById('run-res-wrap');
      if (rw) rw.style.display = '';
      var el = document.getElementById('run-avg-cpu'); if(el) el.textContent = d.avg_cpu_m+'m';
      el = document.getElementById('run-max-cpu'); if(el && d.max_cpu_m) el.textContent = d.max_cpu_m+'m';
      el = document.getElementById('run-avg-mem'); if(el && d.avg_mem_mb!=null) el.textContent = d.avg_mem_mb+' MiB';
      el = document.getElementById('run-max-mem'); if(el && d.max_mem_mb!=null) el.textContent = d.max_mem_mb+' MiB';
    }
    var ls = document.getElementById('run-logsize');
    if (ls && d.log_size_kb) ls.textContent = d.log_size_kb;
  });

  es.addEventListener('done', function() { es.close(); });
  es.onerror = function() { es.close(); };
})();
</script>`, esc(id))
	} else {
		const logLimit = 5000
		lines, _ := h.store.GetLogLinesTail(ctx, id, logLimit)
		if len(lines) == logLimit {
			fmt.Fprintf(w, `<div style="font-family:var(--font-mono);font-size:0.75rem;color:var(--yellow);margin-bottom:6px;">⚠ showing last %d lines — <a href="/api/runs/%s/logs.txt" style="color:var(--accent);">download full log</a></div>`, logLimit, esc(id))
		}
		fmt.Fprint(w, `<div id="log-term" class="log-terminal">`)
		for _, l := range lines {
			fmt.Fprintf(w, `<div class="ll" data-raw="%s">%s</div>`, esc(l.Line), esc(l.Line))
		}
		fmt.Fprint(w, `</div>`)
	}

	fmt.Fprint(w, logSearchJS)
	fmt.Fprint(w, `</div></div>`) // page-content
	fmt.Fprint(w, htmlFootSidebar)
}

// DownloadLogs handles GET /api/runs/{id}/logs.txt and returns raw log lines
// as a plain-text file download.
func (h *Handler) DownloadLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	// An unknown run answers 404, not an empty file: an empty body is what a
	// run that printed nothing returns, and the two must not look alike.
	run, err := h.store.GetJobRun(ctx, id)
	if err != nil {
		slog.Error("failed to look up run for log download", "run_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to look up run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	lines, err := h.store.GetLogLines(ctx, id)
	if err != nil {
		slog.Error("failed to load log lines for download", "run_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to load logs")
		return
	}
	fname := id
	if len(fname) > 8 {
		fname = fname[:8]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="run-%s.log"`, fname))
	for _, l := range lines {
		fmt.Fprintln(w, l.Line)
	}
}

// ── Shared helpers ────────────────────────────────────────────────────────────

// runCursor renders a run's start time as the opaque `before` cursor of the
// next page.
//
// Full nanosecond precision, not RFC3339's whole seconds: the cursor selects
// runs strictly older than it, so truncating to a second silently skips every
// other run that started in the same second as the last row of the page — which
// for a per-minute CronJob with parallel pods is a real loss, not a theoretical
// one.
func runCursor(last storage.JobRun) string {
	return last.StartedAt.Format(time.RFC3339Nano)
}

// parseRunCursor turns a `before` query parameter back into a time.
//
// The cursor keeps the offset of the run it came from rather than being
// converted to UTC, and that is load-bearing: the comparison it feeds is a
// textual one against a column that stores the zone alongside the instant, so a
// cursor in a different zone from the rows would compare wrong on a tie. Parsed
// back here, a "+02:00" offset that matches the process's own zone is resolved
// to that zone, which is where the rows were written.
//
// An absent or unparseable cursor yields the zero time, which means "start at
// the newest run": the value is opaque to the caller, so re-serving the first
// page is more useful than an error they could not act on.
func parseRunCursor(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	// A "+" in a query string decodes to a space, so a client that pasted the
	// cursor in without escaping it arrives here with "…00.5 02:00". Undoing
	// that is cheap and saves a cross-repo consumer a silent reset to page one
	// on every cursor east of Greenwich.
	if t, err := time.Parse(time.RFC3339Nano, strings.Replace(raw, " ", "+", 1)); err == nil {
		return t
	}
	return time.Time{}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
