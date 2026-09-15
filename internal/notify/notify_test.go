package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func at(h, m int) time.Time { return time.Date(2026, 9, 14, h, m, 0, 0, time.UTC) }

func TestQuietHours(t *testing.T) {
	wrap := model.QuietHours{Enabled: true, Start: "23:00", End: "07:00"}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{at(22, 59), false}, {at(23, 0), true}, {at(0, 30), true}, {at(6, 59), true}, {at(7, 0), false}, {at(12, 0), false},
	} {
		if got := InQuietHours(wrap, c.t); got != c.want {
			t.Errorf("wrap %s = %v, want %v", c.t.Format("15:04"), got, c.want)
		}
	}
	day := model.QuietHours{Enabled: true, Start: "12:00", End: "14:00"}
	if !InQuietHours(day, at(13, 0)) || InQuietHours(day, at(14, 0)) || InQuietHours(day, at(11, 59)) {
		t.Error("same-day window")
	}
	if InQuietHours(model.QuietHours{Enabled: false, Start: "00:00", End: "23:59"}, at(1, 0)) {
		t.Error("disabled window is quiet")
	}
	if InQuietHours(model.QuietHours{Enabled: true, Start: "10:00", End: "10:00"}, at(10, 0)) {
		t.Error("empty window is quiet")
	}
	if got := QuietEnd(wrap, at(23, 30)); !got.Equal(time.Date(2026, 9, 15, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("quiet end before midnight = %v", got)
	}
	if got := QuietEnd(wrap, at(3, 0)); !got.Equal(at(7, 0)) {
		t.Errorf("quiet end after midnight = %v", got)
	}
	if !IsCritical(model.EventUpstreamDown) || IsCritical(model.EventCertExpiring) {
		t.Error("critical events")
	}
}

func TestWebhookPayload(t *testing.T) {
	msg := Message{Event: "upstream_down", Level: "error", Title: "grafana is down", Message: "502 from 10.0.0.2:3000", URL: "https://relay/x", At: at(3, 0)}
	var discord map[string]string
	json.Unmarshal(WebhookPayload("https://discord.com/api/webhooks/1/abc", msg), &discord)
	if len(discord) != 1 || !strings.HasPrefix(discord["content"], "**grafana is down**\n502") {
		t.Errorf("discord = %v", discord)
	}
	var slack map[string]string
	json.Unmarshal(WebhookPayload("https://hooks.slack.com/services/T/B/X", msg), &slack)
	if len(slack) != 1 || slack["text"] != "*grafana is down*\n502 from 10.0.0.2:3000\n<https://relay/x>" {
		t.Errorf("slack = %v", slack)
	}
	var generic map[string]any
	json.Unmarshal(WebhookPayload("https://ha.home.lan/api/webhook/relay", msg), &generic)
	for _, k := range []string{"event", "level", "title", "message", "url", "at"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("generic payload missing %s: %v", k, generic)
		}
	}
	if generic["at"] != "2026-09-14T03:00:00Z" {
		t.Errorf("at = %v", generic["at"])
	}
}

func TestSendWebhook(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, body = r, string(b)
	}))
	defer srv.Close()
	msg := Message{Event: "cert_expiring", Level: "warn", Title: "Zertifikat läuft ab", Message: "in 5 days", At: at(1, 0)}
	if err := Send(context.Background(), model.NotificationChannel{Type: TypeWebhook, Config: map[string]string{"url": srv.URL, "secret": "s3"}}, msg); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("X-Relay-Secret") != "s3" || !strings.Contains(body, `"event":"cert_expiring"`) {
		t.Errorf("webhook request: %v %q", got.Header, body)
	}
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 403) }))
	defer fail.Close()
	if err := Send(context.Background(), model.NotificationChannel{Type: TypeWebhook, Config: map[string]string{"url": fail.URL}}, msg); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 error, got %v", err)
	}
}

func TestSettingsSecrets(t *testing.T) {
	prev := &model.NotificationSettings{Channels: []model.NotificationChannel{
		{ID: "c1", Type: TypeSMTP, Name: "Email", Enabled: true, Config: map[string]string{"host": "smtp.x", "port": "465", "from": "relay@x.org", "to": "a@x.org", "password": "hunter2"}},
	}}
	shown := *prev
	Redact(&shown)
	if shown.Channels[0].Config["password"] != "" || shown.Channels[0].Config["passwordSet"] != "true" {
		t.Fatalf("redact: %v", shown.Channels[0].Config)
	}
	if prev.Channels[0].Config["password"] != "hunter2" {
		t.Fatal("redact mutated the original")
	}
	next := &model.NotificationSettings{
		Channels: []model.NotificationChannel{shown.Channels[0], {Type: TypeWebhook, Config: map[string]string{"url": "https://hooks.slack.com/services/a"}}},
		Routes:   map[string][]string{model.EventUpstreamDown: {"c1", "gone"}, "bogus": {"c1"}},
	}
	if err := prepare(prev, next); err != nil {
		t.Fatal(err)
	}
	if next.Channels[0].Config["password"] != "hunter2" {
		t.Error("secret not kept")
	}
	if _, ok := next.Channels[0].Config["passwordSet"]; ok {
		t.Error("marker stored")
	}
	if next.Channels[1].ID == "" || next.Channels[1].Name != "Webhook" {
		t.Errorf("new channel: %+v", next.Channels[1])
	}
	if r := next.Routes[model.EventUpstreamDown]; len(r) != 1 || r[0] != "c1" {
		t.Errorf("routes = %v", next.Routes)
	}
	if _, ok := next.Routes["bogus"]; ok {
		t.Error("unknown event kept")
	}
	bad := &model.NotificationSettings{Channels: []model.NotificationChannel{{Type: TypeWebhook, Config: map[string]string{"url": "ftp://x"}}}}
	err := prepare(&model.NotificationSettings{}, bad)
	if ve, ok := err.(*model.ValidationError); !ok || ve.Fields["channels.0.config.url"] == "" {
		t.Errorf("validation = %v", err)
	}
}

func TestRoutingQuietDedupe(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := core.New(core.Config{DataDir: dir, RunDir: dir, LogDir: dir}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	st.PutSettings(ctx, model.SettingsNotifications, model.NotificationSettings{
		Channels: []model.NotificationChannel{{ID: "n", Type: TypeWebhook, Enabled: true, Config: map[string]string{"url": "http://x"}}},
		Routes: map[string][]string{
			model.EventUpstreamDown: {"n"}, model.EventCertExpiring: {"n"}, model.EventUnknownSignIn: {"n"},
		},
		QuietHours: model.QuietHours{Enabled: true, Start: "23:00", End: "07:00"},
	})
	s := New(app)
	var mu sync.Mutex
	sent := []Message{}
	s.send = func(ctx context.Context, ch model.NotificationChannel, msg Message) error {
		mu.Lock()
		sent = append(sent, msg)
		mu.Unlock()
		return nil
	}
	clock := at(2, 0)
	s.now = func() time.Time { return clock }

	s.route(ctx, core.Notification{Event: model.EventUpstreamDown, Level: "error", Title: "api down"})
	s.route(ctx, core.Notification{Event: model.EventUpstreamDown, Level: "error", Title: "api down"}) // deduped
	s.route(ctx, core.Notification{Event: model.EventCertExpiring, Level: "warn", Title: "cert a"})
	s.route(ctx, core.Notification{Event: model.EventUnknownSignIn, Level: "warn", Title: "sign-in"})
	if len(s.deliveries) != 1 || len(s.held) != 2 {
		t.Fatalf("deliveries=%d held=%d", len(s.deliveries), len(s.held))
	}
	s.deliver(ctx, <-s.deliveries)

	clock = at(6, 0)
	s.flushHeld(ctx)
	if len(s.deliveries) != 0 {
		t.Fatal("flushed during quiet hours")
	}
	clock = at(7, 1)
	s.flushHeld(ctx)
	if len(s.deliveries) != 1 || len(s.held) != 0 {
		t.Fatalf("digest deliveries=%d held=%d", len(s.deliveries), len(s.held))
	}
	s.deliver(ctx, <-s.deliveries)
	if len(sent) != 2 || !strings.Contains(sent[1].Title, "2 alerts") || !strings.Contains(sent[1].Message, "cert a") {
		t.Fatalf("sent = %+v", sent)
	}
	logs, _ := st.ListNotificationLog(ctx, 10)
	statuses := map[string]int{}
	for _, l := range logs {
		statuses[l.Status]++
	}
	if statuses["sent"] != 2 || statuses["queued"] != 2 {
		t.Errorf("log statuses = %v", statuses)
	}
	if key, due := weeklyDue(time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)); !due || key != "2026-W38" {
		t.Errorf("weekly due = %s %v", key, due)
	}
	if _, due := weeklyDue(time.Date(2026, 9, 14, 8, 59, 0, 0, time.UTC)); due {
		t.Error("weekly due before 09:00")
	}
	n, err := s.WeeklySummary(ctx, at(9, 0))
	if err != nil || !strings.Contains(n.Message, "Hosts: 0") {
		t.Errorf("summary = %+v %v", n, err)
	}
}

func TestDropRemovedChannels(t *testing.T) {
	s := &model.NotificationSettings{
		Channels: []model.NotificationChannel{
			{ID: "old", Type: "ntfy", Config: map[string]string{"url": "https://ntfy.sh/x"}},
			{ID: "mail", Type: TypeSMTP, Config: map[string]string{"host": "smtp.x", "from": "relay@x.org", "to": "a@x.org"}},
		},
		Routes: map[string][]string{model.EventUpstreamDown: {"old", "mail"}},
	}
	if err := prepare(&model.NotificationSettings{}, s); err != nil {
		t.Fatalf("legacy ntfy channel blocked saving: %v", err)
	}
	if len(s.Channels) != 1 || s.Channels[0].ID != "mail" {
		t.Errorf("channels = %+v", s.Channels)
	}
	if r := s.Routes[model.EventUpstreamDown]; len(r) != 1 || r[0] != "mail" {
		t.Errorf("routes = %v", s.Routes)
	}
	if err := Send(context.Background(), model.NotificationChannel{Type: "ntfy"}, Message{}); err == nil {
		t.Error("ntfy still sendable")
	}
}
