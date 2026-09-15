package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

// Channel types.
const (
	TypeNtfy    = "ntfy"
	TypeSMTP    = "smtp"
	TypeResend  = "resend"
	TypeWebhook = "webhook"
)

// secretKeys are config keys that are write-only through the API.
var secretKeys = map[string][]string{
	TypeNtfy:    {"token"},
	TypeSMTP:    {"password"},
	TypeResend:  {"apiKey"},
	TypeWebhook: {"secret"},
}

// Message is one delivery.
type Message struct {
	Event   string
	Level   string
	Title   string
	Message string
	URL     string
	At      time.Time
}

func fromNotification(n core.Notification, at time.Time) Message {
	return Message{Event: n.Event, Level: n.Level, Title: n.Title, Message: n.Message, URL: n.URL, At: at}
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Send delivers msg over one channel.
func Send(ctx context.Context, ch model.NotificationChannel, msg Message) error {
	switch ch.Type {
	case TypeNtfy:
		return sendNtfy(ctx, ch.Config, msg)
	case TypeSMTP:
		return sendSMTP(ctx, ch.Config, msg)
	case TypeResend:
		return sendResend(ctx, ch.Config, msg)
	case TypeWebhook:
		return sendWebhook(ctx, ch.Config, msg)
	}
	return fmt.Errorf("unknown channel type %q", ch.Type)
}

// ---------------------------------------------------------------- ntfy

func headerValue(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	for _, r := range s {
		if r > 127 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

func ntfyPriority(cfg map[string]string, level string) string {
	if p := strings.TrimSpace(cfg["priority"]); p != "" {
		return p
	}
	switch level {
	case "error":
		return "high"
	case "warn":
		return "default"
	}
	return "low"
}

func ntfyTags(level, event string) string {
	tag := "information_source"
	switch level {
	case "error":
		tag = "rotating_light"
	case "warn":
		tag = "warning"
	case "ok":
		tag = "white_check_mark"
	}
	if event != "" {
		return tag + "," + event
	}
	return tag
}

func sendNtfy(ctx context.Context, cfg map[string]string, msg Message) error {
	u := strings.TrimSpace(cfg["url"])
	if err := checkHTTPURL(u); err != nil {
		return err
	}
	body := msg.Message
	if body == "" {
		body = msg.Title
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", headerValue(msg.Title))
	req.Header.Set("Priority", ntfyPriority(cfg, msg.Level))
	req.Header.Set("Tags", ntfyTags(msg.Level, msg.Event))
	if isAbsoluteURL(msg.URL) { // ntfy only opens absolute links
		req.Header.Set("Click", msg.URL)
	}
	if tok := strings.TrimSpace(cfg["token"]); tok != "" {
		if user, pass, ok := strings.Cut(tok, ":"); ok && !strings.HasPrefix(tok, "tk_") {
			req.SetBasicAuth(user, pass)
		} else {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return doHTTP(req)
}

// ---------------------------------------------------------------- webhook

// WebhookPayload shapes the JSON body for Discord, Slack or a generic endpoint.
func WebhookPayload(rawURL string, msg Message) []byte {
	u, _ := url.Parse(rawURL)
	host, p := "", ""
	if u != nil {
		host, p = strings.ToLower(u.Hostname()), u.Path
	}
	text := msg.Title
	switch {
	case (host == "discord.com" || host == "discordapp.com" || strings.HasSuffix(host, ".discord.com")) && strings.HasPrefix(p, "/api/webhooks"):
		content := "**" + msg.Title + "**"
		if msg.Message != "" {
			content += "\n" + msg.Message
		}
		if msg.URL != "" {
			content += "\n" + msg.URL
		}
		b, _ := json.Marshal(map[string]string{"content": truncate(content, 2000)})
		return b
	case host == "hooks.slack.com":
		text = "*" + msg.Title + "*"
		if msg.Message != "" {
			text += "\n" + msg.Message
		}
		if msg.URL != "" {
			text += "\n<" + msg.URL + ">"
		}
		b, _ := json.Marshal(map[string]string{"text": text})
		return b
	}
	b, _ := json.Marshal(struct {
		Event   string    `json:"event"`
		Level   string    `json:"level"`
		Title   string    `json:"title"`
		Message string    `json:"message"`
		URL     string    `json:"url"`
		At      time.Time `json:"at"`
	}{msg.Event, msg.Level, msg.Title, msg.Message, msg.URL, msg.At.UTC()})
	return b
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func sendWebhook(ctx context.Context, cfg map[string]string, msg Message) error {
	u := strings.TrimSpace(cfg["url"])
	if err := checkHTTPURL(u); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(WebhookPayload(u, msg)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Relay")
	if secret := cfg["secret"]; secret != "" {
		name := strings.TrimSpace(cfg["secretHeader"])
		if name == "" {
			name = "X-Relay-Secret"
		}
		req.Header.Set(name, secret)
	}
	return doHTTP(req)
}

func checkHTTPURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return fmt.Errorf("invalid URL %q", u)
	}
	return nil
}

func doHTTP(req *http.Request) error {
	res, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		detail := strings.TrimSpace(string(body))
		if detail != "" {
			return fmt.Errorf("HTTP %d: %s", res.StatusCode, truncate(detail, 200))
		}
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------- SMTP

// SMTPSecurity returns tls | starttls | none (defaults from the port).
func SMTPSecurity(cfg map[string]string) string {
	switch s := strings.ToLower(strings.TrimSpace(cfg["security"])); s {
	case "tls", "starttls", "none":
		return s
	}
	if cfg["port"] == "465" {
		return "tls"
	}
	return "starttls"
}

func recipients(s string) []string {
	out := []string{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if a, err := mail.ParseAddress(part); err == nil {
			out = append(out, a.Address)
		}
	}
	return out
}

func buildEmail(from string, to []string, msg Message) []byte {
	var b bytes.Buffer
	id := make([]byte, 12)
	rand.Read(id)
	domain := "relay.local"
	if a, err := mail.ParseAddress(from); err == nil {
		if _, d, ok := strings.Cut(a.Address, "@"); ok {
			domain = d
		}
	}
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Title))
	fmt.Fprintf(&b, "Date: %s\r\n", msg.At.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n")
	if msg.Event != "" {
		fmt.Fprintf(&b, "X-Relay-Event: %s\r\n", msg.Event)
	}
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := msg.Message
	if isAbsoluteURL(msg.URL) {
		body += "\n\n" + msg.URL
	}
	qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n") + "\r\n\r\n-- \r\nRelay\r\n"))
	qp.Close()
	return b.Bytes()
}

func sendSMTP(ctx context.Context, cfg map[string]string, msg Message) error {
	host := strings.TrimSpace(cfg["host"])
	port := strings.TrimSpace(cfg["port"])
	if host == "" {
		return errors.New("SMTP host is required")
	}
	sec := SMTPSecurity(cfg)
	if port == "" {
		switch sec {
		case "tls":
			port = "465"
		case "starttls":
			port = "587"
		default:
			port = "25"
		}
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("invalid SMTP port %q", port)
	}
	from := strings.TrimSpace(cfg["from"])
	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return fmt.Errorf("invalid from address %q", from)
	}
	to := recipients(cfg["to"])
	if len(to) == 0 {
		return errors.New("no valid recipient address")
	}
	addr := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	deadline := time.Now().Add(45 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	tlsConf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	if sec == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConf)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello("relay"); err != nil {
		return err
	}
	if sec == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("server does not support STARTTLS")
		}
		if err := c.StartTLS(tlsConf); err != nil {
			return err
		}
	}
	if user := cfg["username"]; user != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("server does not offer authentication")
		}
		if err := c.Auth(smtp.PlainAuth("", user, cfg["password"], host)); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}
	}
	if err := c.Mail(fromAddr.Address); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("recipient %s rejected: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildEmail(from, to, msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// ---------------------------------------------------------------- Resend

// resendAPI is Resend's API base; config "endpoint" overrides it (tests).
const resendAPI = "https://api.resend.com"

// ResendPayload builds the POST /emails body.
func ResendPayload(cfg map[string]string, msg Message) ([]byte, error) {
	from := strings.TrimSpace(cfg["from"])
	if _, err := mail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("invalid from address %q", from)
	}
	to := recipients(cfg["to"])
	if len(to) == 0 {
		return nil, errors.New("no valid recipient address")
	}
	text := msg.Message
	if isAbsoluteURL(msg.URL) {
		text += "\n\n" + msg.URL
	}
	text += "\n\n-- \nRelay"

	var h strings.Builder
	h.WriteString(`<div style="font-family:system-ui,-apple-system,Segoe UI,sans-serif;font-size:14px;line-height:1.5;color:#141414">`)
	fmt.Fprintf(&h, `<p style="font-size:16px;font-weight:600;margin:0 0 8px">%s</p>`, html.EscapeString(msg.Title))
	for _, para := range strings.Split(strings.TrimSpace(msg.Message), "\n") {
		if para = strings.TrimSpace(para); para != "" {
			fmt.Fprintf(&h, `<p style="margin:0 0 8px">%s</p>`, html.EscapeString(para))
		}
	}
	if isAbsoluteURL(msg.URL) {
		fmt.Fprintf(&h, `<p style="margin:16px 0"><a href="%s" style="background:#141414;color:#fff;padding:8px 14px;border-radius:8px;text-decoration:none">Open in Relay</a></p>`, html.EscapeString(msg.URL))
	}
	h.WriteString(`<p style="margin:24px 0 0;color:#888;font-size:12px">Sent by Relay</p></div>`)

	body := map[string]any{
		"from":    from,
		"to":      to,
		"subject": truncate(msg.Title, 250),
		"text":    text,
		"html":    h.String(),
	}
	if rt := recipients(cfg["replyTo"]); len(rt) > 0 {
		body["reply_to"] = rt
	}
	if msg.Event != "" && resendTagValue.MatchString(msg.Event) {
		body["tags"] = []map[string]string{{"name": "event", "value": msg.Event}}
	}
	return json.Marshal(body)
}

// Resend tag values may only contain ASCII letters, numbers, _ and -.
var resendTagValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func sendResend(ctx context.Context, cfg map[string]string, msg Message) error {
	key := strings.TrimSpace(cfg["apiKey"])
	if key == "" {
		return errors.New("Resend API key is required")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg["endpoint"]), "/")
	if base == "" {
		base = resendAPI
	} else if err := checkHTTPURL(base); err != nil {
		return err
	}
	payload, err := ResendPayload(cfg, msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/emails", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Relay")
	// Retries of the same delivery must not send the email twice.
	sum := sha256.Sum256([]byte(msg.Event + "\x00" + msg.Title + "\x00" + msg.Message + "\x00" + msg.At.UTC().Format(time.RFC3339Nano) + "\x00" + cfg["to"]))
	req.Header.Set("Idempotency-Key", "relay-"+hex.EncodeToString(sum[:12]))

	res, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode >= 200 && res.StatusCode <= 299 {
		return nil
	}
	var apiErr struct {
		Message string `json:"message"`
		Name    string `json:"name"`
	}
	if json.Unmarshal(raw, &apiErr) == nil && apiErr.Message != "" {
		switch res.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("Resend rejected the API key (HTTP %d): %s", res.StatusCode, apiErr.Message)
		}
		return fmt.Errorf("Resend HTTP %d: %s", res.StatusCode, truncate(apiErr.Message, 200))
	}
	if detail := strings.TrimSpace(string(raw)); detail != "" {
		return fmt.Errorf("Resend HTTP %d: %s", res.StatusCode, truncate(detail, 200))
	}
	return fmt.Errorf("Resend HTTP %d", res.StatusCode)
}

// Target describes a channel for display ("smtp.fastmail.com:465 → jonas@…").
func Target(ch model.NotificationChannel) string {
	switch ch.Type {
	case TypeResend:
		return "Resend → " + ch.Config["to"]
	case TypeSMTP:
		port := ch.Config["port"]
		if port == "" {
			port = "587"
		}
		return ch.Config["host"] + ":" + port + " → " + ch.Config["to"]
	}
	return ch.Config["url"]
}
