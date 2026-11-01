package alert

import (
	"fmt"
	"strings"
)

// Config is the ALERT_* environment. Every destination is optional; with none
// configured alerting is off.
type Config struct {
	// On lists the kinds to alert on.
	On []string `env:"ALERT_ON" envSeparator:"," envDefault:"failed,missed,stuck"`
	// LogLines is how many of the run's last log lines an alert carries; 0
	// sends none. Logs can hold secrets: lower it, or set 0, when the
	// destination is wider than the people who may read them.
	LogLines int `env:"ALERT_LOG_LINES" envDefault:"20"`
	// BaseURL is the dashboard's external URL (https://kubecron.example.com),
	// used to link each alert to its run. Empty sends no link.
	BaseURL string `env:"ALERT_BASE_URL"`

	WebhookURL      string `env:"ALERT_WEBHOOK_URL"`
	SlackWebhookURL string `env:"ALERT_SLACK_WEBHOOK_URL"`
	TeamsWebhookURL string `env:"ALERT_TEAMS_WEBHOOK_URL"`

	SMTPHost     string   `env:"ALERT_SMTP_HOST"`
	SMTPPort     int      `env:"ALERT_SMTP_PORT" envDefault:"587"`
	SMTPUsername string   `env:"ALERT_SMTP_USERNAME"`
	SMTPPassword string   `env:"ALERT_SMTP_PASSWORD"`
	SMTPFrom     string   `env:"ALERT_SMTP_FROM"`
	SMTPTo       []string `env:"ALERT_SMTP_TO" envSeparator:","`
}

// Notifiers builds one notifier per configured destination.
func (c Config) Notifiers() ([]Notifier, error) {
	var out []Notifier
	if c.WebhookURL != "" {
		out = append(out, WebhookNotifier{URL: c.WebhookURL})
	}
	if c.SlackWebhookURL != "" {
		out = append(out, SlackNotifier{URL: c.SlackWebhookURL})
	}
	if c.TeamsWebhookURL != "" {
		out = append(out, TeamsNotifier{URL: c.TeamsWebhookURL})
	}
	if c.SMTPHost != "" {
		to := nonEmpty(c.SMTPTo)
		if c.SMTPFrom == "" || len(to) == 0 {
			return nil, fmt.Errorf("ALERT_SMTP_HOST is set: ALERT_SMTP_FROM and ALERT_SMTP_TO are required")
		}
		out = append(out, SMTPNotifier{
			Host: c.SMTPHost, Port: c.SMTPPort,
			Username: c.SMTPUsername, Password: c.SMTPPassword,
			From: c.SMTPFrom, To: to,
		})
	}
	return out, nil
}

// Kinds parses On.
func (c Config) Kinds() (map[Kind]bool, error) {
	out := map[Kind]bool{}
	for _, k := range nonEmpty(c.On) {
		switch kind := Kind(strings.ToLower(k)); kind {
		case KindFailed, KindMissed, KindStuck:
			out[kind] = true
		default:
			return nil, fmt.Errorf("ALERT_ON: unknown kind %q (want failed, missed, stuck)", k)
		}
	}
	return out, nil
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
