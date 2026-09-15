package edge

import (
	"sort"
	"strings"

	edgecfg "github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/model"
)

// ---------------------------------------------------------------- redirects

func isWholeDomain(rd *model.Redirect) bool {
	p := strings.TrimSpace(rd.FromPath)
	return p == "" || p == "/"
}

func (r *renderer) enabledRedirects() []*model.Redirect {
	var out []*model.Redirect
	for i := range r.snap.Redirects {
		rd := &r.snap.Redirects[i]
		if rd.Enabled && len(rd.Domains) > 0 && strings.TrimSpace(rd.To) != "" {
			out = append(out, rd)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Domains[0] != out[j].Domains[0] {
			return out[i].Domains[0] < out[j].Domains[0]
		}
		if len(out[i].FromPath) != len(out[j].FromPath) {
			return len(out[i].FromPath) > len(out[j].FromPath)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// hostDomains is the set of domains served by enabled proxy hosts.
func (r *renderer) hostDomains() map[string]bool {
	out := map[string]bool{}
	for _, h := range r.enabledHosts() {
		for _, d := range h.Domains {
			out[strings.ToLower(strings.TrimSpace(d))] = true
		}
	}
	return out
}

func normDomains(ds []string) []string {
	out := domainList(ds)
	sort.Strings(out)
	return out
}

func redirectCode(c int) int {
	switch c {
	case 301, 302, 307, 308:
		return c
	}
	return 301
}

func redirectTarget(to string, keep bool) string {
	to = strings.TrimSpace(to)
	if !keep {
		return to
	}
	return strings.TrimRight(to, "/")
}

func pathRedirect(rd *model.Redirect) edgecfg.PathRedirect {
	return edgecfg.PathRedirect{
		From:     strings.TrimRight(normPath(rd.FromPath), "/"),
		To:       redirectTarget(rd.To, rd.KeepPath),
		Code:     redirectCode(rd.Code),
		KeepPath: rd.KeepPath,
	}
}

// redirects groups the redirects of domains no proxy host serves by their
// (sorted, lowercase) domain set.
func (r *renderer) redirects() []edgecfg.RedirectGroup {
	served := r.hostDomains()
	idx := map[string]int{}
	var groups [][]*model.Redirect
	var domains [][]string
	for _, rd := range r.enabledRedirects() {
		ds := normDomains(rd.Domains)
		skip := false
		for _, d := range ds {
			if served[d] {
				skip = true
			}
		}
		if skip {
			continue // injected into the proxy host (path redirects) or skipped
		}
		key := strings.Join(ds, " ")
		i, ok := idx[key]
		if !ok {
			i = len(groups)
			idx[key] = i
			groups = append(groups, nil)
			domains = append(domains, ds)
		}
		groups[i] = append(groups[i], rd)
	}

	out := []edgecfg.RedirectGroup{}
	for i, rds := range groups {
		g := edgecfg.RedirectGroup{Domains: domains[i]}
		forceHTTPS := false
		for _, rd := range rds {
			if g.Cert == nil && rd.CertificateID != "" {
				if c, why := r.usableCert(rd.CertificateID); c != nil {
					g.Cert = r.certRef(rd.CertificateID, c)
				} else {
					r.note("redirect %s: %s: serving HTTP only", strings.Join(rd.Domains, ", "), why)
				}
			}
			if rd.ForceHTTPS {
				forceHTTPS = true
			}
		}
		g.ForceHTTPS = g.Cert != nil && forceHTTPS
		for _, rd := range rds {
			if isWholeDomain(rd) {
				if g.Whole == nil {
					g.Whole = &edgecfg.Redirect{To: redirectTarget(rd.To, rd.KeepPath), Code: redirectCode(rd.Code), KeepPath: rd.KeepPath}
				}
				continue
			}
			g.Paths = append(g.Paths, pathRedirect(rd))
		}
		out = append(out, g)
	}
	return out
}

// injectRedirects returns the path redirects for domains served by host h.
// Whole-domain redirects for such domains are skipped (the host wins).
func (r *renderer) injectRedirects(h *model.ProxyHost) []edgecfg.PathRedirect {
	domains := map[string]bool{}
	for _, d := range h.Domains {
		domains[strings.ToLower(strings.TrimSpace(d))] = true
	}
	var out []edgecfg.PathRedirect
	for _, rd := range r.enabledRedirects() {
		match := false
		for _, d := range rd.Domains {
			if domains[strings.ToLower(strings.TrimSpace(d))] {
				match = true
			}
		}
		if !match || isWholeDomain(rd) {
			continue
		}
		out = append(out, pathRedirect(rd))
	}
	return out
}

// ---------------------------------------------------------------- default server

func (r *renderer) defaultServer() edgecfg.DefaultServer {
	dh := r.snap.DefaultHost
	d := edgecfg.DefaultServer{Action: "close", HTTP3: r.quicEnabled()}
	if c, why := r.usableCert(dh.CertificateID); c != nil {
		d.Cert = r.certRef(dh.CertificateID, c)
	} else {
		if dh.CertificateID != "" {
			r.note("default server: %s: using the self-signed placeholder", why)
		}
		d.Cert = r.placeholderCert()
	}
	switch dh.Action {
	case "404":
		d.Action = "404"
	case "redirect":
		if to := strings.TrimSpace(dh.RedirectTo); to == "" {
			r.note("default server: redirect target missing: closing the connection instead")
		} else {
			d.Action, d.RedirectTo = "redirect", to
		}
	case "host":
		h := r.hosts[dh.HostID]
		if h == nil || !h.Enabled {
			r.note("default server: default host %s is missing or disabled: closing the connection instead", dh.HostID)
		} else {
			hh := r.host(h, true)
			d.Action, d.Host = "host", &hh
		}
	}
	return d
}
