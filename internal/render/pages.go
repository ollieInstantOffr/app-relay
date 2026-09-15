package render

// Pages Relay serves itself: the error and maintenance pages (both proxy
// engines) and the page shell of the Relay login page.

import (
	"html"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/model"
)

// Paths Relay reserves on proxy hosts.
const (
	// PortalPrefix serves the Relay login page on hosts protected by it.
	PortalPrefix = "/.relay/"
	// PortalLocationID marks the location that proxies PortalPrefix to Relay.
	PortalLocationID = "relay-portal"
	// ErrorsPath is the internal nginx location of the error page files.
	ErrorsPath = "/.relay-errors/"
)

// ErrorPageCodes are the engine-generated errors Relay's pages replace.
var ErrorPageCodes = []int{403, 404, 429, 500, 502, 503, 504}

// DefaultAccent is the accent colour of Relay's pages.
const DefaultAccent = "#1fa971"

// Accent returns the configured accent colour or the default.
func Accent(set model.ErrorPagesSettings) string {
	if model.ValidAccentColor(set.AccentColor) {
		return set.AccentColor
	}
	return DefaultAccent
}

// ErrorPageHTML returns the page for key ("502", "maintenance" …). A host's
// maintenance title or message (m) replaces the configured maintenance text.
func ErrorPageHTML(set model.ErrorPagesSettings, key string, m *model.Maintenance) string {
	defs := model.DefaultErrorPages().Pages
	p := set.Pages[key]
	if m != nil && (strings.TrimSpace(m.Title) != "" || strings.TrimSpace(m.Message) != "") {
		p = model.ErrorPage{Title: m.Title, Message: m.Message}
	}
	if strings.TrimSpace(p.HTML) != "" {
		return p.HTML
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = defs[key].Title
	}
	msg := strings.TrimSpace(p.Message)
	if msg == "" {
		msg = defs[key].Message
	}
	var b strings.Builder
	if _, err := strconv.Atoi(key); err == nil {
		b.WriteString(`<p class="code">ERROR ` + key + `</p>`)
	}
	b.WriteString(`<h1>` + html.EscapeString(title) + `</h1>`)
	b.WriteString(`<p>` + strings.ReplaceAll(html.EscapeString(msg), "\n", "<br>") + `</p>`)
	refresh := 0
	if key == "maintenance" {
		refresh = 60
	}
	return PageShell(title, Accent(set), set.BrandName, b.String(), refresh)
}

// PageShell wraps body (trusted HTML) in Relay's page design.
func PageShell(title, accent, brand, body string, refreshSeconds int) string {
	if !model.ValidAccentColor(accent) {
		accent = DefaultAccent
	}
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">`)
	if refreshSeconds > 0 {
		b.WriteString(`<meta http-equiv="refresh" content="` + strconv.Itoa(refreshSeconds) + `">`)
	}
	b.WriteString(`<title>` + html.EscapeString(title) + `</title><style>`)
	b.WriteString(strings.ReplaceAll(pageCSS, "ACCENT", accent))
	b.WriteString(`</style></head><body><main><div class="bar"></div>`)
	b.WriteString(body)
	if brand = strings.TrimSpace(brand); brand != "" {
		b.WriteString(`<div class="brand">` + html.EscapeString(brand) + `</div>`)
	}
	b.WriteString("</main></body></html>\n")
	return b.String()
}

const pageCSS = `:root{color-scheme:light dark;--bg:#f7f7f5;--card:#fff;--ink:#16181d;--muted:#5f6470;--line:#e7e6e1;--field:#fff;--accent:ACCENT;--danger:#c93a3a}
@media (prefers-color-scheme:dark){:root{--bg:#111214;--card:#1a1b1e;--ink:#ecedef;--muted:#9a9ea8;--line:#2c2e33;--field:#141518;--danger:#f07171}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px;background:var(--bg);color:var(--ink);font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Inter,Roboto,Helvetica,Arial,sans-serif}
main{width:100%;max-width:420px;background:var(--card);border:1px solid var(--line);border-radius:16px;padding:34px 32px;box-shadow:0 1px 2px rgba(0,0,0,.04),0 12px 32px rgba(0,0,0,.06)}
.bar{width:36px;height:4px;border-radius:2px;background:var(--accent);margin-bottom:22px}
.code{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.08em;color:var(--accent);margin:0 0 12px}
h1{font-size:22px;line-height:1.25;margin:0 0 10px;font-weight:650;letter-spacing:-.01em}
p{margin:0;color:var(--muted)}
.brand{margin-top:28px;padding-top:16px;border-top:1px solid var(--line);font-size:13px;color:var(--muted)}
form{margin-top:22px;display:flex;flex-direction:column;gap:14px}
label{display:flex;flex-direction:column;gap:6px;font-size:13px;font-weight:550;color:var(--ink)}
input{font:inherit;height:40px;padding:0 12px;border-radius:9px;border:1px solid var(--line);background:var(--field);color:var(--ink);outline:none}
input:focus{border-color:var(--accent);box-shadow:0 0 0 3px color-mix(in srgb,var(--accent) 22%,transparent)}
button,.button{font:inherit;font-weight:600;height:42px;border:0;border-radius:9px;background:var(--accent);color:#fff;cursor:pointer;display:inline-flex;align-items:center;justify-content:center;text-decoration:none;padding:0 18px}
button:hover,.button:hover{filter:brightness(1.06)}
.error{margin-top:18px;padding:10px 12px;border-radius:9px;font-size:14px;color:var(--danger);background:color-mix(in srgb,var(--danger) 10%,transparent)}
.hint{font-size:13px;margin-top:14px}
.host{font-weight:600;color:var(--ink)}
`
