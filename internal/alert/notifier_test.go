package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sampleEvent() Event {
	exit := 1
	return Event{
		Kind: KindFailed, Cluster: "prod", Namespace: "billing", CronJob: "export",
		Summary: "Failed after 3m12s with exit code 1.", RunID: "r1", ExitCode: &exit,
		LogTail: []string{"connecting", "boom"}, URL: "https://kc.example/run",
		At: time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC),
	}
}

// capture serves one request and hands back its decoded JSON body.
func capture(t *testing.T, status int) (*httptest.Server, func() map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("body is not JSON: %s", b)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		got <- body
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]any { return <-got }
}

func TestWebhook_SendsTheEvent(t *testing.T) {
	srv, body := capture(t, http.StatusNoContent)
	if err := (WebhookNotifier{URL: srv.URL}).Send(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
	b := body()
	if b["kind"] != "failed" || b["cronjob"] != "export" || b["run_id"] != "r1" {
		t.Errorf("body = %v", b)
	}
}

func TestSlack_Payload(t *testing.T) {
	srv, body := capture(t, http.StatusOK)
	if err := (SlackNotifier{URL: srv.URL}).Send(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
	b := body()
	blocks, _ := b["blocks"].([]any)
	if !strings.Contains(b["text"].(string), "billing/export failed on prod") || len(blocks) != 2 {
		t.Fatalf("payload = %v", b)
	}
	logs := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if !strings.HasPrefix(logs, "```") || !strings.Contains(logs, "boom") {
		t.Errorf("log block = %q", logs)
	}
}

func TestTeams_Payload(t *testing.T) {
	srv, body := capture(t, http.StatusAccepted)
	if err := (TeamsNotifier{URL: srv.URL}).Send(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
	b := body()
	att := b["attachments"].([]any)[0].(map[string]any)
	if b["type"] != "message" || att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("payload = %v", b)
	}
	card := att["content"].(map[string]any)
	if card["type"] != "AdaptiveCard" || card["actions"] == nil {
		t.Errorf("card = %v", card)
	}
}

func TestPost_ErrorStatusAndNoSecretInError(t *testing.T) {
	srv, _ := capture(t, http.StatusForbidden)
	if err := (SlackNotifier{URL: srv.URL + "/T000/B000/s3cr3t"}).Send(context.Background(), sampleEvent()); err == nil {
		t.Error("403 accepted as delivered")
	}
	err := (SlackNotifier{URL: "http://127.0.0.1:1/T000/B000/s3cr3t"}).Send(context.Background(), sampleEvent())
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("error = %v; want a failure that does not print the webhook secret", err)
	}
}

func TestBuildMail_HeadersCannotBeInjected(t *testing.T) {
	e := sampleEvent()
	e.CronJob = "export\r\nBcc: attacker@example.com"
	msg := string(buildMail("kubecron@example.com", []string{"ops@example.com"}, e))

	head, body, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		t.Fatal("no header/body separator")
	}
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("injected header: %q", line)
		}
	}
	if !strings.Contains(head, "Subject: =?utf-8?q?") {
		t.Errorf("subject not encoded: %q", head)
	}
	if !strings.Contains(body, "boom\r\n") {
		t.Errorf("body lines not CRLF-terminated: %q", body)
	}
}

func TestTruncateHead_KeepsTheEnd(t *testing.T) {
	s := strings.Repeat("line\n", 1000) + "the error"
	got := truncateHead(s, 100)
	if len(got) > 100 || !strings.HasSuffix(got, "the error") || !strings.HasPrefix(got, "…\n") {
		t.Errorf("truncateHead = %q (len %d)", got, len(got))
	}
}
