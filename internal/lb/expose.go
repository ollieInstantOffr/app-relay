package lb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render/edge"
	"github.com/instantoffr/relay/internal/render/haproxy"
	"github.com/instantoffr/relay/internal/render/nginx"
	"github.com/instantoffr/relay/internal/store"
)

// ExposeRequest is the body of POST /api/lb/expose (and its preview).
type ExposeRequest struct {
	BackendID   string `json:"backendId"`
	Domain      string `json:"domain"`
	Certificate struct {
		Mode          string `json:"mode"` // request | existing | none
		CertificateID string `json:"certificateId,omitempty"`
		Challenge     string `json:"challenge,omitempty"`
		DNSProviderID string `json:"dnsProviderId,omitempty"`
	} `json:"certificate"`
	ForceHTTPS bool `json:"forceHttps"`
	Websockets bool `json:"websockets"`
	Access     struct {
		Mode         string `json:"mode"` // public | list
		AccessListID string `json:"accessListId,omitempty"`
	} `json:"access"`
	ForwardAuth   *model.ForwardAuth `json:"forwardAuth,omitempty"`
	RateLimit     *model.RateLimit   `json:"rateLimit,omitempty"`
	BlockExploits bool               `json:"blockExploits"`
	GeoBlock      *model.GeoBlock    `json:"geoBlock,omitempty"`
	NoIndex       bool               `json:"noIndex"`
	ApplyNow      bool               `json:"applyNow"`
}

type exposePlan struct {
	snap     *model.Snapshot
	backend  *model.Backend
	frontend model.Frontend
	host     model.ProxyHost
	certReq  *core.CertRequest
	cert     *model.Certificate // existing certificate
}

var exposeDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$|^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// certCovers reports whether a certificate domain list covers domain.
func certCovers(domains []string, domain string) bool {
	for _, d := range domains {
		d = strings.ToLower(d)
		if d == domain {
			return true
		}
		if strings.HasPrefix(d, "*.") {
			suffix := d[1:]
			if strings.HasSuffix(domain, suffix) && !strings.Contains(strings.TrimSuffix(domain, suffix), ".") && len(domain) > len(suffix) {
				return true
			}
		}
	}
	return false
}

// usedPorts returns ports bound on the host according to the engine agents.
func (h *handlers) usedPorts(ctx context.Context) map[int]bool {
	used := map[int]bool{}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pc, _ := h.app.Proxy(ctx)
	for _, c := range []*agent.Client{h.app.HAProxy, pc} {
		if c == nil {
			continue
		}
		if res, err := c.Listeners(cctx); err == nil {
			for _, l := range res.Listeners {
				if l.Proto == "tcp" || l.Proto == "" {
					used[l.Port] = true
				}
			}
		}
	}
	return used
}

// freeExposePort returns the first port ≥ start not used by config or the host.
func (h *handlers) freeExposePort(ctx context.Context, snap *model.Snapshot, start int) (int, error) {
	if start < 1024 {
		start = 10080
	}
	used := h.usedPorts(ctx)
	for p := start; p <= 65535; p++ {
		if !used[p] && portOwner(h.app, snap, "", "127.0.0.1", p) == "" {
			return p, nil
		}
	}
	return 0, httpx.Errorf(http.StatusConflict, "no_port", fmt.Sprintf("no free port at or above %d", start))
}

func uniqueFrontendName(snap *model.Snapshot, base string) string {
	if len(base) > 60 {
		base = base[:60]
	}
	taken := map[string]bool{strings.ToLower(haproxy.StatsFrontendName): true}
	for _, f := range snap.Frontends {
		taken[strings.ToLower(f.Name)] = true
	}
	name := base
	for i := 2; taken[strings.ToLower(name)]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func (h *handlers) planExpose(ctx context.Context, req *ExposeRequest) (*exposePlan, error) {
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	p := &exposePlan{snap: snap}
	e := model.Errs{}
	for i := range snap.Backends {
		if snap.Backends[i].ID == req.BackendID {
			p.backend = &snap.Backends[i]
		}
	}
	if p.backend == nil {
		return nil, httpx.Errorf(http.StatusNotFound, "not_found", "backend not found")
	}
	if p.backend.Mode != "http" {
		e.Add("backendId", "Only HTTP backends can be exposed through the reverse proxy; use a stream for TCP")
	}

	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Domain)), ".")
	switch {
	case domain == "":
		e.Add("domain", "Enter the public domain")
	case !exposeDomainRe.MatchString(domain):
		e.Add("domain", "Not a valid domain name")
	default:
		for _, host := range snap.Hosts {
			for _, d := range host.Domains {
				if strings.EqualFold(d, domain) {
					e.Add("domain", "%s is already served by a proxy host", domain)
				}
			}
		}
		for _, r := range snap.Redirects {
			for _, d := range r.Domains {
				if strings.EqualFold(d, domain) {
					e.Add("domain", "%s is already used by a redirect", domain)
				}
			}
		}
	}

	switch req.Certificate.Mode {
	case "request":
		ch := req.Certificate.Challenge
		if ch == "" {
			ch = snap.TLS.PreferredChallenge
		}
		if ch == "" {
			ch = model.ChallengeHTTP01
		}
		if ch != model.ChallengeHTTP01 && ch != model.ChallengeDNS01 && ch != model.ChallengeTLSALPN01 {
			e.Add("certificate.challenge", "Unknown challenge type")
		}
		if ch == model.ChallengeDNS01 && req.Certificate.DNSProviderID == "" {
			e.Add("certificate.dnsProviderId", "DNS-01 needs a DNS provider")
		}
		p.certReq = &core.CertRequest{
			Domains: []string{domain}, Challenge: ch, DNSProviderID: req.Certificate.DNSProviderID,
			Staging: snap.TLS.ACMEProvider == model.CertLetsEncryptStaging, AutoRenew: true,
		}
	case "existing":
		for i := range snap.Certificates {
			if snap.Certificates[i].ID == req.Certificate.CertificateID {
				p.cert = &snap.Certificates[i]
			}
		}
		switch {
		case p.cert == nil:
			e.Add("certificate.certificateId", "Pick a certificate")
		case domain != "" && !certCovers(p.cert.Domains, domain):
			e.Add("certificate.certificateId", "%s does not cover %s", p.cert.Name, domain)
		}
	case "none", "":
	default:
		e.Add("certificate.mode", "Mode must be request, existing or none")
	}

	accessListID := ""
	if req.Access.Mode == "list" {
		found := false
		for _, al := range snap.AccessLists {
			if al.ID == req.Access.AccessListID {
				found = true
			}
		}
		if !found {
			e.Add("access.accessListId", "Pick an access list")
		}
		accessListID = req.Access.AccessListID
	}
	if err := e.Err(); err != nil {
		return nil, err
	}

	port, err := h.freeExposePort(ctx, snap, snap.HAProxy.ExposePortStart)
	if err != nil {
		return nil, err
	}
	hostID := store.NewID()
	p.frontend = model.Frontend{
		Name: uniqueFrontendName(snap, "fe-"+p.backend.Name), Mode: "http",
		Bind: fmt.Sprintf("127.0.0.1:%d", port), Rules: []model.FrontendRule{},
		DefaultBackendID: p.backend.ID, Enabled: true, Source: model.SourceExpose, HostID: hostID,
	}
	hasCert := p.certReq != nil || p.cert != nil
	p.host = model.ProxyHost{
		Meta:    model.Meta{ID: hostID},
		Domains: []string{domain}, Enabled: true,
		Upstream:      model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: port, BackendID: p.backend.ID},
		Websockets:    req.Websockets,
		BlockExploits: req.BlockExploits,
		AccessListID:  accessListID,
		ForceHTTPS:    req.ForceHTTPS && hasCert,
		HTTP2:         snap.General.Defaults.HTTP2,
		HSTS:          "inherit",
		Locations:     []model.Location{},
		NoIndex:       req.NoIndex,
		Source:        model.SourceExpose,
		SourceRef:     p.backend.ID,
	}
	if p.cert != nil {
		p.host.CertificateID = p.cert.ID
	}
	if req.ForwardAuth != nil && req.ForwardAuth.Enabled {
		p.host.ForwardAuth = *req.ForwardAuth
	}
	if req.RateLimit != nil && req.RateLimit.Enabled {
		p.host.RateLimit = *req.RateLimit
	}
	if req.GeoBlock != nil && req.GeoBlock.Enabled {
		p.host.GeoBlock = *req.GeoBlock
	}
	if p.host.GeoBlock.AllowCountries == nil {
		p.host.GeoBlock.AllowCountries = []string{}
	}
	return p, nil
}

func (h *handlers) expose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req ExposeRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	p, err := h.planExpose(ctx, &req)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	fe := &p.frontend
	host := &p.host
	if err := mergeErrs(fe.Validate(), checkFrontend(h.app, p.snap, fe)); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// Run the hosts slice's checks before anything is created. A requested
	// certificate does not exist yet, so validate without it first.
	wantForce := host.ForceHTTPS
	if p.certReq != nil {
		host.ForceHTTPS = false
	}
	if hk := httpx.HostHooks.BeforeSave; hk != nil {
		if err := hk(r, nil, host); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}
	if v, ok := any(host).(model.Validator); ok {
		if err := v.Validate(); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}

	var cert *model.Certificate
	if p.certReq != nil {
		if h.app.Certs == nil {
			httpx.Fail(w, r, core.ErrNotImplemented)
			return
		}
		cert, err = h.app.Certs.Request(ctx, *p.certReq)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		host.CertificateID = cert.ID
		host.ForceHTTPS = wantForce
	}

	if err := h.app.Store.Frontends().Create(ctx, fe); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.app.Store.Hosts().Create(ctx, host); err != nil {
		_ = h.app.Store.Frontends().Delete(context.WithoutCancel(ctx), fe.ID)
		httpx.Fail(w, r, err)
		return
	}
	domain := host.Domains[0]
	h.app.Audit(ctx, core.AuditEntry{Action: "backend.expose", Target: p.backend.Name, Detail: fmt.Sprintf("%s → %s → %s", domain, fe.Bind, p.backend.Name), Result: "saved"})
	h.app.Changed(ctx, model.KindFrontend, fe.ID, fe.Name, core.ActionCreated)
	h.app.Changed(ctx, model.KindHost, host.ID, domain, core.ActionCreated)
	h.app.Activity(ctx, "backend.exposed", "info", fmt.Sprintf("Backend %s exposed at %s", p.backend.Name, domain), domain, "frontend "+fe.Name+" · "+fe.Bind)

	resp := map[string]any{"host": host, "frontend": fe}
	if cert != nil {
		resp["certificate"] = cert
	}
	if req.ApplyNow {
		if h.app.Engine == nil {
			resp["applyError"] = "apply engine unavailable"
		} else if v, err := h.app.Engine.Apply(ctx, core.ApplyOptions{Summary: fmt.Sprintf("Expose %s at %s", p.backend.Name, domain)}); err != nil {
			resp["applyError"] = err.Error()
			if v != nil {
				resp["version"] = v
			}
		} else {
			resp["version"] = v
		}
	}
	httpx.WriteJSON(w, http.StatusCreated, resp)
}

type exposePreview struct {
	// Nginx* describe the active proxy engine's files (see Engine); the field
	// names are kept for compatibility.
	Engine        string `json:"engine"` // nginx | edge
	Nginx         string `json:"nginx"`
	NginxValid    *bool  `json:"nginxValid"`
	NginxOutput   string `json:"nginxOutput"`
	HAProxy       string `json:"haproxy"`
	HAProxyValid  *bool  `json:"haproxyValid"`
	HAProxyOutput string `json:"haproxyOutput"`
	Port          int    `json:"port"`
	FrontendName  string `json:"frontendName"`
	BackendName   string `json:"backendName"`
}

func (h *handlers) exposePreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req ExposeRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	p, err := h.planExpose(ctx, &req)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	_, port, _ := model.SplitBind(p.frontend.Bind)
	out := exposePreview{Port: port, FrontendName: p.frontend.Name, BackendName: p.backend.Name}
	env := h.env(ctx)

	next := *p.snap
	next.Frontends = append(append([]model.Frontend{}, p.snap.Frontends...), p.frontend)
	host := p.host
	next.Certificates = append([]model.Certificate{}, p.snap.Certificates...)
	if p.certReq != nil {
		// Stand-in for the certificate that will be requested.
		host.CertificateID = "pending-" + host.ID
		next.Certificates = append(next.Certificates, model.Certificate{
			Meta: model.Meta{ID: host.CertificateID}, Name: host.Domains[0], Domains: host.Domains,
			Provider: certProviderFor(p.snap.TLS), Challenge: p.certReq.Challenge, Status: model.CertStatusPending, History: []model.CertEvent{},
		})
	}
	next.Hosts = append(append([]model.ProxyHost{}, p.snap.Hosts...), host)

	// haproxy: the new frontend section + a full-config check
	out.HAProxy = haproxy.RenderFrontend(&next, &p.frontend) + "\n# backend " + p.backend.Name + " unchanged\n"
	if err := mergeErrs(p.frontend.Validate(), checkFrontend(h.app, p.snap, &p.frontend)); err != nil {
		f := false
		out.HAProxyValid, out.HAProxyOutput = &f, err.Error()
	} else if cfg, err := haproxy.Render(&next, env); err != nil {
		f := false
		out.HAProxyValid, out.HAProxyOutput = &f, err.Error()
	} else if v := h.validateConfig(ctx, cfg); v.Checked == "haproxy" {
		out.HAProxyValid, out.HAProxyOutput = &v.Valid, v.Output
	} else {
		out.HAProxyOutput = v.Output
	}

	// proxy engine: files that the new host adds or changes
	pc, engine := h.app.Proxy(ctx)
	renderProxy := nginx.Render
	name := "nginx"
	if engine == agent.EngineEdge {
		renderProxy, name = edge.Render, "Relay Edge"
	}
	out.Engine = engine
	before, err1 := renderProxy(p.snap, env)
	after, err2 := renderProxy(&next, env)
	switch {
	case err2 != nil:
		f := false
		out.NginxValid, out.NginxOutput = &f, err2.Error()
	case err1 != nil:
		out.Nginx = joinFiles(after, nil, host.ID)
	default:
		out.Nginx = joinFiles(after, before, host.ID)
	}
	if err2 == nil && len(after) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		res, err := pc.Validate(cctx, after)
		cancel()
		if err == nil {
			out.NginxValid, out.NginxOutput = &res.OK, strings.TrimSpace(res.Output)
		} else {
			var ua agent.ErrUnavailable
			if errors.As(err, &ua) {
				out.NginxOutput = name + " agent not reachable — not validated"
			} else {
				out.NginxOutput = name + " validation unavailable: " + err.Error()
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// joinFiles returns the files in after that are new or changed compared with
// before, preferring files named after the host id.
func joinFiles(after, before agent.Files, hostID string) string {
	var keys []string
	for k, v := range after {
		if strings.Contains(k, hostID) {
			keys = append(keys, k)
			continue
		}
		if before != nil {
			if prev, ok := before[k]; !ok || prev != v {
				keys = append(keys, k)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		ci, cj := strings.Contains(keys[i], hostID), strings.Contains(keys[j], hostID)
		if ci != cj {
			return ci
		}
		return keys[i] < keys[j]
	})
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("\n")
		}
		if len(keys) > 1 {
			b.WriteString("# " + k + "\n")
		}
		b.WriteString(strings.TrimRight(after[k], "\n") + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------- convert host

var nonName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func (h *handlers) fromHost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	prev, err := h.app.Store.Hosts().Get(ctx, chi.URLParam(r, "hostId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if prev.Upstream.BackendID != "" {
		httpx.Fail(w, r, httpx.Errorf(http.StatusConflict, "conflict", "this host already routes through a load balancer backend"))
		return
	}
	if prev.System {
		httpx.Fail(w, r, httpx.Errorf(http.StatusConflict, "conflict", "the Relay admin host can't be load balanced"))
		return
	}
	u := prev.Upstream
	if strings.TrimSpace(u.Host) == "" || u.Port == 0 {
		httpx.Fail(w, r, &model.ValidationError{Fields: map[string]string{"upstream": "Host has no upstream address to convert"}})
		return
	}
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	base := "pool"
	if len(prev.Domains) > 0 {
		base = strings.TrimPrefix(strings.Split(prev.Domains[0], ".")[0], "*")
		if base == "" && strings.Count(prev.Domains[0], ".") > 1 {
			base = strings.Split(prev.Domains[0], ".")[1]
		}
	}
	base = strings.Trim(nonName.ReplaceAllString(base, "-"), "-.")
	if base == "" {
		base = "pool"
	}
	name := base
	for i := 2; ; i++ {
		clash := false
		for _, b := range snap.Backends {
			if strings.EqualFold(b.Name, name) {
				clash = true
			}
		}
		if !clash {
			break
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}

	backend := model.Backend{
		Name: name, Mode: "http", Algorithm: "roundrobin",
		Servers:      []model.Server{{Address: u.Host, Port: u.Port, Weight: 100, Role: model.ServerActive, Check: true}},
		HealthCheck:  model.HealthCheck{Type: "http", Method: "GET", Path: "/", ExpectStatus: "2xx"},
		TLSReencrypt: u.Scheme == "https", TLSVerify: u.Scheme == "https" && prev.UpstreamTLSVerify,
		Retries: 3, Source: model.SourceManual,
	}
	normalizeBackend(snap.HAProxy, nil, &backend)
	if err := backend.Validate(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	backend.ID = store.NewID()

	port, err := h.freeExposePort(ctx, snap, snap.HAProxy.ExposePortStart)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	snapWith := *snap
	snapWith.Backends = append(append([]model.Backend{}, snap.Backends...), backend)
	fe := model.Frontend{
		Name: uniqueFrontendName(snap, "fe-"+name), Mode: "http", Bind: fmt.Sprintf("127.0.0.1:%d", port),
		Rules: []model.FrontendRule{}, DefaultBackendID: backend.ID, Enabled: true, Source: model.SourceExpose, HostID: prev.ID,
	}
	if err := mergeErrs(fe.Validate(), checkFrontend(h.app, &snapWith, &fe)); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	next := *prev
	next.Upstream = model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: port, Path: u.Path, BackendID: backend.ID}
	next.UpstreamTLSVerify = false
	if hk := httpx.HostHooks.BeforeSave; hk != nil {
		if err := hk(r, prev, &next); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}
	if v, ok := any(&next).(model.Validator); ok {
		if err := v.Validate(); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}

	if err := h.app.Store.Backends().Create(ctx, &backend); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.app.Store.Frontends().Create(ctx, &fe); err != nil {
		_ = h.app.Store.Backends().Delete(context.WithoutCancel(ctx), backend.ID)
		httpx.Fail(w, r, err)
		return
	}
	if err := h.app.Store.Hosts().Update(ctx, &next); err != nil {
		_ = h.app.Store.Frontends().Delete(context.WithoutCancel(ctx), fe.ID)
		_ = h.app.Store.Backends().Delete(context.WithoutCancel(ctx), backend.ID)
		httpx.Fail(w, r, err)
		return
	}
	domain := firstDomain(prev.Domains)
	h.app.Audit(ctx, core.AuditEntry{Action: "backend.create", Target: backend.Name, Detail: "converted from host " + domain + " (" + fmt.Sprintf("%s:%d", u.Host, u.Port) + ")", Result: "saved"})
	h.app.Changed(ctx, model.KindBackend, backend.ID, backend.Name, core.ActionCreated)
	h.app.Changed(ctx, model.KindFrontend, fe.ID, fe.Name, core.ActionCreated)
	h.app.Changed(ctx, model.KindHost, next.ID, domain, core.ActionUpdated)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"backend": backend, "frontend": fe, "host": next})
}

// certProviderFor mirrors the certs slice: the provider a newly requested
// certificate gets under the current TLS settings.
func certProviderFor(t model.TLSSettings) string {
	switch t.ACMEProvider {
	case model.CertLetsEncryptStaging:
		return model.CertLetsEncryptStaging
	case model.ACMEProviderCustom:
		return model.CertACME
	}
	return model.CertLetsEncrypt
}
