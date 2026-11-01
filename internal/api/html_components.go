package api

import (
	"fmt"
	"net/url"
	"time"

	"github.com/kubecron/kubecron/internal/runcheck"
	"github.com/kubecron/kubecron/internal/storage"
)

// cronJobRowData bundles the pre-computed display data for a single CronJob
// table row. Used by both ClusterDetail and NamespaceDetail to avoid
// duplicating the rendering logic.
type cronJobRowData struct {
	ClusterID string
	CronJob   storage.CronJob
	NextRun   time.Time
	LastRun   *storage.JobRun
	Stats7d   *storage.RunStats
	Durations []int64
	IsMissed  bool
	// Stuck is the latest run's runcheck verdict.
	Stuck runcheck.Verdict
	// ScheduleError is set when the cron expression or its time zone could not
	// be resolved, so the row shows no countdown instead of a wrong one.
	ScheduleError bool
	IsConcurrent  bool
}

// renderCronJobRow returns the HTML <tr>…</tr> for a CronJob in the table.
func renderCronJobRow(d cronJobRowData) string {
	lastStatus := `<span style="color:var(--muted);">—</span>`
	if d.LastRun != nil {
		lastStatus = statusBadge(d.LastRun.Status)
	}
	if d.IsConcurrent {
		lastStatus += ` <span class="badge" style="border-color:rgba(236,201,75,0.4);color:var(--yellow);">⚠ concurrent</span>`
	}

	stats7d := `<span style="color:var(--muted);">—</span>`
	if d.Stats7d != nil && d.Stats7d.Total > 0 {
		color := "var(--green)"
		if d.Stats7d.Failed > 0 {
			color = "var(--yellow)"
		}
		if d.Stats7d.Succeeded == 0 {
			color = "var(--red)"
		}
		stats7d = fmt.Sprintf(`<span style="font-family:var(--font-mono);font-size:0.8rem;color:%s;">%d/%d</span>`,
			color, d.Stats7d.Succeeded, d.Stats7d.Total)
	}
	stats7d += sparklineSVG(d.Durations)

	resources := `<span style="color:var(--muted);">—</span>`
	if d.CronJob.CPURequest != nil || d.CronJob.MemoryRequest != nil {
		resources = ""
		if d.CronJob.CPURequest != nil {
			resources += fmt.Sprintf(`<span class="badge" style="margin-right:3px;">cpu:%s</span>`, esc(*d.CronJob.CPURequest))
		}
		if d.CronJob.MemoryRequest != nil {
			resources += fmt.Sprintf(`<span class="badge">mem:%s</span>`, esc(*d.CronJob.MemoryRequest))
		}
	}

	nameColor := "var(--accent)"
	suspendedTag := ""
	if d.CronJob.Suspended {
		nameColor = "var(--yellow)"
		suspendedTag = ` <span class="badge badge-suspended">paused</span>`
	}
	if d.IsMissed {
		suspendedTag += ` <span class="badge" style="border-color:rgba(252,129,129,0.4);color:var(--red);">missed</span>`
	}
	if d.Stuck.Stuck {
		suspendedTag += fmt.Sprintf(` <span class="badge" style="border-color:rgba(252,129,129,0.4);color:var(--red);" title="%s">stuck</span>`, esc(d.Stuck.Reason))
	}

	// The zone the schedule is evaluated in is part of what the schedule means,
	// so show it whenever the CronJob declares one (DOM-1).
	scheduleCell := fmt.Sprintf(`<code style="font-size:0.8rem;color:var(--muted);">%s</code>`, esc(d.CronJob.Schedule))
	if tz := d.CronJob.TZ(); tz != "" {
		scheduleCell += fmt.Sprintf(
			`<br><span style="font-family:var(--font-mono);font-size:0.7rem;color:var(--muted);opacity:0.75;" title="CronJob spec.timeZone">%s</span>`,
			esc(tz))
	}

	// An unparseable expression or unknown zone means we cannot say when the job
	// runs next. Say that, rather than counting down to a wrong instant.
	nextRunCell := countdownSpan(d.NextRun)
	if d.ScheduleError {
		nextRunCell = `<span class="badge" style="border-color:rgba(252,129,129,0.4);color:var(--red);" title="Unparseable schedule or unknown time zone">unresolved</span>`
	}

	return fmt.Sprintf(`
<tr style="cursor:pointer;" onclick="window.location='/clusters/%s/cronjobs/%s/%s/runs'">
  <td><span style="font-family:var(--font-mono);color:%s;">%s</span>%s</td>
  <td>%s</td>
  <td>%s</td>
  <td>%s</td>
  <td>%s</td>
  <td>%s</td>
  <td style="white-space:nowrap;" onclick="event.stopPropagation()">%s</td>
</tr>`,
		esc(d.ClusterID), esc(d.CronJob.Namespace), esc(d.CronJob.Name),
		nameColor, esc(d.CronJob.Name), suspendedTag,
		scheduleCell,
		nextRunCell,
		lastStatus, stats7d, resources,
		actionButtons(d.ClusterID, d.CronJob.Namespace, d.CronJob.Name, d.CronJob.Suspended),
	)
}

// actionButtons returns the suspend/resume and trigger button HTML for a CronJob.
func actionButtons(clusterID, ns, name string, suspended bool) string {
	var suspendBtn string
	if suspended {
		suspendBtn = fmt.Sprintf(
			`<button class="btn" style="font-size:0.75rem;"
			  hx-post="/api/clusters/%s/cronjobs/%s/%s/resume"
			  hx-confirm="Resume %s?"
			  hx-swap="none"
			  hx-on::after-request="if(event.detail.successful){showToast('Resumed',true);setTimeout(()=>location.reload(),800);}">▶ Resume</button>`,
			esc(clusterID), esc(ns), esc(name), esc(name))
	} else {
		suspendBtn = fmt.Sprintf(
			`<button class="btn ghost" style="font-size:0.75rem;"
			  hx-post="/api/clusters/%s/cronjobs/%s/%s/suspend"
			  hx-confirm="Suspend %s?"
			  hx-swap="none"
			  hx-on::after-request="if(event.detail.successful){showToast('Suspended',true);setTimeout(()=>location.reload(),800);}">⏸</button>`,
			esc(clusterID), esc(ns), esc(name), esc(name))
	}
	triggerBtn := fmt.Sprintf(
		`<button class="btn" style="font-size:0.75rem;margin-left:4px;"
		  hx-post="/api/clusters/%s/cronjobs/%s/%s/trigger"
		  hx-confirm="Trigger a manual run of %s?"
		  hx-swap="none"
		  hx-on::after-request="if(event.detail.successful){var d=JSON.parse(event.detail.xhr.responseText);window.location='/clusters/%s/cronjobs/%s/%s/runs/'+d.run_id;}else{showToast('Trigger failed',false);}">▶ Run</button>`,
		esc(clusterID), esc(ns), esc(name), esc(name),
		esc(clusterID), esc(ns), esc(name))
	return suspendBtn + triggerBtn
}

// cronJobTableOpen is the table wrapper + thead, without <tbody>.
// Use cronJobTableBody or cronJobTableBodyPoll to open the <tbody>.
const cronJobTableOpen = `<div class="card" style="padding:0;overflow:hidden;"><table>
<thead><tr>
  <th>Name</th><th>Schedule</th><th>Next run</th>
  <th>Last status</th><th>7d</th><th>Resources</th><th>Actions</th>
</tr></thead>`

// cronJobTableBodyPoll opens a <tbody> that polls the given URL every 10 s for
// fresh <tr> rows, replacing its content in place via HTMX.
func cronJobTableBodyPoll(url string) string {
	return fmt.Sprintf(`<tbody hx-get="%s" hx-trigger="every 10s" hx-swap="innerHTML">`, esc(url))
}

// renderRunRow returns the HTML <tr> for a single run in the run-list table.
func renderRunRow(run storage.JobRun, clusterID, ns, name string) string {
	shortID := run.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	duration := `<span style="color:var(--muted);">—</span>`
	if run.DurationMs != nil {
		duration = fmt.Sprintf(`<span style="font-family:var(--font-mono);">%s</span>`, fmtDuration(*run.DurationMs))
	}
	exitCode := `<span style="color:var(--muted);">—</span>`
	if run.ExitCode != nil {
		color := "var(--accent)"
		if *run.ExitCode != 0 {
			color = "var(--red)"
		}
		exitCode = fmt.Sprintf(`<span style="font-family:var(--font-mono);color:%s;">%d</span>`, color, *run.ExitCode)
	}
	trigger := `<span class="badge" style="border:1px solid var(--border);color:var(--muted);">scheduled</span>`
	if run.Trigger == "manual" {
		trigger = `<span class="badge" style="border:1px solid var(--accent);color:var(--accent);">manual</span>`
	}
	return fmt.Sprintf(`<tr style="cursor:pointer;" data-day="%s" onclick="window.location='/clusters/%s/cronjobs/%s/%s/runs/%s'">
  <td><code style="color:var(--accent);">%s…</code></td>
  <td>%s</td>
  <td>%s</td>
  <td style="font-family:var(--font-mono);font-size:0.8rem;color:var(--muted);">%s</td>
  <td>%s</td>
  <td>%s</td>
  <td style="font-family:var(--font-mono);font-size:0.8rem;color:var(--muted);">%s</td>
</tr>`,
		esc(run.StartedAt.Format("2006-01-02")),
		esc(clusterID), esc(ns), esc(name), esc(run.ID), esc(shortID),
		trigger,
		statusBadge(run.Status),
		esc(run.StartedAt.Format("2006-01-02 15:04:05")),
		duration, exitCode,
		esc(derefStr(run.NodeName)),
	)
}

// runLoadMoreBtn returns the button markup for the load-more section.
func runLoadMoreBtn(clusterID, ns, name, cursor, day string) string {
	u := "/clusters/" + url.PathEscape(clusterID) +
		"/cronjobs/" + url.PathEscape(ns) +
		"/" + url.PathEscape(name) +
		"/runs/more?before=" + url.QueryEscape(cursor)
	if day != "" {
		u += "&day=" + url.QueryEscape(day)
	}
	return fmt.Sprintf(
		`<button class="btn" hx-get="%s" hx-target="#runs-tbody" hx-swap="beforeend">Load more</button>`,
		esc(u))
}

// runLoadMoreWrap renders the "Load more" section below the runs table (initial render).
func runLoadMoreWrap(clusterID, ns, name, cursor, day string) string {
	return `<div id="load-more-wrap" style="text-align:center;padding:0.75rem 0;">` +
		runLoadMoreBtn(clusterID, ns, name, cursor, day) + `</div>`
}

// runLoadMoreOOB returns an HTMX out-of-band innerHTML update for the load-more div.
// Pass cursor="" to clear the button (last page reached).
func runLoadMoreOOB(clusterID, ns, name, cursor, day string) string {
	inner := ""
	if cursor != "" {
		inner = runLoadMoreBtn(clusterID, ns, name, cursor, day)
	}
	return `<div id="load-more-wrap" hx-swap-oob="innerHTML">` + inner + `</div>`
}
