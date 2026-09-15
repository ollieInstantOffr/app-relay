package edge

import (
	"fmt"
	"math"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	edgecfg "github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
)

// ---------------------------------------------------------------- TLS helpers

// usableCert returns the certificate when it has been issued (NotAfter set,
// PEM files written). Status is not used: a certificate whose renewal failed
// keeps serving its existing files; pending certificates get no TLS yet.
func (r *renderer) usableCert(id string) (*model.Certificate, string) {
	if id == "" {
		return nil, ""
	}
	c := r.certs[id]
	if c == nil {
		return nil, "certificate " + id + " does not exist"
	}
	if c.NotAfter != nil && !c.NotAfter.IsZero() {
		return c, ""
	}
	if c.Status != "" && c.Status != model.CertStatusValid {
		return nil, fmt.Sprintf("certificate %s has not been issued yet (%s)", c.Name, c.Status)
	}
	return nil, fmt.Sprintf("certificate %s has not been issued yet", c.Name)
}

func (r *renderer) certRef(id string, c *model.Certificate) *edgecfg.CertRef {
	full, key := r.env.CertPaths(id)
	return &edgecfg.CertRef{
		ID: id, CertFile: full, KeyFile: key,
		OCSPStapling: c != nil && r.snap.TLS.OCSPStapling && c.Provider == model.CertCustom,
	}
}

// placeholderCert is the self-signed certificate served for unknown SNI.
func (r *renderer) placeholderCert() *edgecfg.CertRef {
	full, key := r.env.DefaultCertPaths()
	return &edgecfg.CertRef{ID: "_default", CertFile: full, KeyFile: key}
}

var cipherProfiles = map[string]bool{"modern": true, "intermediate": true, "old": true}

func (r *renderer) cipherProfile(host string) string {
	for _, p := range []string{host, r.snap.TLS.CipherProfile} {
		if cipherProfiles[p] {
			return p
		}
	}
	return "intermediate"
}

func (r *renderer) hstsHeader(mode string) string {
	h := r.snap.TLS.HSTS
	on := h.Enabled
	switch mode {
	case "on":
		on = true
	case "off":
		on = false
	}
	if !on {
		return ""
	}
	age := h.MaxAgeSeconds
	if age <= 0 {
		age = 15768000
	}
	v := "max-age=" + strconv.Itoa(age)
	if h.IncludeSubdomains {
		v += "; includeSubDomains"
	}
	if h.Preload {
		v += "; preload"
	}
	return v
}

func (r *renderer) http3For(h *model.ProxyHost) bool {
	on := r.snap.General.HTTP3
	if h.HTTP3 != nil {
		on = *h.HTTP3
	}
	return on && r.mod("http_v3")
}

// quicEnabled reports whether any enabled host with a usable certificate
// serves HTTP/3 (then the QUIC listener opens and the default server announces it).
func (r *renderer) quicEnabled() bool {
	if !r.mod("http_v3") {
		return false
	}
	for _, h := range r.enabledHosts() {
		if c, _ := r.usableCert(h.CertificateID); c != nil && r.http3For(h) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- hosts

// host resolves one proxy host. asDefault renders the host as served by the
// default server: no own certificate, HTTPS redirect, HSTS or Alt-Svc (the
// nginx default server only includes the host body).
func (r *renderer) host(h *model.ProxyHost, asDefault bool) edgecfg.Host {
	out := edgecfg.Host{
		ID:            h.ID,
		Domains:       domainList(h.Domains),
		BlockExploits: h.BlockExploits,
		NoIndex:       h.NoIndex,
		MaxBodyBytes:  r.maxBodyBytes(h),
	}
	if !asDefault {
		cert, why := r.usableCert(h.CertificateID)
		if h.CertificateID != "" && cert == nil {
			r.note("host %s: %s: serving HTTP only until it is valid", hostLabel(h), why)
		}
		if cert != nil {
			out.Cert = r.certRef(h.CertificateID, cert)
			out.ForceHTTPS = h.ForceHTTPS
			out.HTTP2 = h.HTTP2
			out.HTTP3 = r.http3For(h)
			out.CipherProfile = r.cipherProfile(h.CipherProfile)
			out.HSTS = r.hstsHeader(h.HSTS)
		}
	}
	if h.ProxyReadTimeout > 0 {
		out.ReadTimeoutSec = h.ProxyReadTimeout
	}
	if h.ProxySendTimeout > 0 {
		out.SendTimeoutSec = h.ProxySendTimeout
	}
	out.RateLimit = r.rateLimit(h)
	if h.Maintenance.Enabled {
		m := &edgecfg.Maintenance{Page: render.ErrorPageHTML(r.snap.ErrorPages.WithDefaults(), "maintenance", &h.Maintenance)}
		if al := r.lists[h.Maintenance.BypassAccessListID]; al != nil {
			m.Bypass, m.BypassDefault = geoRules(al)
		}
		out.Maintenance = m
	}
	// nginx only emits proxy_ssl_verify when an upstream uses https; without it
	// verification is off (also for the forward-auth subrequest).
	out.UpstreamTLSVerify = h.UpstreamTLSVerify && usesHTTPS(h)
	if forwardAuthEnabled(h) {
		out.ForwardAuth = &edgecfg.ForwardAuth{
			VerifyURL:        strings.TrimSpace(h.ForwardAuth.VerifyURL),
			SignInURL:        strings.TrimSpace(h.ForwardAuth.SignInURL),
			PassRemoteUser:   h.ForwardAuth.PassRemoteUser,
			PassRemoteGroups: h.ForwardAuth.PassRemoteGroups,
		}
	}
	out.Locations = []edgecfg.Location{}
	for _, l := range hostLocations(h) {
		out.Locations = append(out.Locations, r.location(h, l))
	}
	out.PathRedirects = r.injectRedirects(h)
	return out
}

// domainList lowercases and trims domains, dropping empty ones (nginx server_name).
func domainList(ds []string) []string {
	out := []string{}
	for _, d := range ds {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			out = append(out, d)
		}
	}
	return out
}

var sizeRe = regexp.MustCompile(`^([0-9]+)([kKmMgG]?)$`)

const defaultMaxBody = 1 << 20 // nginx client_max_body_size 1m (http level)

// maxBodyBytes converts the nginx size of a host; unset or invalid sizes keep
// the 1m default and "0" means unlimited.
func (r *renderer) maxBodyBytes(h *model.ProxyHost) int64 {
	s := strings.TrimSpace(h.MaxBodySize)
	m := sizeRe.FindStringSubmatch(s)
	if s == "" || m == nil {
		return defaultMaxBody
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	mult := int64(1)
	switch strings.ToLower(m[2]) {
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	}
	if err != nil || n > math.MaxInt64/mult {
		r.fail("host %s: max body size %q is too large", hostLabel(h), s)
		return defaultMaxBody
	}
	return n * mult
}

// rateLimit mirrors limit_req_zone / limit_req plus the exempt geo map.
func (r *renderer) rateLimit(h *model.ProxyHost) *edgecfg.RateLimit {
	if !h.RateLimit.Enabled || h.RateLimit.RequestsPerSecond <= 0 {
		return nil
	}
	rl := &edgecfg.RateLimit{RequestsPerSecond: h.RateLimit.RequestsPerSecond, Burst: max(h.RateLimit.Burst, 0)}
	al := r.lists[h.RateLimit.ExemptAccessListID]
	if al == nil {
		return rl
	}
	// nginx geo: the most specific network decides; a network listed twice
	// takes the later value.
	index := map[netip.Prefix]int{}
	for _, rule := range al.Rules {
		cidr := strings.TrimSpace(rule.CIDR)
		if strings.EqualFold(cidr, "all") {
			if rule.Action == "allow" {
				rl.ExemptDefault = true
			}
			continue
		}
		if !validCIDR(cidr) {
			continue
		}
		er := edgecfg.ExemptRule{CIDR: cidr, Exempt: rule.Action == "allow"}
		if p, ok := prefixOf(cidr); ok {
			if i, dup := index[p]; dup {
				rl.Exempt[i].Exempt = er.Exempt
				continue
			}
			index[p] = len(rl.Exempt)
		}
		rl.Exempt = append(rl.Exempt, er)
	}
	return rl
}

func prefixOf(cidr string) (netip.Prefix, bool) {
	if strings.Contains(cidr, "/") {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return netip.Prefix{}, false
		}
		return p.Masked(), true
	}
	a, err := netip.ParseAddr(cidr)
	if err != nil {
		return netip.Prefix{}, false
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), true
}

func usesHTTPS(h *model.ProxyHost) bool {
	if strings.EqualFold(h.Upstream.Scheme, "https") {
		return true
	}
	for _, l := range h.Locations {
		if l.Kind == model.LocationProxy && strings.EqualFold(l.Upstream.Scheme, "https") {
			return true
		}
	}
	return false
}

func forwardAuthEnabled(h *model.ProxyHost) bool {
	return h.ForwardAuth.Enabled && strings.TrimSpace(h.ForwardAuth.VerifyURL) != ""
}

// ---------------------------------------------------------------- locations

const acmePath = "/.well-known/acme-challenge/"

type hostLocation struct {
	model.Location
	skipForwardAuth bool
}

// hostLocations returns the host's locations (deduplicated, longest path
// first) with a default "/" when missing, plus "/.well-known/" without
// forward auth when SkipWellKnown is set.
func hostLocations(h *model.ProxyHost) []hostLocation {
	seen := map[string]bool{acmePath: true}
	out := []hostLocation{}
	for _, l := range h.Locations {
		p := normPath(l.Path)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		l.Path = p
		out = append(out, hostLocation{Location: l})
	}
	if !seen["/"] {
		out = append(out, hostLocation{Location: model.Location{ID: "default", Path: "/", Kind: model.LocationSame, Websockets: h.Websockets}})
	}
	if h.ForwardAuth.Enabled && h.ForwardAuth.SkipWellKnown && !seen["/.well-known/"] {
		out = append(out, hostLocation{Location: model.Location{ID: "well-known", Path: "/.well-known/", Kind: model.LocationSame, Websockets: h.Websockets}, skipForwardAuth: true})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].Path) != len(out[j].Path) {
			return len(out[i].Path) > len(out[j].Path)
		}
		return out[i].Path < out[j].Path
	})
	return out
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (r *renderer) location(h *model.ProxyHost, l hostLocation) edgecfg.Location {
	out := edgecfg.Location{Path: l.Path, SkipMaintenance: l.ID == render.PortalLocationID}
	if l.Kind == model.LocationDeny {
		// nginx `return 403` runs before the access phase: no auth applies.
		out.Kind = "deny"
		return out
	}
	out.Kind = "proxy"
	up := h.Upstream
	if l.Kind == model.LocationProxy {
		up = l.Upstream
	}
	out.Upstream = upstream(up)
	// nginx only strips a prefix for non-root locations.
	out.StripPrefix = l.StripPrefix && l.Path != "/"
	out.Websockets = l.Websockets
	if l.Path == "/" && l.ID == "default" {
		out.Websockets = h.Websockets
	}
	out.Cache = h.CacheAssets || l.Cache
	for _, hd := range l.Headers {
		if !headerNameRe.MatchString(hd.Name) {
			continue
		}
		out.Headers = append(out.Headers, edgecfg.Header{Name: hd.Name, Value: oneLine(hd.Value)})
	}
	if !l.NoAuth {
		listID := h.AccessListID
		if l.AccessListID != "" {
			listID = l.AccessListID
		}
		if listID != "" {
			if r.lists[listID] == nil {
				out.DenyAll = true
			} else {
				out.AccessListID = listID
				r.usedLists[listID] = true
			}
		}
	}
	out.ForwardAuth = forwardAuthActive(h, l)
	return out
}

func forwardAuthActive(h *model.ProxyHost, l hostLocation) bool {
	return !l.NoAuth && !l.skipForwardAuth && forwardAuthEnabled(h)
}

// upstream resolves scheme, default port and base path like nginx's proxy_pass
// plus the base-path rewrite.
func upstream(u model.Upstream) edgecfg.Upstream {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" {
		scheme = "http"
	}
	host := strings.TrimSpace(u.Host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && net.ParseIP(host[1:len(host)-1]) != nil {
		host = host[1 : len(host)-1]
	}
	port := u.Port
	if port <= 0 {
		port = 80
		if scheme == "https" {
			port = 443
		}
	}
	base := strings.TrimRight(strings.TrimSpace(u.Path), "/")
	if base != "" && !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	return edgecfg.Upstream{Scheme: scheme, Host: host, Port: port, Path: base}
}

// geoRules turns an access list into nginx geo semantics (the most specific
// network decides, a network listed twice takes the later value): allowed
// networks map to true; "all" sets the default.
func geoRules(al *model.AccessList) ([]edgecfg.ExemptRule, bool) {
	var out []edgecfg.ExemptRule
	def := false
	index := map[netip.Prefix]int{}
	for _, rule := range al.Rules {
		cidr := strings.TrimSpace(rule.CIDR)
		if strings.EqualFold(cidr, "all") {
			def = rule.Action == "allow"
			continue
		}
		if !validCIDR(cidr) {
			continue
		}
		er := edgecfg.ExemptRule{CIDR: cidr, Exempt: rule.Action == "allow"}
		if p, ok := prefixOf(cidr); ok {
			if i, dup := index[p]; dup {
				out[i].Exempt = er.Exempt
				continue
			}
			index[p] = len(out)
		}
		out = append(out, er)
	}
	return out, def
}
