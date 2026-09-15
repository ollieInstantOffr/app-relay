package model

import (
	"regexp"
	"strings"
)

// SettingsErrorPages holds the pages Relay shows instead of the engine's
// plain error pages, and the maintenance page.
const SettingsErrorPages = "error_pages"

// ErrorPageKeys are the replaceable pages: errors the proxy engine generates
// itself (upstream down, access denied, rate limited …) plus maintenance.
var ErrorPageKeys = []string{"403", "404", "429", "500", "502", "503", "504", "maintenance"}

// MaxErrorPageHTML bounds a custom HTML page.
const MaxErrorPageHTML = 100 << 10

type ErrorPage struct {
	Title   string `json:"title"`
	Message string `json:"message"`
	// HTML replaces the built-in design entirely ("" = built-in design).
	HTML string `json:"html,omitempty"`
}

type ErrorPagesSettings struct {
	// Enabled: Relay's pages replace the engine's plain pages for errors the
	// engine generates. Pages returned by upstream apps pass through.
	Enabled     bool                 `json:"enabled"`
	BrandName   string               `json:"brandName"`
	AccentColor string               `json:"accentColor"` // #rrggbb, "" = default
	Pages       map[string]ErrorPage `json:"pages"`
}

// DefaultErrorPages are the built-in titles and messages.
func DefaultErrorPages() ErrorPagesSettings {
	return ErrorPagesSettings{Pages: map[string]ErrorPage{
		"403":         {Title: "Access denied", Message: "You don't have permission to view this page."},
		"404":         {Title: "Page not found", Message: "The page you're looking for doesn't exist."},
		"429":         {Title: "Slow down", Message: "Too many requests. Please wait a moment and try again."},
		"500":         {Title: "Something went wrong", Message: "The server ran into a problem. Please try again shortly."},
		"502":         {Title: "Service unavailable", Message: "The app behind this address isn't responding right now. Please try again in a minute."},
		"503":         {Title: "Temporarily unavailable", Message: "This service is temporarily unavailable. Please try again shortly."},
		"504":         {Title: "Taking too long", Message: "The app didn't respond in time. Please try again in a minute."},
		"maintenance": {Title: "Down for maintenance", Message: "We're making some improvements and will be back shortly."},
	}}
}

// WithDefaults fills pages that are missing (versions applied before error
// pages existed have none).
func (s ErrorPagesSettings) WithDefaults() ErrorPagesSettings {
	pages := make(map[string]ErrorPage, len(ErrorPageKeys))
	for k, p := range DefaultErrorPages().Pages {
		pages[k] = p
	}
	for k, p := range s.Pages {
		if _, ok := pages[k]; ok {
			pages[k] = p
		}
	}
	s.Pages = pages
	return s
}

var accentColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// ValidAccentColor reports whether c is a #rrggbb colour.
func ValidAccentColor(c string) bool { return accentColorRe.MatchString(c) }

func (s *ErrorPagesSettings) Normalize() {
	s.BrandName = strings.TrimSpace(s.BrandName)
	s.AccentColor = strings.ToLower(strings.TrimSpace(s.AccentColor))
	*s = s.WithDefaults()
	for k, p := range s.Pages {
		p.Title = strings.TrimSpace(p.Title)
		p.Message = strings.TrimSpace(strings.ReplaceAll(p.Message, "\r\n", "\n"))
		if strings.TrimSpace(p.HTML) == "" {
			p.HTML = ""
		}
		s.Pages[k] = p
	}
}

func (s *ErrorPagesSettings) Validate() error {
	e := Errs{}
	if len(s.BrandName) > 80 {
		e.Add("brandName", "Keep the brand name under 80 characters")
	}
	if s.AccentColor != "" && !ValidAccentColor(s.AccentColor) {
		e.Add("accentColor", "Use a colour like #1fa971")
	}
	for _, k := range ErrorPageKeys {
		p := s.Pages[k]
		if len(p.Title) > 120 {
			e.Add("pages."+k+".title", "Keep the title under 120 characters")
		}
		if len(p.Message) > 2000 {
			e.Add("pages."+k+".message", "Keep the message under 2000 characters")
		}
		if len(p.HTML) > MaxErrorPageHTML {
			e.Add("pages."+k+".html", "Custom HTML must be under 100 KB")
		}
	}
	return e.Err()
}
