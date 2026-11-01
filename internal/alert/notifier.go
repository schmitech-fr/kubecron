package alert

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Notifier delivers an Event to one destination.
type Notifier interface {
	// Name labels the notifier in logs and in kubecron_alerts_sent_total.
	Name() string
	Send(ctx context.Context, e Event) error
}

// httpTimeout bounds one delivery: a hung webhook must not hold up the others.
const httpTimeout = 10 * time.Second

var httpClient = &http.Client{Timeout: httpTimeout}

func postJSON(ctx context.Context, target string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		// The URL carries the webhook's secret: *url.Error prints it, so report
		// only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("post: %w", ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// WebhookNotifier posts the Event itself as JSON, for anything that can
// receive one (an automation, a ticketing bridge, a custom bot).
type WebhookNotifier struct{ URL string }

func (n WebhookNotifier) Name() string { return "webhook" }
func (n WebhookNotifier) Send(ctx context.Context, e Event) error {
	return postJSON(ctx, n.URL, e)
}

// slackTextLimit is Slack's limit on a section block's text.
const slackTextLimit = 3000

// SlackNotifier posts to a Slack incoming webhook.
type SlackNotifier struct{ URL string }

func (n SlackNotifier) Name() string { return "slack" }
func (n SlackNotifier) Send(ctx context.Context, e Event) error {
	head := "*" + e.Title() + "*\n" + e.Summary
	if e.URL != "" {
		head += "\n<" + e.URL + "|Open the run>"
	}
	blocks := []map[string]any{section(head)}
	if len(e.LogTail) > 0 {
		blocks = append(blocks, section("```"+truncateHead(strings.Join(e.LogTail, "\n"), slackTextLimit-6)+"```"))
	}
	return postJSON(ctx, n.URL, map[string]any{"text": e.Title(), "blocks": blocks})
}

func section(text string) map[string]any {
	return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
}

// TeamsNotifier posts an Adaptive Card to a Microsoft Teams Workflows webhook
// (the successor of the retired Office 365 connectors).
type TeamsNotifier struct{ URL string }

func (n TeamsNotifier) Name() string { return "teams" }
func (n TeamsNotifier) Send(ctx context.Context, e Event) error {
	body := []map[string]any{
		{"type": "TextBlock", "text": e.Title(), "weight": "Bolder", "size": "Medium", "wrap": true},
		{"type": "TextBlock", "text": e.Summary, "wrap": true},
	}
	if len(e.LogTail) > 0 {
		body = append(body, map[string]any{
			"type": "TextBlock", "text": strings.Join(e.LogTail, "\n"),
			"fontType": "Monospace", "wrap": true, "size": "Small",
		})
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
	if e.URL != "" {
		card["actions"] = []map[string]any{{"type": "Action.OpenUrl", "title": "Open the run", "url": e.URL}}
	}
	return postJSON(ctx, n.URL, map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content":     card,
		}},
	})
}

// SMTPNotifier sends a plain-text mail. Port 465 uses implicit TLS; any other
// port uses STARTTLS when the server offers it. Authentication is only sent
// over TLS (net/smtp refuses otherwise, except to localhost).
type SMTPNotifier struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
}

func (n SMTPNotifier) Name() string { return "smtp" }
func (n SMTPNotifier) Send(ctx context.Context, e Event) error {
	addr := net.JoinHostPort(n.Host, strconv.Itoa(n.Port))
	msg := buildMail(n.From, n.To, e)
	var auth smtp.Auth
	if n.Username != "" {
		auth = smtp.PlainAuth("", n.Username, n.Password, n.Host)
	}
	if n.Port != 465 {
		return smtp.SendMail(addr, auth, n.From, n.To, msg)
	}

	dialer := &net.Dialer{Timeout: httpTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: n.Host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, n.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(n.From); err != nil {
		return err
	}
	for _, to := range n.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// buildMail renders the RFC 5322 message. Header values come from Kubernetes
// object names and log-derived text, so CR and LF are stripped from them: a
// newline in a header value would let it add headers of its own.
func buildMail(from string, to []string, e Event) []byte {
	var b strings.Builder
	header := func(k, v string) {
		b.WriteString(k + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(v) + "\r\n")
	}
	header("From", from)
	header("To", strings.Join(to, ", "))
	header("Subject", mimeWord(e.Title()))
	header("Date", e.At.Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(e.Text(), "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

// mimeWord encodes a header value that is not plain ASCII (the title carries
// an emoji) as an RFC 2047 encoded-word.
func mimeWord(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?utf-8?q?" + qEncode(s) + "?="
		}
	}
	return s
}

func qEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c == ' ':
			b.WriteByte('_')
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '-', c == '.', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "=%02X", c)
		}
	}
	return b.String()
}

// truncateHead keeps the end of s within limit bytes: for a log tail, the last
// lines are the ones that explain the failure.
func truncateHead(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const marker = "…\n"
	cut := s[len(s)-(limit-len(marker)):]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return marker + cut
}
