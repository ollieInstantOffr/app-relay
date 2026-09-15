package notify

import (
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// setSuffix marks a redacted secret that is stored ("passwordSet": "true").
const setSuffix = "Set"

// Redact blanks secrets in place and flags which ones are stored.
func Redact(s *model.NotificationSettings) {
	chans := make([]model.NotificationChannel, len(s.Channels))
	for i, ch := range s.Channels {
		cfg := map[string]string{}
		for k, v := range ch.Config {
			cfg[k] = v
		}
		for _, k := range secretKeys[ch.Type] {
			if cfg[k] != "" {
				cfg[k+setSuffix] = "true"
			}
			cfg[k] = ""
		}
		ch.Config = cfg
		chans[i] = ch
	}
	s.Channels = chans
}

// MergeSecrets fills blank secrets of ch from the stored channel with the
// same id and strips redaction markers.
func MergeSecrets(stored []model.NotificationChannel, ch *model.NotificationChannel) {
	if ch.Config == nil {
		ch.Config = map[string]string{}
	}
	for _, k := range secretKeys[ch.Type] {
		delete(ch.Config, k+setSuffix)
		if ch.Config[k] != "" {
			continue
		}
		for _, p := range stored {
			if p.ID == ch.ID && p.ID != "" && p.Type == ch.Type {
				if v := p.Config[k]; v != "" {
					ch.Config[k] = v
				}
			}
		}
	}
}

// ValidateChannel checks one channel's config; field keys are relative
// ("config.url").
func ValidateChannel(ch model.NotificationChannel) model.Errs {
	errs := model.Errs{}
	cfg := ch.Config
	switch ch.Type {
	case TypeNtfy, TypeWebhook:
		if err := checkHTTPURL(strings.TrimSpace(cfg["url"])); err != nil {
			errs.Add("config.url", "enter an http(s) URL")
		}
	case TypeSMTP:
		if strings.TrimSpace(cfg["host"]) == "" {
			errs.Add("config.host", "SMTP host is required")
		}
		if p := strings.TrimSpace(cfg["port"]); p != "" {
			if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
				errs.Add("config.port", "invalid port")
			}
		}
		if _, err := mail.ParseAddress(strings.TrimSpace(cfg["from"])); err != nil {
			errs.Add("config.from", "enter a valid sender address")
		}
		if len(recipients(cfg["to"])) == 0 {
			errs.Add("config.to", "enter at least one recipient")
		}
		switch strings.ToLower(cfg["security"]) {
		case "", "tls", "starttls", "none":
		default:
			errs.Add("config.security", "use tls, starttls or none")
		}
	case TypeResend:
		if strings.TrimSpace(cfg["apiKey"]) == "" {
			errs.Add("config.apiKey", "Resend API key is required")
		}
		if _, err := mail.ParseAddress(strings.TrimSpace(cfg["from"])); err != nil {
			errs.Add("config.from", "enter a sender address on a domain verified in Resend")
		}
		if len(recipients(cfg["to"])) == 0 {
			errs.Add("config.to", "enter at least one recipient")
		}
		if rt := strings.TrimSpace(cfg["replyTo"]); rt != "" && len(recipients(rt)) == 0 {
			errs.Add("config.replyTo", "enter a valid reply-to address")
		}
		if ep := strings.TrimSpace(cfg["endpoint"]); ep != "" {
			if err := checkHTTPURL(ep); err != nil {
				errs.Add("config.endpoint", "enter an http(s) URL")
			}
		}
	default:
		errs.Add("type", "choose ntfy, smtp, resend or webhook")
	}
	return errs
}

func defaultName(t string) string {
	switch t {
	case TypeNtfy:
		return "ntfy"
	case TypeSMTP:
		return "Email (SMTP)"
	case TypeResend:
		return "Email (Resend)"
	case TypeWebhook:
		return "Webhook"
	}
	return t
}

// RegisterSettingsHook installs secret handling and validation for the
// notifications settings document.
func RegisterSettingsHook() {
	httpx.SettingsHooks[model.SettingsNotifications] = &httpx.SettingsHook{
		Decorate: func(r *http.Request, v any) any {
			s := *(v.(*model.NotificationSettings))
			Redact(&s)
			return s
		},
		BeforeSave: func(r *http.Request, prev, next any) error {
			return prepare(prev.(*model.NotificationSettings), next.(*model.NotificationSettings))
		},
		AfterSave: func(r *http.Request, prev, next any) {
			Redact(next.(*model.NotificationSettings))
		},
	}
}

func prepare(prev, next *model.NotificationSettings) error {
	errs := model.Errs{}
	if next.Channels == nil {
		next.Channels = []model.NotificationChannel{}
	}
	ids := map[string]bool{}
	for i := range next.Channels {
		ch := &next.Channels[i]
		ch.Type = strings.ToLower(strings.TrimSpace(ch.Type))
		ch.Name = strings.TrimSpace(ch.Name)
		if ch.Name == "" {
			ch.Name = defaultName(ch.Type)
		}
		if ch.ID == "" || ids[ch.ID] {
			ch.ID = store.NewID()
		}
		ids[ch.ID] = true
		clean := map[string]string{}
		for k, v := range ch.Config {
			if strings.HasSuffix(k, setSuffix) {
				continue
			}
			if !isSecret(ch.Type, k) {
				v = strings.TrimSpace(v)
			}
			clean[k] = v
		}
		ch.Config = clean
		MergeSecrets(prev.Channels, ch)
		for field, msg := range ValidateChannel(*ch) {
			errs.Add(fmt.Sprintf("channels.%d.%s", i, field), "%s", msg)
		}
	}
	known := map[string]bool{}
	for _, e := range Events {
		known[e] = true
	}
	routes := map[string][]string{}
	for event, chans := range next.Routes {
		if !known[event] {
			continue
		}
		list := []string{}
		seen := map[string]bool{}
		for _, id := range chans {
			if ids[id] && !seen[id] {
				seen[id] = true
				list = append(list, id)
			}
		}
		routes[event] = list
	}
	next.Routes = routes
	if next.QuietHours.Start == "" {
		next.QuietHours.Start = "23:00"
	}
	if next.QuietHours.End == "" {
		next.QuietHours.End = "07:00"
	}
	if _, ok := parseClock(next.QuietHours.Start); !ok {
		errs.Add("quietHours.start", "use HH:MM")
	}
	if _, ok := parseClock(next.QuietHours.End); !ok {
		errs.Add("quietHours.end", "use HH:MM")
	}
	return errs.Err()
}

func isSecret(t, key string) bool {
	for _, k := range secretKeys[t] {
		if k == key {
			return true
		}
	}
	return false
}
