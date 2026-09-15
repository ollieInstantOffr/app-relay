package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// isAbsoluteURL reports whether u is an absolute http(s) URL.
func isAbsoluteURL(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")
}

// absoluteURL turns an in-app link ("/hosts?edit=…") into a link on the admin
// domain so it works from a phone notification or an email. Without an admin
// domain the relative link is returned unchanged (channels skip it).
func (s *Service) absoluteURL(ctx context.Context, u string) string {
	if u == "" || isAbsoluteURL(u) || !strings.HasPrefix(u, "/") {
		return u
	}
	base := s.baseURL(ctx)
	if base == "" {
		return u
	}
	return base + u
}

func (s *Service) baseURL(ctx context.Context) string {
	g, err := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral)
	if err != nil || strings.TrimSpace(g.AdminDomain) == "" {
		return ""
	}
	domain := strings.ToLower(strings.TrimSpace(g.AdminDomain))
	secure := false
	if hosts, err := s.app.Store.Hosts().List(ctx); err == nil {
		for _, h := range hosts {
			if h.System && h.Enabled && h.CertificateID != "" {
				secure = true
			}
		}
	}
	return baseFor(domain, secure, g.HTTPPort, g.HTTPSPort)
}

func baseFor(domain string, secure bool, httpPort, httpsPort int) string {
	if secure {
		if httpsPort > 0 && httpsPort != 443 {
			return fmt.Sprintf("https://%s:%d", domain, httpsPort)
		}
		return "https://" + domain
	}
	if httpPort > 0 && httpPort != 80 {
		return fmt.Sprintf("http://%s:%d", domain, httpPort)
	}
	return "http://" + domain
}
