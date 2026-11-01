// Package alert sends a message when a CronJob run fails, a scheduled run is
// missed, or a run is stuck — with the run's last log lines attached, which is
// the part Alertmanager cannot do (PRODUCT §3.2).
//
// Alerts are evaluated from stored state on a ticker, not from watcher events:
// that makes them survive a restart, and the alerts_sent table makes each one
// go out once.
package alert

import (
	"fmt"
	"strings"
	"time"
)

// Kind is what an alert reports.
type Kind string

const (
	KindFailed Kind = "failed"
	KindMissed Kind = "missed"
	KindStuck  Kind = "stuck"
)

// Event is one alert, the same for every notifier. It is also the generic
// webhook's JSON body, so its field names are a public shape: add, never
// rename.
type Event struct {
	Kind      Kind   `json:"kind"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	CronJob   string `json:"cronjob"`
	// Summary is one human sentence: what happened and how it compares with
	// what usually happens.
	Summary string `json:"summary"`

	RunID      string     `json:"run_id,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	// LogTail is the run's last captured log lines, oldest first.
	LogTail []string `json:"log_tail,omitempty"`
	// URL links to the run (or the CronJob) on the dashboard, when
	// ALERT_BASE_URL is set.
	URL string `json:"url,omitempty"`

	At time.Time `json:"at"`
}

// Title is the one-line headline used as Slack text, Teams title and mail
// subject.
func (e Event) Title() string {
	icon := map[Kind]string{KindFailed: "❌", KindMissed: "⏰", KindStuck: "⏳"}[e.Kind]
	return fmt.Sprintf("%s %s/%s %s on %s", icon, e.Namespace, e.CronJob, e.Kind, e.Cluster)
}

// Text renders the event as plain text: headline, summary, link, log tail.
func (e Event) Text() string {
	var b strings.Builder
	b.WriteString(e.Title())
	b.WriteString("\n")
	b.WriteString(e.Summary)
	b.WriteString("\n")
	if e.URL != "" {
		b.WriteString(e.URL)
		b.WriteString("\n")
	}
	if len(e.LogTail) > 0 {
		fmt.Fprintf(&b, "\nLast %d log lines:\n", len(e.LogTail))
		b.WriteString(strings.Join(e.LogTail, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}
