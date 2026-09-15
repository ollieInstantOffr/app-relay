package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/instantoffr/relay/internal/model"
)

func TestResendSend(t *testing.T) {
	var mu sync.Mutex
	var got struct {
		auth, idem, ctype, path string
		body                    map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got.auth, got.idem, got.ctype, got.path = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got.body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`))
	}))
	defer srv.Close()

	ch := model.NotificationChannel{Type: TypeResend, Config: map[string]string{
		"apiKey": "re_test_123", "from": "Relay <relay@example.com>", "to": "ops@example.com, jonas@example.com",
		"replyTo": "noreply@example.com", "endpoint": srv.URL,
	}}
	msg := Message{Event: model.EventUpstreamDown, Level: "error", Title: "grafana <down>", Message: "502 from 10.0.0.2:3000", URL: "https://proxy.home.lan/hosts?edit=1", At: at(3, 0)}
	if err := Send(context.Background(), ch, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.path != "/emails" || got.auth != "Bearer re_test_123" || got.ctype != "application/json" || !strings.HasPrefix(got.idem, "relay-") {
		t.Fatalf("request = %+v", got)
	}
	if got.body["from"] != "Relay <relay@example.com>" || got.body["subject"] != "grafana <down>" {
		t.Errorf("body = %v", got.body)
	}
	if to, _ := got.body["to"].([]any); len(to) != 2 || to[0] != "ops@example.com" {
		t.Errorf("to = %v", got.body["to"])
	}
	if rt, _ := got.body["reply_to"].([]any); len(rt) != 1 {
		t.Errorf("reply_to = %v", got.body["reply_to"])
	}
	htmlBody, _ := got.body["html"].(string)
	if !strings.Contains(htmlBody, "grafana &lt;down&gt;") || !strings.Contains(htmlBody, `href="https://proxy.home.lan/hosts?edit=1"`) {
		t.Errorf("html not escaped or missing link: %s", htmlBody)
	}
	if text, _ := got.body["text"].(string); !strings.Contains(text, "502 from") || !strings.Contains(text, "https://proxy.home.lan") {
		t.Errorf("text = %q", text)
	}
	if tags, _ := got.body["tags"].([]any); len(tags) != 1 {
		t.Errorf("tags = %v", got.body["tags"])
	}

	// Same delivery retried → same idempotency key.
	first := got.idem
	mu.Unlock()
	Send(context.Background(), ch, msg)
	mu.Lock()
	if got.idem != first {
		t.Errorf("idempotency key changed on retry: %s vs %s", got.idem, first)
	}
}

func TestResendErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"statusCode":401,"message":"API key is invalid","name":"validation_error"}`))
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"statusCode":422,"message":"The example.com domain is not verified.","name":"validation_error"}`))
	}))
	defer srv.Close()
	cfg := map[string]string{"from": "relay@example.com", "to": "ops@example.com", "endpoint": srv.URL}

	cfg["apiKey"] = "bad"
	err := Send(context.Background(), model.NotificationChannel{Type: TypeResend, Config: cfg}, Message{Title: "t", At: at(1, 0)})
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") || !strings.Contains(err.Error(), "API key is invalid") {
		t.Errorf("401 error = %v", err)
	}
	cfg["apiKey"] = "good"
	err = Send(context.Background(), model.NotificationChannel{Type: TypeResend, Config: cfg}, Message{Title: "t", At: at(1, 0)})
	if err == nil || !strings.Contains(err.Error(), "domain is not verified") {
		t.Errorf("422 error = %v", err)
	}
	delete(cfg, "apiKey")
	if err := Send(context.Background(), model.NotificationChannel{Type: TypeResend, Config: cfg}, Message{Title: "t"}); err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Errorf("missing key error = %v", err)
	}
}

func TestResendValidationAndSecrets(t *testing.T) {
	errs := ValidateChannel(model.NotificationChannel{Type: TypeResend, Config: map[string]string{"from": "nope", "to": "", "replyTo": "x"}})
	for _, k := range []string{"config.apiKey", "config.from", "config.to", "config.replyTo"} {
		if errs[k] == "" {
			t.Errorf("missing validation error %s: %v", k, errs)
		}
	}
	ok := model.NotificationChannel{Type: TypeResend, Config: map[string]string{"apiKey": "re_x", "from": "relay@example.com", "to": "a@example.com"}}
	if errs := ValidateChannel(ok); len(errs) != 0 {
		t.Errorf("valid channel errors: %v", errs)
	}

	stored := model.NotificationSettings{Channels: []model.NotificationChannel{{ID: "c1", Type: TypeResend, Name: "Resend", Config: map[string]string{"apiKey": "re_secret", "from": "relay@example.com", "to": "a@example.com"}}}}
	redacted := stored
	Redact(&redacted)
	if redacted.Channels[0].Config["apiKey"] != "" || redacted.Channels[0].Config["apiKeySet"] != "true" {
		t.Errorf("redacted = %v", redacted.Channels[0].Config)
	}
	if stored.Channels[0].Config["apiKey"] != "re_secret" {
		t.Error("Redact mutated the stored settings")
	}
	next := model.NotificationSettings{Channels: []model.NotificationChannel{{ID: "c1", Type: TypeResend, Config: map[string]string{"apiKeySet": "true", "from": "relay@example.com", "to": "a@example.com"}}}}
	if err := prepare(&stored, &next); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if next.Channels[0].Config["apiKey"] != "re_secret" || next.Channels[0].Name != "Email (Resend)" {
		t.Errorf("kept secret/default name = %v %q", next.Channels[0].Config, next.Channels[0].Name)
	}
	if Target(next.Channels[0]) != "Resend → a@example.com" {
		t.Errorf("target = %q", Target(next.Channels[0]))
	}
}
